package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type PaymentMethodStatus struct {
	CompletedAt pgtype.Timestamptz `json:"completed_at"`
	Mode        string             `json:"mode"`
	Status      string             `json:"status"`
}

// PaymentUpdate either redirects to hosted setup or reports an update that
// finished before a new one could begin.
type PaymentUpdate struct {
	CompletedAt *pgtype.Timestamptz `json:"completed_at,omitempty"`
	Status      string              `json:"status,omitempty"`
	URL         *string             `json:"url"`
}

func (s RecurringService) PaymentMethodStatus(ctx context.Context, token string) (PaymentMethodStatus, error) {
	return donorRead(ctx, s.DB, token, func(q *queries.Queries, d queries.Donor, _ time.Time) (PaymentMethodStatus, error) {
		row, err := q.LatestPaymentUpdate(ctx, d.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			return PaymentMethodStatus{Mode: "test", Status: "NONE"}, nil
		}
		if err != nil {
			return PaymentMethodStatus{}, err
		}
		return PaymentMethodStatus{Mode: "test", Status: row.Status, CompletedAt: row.CompletedAt}, nil
	})
}

// paymentSetupPrefetch is the processor state BeginPaymentUpdate may need,
// fetched before the donor lock is taken.
type paymentSetupPrefetch struct {
	unfinished   *paymentUpdateFacts
	subscription *subscriptionSnapshot
}

func (s RecurringService) prefetchPaymentSetup(ctx context.Context, token string, requestID pgtype.UUID) (paymentSetupPrefetch, error) {
	var out paymentSetupPrefetch
	q := queries.New(s.DB)
	now := time.Now().UTC()
	d, err := authenticate(ctx, q, token, now)
	if err != nil {
		return out, err
	}
	if _, err = q.PaymentUpdateSnapshot(ctx, requestID); !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	row, err := q.UnfinishedPaymentUpdate(ctx, d.ID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	if err == nil {
		if !row.StripeSessionID.Valid {
			return out, nil
		}
		facts, err := fetchPaymentUpdate(ctx, q, s.Gateway, row, "", now)
		if err != nil {
			return out, err
		}
		out.unfinished = &facts
		if facts.status != "EXPIRED" {
			return out, nil
		}
	}
	out.subscription, err = s.prefetchSubscription(ctx, d.ID)
	return out, err
}

// BeginPaymentUpdate returns hosted card setup for the donor's monthly gift.
// The update commits under the donor lock, its session is created without
// that lock, and the session is then recorded. An update already in progress
// is resumed, and one that finished meanwhile is reported instead.
func (s RecurringService) BeginPaymentUpdate(ctx context.Context, token string, requestID pgtype.UUID) (PaymentUpdate, error) {
	if !ValidRequestID(requestID) {
		return PaymentUpdate{}, payments.ErrFacts
	}
	site, err := site(s.Origin)
	if err != nil {
		return PaymentUpdate{}, err
	}
	return withFreshSnapshot(
		func() (paymentSetupPrefetch, error) { return s.prefetchPaymentSetup(ctx, token, requestID) },
		func(prefetch paymentSetupPrefetch) (PaymentUpdate, error) {
			if s.Gateway == nil {
				return PaymentUpdate{}, payments.ErrUnconfigured
			}
			owner := donorOwner(token)
			var updateID pgtype.UUID
			finished, err := donorWrite(ctx, s.DB, owner, func(tx pgx.Tx, d queries.Donor, now time.Time) (*PaymentUpdate, error) {
				id, finished, err := s.stagePaymentUpdate(ctx, queries.New(tx), d, requestID, prefetch, site, now)
				updateID = id
				return finished, err
			})
			if err != nil {
				return PaymentUpdate{}, err
			}
			if finished != nil {
				return *finished, nil
			}
			return s.openPaymentSession(ctx, owner, updateID)
		}, ErrSubscriptionConflict,
	)
}

// stagePaymentUpdate returns the update to open: requestID's own, the
// donor's unfinished one, or a new one. It settles an unfinished update the
// processor has completed, returning its outcome instead.
func (s RecurringService) stagePaymentUpdate(
	ctx context.Context,
	q *queries.Queries,
	d queries.Donor,
	requestID pgtype.UUID,
	prefetch paymentSetupPrefetch,
	site publicSite,
	now time.Time,
) (pgtype.UUID, *PaymentUpdate, error) {
	row, err := q.PaymentUpdateByID(ctx, requestID)
	if err == nil {
		if row.DonorID != d.ID || terminalPaymentUpdate(row.Status) {
			return pgtype.UUID{}, nil, ErrSubscriptionConflict
		}
		return row.ID, nil, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, nil, err
	}
	row, err = q.UnfinishedPaymentUpdate(ctx, d.ID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, nil, err
	}
	if err == nil {
		if !row.StripeSessionID.Valid {
			return row.ID, nil, nil
		}
		facts := prefetch.unfinished
		if facts == nil || facts.update != row.ID {
			return pgtype.UUID{}, nil, errRefetch
		}
		status, err := applyPaymentUpdate(ctx, q, row, *facts, now)
		if err != nil {
			return pgtype.UUID{}, nil, err
		}
		if status == "APPLIED" || status == "CANCELLED" {
			if row, err = q.PaymentUpdateByID(ctx, row.ID); err != nil {
				return pgtype.UUID{}, nil, err
			}
			return pgtype.UUID{}, &PaymentUpdate{Status: row.Status, CompletedAt: &row.CompletedAt}, nil
		}
		if status != "EXPIRED" {
			return row.ID, nil, nil
		}
	}
	return requestID, nil, s.insertPaymentUpdate(ctx, q, d, requestID, prefetch.subscription, site, now)
}

// insertPaymentUpdate records a new card setup for the donor's live
// subscription, with the hosted session parameters it will be opened with.
func (s RecurringService) insertPaymentUpdate(
	ctx context.Context,
	q *queries.Queries,
	d queries.Donor,
	requestID pgtype.UUID,
	snapshot *subscriptionSnapshot,
	site publicSite,
	now time.Time,
) error {
	attempt, err := q.LatestMonthlyAttempt(ctx, d.ID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !attempt.StripeSubscriptionID.Valid) {
		return ErrSubscriptionConflict
	}
	if err != nil {
		return err
	}
	remote, err := lockedSubscription(ctx, q, attempt, snapshot)
	if err != nil {
		return err
	}
	metadata, err := remote.Object("metadata")
	customer := remote.Text("customer")
	if err != nil || metadata.Text("dad_checkout_id") != attempt.CheckoutID.String() ||
		remote.Text("id") != attempt.StripeSubscriptionID.String ||
		!remote.BoolIs("livemode", false) ||
		!strings.HasPrefix(customer, "cus_") {
		return payments.ErrFacts
	}
	if !renews(remote.Text("status")) {
		return ErrSubscriptionConflict
	}
	marker := map[string]string{"dad_payment_update_id": requestID.String()}
	params, err := json.Marshal(
		map[string]any{
			"mode":                         "setup",
			"customer":                     customer,
			"allowed_payment_method_types": []string{"card"},
			"metadata":                     marker,
			"setup_intent_data":            map[string]any{"metadata": marker},
			"success_url":                  site.paymentMethod(),
			"cancel_url":                   site.paymentMethod(),
			"expires_at":                   now.Add(time.Hour).Unix(),
		},
	)
	if err != nil {
		return err
	}
	return q.InsertPaymentUpdate(
		ctx,
		queries.InsertPaymentUpdateParams{
			ID:            requestID,
			DonorID:       d.ID,
			AttemptID:     attempt.ID,
			CustomerID:    customer,
			RequestParams: params,
			CreatedAt:     timestamp(now),
		},
	)
}

// openPaymentSession creates the update's hosted session, unless it already
// has one, and records it. A session is not created in the last half hour
// before the frozen parameters expire.
func (s RecurringService) openPaymentSession(ctx context.Context, owner ownerResolver, id pgtype.UUID) (PaymentUpdate, error) {
	row, err := queries.New(s.DB).PaymentUpdateSnapshot(ctx, id)
	if err != nil {
		return PaymentUpdate{}, err
	}
	if terminalPaymentUpdate(row.Status) {
		return PaymentUpdate{}, ErrSubscriptionConflict
	}
	if row.HostedUrl.Valid {
		return PaymentUpdate{URL: &row.HostedUrl.String}, nil
	}
	params, err := payments.DecodeObject(row.RequestParams)
	if err != nil {
		return PaymentUpdate{}, err
	}
	expires, err := params.Int("expires_at")
	if err != nil {
		return PaymentUpdate{}, err
	}
	if time.Now().Unix() >= expires-1800 {
		return PaymentUpdate{}, ErrSubscriptionConflict
	}
	remote, err := s.Gateway.CreateCheckout(ctx, row.RequestParams, "dad-payment-setup-"+row.ID.String())
	if err != nil {
		return PaymentUpdate{}, err
	}
	if err = payments.ValidateRemoteCheckout(remote); err != nil {
		return PaymentUpdate{}, err
	}
	return donorWrite(ctx, s.DB, owner, func(tx pgx.Tx, d queries.Donor, _ time.Time) (PaymentUpdate, error) {
		q := queries.New(tx)
		row, err := q.PaymentUpdateByID(ctx, id)
		if err != nil {
			return PaymentUpdate{}, err
		}
		if row.DonorID != d.ID || terminalPaymentUpdate(row.Status) {
			return PaymentUpdate{}, ErrSubscriptionConflict
		}
		if row.StripeSessionID.Valid && row.StripeSessionID.String != remote.ID {
			return PaymentUpdate{}, payments.ErrFacts
		}
		if row.HostedUrl.Valid {
			return PaymentUpdate{URL: &row.HostedUrl.String}, nil
		}
		err = q.SavePaymentUpdate(
			ctx,
			queries.SavePaymentUpdateParams{
				ID:              row.ID,
				StripeSessionID: text(remote.ID),
				Status:          "OPEN",
				HostedUrl:       text(remote.URL),
				CompletedAt:     row.CompletedAt,
			},
		)
		if err != nil {
			return PaymentUpdate{}, err
		}
		return PaymentUpdate{URL: &remote.URL}, nil
	})
}
