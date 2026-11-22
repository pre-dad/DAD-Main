package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Options are offered from the subscription as last synced; each command
// checks the processor's current state before it changes anything.

// AmountOptions offers a new amount from the next renewal. An unavailable
// offer carries only its state.
type AmountOptions struct {
	AmountMinor    int64         `json:"amount_minor"`
	Change         *AmountChange `json:"change"`
	EffectiveAt    time.Time     `json:"effective_at"`
	State          string        `json:"state"`
	SubscriptionID string        `json:"subscription_id"`
}

func (o AmountOptions) MarshalJSON() ([]byte, error) {
	if o.State != "ready" {
		return json.Marshal(struct {
			State string `json:"state"`
		}{o.State})
	}
	type options AmountOptions
	return json.Marshal(options(o))
}

// latestSubscription returns the donor's latest monthly attempt, locked, and
// whether it has a subscription to act on yet.
func latestSubscription(ctx context.Context, q *queries.Queries, donor pgtype.UUID) (queries.DonorStripeCheckout, bool, error) {
	attempt, err := q.LatestMonthlyAttempt(ctx, donor)
	if errors.Is(err, pgx.ErrNoRows) {
		return attempt, false, nil
	}
	return attempt, err == nil && attempt.StripeSubscriptionID.Valid, err
}

// AmountOptions offers the donor's next-renewal amount change.
func (s RecurringService) AmountOptions(ctx context.Context, token string) (AmountOptions, error) {
	return donorRead(ctx, s.DB, token, func(q *queries.Queries, d queries.Donor, now time.Time) (AmountOptions, error) {
		unavailable := AmountOptions{State: "unavailable"}
		attempt, sub, err := syncedSubscription(ctx, q, d.ID)
		if err != nil || sub == nil {
			return unavailable, err
		}
		if sub.Status != "active" || sub.StopsAt.Valid || sub.ExternalChange {
			return unavailable, nil
		}
		draft, err := q.CheckoutDraft(ctx, attempt.CheckoutID)
		if err != nil {
			return AmountOptions{}, err
		}
		amount, err := nextAmount(ctx, q, draft)
		if err != nil {
			return AmountOptions{}, err
		}
		out := AmountOptions{State: "ready", SubscriptionID: attempt.ID.String(), AmountMinor: amount, EffectiveAt: sub.PeriodEnd.Time}
		change, err := q.LatestAmountChange(ctx, draft.ID)
		switch {
		case err == nil:
			out.Change = amountView(change)
		case !errors.Is(err, pgx.ErrNoRows):
			return AmountOptions{}, err
		}
		return out, nil
	})
}
