package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/recognition"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func queueMessage(ctx context.Context, q *queries.Queries, donor pgtype.UUID, kind, key string, details any, at time.Time) error {
	id, err := NewID()
	if err != nil {
		return err
	}
	data, err := json.Marshal(details)
	if err != nil {
		return err
	}
	return q.InsertFinancialMessage(
		ctx,
		queries.InsertFinancialMessageParams{ID: id, DonorID: donor, Kind: kind, DedupKey: key, Context: data, AvailableAt: timestamp(at)},
	)
}

// refreshProjection recomputes the donor's recognition and queues the
// messages and push notifications its facts call for, which commit together
// with it. Delivery is a separate worker. It takes the donor lock if the
// caller does not hold it.
func refreshProjection(ctx context.Context, tx pgx.Tx, id pgtype.UUID, now time.Time) (projection, error) {
	q := queries.New(tx)
	result, err := projectFacts(ctx, q, id, now)
	if err != nil {
		return result, err
	}
	if err = q.QueueFinancialIntents(ctx, queries.QueueFinancialIntentsParams{DonorID: id, AsOf: timestamp(now)}); err != nil {
		return result, err
	}
	if err = queuePushIntents(ctx, q, result, now); err != nil {
		return result, err
	}
	return result, awardBadges(ctx, q, result.donor, now)
}

func seedProjection(ctx context.Context, q *queries.Queries, donor pgtype.UUID, now time.Time) error {
	id, err := NewID()
	if err != nil {
		return err
	}
	return q.SeedProjection(ctx, queries.SeedProjectionParams{ID: id, DonorID: donor, ComputedAt: timestamp(now)})
}

// ProjectionBatch bounds how many due donors one RefreshDueProjections step
// claims. It is large enough to amortize transaction round trips across a
// backlog, small enough to keep one transaction's FOR UPDATE lock set and step
// time well within the worker's StepBudget.
const ProjectionBatch = 64

const (
	// OutdatedBatch bounds one transaction of catch-up work on projections
	// computed under earlier rules. Its donors stay locked until it commits, so
	// it is sized to keep that to about a second (see docs/operations.md), while
	// still amortizing round trips: at 64 the rate is about 60% of the plateau.
	OutdatedBatch = 256
	// CatchUpSlice is how long one worker step keeps starting catch-up
	// transactions. It is half of workers.StepBudget, leaving room for the
	// transaction in flight and the sweeps that follow in the same step.
	CatchUpSlice = time.Minute
)

// RefreshNextProjection refreshes the donor whose projection is due soonest,
// or else one computed under earlier rules. It is a thin wrapper over
// RefreshDueProjections for callers that advance the queue one donor at a time.
func RefreshNextProjection(ctx context.Context, pool *pgxpool.Pool, now time.Time) (bool, error) {
	n, err := RefreshDueProjections(ctx, pool, now, 1)
	return n > 0, err
}

// RefreshDueProjections claims up to batch donors whose projections are due
// soonest, or else donors computed under earlier rules when none are due, and
// refreshes each in one outer transaction. Every donor is refreshed in its own
// savepoint, so a donor whose facts cannot be projected is deferred with
// backoff and its failure recorded for the monitor rather than aborting the
// batch. It returns the number of donors claimed.
func RefreshDueProjections(ctx context.Context, pool *pgxpool.Pool, now time.Time, batch int) (int, error) {
	return refreshProjections(ctx, pool, now, batch, batch)
}

// CatchUpProjections refreshes projections for up to slice, in transactions of
// at most ProjectionBatch due donors or OutdatedBatch outdated ones. Due
// donors are claimed first by every transaction, so a rules change delays a
// due projection by at most one transaction, never by the whole backlog. It
// stops when nothing is left, the slice is spent, or ctx ends, and reports how
// many donors it claimed even when it returns an error. Concurrent callers
// claim disjoint donors.
func CatchUpProjections(ctx context.Context, pool *pgxpool.Pool, clock func() time.Time, slice time.Duration) (int, error) {
	deadline := time.Now().Add(slice)
	total := 0
	for {
		n, err := refreshProjections(ctx, pool, clock(), ProjectionBatch, OutdatedBatch)
		total += n
		if err != nil || n == 0 || !time.Now().Before(deadline) {
			return total, err
		}
	}
}

func refreshProjections(ctx context.Context, pool *pgxpool.Pool, now time.Time, dueBatch, outdatedBatch int) (int, error) {
	processed := 0
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		q := queries.New(tx)
		version, err := currentRulesVersion(ctx, q)
		if err != nil {
			return err
		}
		ids, err := q.DueProjectionDonors(ctx, queries.DueProjectionDonorsParams{NextRefreshAt: timestamp(now), Batch: int32(dueBatch)})
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			ids, err = q.OutdatedProjectionDonors(
				ctx,
				queries.OutdatedProjectionDonorsParams{RulesVersion: version, ComputedAt: timestamp(now), Batch: int32(outdatedBatch)},
			)
			if err != nil {
				return err
			}
		}
		for _, id := range ids {
			refreshErr := pgx.BeginFunc(ctx, tx, func(work pgx.Tx) error {
				_, err := refreshProjection(ctx, work, id, now)
				return err
			})
			if refreshErr != nil {
				category, ok := projectionFailure(refreshErr)
				if !ok {
					return refreshErr
				}
				if err := q.DeferProjection(ctx, queries.DeferProjectionParams{DonorID: id, RefreshError: text(category), AsOf: timestamp(now), RulesVersion: version}); err != nil {
					return err
				}
			}
			processed++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return processed, nil
}

// projectionFailure classifies errors that retrying cannot fix until the
// donor's facts change.
func projectionFailure(err error) (string, bool) {
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, recognition.ErrInvalid), errors.Is(err, payments.ErrFacts):
		return "InvalidFacts", true
	case errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "22"):
		return "DataException", true
	case errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "23"):
		return "ConstraintViolation", true
	}
	return "", false
}

// queueLoginEmail queues a sign-in link for the donor with email, if any,
// returning them to next unless it is empty. The link itself is minted by the
// delivery worker.
func queueLoginEmail(ctx context.Context, q *queries.Queries, email, next string, now time.Time) error {
	donor, err := q.DonorByEmail(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	donor, err = q.LockDonor(ctx, donor.ID)
	if err != nil {
		return err
	}
	if donor.Disabled {
		return nil
	}
	id, err := NewID()
	if err != nil {
		return err
	}
	return q.QueueLoginEmail(
		ctx,
		queries.QueueLoginEmailParams{
			ID:          id,
			DonorID:     donor.ID,
			AvailableAt: timestamp(now),
			ReturnPath:  pgtype.Text{String: next, Valid: next != ""},
		},
	)
}
