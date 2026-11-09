package store

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"

	"dad/donor-api/internal/credentials"
	"dad/donor-api/internal/geo"
	"dad/donor-api/internal/identity"
	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type CheckoutService struct {
	DB      *pgxpool.Pool
	Gateway payments.CheckoutGateway
	Origin  string
	// NativeReturnOrigin is the origin the donor apps' hosted checkout returns
	// to; it defaults to Origin.
	NativeReturnOrigin string
	// FoundingClaims matches a giver's address to the website waitlist (D-6).
	FoundingClaims FoundingClaimKey
}

// BeginCheckout returns a hosted checkout for the verified, signed-in donor.
// Frozen processor parameters commit before the processor is called, and
// a retry with the same request ID returns the same session.
func (s CheckoutService) BeginCheckout(ctx context.Context, request payments.CheckoutRequest) (payments.CheckoutResult, error) {
	hosted, err := s.begin(ctx, request)
	if err != nil {
		return payments.CheckoutResult{}, err
	}
	return payments.CheckoutResult{CheckoutID: hosted.checkoutID, URL: hosted.url}, nil
}

// BeginNativeCheckout opens a hosted monthly checkout for the signed-in donor
// behind token, whose account supplies its verified email. It is
// the same checkout the website begins, so the processor's webhooks complete
// the gift the same way; only the pages it returns to differ. A referral
// visitor token the app captured credits its referrer as the web's cookie does.
func (s CheckoutService) BeginNativeCheckout(
	ctx context.Context,
	token string,
	requestID pgtype.UUID,
	amount int64,
	visitor *string,
) (payments.NativeCheckoutResult, error) {
	empty := payments.NativeCheckoutResult{}
	if s.Gateway == nil {
		return empty, payments.ErrUnconfigured
	}
	origin := cmp.Or(s.NativeReturnOrigin, s.Origin)
	if _, err := site(origin); err != nil {
		return empty, err
	}
	donor, err := authenticate(ctx, queries.New(s.DB), token, time.Now().UTC())
	if err != nil {
		return empty, err
	}
	hosted, err := s.begin(ctx, payments.CheckoutRequest{
		Session:      token,
		RequestID:    requestID,
		Email:        donor.Email,
		Amount:       amount,
		Frequency:    "MONTHLY",
		NativeReturn: origin,
		Visitor:      visitor,
	})
	if err != nil {
		return empty, err
	}
	return payments.NativeCheckoutResult{ExpiresAt: hosted.expiresAt, SessionID: hosted.sessionID, URL: hosted.url}, nil
}

// hostedCheckout is a checkout whose processor session is recorded.
type hostedCheckout struct {
	checkoutID string
	sessionID  string
	url        string
	expiresAt  time.Time
}

// checkoutExpiry is when the frozen parameters let the hosted checkout lapse.
func checkoutExpiry(params []byte) (time.Time, error) {
	object, err := payments.DecodeObject(params)
	if err != nil {
		return time.Time{}, err
	}
	return object.Unix("expires_at")
}

func (s CheckoutService) begin(ctx context.Context, request payments.CheckoutRequest) (hostedCheckout, error) {
	empty := hostedCheckout{}
	if s.Gateway == nil {
		return empty, payments.ErrUnconfigured
	}
	site, err := site(s.Origin)
	if err != nil {
		return empty, err
	}
	if !ValidRequestID(request.RequestID) || utf8.RuneCountInString(request.Email) > 320 || !ValidGiftAmount(request.Amount) {
		return empty, payments.ErrCheckoutInput
	}
	email, err := identity.NormalizeEmail(request.Email)
	if err != nil {
		return empty, payments.ErrCheckoutInput
	}
	request.Email = email
	preimage, err := credentials.CheckoutPreimage(email, request.Amount, request.Frequency, request.Visitor)
	if err != nil {
		return empty, payments.ErrCheckoutInput
	}
	attempt, err := s.prepare(ctx, site, request, HashToken(preimage), time.Now().UTC())
	if err != nil {
		return empty, err
	}
	expires, err := checkoutExpiry(attempt.RequestParams)
	if err != nil {
		return empty, err
	}
	hosted := hostedCheckout{checkoutID: attempt.CheckoutID.String(), expiresAt: expires}
	if attempt.HostedUrl.Valid {
		hosted.sessionID, hosted.url = attempt.StripeSessionID.String, attempt.HostedUrl.String
		return hosted, nil
	}
	remote, err := s.Gateway.CreateCheckout(ctx, attempt.RequestParams, "dad-checkout-"+request.RequestID.String())
	if err != nil {
		return empty, payments.ErrProcessorUnavailable
	}
	if err = payments.ValidateRemoteCheckout(remote); err != nil {
		return empty, err
	}
	err = pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		q := queries.New(tx)
		current, err := q.LockStripeCheckout(ctx, request.RequestID)
		if err != nil {
			return err
		}
		if current.StripeSessionID.Valid && current.StripeSessionID.String != remote.ID {
			return payments.ErrCheckoutConflict
		}
		return q.AcknowledgeCheckout(
			ctx,
			queries.AcknowledgeCheckoutParams{ID: request.RequestID, StripeSessionID: text(remote.ID), HostedUrl: text(remote.URL)},
		)
	})
	if err != nil {
		return empty, err
	}
	hosted.sessionID, hosted.url = remote.ID, remote.URL
	return hosted, nil
}

// prepare records the authenticated donor's checkout with any valid referral
// attribution, and the attempt's frozen processor parameters, or returns the
// attempt already recorded for request. A monthly gift reserves the donor's
// one recurring slot.
func (s CheckoutService) prepare(
	ctx context.Context,
	site publicSite,
	request payments.CheckoutRequest,
	digest string,
	now time.Time,
) (queries.DonorStripeCheckout, error) {
	empty := queries.DonorStripeCheckout{}
	for range 3 {
		attempt, err := transact(ctx, s.DB, pgx.TxOptions{}, func(tx pgx.Tx) (queries.DonorStripeCheckout, error) {
			return prepareAttempt(ctx, queries.New(tx), s.FoundingClaims, site, request, digest, now)
		})
		if reused, ok := errors.AsType[openCheckout](err); ok {
			return reused.attempt, nil
		}
		stale, ok := errors.AsType[staleCheckout](err)
		if !ok {
			return attempt, err
		}
		if err = s.retire(ctx, stale.attempt); err != nil {
			return empty, err
		}
	}
	return empty, payments.ErrCheckoutConflict
}

// openCheckout is returned out of prepareAttempt, rolling back the request's
// own rows, when the donor's earlier unpaid native checkout already answers
// it: same amount, same return pages, and time left to pay.
type openCheckout struct{ attempt queries.DonorStripeCheckout }

func (openCheckout) Error() string { return "an equivalent checkout is already open" }

// staleCheckout is returned the same way when the donor's earlier unpaid
// checkout holds the recurring slot but cannot answer the request, so
// it must be expired at the processor before the slot is free.
type staleCheckout struct{ attempt queries.DonorStripeCheckout }

func (staleCheckout) Error() string { return "an unpaid checkout holds the recurring slot" }

// reuseMargin is the time an open checkout must have left to be handed back.
const reuseMargin = 5 * time.Minute

// restartBudget caps the hosted checkouts, counting the new one, begun for one donor in restartWindow
// before a website request may expire another open one. The website cannot
// tell the donor from someone who typed their email, so this bounds how often
// a stranger can make the processor close the donor's checkout.
//
// A checkout begun within restartGrace is never expired by another website
// request. Requests racing to restart would otherwise keep expiring each
// other's replacement and leave nothing open; a person abandoning a page
// needs longer than that to come back.
const (
	restartBudget = 10
	restartWindow = time.Hour
	restartGrace  = 15 * time.Second
)

// heldCheckout decides what the recurring slot's holder means for a request:
// the holder itself, a holder to expire, or a conflict. Only an open, unpaid
// checkout is ever touched; a paid or pending one is the conflict it always
// was. A signed-in app request may be handed its own open session back if it
// credits the same referrer as the request would. A website request never is:
// its caller is whoever typed the email, and the session's URL pays on the
// donor's behalf, so it is expired and replaced.
func heldCheckout(
	ctx context.Context,
	q *queries.Queries,
	donor queries.Donor,
	request payments.CheckoutRequest,
	referrer pgtype.UUID,
	now time.Time,
) error {
	held, err := q.ReservedAttempt(ctx, donor.ID)
	if err != nil || held.Status != "OPEN" || !held.HostedUrl.Valid || !held.StripeSessionID.Valid {
		return payments.ErrCheckoutConflict
	}
	if request.NativeReturn == "" {
		if now.Before(held.CreatedAt.Time.Add(restartGrace)) {
			return payments.ErrCheckoutConflict
		}
		recent, err := q.CountRecentCheckoutAttempts(
			ctx,
			queries.CountRecentCheckoutAttemptsParams{DonorID: donor.ID, CreatedAt: timestamp(now.Add(-restartWindow))},
		)
		if err != nil {
			return err
		}
		if recent > restartBudget {
			return payments.ErrCheckoutThrottled
		}
		return staleCheckout{held}
	}
	draft, err := q.CheckoutDraft(ctx, held.CheckoutID)
	if err != nil {
		return err
	}
	object, err := payments.DecodeObject(held.RequestParams)
	if err != nil {
		return err
	}
	expires, err := object.Unix("expires_at")
	if err != nil {
		return err
	}
	sameReturn := object.Text("success_url") == publicSite(request.NativeReturn).nativeCheckoutReturn()
	if draft.DonorID == donor.ID && draft.AmountMinor == request.Amount && draft.ReferrerID == referrer && sameReturn &&
		expires.After(now.Add(reuseMargin)) {
		return openCheckout{held}
	}
	return staleCheckout{held}
}

// retire expires an unpaid checkout at the processor, then frees its
// reservation. No database lock is held across the processor call, so a
// failure leaves the reservation untouched and a retry finds it again. If the
// donor paid first the gift is active, and the webhook records it.
func (s CheckoutService) retire(ctx context.Context, stale queries.DonorStripeCheckout) error {
	status, err := s.Gateway.ExpireCheckout(ctx, stale.StripeSessionID.String, "dad-checkout-expire-"+stale.ID.String())
	if err != nil {
		return payments.ErrProcessorUnavailable
	}
	if status != "expired" {
		return payments.ErrCheckoutConflict
	}
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		q := queries.New(tx)
		current, err := q.LockStripeCheckout(ctx, stale.ID)
		if err != nil {
			return err
		}
		if current.Status == "COMPLETE" {
			return payments.ErrCheckoutConflict
		}
		if current.Status == "OPEN" {
			err = q.RecordCheckoutSession(
				ctx,
				queries.RecordCheckoutSessionParams{ID: current.ID, StripeSessionID: current.StripeSessionID, Status: "EXPIRED"},
			)
			if err != nil {
				return err
			}
		}
		return q.ReleaseReservation(ctx, current.ID)
	})
}

func prepareAttempt(
	ctx context.Context,
	q *queries.Queries,
	claims FoundingClaimKey,
	site publicSite,
	request payments.CheckoutRequest,
	digest string,
	now time.Time,
) (queries.DonorStripeCheckout, error) {
	empty := queries.DonorStripeCheckout{}
	user, err := lockSignedIn(ctx, q, request.Session)
	if err != nil {
		return empty, err
	}
	// Never resolve a caller-supplied address before proving it belongs to this session.
	if request.Email != user.Email {
		return empty, ErrDenied
	}
	donor := user.Donor
	if err = claims.grant(ctx, q, donor.ID, donor.Email, now); err != nil {
		return empty, err
	}
	projection, err := NewID()
	if err != nil {
		return empty, err
	}
	if err = q.SeedProjection(ctx, queries.SeedProjectionParams{ID: projection, DonorID: donor.ID, ComputedAt: timestamp(now)}); err != nil {
		return empty, err
	}
	if request.Name != "" {
		zone, e := geo.Timezone(request.Zip)
		if e != nil || len(request.Name) > 150 || !plainText(request.Name, false) || len(request.City) > 80 ||
			!plainText(request.City, false) {
			return empty, payments.ErrCheckoutInput
		}
		if e = q.SaveSignupProfile(ctx, queries.SaveSignupProfileParams{ID: donor.ID, FullName: text(request.Name), Zip: text(request.Zip), City: text(request.City), Timezone: text(zone)}); e != nil {
			return empty, e
		}
	}
	attempt, err := q.StripeCheckout(ctx, request.RequestID)
	if err == nil {
		draft, e := q.CheckoutDraft(ctx, attempt.CheckoutID)
		if e != nil {
			return empty, e
		}
		if draft.DonorID != donor.ID {
			return empty, ErrDenied
		}
		if attempt.RequestHash != digest || attempt.Status == "EXPIRED" || attempt.Status == "CANCELLED" || attempt.Status == "COMPLETE" {
			return empty, payments.ErrCheckoutConflict
		}
		if !attempt.HostedUrl.Valid && !now.Before(attempt.CreatedAt.Time.Add(commandRetryWindow)) {
			return empty, payments.ErrCheckoutConflict
		}
		return attempt, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return empty, err
	}
	draft, err := checkoutDraft(ctx, q, donor, request, now)
	if err != nil {
		return empty, err
	}
	if err = q.InsertCheckout(ctx, draft); err != nil {
		return empty, err
	}
	var journey pgtype.UUID
	if journey.Scan(request.Journey) == nil && journey.Valid {
		if err = q.AttachCheckoutJourney(ctx, queries.AttachCheckoutJourneyParams{ID: draft.ID, JourneyID: journey}); err != nil {
			return empty, err
		}
	}
	raw, err := checkoutParams(site, donor.Email, draft, request)
	if err != nil {
		return empty, err
	}
	if err = q.InsertStripeCheckout(ctx, queries.InsertStripeCheckoutParams{ID: request.RequestID, CheckoutID: draft.ID, RequestHash: digest, RequestParams: raw, CreatedAt: timestamp(now)}); err != nil {
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == uniqueViolation {
			return empty, payments.ErrCheckoutConflict
		}
		return empty, err
	}
	if request.Frequency == "MONTHLY" {
		reservation, err := NewID()
		if err != nil {
			return empty, err
		}
		if _, err = q.ReserveRecurring(ctx, queries.ReserveRecurringParams{ID: reservation, DonorID: donor.ID, AttemptID: request.RequestID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return empty, heldCheckout(ctx, q, donor, request, draft.ReferrerID, now)
			}
			return empty, err
		}
	}
	return q.StripeCheckout(ctx, request.RequestID)
}

// lockGuest returns the donor for email, created as an unclaimed guest if
// new, locked, granted any waitlist claim on the address, and with a
// projection row for the refresh worker to find.
func lockGuest(ctx context.Context, q *queries.Queries, claims FoundingClaimKey, email string, now time.Time) (queries.Donor, error) {
	id, err := NewID()
	if err != nil {
		return queries.Donor{}, err
	}
	if err = reserveEmailIdentity(ctx, q, email, identityDonor, pgtype.UUID{}); err != nil {
		return queries.Donor{}, err
	}
	if err = q.InsertGuest(ctx, queries.InsertGuestParams{ID: id, Email: email}); err != nil {
		return queries.Donor{}, err
	}
	donor, err := q.DonorByEmail(ctx, email)
	if err != nil {
		return queries.Donor{}, err
	}
	if donor, err = q.LockDonor(ctx, donor.ID); err != nil {
		return queries.Donor{}, err
	}
	if donor.Disabled {
		return queries.Donor{}, ErrDenied
	}
	if err = claims.grant(ctx, q, donor.ID, email, now); err != nil {
		return queries.Donor{}, err
	}
	projection, err := NewID()
	if err != nil {
		return queries.Donor{}, err
	}
	return donor, q.SeedProjection(ctx, queries.SeedProjectionParams{ID: projection, DonorID: donor.ID, ComputedAt: timestamp(now)})
}

// checkoutDraft is a new checkout carrying the visitor's referral, if its
// window is still open and the referrer is not the donor.
func checkoutDraft(
	ctx context.Context,
	q *queries.Queries,
	donor queries.Donor,
	request payments.CheckoutRequest,
	now time.Time,
) (queries.InsertCheckoutParams, error) {
	id, err := NewID()
	if err != nil {
		return queries.InsertCheckoutParams{}, err
	}
	draft := queries.InsertCheckoutParams{
		ID:          id,
		DonorID:     donor.ID,
		AmountMinor: request.Amount,
		Frequency:   request.Frequency,
		CreatedAt:   timestamp(now),
	}
	if request.Visitor == nil || *request.Visitor == "" {
		return draft, nil
	}
	touch, err := q.ValidReferralTouch(
		ctx,
		queries.ValidReferralTouchParams{TokenHash: HashToken(*request.Visitor), ExpiresAt: timestamp(now), ID: donor.ID},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return draft, nil
	}
	if err != nil {
		return queries.InsertCheckoutParams{}, err
	}
	draft.ReferrerID, draft.ReferralKind, draft.ReferralExpiresAt = touch.ReferrerID, text(touch.Kind), touch.ExpiresAt
	return draft, nil
}

// checkoutLifetime is how long a hosted checkout stays open. An unpaid monthly
// checkout holds the donor's recurring reservation until it expires, so it is
// kept short; Stripe accepts no less than 30 minutes, and the margin lets a
// retried request still meet that.
const checkoutLifetime = time.Hour

// checkoutParams are the hosted checkout parameters frozen with an attempt,
// so a retried request sends the processor exactly the same thing.
func checkoutParams(site publicSite, email string, draft queries.InsertCheckoutParams, request payments.CheckoutRequest) ([]byte, error) {
	checkoutID := draft.ID
	metadata := map[string]string{"dad_checkout_id": checkoutID.String()}
	if draft.ReferrerID.Valid {
		metadata["dad_referrer_id"] = draft.ReferrerID.String()
		metadata["dad_referral_kind"] = draft.ReferralKind.String
	}
	price := map[string]any{
		"currency":     "usd",
		"unit_amount":  request.Amount,
		"product_data": map[string]string{"name": "DAD monthly giving"},
		"recurring":    map[string]string{"interval": "month"},
	}
	success, cancel := site.checkoutReturn(request.Amount, request.Frequency), site.donate()
	if request.NativeReturn != "" {
		native := publicSite(request.NativeReturn)
		success, cancel = native.nativeCheckoutReturn(), native.nativeCheckoutCancel()
	}
	params := map[string]any{
		"mode":                         "subscription",
		"customer_email":               email,
		"client_reference_id":          checkoutID.String(),
		"metadata":                     metadata,
		"line_items":                   []any{map[string]any{"price_data": price, "quantity": 1}},
		"allowed_payment_method_types": []string{"card"},
		"success_url":                  success,
		"cancel_url":                   cancel,
		"expires_at":                   draft.CreatedAt.Time.Add(checkoutLifetime).Unix(),
		"subscription_data":            map[string]any{"metadata": metadata},
	}
	return json.Marshal(params)
}
