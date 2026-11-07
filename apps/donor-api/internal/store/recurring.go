package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrSubscriptionConflict = errors.New("billing details changed or require reconciliation; refresh before confirming")

type RecurringService struct {
	DB      *pgxpool.Pool
	Gateway payments.SubscriptionGateway
	Origin  string
}

type CancellationRequest struct {
	RequestID, AttemptID pgtype.UUID
	PeriodEnd            time.Time
}

type AmountRequest struct {
	RequestID, AttemptID pgtype.UUID
	Amount, Previous     int64
	EffectiveAt          time.Time
}

// RecurringDetails describes the donor's monthly gift. State is omitted only by
// a staff quote for a donor without one.
type RecurringDetails struct {
	Mode string `json:"mode"`
	// ObservedAt is when the subscription was last synced from the processor.
	ObservedAt   *time.Time    `json:"observed_at,omitempty"`
	State        string        `json:"state,omitempty"`
	Subscription *Subscription `json:"subscription"`
}

type Subscription struct {
	AmountMinor int64      `json:"amount_minor"`
	Currency    string     `json:"currency"`
	ID          string     `json:"id"`
	PeriodEnd   time.Time  `json:"period_end"`
	Status      string     `json:"status"`
	StopsAt     *time.Time `json:"stops_at"`
}

type AmountChange struct {
	AmountMinor   int64     `json:"amount_minor"`
	Confirmed     bool      `json:"confirmed"`
	EffectiveAt   time.Time `json:"effective_at"`
	PreviousMinor int64     `json:"previous_minor"`
	RequestID     string    `json:"request_id"`
}

func ownedAttempt(ctx context.Context, q *queries.Queries, donor, id pgtype.UUID) (queries.DonorStripeCheckout, error) {
	row, err := q.OwnedMonthlyAttempt(ctx, queries.OwnedMonthlyAttemptParams{DonorID: donor, ID: id})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !row.StripeSubscriptionID.Valid) {
		return row, ErrSubscriptionConflict
	}
	return row, err
}

func nextAmount(ctx context.Context, q *queries.Queries, draft queries.DonorCheckout) (int64, error) {
	amount, err := q.LatestConfirmedAmount(ctx, draft.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return draft.AmountMinor, nil
	}
	return amount, err
}

func subscriptionView(
	ctx context.Context,
	q *queries.Queries,
	attempt queries.DonorStripeCheckout,
	row queries.DonorSubscription,
) (RecurringDetails, error) {
	draft, err := q.CheckoutDraft(ctx, attempt.CheckoutID)
	if err != nil {
		return RecurringDetails{}, err
	}
	amount, err := nextAmount(ctx, q, draft)
	if err != nil {
		return RecurringDetails{}, err
	}
	sub := &Subscription{ID: attempt.ID.String(), AmountMinor: amount, Currency: "USD", Status: row.Status, PeriodEnd: row.PeriodEnd.Time}
	if row.StopsAt.Valid {
		sub.StopsAt = &row.StopsAt.Time
	}
	return RecurringDetails{Mode: "test", ObservedAt: &row.ObservedAt.Time, State: "ready", Subscription: sub}, nil
}

// syncedSubscription returns the donor's latest monthly attempt and its
// subscription as last synced from the processor, which is nil until the
// attempt has a subscription and that has been synced.
func syncedSubscription(
	ctx context.Context,
	q *queries.Queries,
	donor pgtype.UUID,
) (queries.DonorStripeCheckout, *queries.DonorSubscription, error) {
	attempt, err := q.LatestMonthlyAttemptSnapshot(ctx, donor)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !attempt.StripeSubscriptionID.Valid) {
		return attempt, nil, nil
	}
	if err != nil {
		return attempt, nil, err
	}
	row, err := q.SubscriptionForCheckout(ctx, attempt.CheckoutID)
	if errors.Is(err, pgx.ErrNoRows) {
		return attempt, nil, nil
	}
	return attempt, &row, err
}

// storedDetails describes the donor's monthly gift as last synced. A started
// monthly gift is preparing until its subscription is first synced.
func storedDetails(ctx context.Context, q *queries.Queries, donor pgtype.UUID) (RecurringDetails, error) {
	attempt, row, err := syncedSubscription(ctx, q, donor)
	if err != nil {
		return RecurringDetails{}, err
	}
	if row == nil {
		state := "preparing"
		if !attempt.ID.Valid {
			state = "ready"
		}
		return RecurringDetails{Mode: "test", State: state}, nil
	}
	return subscriptionView(ctx, q, attempt, *row)
}

// prefetchSubscription reads the donor's latest subscription from the processor
// before any lock is taken.
func (s RecurringService) prefetchSubscription(ctx context.Context, donor pgtype.UUID) (*subscriptionSnapshot, error) {
	q := queries.New(s.DB)
	attempt, err := q.LatestMonthlyAttemptSnapshot(ctx, donor)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !attempt.StripeSubscriptionID.Valid) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return fetchSubscription(ctx, q, newSubFetcher(s.Gateway), attempt.ID, attempt.StripeSubscriptionID.String)
}

// refresh syncs the latest subscription of owner's donor before a command,
// reading the processor with no lock held. The sync commits on its own, so a
// command refused because the stored state was behind still leaves the next
// view current. The snapshot returned is the state saved.
func (s RecurringService) refresh(ctx context.Context, owner ownerResolver, donor pgtype.UUID) (*subscriptionSnapshot, error) {
	snapshot, err := s.prefetchSubscription(ctx, donor)
	if err != nil || snapshot == nil {
		return nil, err
	}
	return donorWrite(ctx, s.DB, owner, func(tx pgx.Tx, d queries.Donor, now time.Time) (*subscriptionSnapshot, error) {
		attempt, ok, err := latestSubscription(ctx, queries.New(tx), d.ID)
		if err != nil || !ok {
			return nil, err
		}
		row, remote, err := s.read(ctx, tx, attempt, now, viewOnly, snapshot)
		if err != nil {
			return nil, err
		}
		if _, err = refreshProjection(ctx, tx, d.ID, now); err != nil {
			return nil, err
		}
		return &subscriptionSnapshot{
			attempt:  attempt.ID,
			id:       attempt.StripeSubscriptionID.String,
			observed: row.ObservedAt,
			remote:   remote,
		}, nil
	})
}

// donorRefresh is refresh for the donor signed in with token.
func (s RecurringService) donorRefresh(ctx context.Context, token string) (*subscriptionSnapshot, error) {
	d, err := authenticate(ctx, queries.New(s.DB), token, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	return s.refresh(ctx, donorOwner(token), d.ID)
}

// Recurring commands lease their subscription while a processor request is
// outstanding, so a second command cannot send a conflicting mutation once the
// donor lock is released. A request is bounded well inside the lease, so an
// expired lease has no request still in flight.
const (
	leaseTerm    = 2 * time.Minute
	submitWindow = time.Minute
)

type subscriptionLease struct {
	attempt, command pgtype.UUID
	subscription     string
}

// Even an identical retry must wait: otherwise it can release the lease while
// the original request is still in flight. Crash recovery waits for expiry.
func leaseSubscription(
	ctx context.Context,
	q *queries.Queries,
	donor pgtype.UUID,
	attempt queries.DonorStripeCheckout,
	command pgtype.UUID,
	now time.Time,
) (*subscriptionLease, error) {
	n, err := q.AcquireSubscriptionLease(
		ctx,
		queries.AcquireSubscriptionLeaseParams{
			AttemptID: attempt.ID,
			DonorID:   donor,
			CommandID: command,
			ExpiresAt: timestamp(now.Add(leaseTerm)),
			Now:       timestamp(now),
		},
	)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, ErrSubscriptionConflict
	}
	return &subscriptionLease{attempt: attempt.ID, command: command, subscription: attempt.StripeSubscriptionID.String}, nil
}

func releaseLease(ctx context.Context, q *queries.Queries, attempt, command pgtype.UUID) error {
	return q.ReleaseSubscriptionLease(ctx, queries.ReleaseSubscriptionLeaseParams{AttemptID: attempt, CommandID: command})
}

// leasedCommand runs a recurring command in the three steps every processor
// mutation takes. prepare validates and records the command under the donor
// lock, returning either the command's settled result or a lease on its
// subscription. mutate then calls the processor with no lock held, bounded
// well inside the lease, and complete applies the subscription it produced
// under the lock again. The lease is released however the command ends; one
// that cannot be released expires on its own.
func leasedCommand[T any](ctx context.Context, s RecurringService, owner ownerResolver,
	prepare func(pgx.Tx, queries.Donor, time.Time) (*T, *subscriptionLease, error),
	mutate func(context.Context, *subscriptionLease) error,
	complete func(pgx.Tx, queries.Donor, time.Time, *subscriptionSnapshot) (T, error),
) (T, error) {
	var zero T
	var lease *subscriptionLease
	settled, err := donorWrite(ctx, s.DB, owner, func(tx pgx.Tx, d queries.Donor, now time.Time) (*T, error) {
		result, acquired, err := prepare(tx, d, now)
		lease = acquired
		return result, err
	})
	if err != nil {
		return zero, err
	}
	if settled != nil {
		return *settled, nil
	}
	defer s.release(ctx, lease)
	request, cancel := context.WithTimeout(ctx, submitWindow)
	err = mutate(request, lease)
	cancel()
	if err != nil {
		return zero, err
	}
	snapshot, err := fetchSubscription(ctx, queries.New(s.DB), newSubFetcher(s.Gateway), lease.attempt, lease.subscription)
	if err != nil {
		return zero, err
	}
	return donorWrite(ctx, s.DB, owner, func(tx pgx.Tx, d queries.Donor, now time.Time) (T, error) {
		return complete(tx, d, now, snapshot)
	})
}

func (s RecurringService) release(ctx context.Context, lease *subscriptionLease) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := releaseLease(ctx, queries.New(s.DB), lease.attempt, lease.command); err != nil {
		slog.Warn("subscription lease release failed; waiting for expiry", "cause", fmt.Sprintf("%T", err))
	}
}

// subscriptionUse is what a command needs of the subscription it reads.
type subscriptionUse int

const (
	viewOnly subscriptionUse = iota
	// forAmount requires no schedule or pending update, and refuses a
	// subscription Stripe reports as pause-collecting, which a new amount
	// could conflict with.
	forAmount
)

// read reconciles the locked attempt's subscription with the processor's
// current state and checks it suits use.
func (s RecurringService) read(
	ctx context.Context,
	tx pgx.Tx,
	attempt queries.DonorStripeCheckout,
	now time.Time,
	use subscriptionUse,
	snapshot *subscriptionSnapshot,
) (queries.DonorSubscription, payments.Object, error) {
	q := queries.New(tx)
	remote, err := currentSubscription(ctx, q, newSubFetcher(s.Gateway), attempt, snapshot)
	if err != nil {
		return queries.DonorSubscription{}, nil, err
	}
	draft, err := q.CheckoutDraft(ctx, attempt.CheckoutID)
	if err != nil {
		return queries.DonorSubscription{}, nil, err
	}
	row, err := reconcileRemoteSubscription(ctx, q, draft, attempt, remote, now)
	if err != nil {
		return queries.DonorSubscription{}, nil, err
	}
	if use != viewOnly && row.ExternalChange {
		return queries.DonorSubscription{}, nil, ErrSubscriptionConflict
	}
	if use == forAmount && !remote.Absent("pause_collection") {
		return queries.DonorSubscription{}, nil, ErrSubscriptionConflict
	}
	return row, remote, nil
}

// Details describes the donor's monthly gift as last synced from the processor.
func (s RecurringService) Details(ctx context.Context, token string) (RecurringDetails, error) {
	return donorRead(ctx, s.DB, token, func(q *queries.Queries, d queries.Donor, _ time.Time) (RecurringDetails, error) {
		return storedDetails(ctx, q, d.ID)
	})
}

func (s RecurringService) Cancel(ctx context.Context, token string, r CancellationRequest) (RecurringDetails, error) {
	if !ValidRequestID(r.RequestID) || r.PeriodEnd.IsZero() {
		return RecurringDetails{}, payments.ErrFacts
	}
	snapshot, err := s.donorRefresh(ctx, token)
	if err != nil {
		return RecurringDetails{}, err
	}
	return s.cancel(ctx, donorOwner(token), snapshot, r)
}

// cancel schedules the cancellation for owner's donor, who may be acting
// themselves or through staff.
func (s RecurringService) cancel(
	ctx context.Context,
	owner ownerResolver,
	snapshot *subscriptionSnapshot,
	r CancellationRequest,
) (RecurringDetails, error) {
	prepare := func(tx pgx.Tx, d queries.Donor, now time.Time) (*RecurringDetails, *subscriptionLease, error) {
		q := queries.New(tx)
		attempt, err := ownedAttempt(ctx, q, d.ID, r.AttemptID)
		if err != nil {
			return nil, nil, err
		}
		action, err := q.CancellationByID(ctx, r.RequestID)
		if errors.Is(err, pgx.ErrNoRows) {
			action = queries.DonorCancellation{
				ID:                 r.RequestID,
				DonorID:            d.ID,
				AttemptID:          attempt.ID,
				ConfirmedPeriodEnd: timestamp(r.PeriodEnd),
				CreatedAt:          timestamp(now),
			}
			err = q.InsertCancellation(
				ctx,
				queries.InsertCancellationParams{
					ID:                 action.ID,
					DonorID:            action.DonorID,
					AttemptID:          action.AttemptID,
					ConfirmedPeriodEnd: action.ConfirmedPeriodEnd,
					CreatedAt:          action.CreatedAt,
				},
			)
		} else if err == nil && (action.DonorID != d.ID || action.AttemptID != attempt.ID || !action.ConfirmedPeriodEnd.Time.Equal(r.PeriodEnd)) {
			err = ErrSubscriptionConflict
		}
		if err != nil {
			return nil, nil, err
		}
		row, _, err := s.read(ctx, tx, attempt, now, viewOnly, snapshot)
		if err != nil {
			return nil, nil, err
		}
		if action.CompletedAt.Valid || row.StopsAt.Valid {
			view, err := completeCancellation(ctx, tx, d.ID, attempt, action, row, now)
			return &view, nil, err
		}
		if !row.PeriodEnd.Time.Equal(r.PeriodEnd) || !now.Before(r.PeriodEnd) || now.Sub(action.CreatedAt.Time) >= commandRetryWindow ||
			!renews(row.Status) {
			return nil, nil, ErrSubscriptionConflict
		}
		lease, err := leaseSubscription(ctx, q, d.ID, attempt, action.ID, now)
		return nil, lease, err
	}
	mutate := func(ctx context.Context, lease *subscriptionLease) error {
		return s.Gateway.ScheduleCancellation(ctx, lease.subscription, r.PeriodEnd.Unix(), "dad-cancel-"+r.RequestID.String())
	}
	complete := func(tx pgx.Tx, d queries.Donor, now time.Time, snapshot *subscriptionSnapshot) (RecurringDetails, error) {
		q := queries.New(tx)
		attempt, err := ownedAttempt(ctx, q, d.ID, r.AttemptID)
		if err != nil {
			return RecurringDetails{}, err
		}
		action, err := q.CancellationByID(ctx, r.RequestID)
		if err != nil {
			return RecurringDetails{}, err
		}
		row, _, err := s.read(ctx, tx, attempt, now, viewOnly, snapshot)
		if err != nil {
			return RecurringDetails{}, err
		}
		if !action.CompletedAt.Valid && !row.StopsAt.Valid {
			return RecurringDetails{}, ErrSubscriptionConflict
		}
		return completeCancellation(ctx, tx, d.ID, attempt, action, row, now)
	}
	return leasedCommand(ctx, s, owner, prepare, mutate, complete)
}

// renews reports whether a subscription in status will renew, which a
// cancellation stops and a card update pays for.
func renews(status string) bool {
	return status == "active" || status == "past_due" || status == "unpaid"
}

func completeCancellation(
	ctx context.Context,
	tx pgx.Tx,
	donor pgtype.UUID,
	attempt queries.DonorStripeCheckout,
	action queries.DonorCancellation,
	row queries.DonorSubscription,
	now time.Time,
) (RecurringDetails, error) {
	q := queries.New(tx)
	if !action.CompletedAt.Valid {
		if err := q.CompleteCancellation(ctx, queries.CompleteCancellationParams{ID: action.ID, CompletedAt: timestamp(now)}); err != nil {
			return RecurringDetails{}, err
		}
	}
	if err := releaseLease(ctx, q, attempt.ID, action.ID); err != nil {
		return RecurringDetails{}, err
	}
	if _, err := refreshProjection(ctx, tx, donor, now); err != nil {
		return RecurringDetails{}, err
	}
	return subscriptionView(ctx, q, attempt, row)
}

func amountView(c queries.DonorAmountChange) *AmountChange {
	return &AmountChange{
		RequestID:     c.ID.String(),
		AmountMinor:   c.AmountMinor,
		PreviousMinor: c.PreviousMinor,
		EffectiveAt:   c.EffectiveAt.Time,
		Confirmed:     c.ConfirmedAt.Valid,
	}
}
