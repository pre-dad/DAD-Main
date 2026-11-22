package store

import (
	"context"
	"errors"
	"time"

	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const anchorBackfillInterval = time.Hour

// AnchorBackfill syncs subscriptions saved before their billing anchor was
// stored, which may otherwise wait a month for an event about them. A pass
// starts once an interval and syncs one subscription per call; one that fails
// is retried by the next pass, so it cannot hold up the rest. Once every
// subscription has an anchor, a pass is a single empty query.
type AnchorBackfill struct {
	DB      *pgxpool.Pool
	Gateway payments.FinancialGateway
	next    time.Time
	cursor  *pgtype.UUID
}

// SyncNext syncs the next subscription if a pass is due or under way,
// reporting whether it found one.
func (b *AnchorBackfill) SyncNext(ctx context.Context, now time.Time) (bool, error) {
	if b.cursor == nil {
		if now.Before(b.next) {
			return false, nil
		}
		b.cursor = &pgtype.UUID{Valid: true}
	}
	row, err := queries.New(b.DB).NextSubscriptionWithoutAnchor(ctx, *b.cursor)
	if err != nil {
		b.cursor, b.next = nil, now.Add(anchorBackfillInterval)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	b.cursor = &row.ID
	return true, b.sync(ctx, row, now)
}

// sync reads the subscription with no lock held, then reconciles it under
// the donor lock as a subscription event would.
func (b *AnchorBackfill) sync(ctx context.Context, row queries.NextSubscriptionWithoutAnchorRow, now time.Time) error {
	_, err := withFreshSnapshot(
		func() (*subscriptionSnapshot, error) {
			return fetchSubscription(ctx, queries.New(b.DB), newSubFetcher(b.Gateway), row.ID, row.StripeSubscriptionID.String)
		},
		func(snapshot *subscriptionSnapshot) (struct{}, error) {
			fetcher := &subFetcher{frozen: true}
			return struct{}{}, pgx.BeginFunc(ctx, b.DB, func(tx pgx.Tx) error {
				q := queries.New(tx)
				if _, err := q.LockDonor(ctx, row.DonorID); err != nil {
					return err
				}
				draft, err := q.CheckoutDraft(ctx, row.CheckoutID)
				if err != nil {
					return err
				}
				attempt, err := q.AttemptForCheckout(ctx, row.CheckoutID)
				if err != nil {
					return err
				}
				if attempt.ID != row.ID || attempt.StripeSubscriptionID != row.StripeSubscriptionID {
					return payments.ErrFacts
				}
				if _, err = reconcileSubscription(ctx, q, fetcher, draft, attempt, snapshot, now); err != nil {
					return err
				}
				_, err = refreshProjection(ctx, tx, row.DonorID, now)
				return err
			})
		}, ErrSubscriptionConflict,
	)
	return err
}
