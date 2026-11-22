package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// maxStripeWait bounds a reservation made without a deadline.
const maxStripeWait = 30 * time.Second

// ErrStripeBudget reports that a bucket has no request slot before the
// caller's deadline. It wraps context.DeadlineExceeded, which is how the
// caller would otherwise have ended.
var ErrStripeBudget = fmt.Errorf("stripe request budget exhausted: %w", context.DeadlineExceeded)

// StripeBudget is the Stripe account's request ceiling shared by every
// process that holds one, however many replicas run. Each request reserves
// its bucket's next slot in one statement and then sleeps until that slot, so
// callers queued behind a burst never poll the database.
type StripeBudget struct {
	DB *pgxpool.Pool
	// PerSecond is the account's ceiling; every process must use the same one.
	PerSecond float64
}

// Acquire reserves bucket's next slot and blocks until it arrives. It returns
// ErrStripeBudget when the slot is further off than the wait allows.
func (b StripeBudget) Acquire(ctx context.Context, bucket string) error {
	rate, burst, ok := payments.BucketRate(bucket, b.PerSecond)
	if !ok {
		return fmt.Errorf("%w %q", payments.ErrUnknownBucket, bucket)
	}
	limit := maxStripeWait
	if deadline, ok := ctx.Deadline(); ok {
		limit = min(limit, time.Until(deadline))
	}
	if limit <= 0 {
		return ErrStripeBudget
	}
	interval := 1 / rate
	wait, err := queries.New(b.DB).ReserveStripeRequest(ctx, queries.ReserveStripeRequestParams{
		Bucket: bucket, IntervalSeconds: interval, ToleranceSeconds: (burst - 1) * interval, MaxWaitSeconds: limit.Seconds(),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrStripeBudget
	}
	if err != nil || wait <= 0 {
		return err
	}
	timer := time.NewTimer(time.Duration(wait * float64(time.Second)))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
