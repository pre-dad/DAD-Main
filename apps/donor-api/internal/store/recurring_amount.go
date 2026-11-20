package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// A gift is a whole number of dollars a day from $1 to $30, billed monthly as
// the daily amount × 30 (PRD G-1, G-2, §14.3).
const (
	dayMinor     = 100 * 30
	maxGiftMinor = 30 * dayMinor
)

// ValidGiftAmount reports whether a monthly amount is one a donor may choose.
func ValidGiftAmount(minor int64) bool {
	return minor >= dayMinor && minor <= maxGiftMinor && minor%dayMinor == 0
}

// ChangeAmount sets a new monthly amount from the next renewal, without
// proration. r.Previous must still be the amount that renewal would charge,
// which may predate the daily rule.
func (s RecurringService) ChangeAmount(ctx context.Context, token string, r AmountRequest) (AmountChange, error) {
	if !ValidRequestID(r.RequestID) || !ValidGiftAmount(r.Amount) || r.Previous <= 0 || r.EffectiveAt.IsZero() {
		return AmountChange{}, payments.ErrFacts
	}
	snapshot, err := s.donorRefresh(ctx, token)
	if err != nil {
		return AmountChange{}, err
	}
	var pending queries.DonorAmountChange
	prepare := func(tx pgx.Tx, d queries.Donor, now time.Time) (*AmountChange, *subscriptionLease, error) {
		q := queries.New(tx)
		attempt, err := ownedAttempt(ctx, q, d.ID, r.AttemptID)
		if err != nil {
			return nil, nil, err
		}
		command, err := q.AmountChangeByID(ctx, r.RequestID)
		fresh := errors.Is(err, pgx.ErrNoRows)
		if err != nil && !fresh {
			return nil, nil, err
		}
		if !fresh {
			if command.DonorID != d.ID || command.CheckoutID != attempt.CheckoutID || command.AmountMinor != r.Amount ||
				command.PreviousMinor != r.Previous ||
				!command.EffectiveAt.Time.Equal(r.EffectiveAt) {
				return nil, nil, ErrSubscriptionConflict
			}
			if command.ConfirmedAt.Valid {
				return amountView(command), nil, releaseLease(ctx, q, attempt.ID, command.ID)
			}
		}
		schedule, remote, err := s.read(ctx, tx, attempt, now, forAmount, snapshot)
		if err != nil {
			return nil, nil, err
		}
		if fresh {
			if err = insertAmountChange(ctx, q, d.ID, attempt.CheckoutID, schedule, remote, r, now); err != nil {
				return nil, nil, err
			}
		}
		if command, err = q.AmountChangeByID(ctx, r.RequestID); err != nil {
			return nil, nil, err
		}
		if command.ConfirmedAt.Valid {
			return amountView(command), nil, releaseLease(ctx, q, attempt.ID, command.ID)
		}
		if !amountChangeable(schedule, command.EffectiveAt.Time, now) || now.Sub(command.CreatedAt.Time) >= commandRetryWindow {
			return nil, nil, ErrSubscriptionConflict
		}
		pending = command
		lease, err := leaseSubscription(ctx, q, d.ID, attempt, command.ID, now)
		return nil, lease, err
	}
	mutate := func(ctx context.Context, lease *subscriptionLease) error {
		return s.Gateway.ChangeMonthlyAmount(
			ctx,
			lease.subscription,
			pending.ItemID,
			pending.ProductID,
			pending.AmountMinor,
			"dad-amount-"+pending.ID.String(),
		)
	}
	complete := func(tx pgx.Tx, d queries.Donor, now time.Time, snapshot *subscriptionSnapshot) (AmountChange, error) {
		q := queries.New(tx)
		attempt, err := ownedAttempt(ctx, q, d.ID, r.AttemptID)
		if err != nil {
			return AmountChange{}, err
		}
		if _, _, err = s.read(ctx, tx, attempt, now, forAmount, snapshot); err != nil {
			return AmountChange{}, err
		}
		command, err := q.AmountChangeByID(ctx, r.RequestID)
		if err != nil {
			return AmountChange{}, err
		}
		if !command.ConfirmedAt.Valid {
			return AmountChange{}, ErrSubscriptionConflict
		}
		return *amountView(command), releaseLease(ctx, q, attempt.ID, command.ID)
	}
	return leasedCommand(ctx, s, donorOwner(token), prepare, mutate, complete)
}

// amountChangeable reports whether a change effective at the subscription's
// next renewal can still be sent: the processor needs a minute's margin.
func amountChangeable(sub queries.DonorSubscription, effective, now time.Time) bool {
	return sub.Status == "active" && !sub.StopsAt.Valid && sub.PeriodEnd.Time.Equal(effective) &&
		now.Before(effective.Add(-time.Minute))
}

// insertAmountChange records r if no earlier change is still unconfirmed and
// r.Previous is what the next renewal would charge.
func insertAmountChange(
	ctx context.Context,
	q *queries.Queries,
	donor, checkout pgtype.UUID,
	sub queries.DonorSubscription,
	remote payments.Object,
	r AmountRequest,
	now time.Time,
) error {
	draft, err := q.CheckoutDraft(ctx, checkout)
	if err != nil {
		return err
	}
	prior, err := q.LatestAmountChange(ctx, draft.ID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err == nil && !prior.ConfirmedAt.Valid {
		return ErrSubscriptionConflict
	}
	previous, err := nextAmount(ctx, q, draft)
	if err != nil {
		return err
	}
	if !amountChangeable(sub, r.EffectiveAt, now) || previous != r.Previous || r.Amount == r.Previous {
		return ErrSubscriptionConflict
	}
	itemID, product, err := amountChangeItem(remote)
	if err != nil {
		return err
	}
	return q.InsertAmountChange(
		ctx,
		queries.InsertAmountChangeParams{
			ID:            r.RequestID,
			DonorID:       donor,
			CheckoutID:    draft.ID,
			PreviousMinor: r.Previous,
			AmountMinor:   r.Amount,
			EffectiveAt:   timestamp(r.EffectiveAt),
			ItemID:        itemID,
			ProductID:     product,
			CreatedAt:     timestamp(now),
		},
	)
}

// amountChangeItem returns the single monthly item and its product, which an
// amount change replaces in place.
func amountChangeItem(remote payments.Object) (string, string, error) {
	items, err := remote.Object("items")
	if err != nil {
		return "", "", err
	}
	lines, err := items.Array("data")
	if err != nil || len(lines) != 1 {
		return "", "", payments.ErrFacts
	}
	price, err := lines[0].Object("price")
	if err != nil {
		return "", "", err
	}
	itemID, product := lines[0].Text("id"), price.Text("product")
	if !strings.HasPrefix(product, "prod_") || !strings.HasPrefix(itemID, "si_") {
		return "", "", payments.ErrFacts
	}
	return itemID, product, nil
}
