package store

import (
	"context"
	"errors"
	"time"

	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func terminalSubscription(status string) bool {
	return status == "canceled" || status == "incomplete_expired"
}

// subFetcher memoizes RetrieveSubscription for the span of a single ProcessNext
// step, so a subscription resolved once is reused rather than fetched again. It
// is created per step and never shared across donors, so it introduces no
// cross-donor coupling.
type subFetcher struct {
	gateway payments.FinancialGateway
	seen    map[string]payments.Object
}

func newSubFetcher(gateway payments.FinancialGateway) *subFetcher {
	return &subFetcher{gateway: gateway, seen: map[string]payments.Object{}}
}

// retrieve returns the processor's view of id, fetching it at most once per
// step for a given id. It is for a resolution that is content with any state
// read during this step (a first resolution): the under-lock conflict re-read
// uses refresh instead, so this memo never masks a needed fresh read.
func (f *subFetcher) retrieve(ctx context.Context, id string) (payments.Object, error) {
	if cached, ok := f.seen[id]; ok {
		return cached, nil
	}
	return f.refresh(ctx, id)
}

// refresh always reads the processor and records the result for the rest of
// the step. The under-lock conflict path calls this so a state that moved
// after the pre-lock read is observed, never a memoized pre-lock value.
func (f *subFetcher) refresh(ctx context.Context, id string) (payments.Object, error) {
	if f.gateway == nil {
		return nil, payments.ErrUnconfigured
	}
	remote, err := f.gateway.RetrieveSubscription(ctx, id)
	if err != nil {
		return nil, err
	}
	f.seen[id] = remote
	return remote, nil
}

// subscriptionSnapshot is processor state fetched without holding the donor
// lock, with the reconciliation it was read after.
type subscriptionSnapshot struct {
	attempt  pgtype.UUID
	id       string
	observed pgtype.Timestamptz
	remote   payments.Object
}

// fetchSubscription reads an attempt's subscription from the processor. The
// caller holds no donor lock, so a slow processor cannot block other work. The
// read goes through the step's memo so a later resolution in the same step
// reuses it.
func fetchSubscription(
	ctx context.Context,
	q *queries.Queries,
	fetcher *subFetcher,
	attempt pgtype.UUID,
	id string,
) (*subscriptionSnapshot, error) {
	observed, err := q.SubscriptionObservedAt(ctx, attempt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	remote, err := fetcher.retrieve(ctx, id)
	if err != nil {
		return nil, err
	}
	return &subscriptionSnapshot{attempt: attempt, id: id, observed: observed, remote: remote}, nil
}

// currentSubscription returns processor state for the locked attempt. A
// snapshot is used only if nothing was reconciled after it was read; otherwise
// it may predate the stored state, so the processor is read again — but still
// through the step memo, so a subscription already resolved this step is not
// fetched twice.
func currentSubscription(
	ctx context.Context,
	q *queries.Queries,
	fetcher *subFetcher,
	attempt queries.DonorStripeCheckout,
	snapshot *subscriptionSnapshot,
) (payments.Object, error) {
	if snapshot != nil && snapshot.attempt == attempt.ID && snapshot.id == attempt.StripeSubscriptionID.String {
		observed, err := q.SubscriptionObservedAt(ctx, attempt.ID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if observed.Valid == snapshot.observed.Valid && observed.Time.Equal(snapshot.observed.Time) {
			return snapshot.remote, nil
		}
	}
	return fetcher.refresh(ctx, attempt.StripeSubscriptionID.String)
}

// The caller holds the donor lock. Use current processor state rather than an
// event snapshot so delayed delivery cannot undo a newer cancellation.
func reconcileSubscription(
	ctx context.Context,
	q *queries.Queries,
	fetcher *subFetcher,
	draft queries.DonorCheckout,
	attempt queries.DonorStripeCheckout,
	snapshot *subscriptionSnapshot,
	now time.Time,
) (queries.DonorSubscription, error) {
	remote, err := currentSubscription(ctx, q, fetcher, attempt, snapshot)
	if err != nil {
		return queries.DonorSubscription{}, err
	}
	return reconcileRemoteSubscription(ctx, q, draft, attempt, remote, now)
}

// reconcileRemoteSubscription saves the processor's view of the attempt's
// subscription, confirming any amount change it shows, with everything donor
// views read from it. Only a single-item monthly USD subscription is supported.
func reconcileRemoteSubscription(
	ctx context.Context,
	q *queries.Queries,
	draft queries.DonorCheckout,
	attempt queries.DonorStripeCheckout,
	remote payments.Object,
	now time.Time,
) (queries.DonorSubscription, error) {
	item, price, err := monthlyItem(draft, attempt, remote)
	if err != nil {
		return queries.DonorSubscription{}, err
	}
	_, end, err := monthlyPeriod(item, "current_period_start", "current_period_end")
	if err != nil {
		return queries.DonorSubscription{}, err
	}
	status := remote.Text("status")
	switch status {
	case "active", "past_due", "unpaid", "canceled", "incomplete", "incomplete_expired":
	default:
		return queries.DonorSubscription{}, payments.ErrFacts
	}
	atPeriodEnd := remote.BoolIs("cancel_at_period_end", true)
	if !atPeriodEnd && !remote.BoolIs("cancel_at_period_end", false) {
		return queries.DonorSubscription{}, payments.ErrFacts
	}
	anchor, err := remote.Unix("billing_cycle_anchor")
	if err != nil {
		return queries.DonorSubscription{}, err
	}
	cancelAt := !remote.Absent("cancel_at")
	if cancelAt {
		if at, err := remote.Unix("cancel_at"); err != nil || !at.Equal(end) {
			return queries.DonorSubscription{}, payments.ErrFacts
		}
	}
	if err = reconcileAmount(ctx, q, draft, item, price, end, now); err != nil {
		return queries.DonorSubscription{}, err
	}
	var stops pgtype.Timestamptz
	if cancelAt || atPeriodEnd || terminalSubscription(status) {
		stops = timestamp(end)
	}
	id, err := NewID()
	if err != nil {
		return queries.DonorSubscription{}, err
	}
	external := !remote.Absent("schedule") || !remote.Absent("pending_update")
	row, err := q.SaveSubscription(
		ctx,
		queries.SaveSubscriptionParams{
			ID:             id,
			AttemptID:      attempt.ID,
			DonorID:        draft.DonorID,
			Status:         status,
			PeriodEnd:      timestamp(end),
			StopsAt:        stops,
			BillingAnchor:  timestamp(anchor),
			ExternalChange: external,
			ObservedAt:     timestamp(now),
		},
	)
	if err != nil || !terminalSubscription(status) {
		return row, err
	}
	if err = q.CancelAttempt(ctx, attempt.ID); err != nil {
		return row, err
	}
	return row, q.ReleaseReservation(ctx, attempt.ID)
}

// monthlyItem returns the subscription's only item and its price, which must
// bill draft's checkout once a month.
func monthlyItem(
	draft queries.DonorCheckout,
	attempt queries.DonorStripeCheckout,
	remote payments.Object,
) (payments.Object, payments.Object, error) {
	metadata, e1 := remote.Object("metadata")
	items, e2 := remote.Object("items")
	lines, e3 := items.Array("data")
	if draft.Frequency != "MONTHLY" || !attempt.StripeSubscriptionID.Valid || remote.Text("id") != attempt.StripeSubscriptionID.String ||
		!remote.BoolIs("livemode", false) ||
		e1 != nil ||
		metadata.Text("dad_checkout_id") != draft.ID.String() ||
		e2 != nil ||
		!items.BoolIs("has_more", false) ||
		e3 != nil ||
		len(lines) != 1 {
		return nil, nil, payments.ErrFacts
	}
	item := lines[0]
	price, e1 := item.Object("price")
	recurring, e2 := price.Object("recurring")
	count, e3 := recurring.Int("interval_count")
	quantity, e4 := item.Int("quantity")
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || price.Text("currency") != "usd" || recurring.Text("interval") != "month" ||
		count != 1 ||
		quantity != 1 {
		return nil, nil, payments.ErrFacts
	}
	return item, price, nil
}

// monthlyPeriod reads a billing period's bounds from o, which must span
// between 27 and 32 days.
func monthlyPeriod(o payments.Object, startKey, endKey string) (time.Time, time.Time, error) {
	start, e1 := o.Unix(startKey)
	end, e2 := o.Unix(endKey)
	if e1 != nil || e2 != nil || end.Before(start.Add(27*24*time.Hour)) || end.After(start.Add(32*24*time.Hour)) {
		return time.Time{}, time.Time{}, payments.ErrFacts
	}
	return start, end, nil
}

// reconcileAmount confirms the latest amount change once the processor bills
// it from the renewal it was scheduled for.
func reconcileAmount(
	ctx context.Context,
	q *queries.Queries,
	draft queries.DonorCheckout,
	item, price payments.Object,
	end, now time.Time,
) error {
	actual, err := price.Int("unit_amount")
	if err != nil {
		return err
	}
	change, err := q.LatestAmountChange(ctx, draft.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		if actual != draft.AmountMinor {
			return payments.ErrFacts
		}
		return nil
	}
	if err != nil {
		return err
	}
	if item.Text("id") != change.ItemID || price.Text("product") != change.ProductID {
		return payments.ErrFacts
	}
	if actual == change.AmountMinor {
		if !change.ConfirmedAt.Valid && end.Unix() != change.EffectiveAt.Time.Unix() {
			return payments.ErrFacts
		}
		return q.ConfirmAmountChange(ctx, queries.ConfirmAmountChangeParams{ID: change.ID, ConfirmedAt: timestamp(now)})
	}
	if !change.ConfirmedAt.Valid && actual == change.PreviousMinor {
		return nil
	}
	return payments.ErrFacts
}
