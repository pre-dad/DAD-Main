package store

import (
	"context"
	"errors"
	"time"

	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// eventAlertBatch is how many donors one queue step considers, so a large
// audience never holds a single transaction open.
const eventAlertBatch = 256

type EventAlertService struct{ DB *pgxpool.Pool }

func (s EventAlertService) QueueNext(ctx context.Context, now time.Time) (bool, error) {
	return transact(ctx, s.DB, pgx.TxOptions{}, func(tx pgx.Tx) (bool, error) {
		q := queries.New(tx)
		job, err := q.NextEventAlertJob(ctx)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		batch, err := q.QueueEventAlertBatch(ctx, queries.QueueEventAlertBatchParams{
			EventID: job.EventID, AfterDonor: job.LastDonorID, AsOf: timestamp(now), BatchSize: eventAlertBatch,
		})
		if err != nil {
			return false, err
		}
		var completed pgtype.Timestamptz
		if batch.Considered < eventAlertBatch {
			completed = timestamp(now)
		}
		err = q.SaveEventAlertProgress(ctx, queries.SaveEventAlertProgressParams{
			EventID: job.EventID, LastDonorID: batch.LastDonor, CompletedAt: completed, UpdatedAt: timestamp(now),
		})
		return true, err
	})
}
