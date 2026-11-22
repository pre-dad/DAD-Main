package store

import (
	"context"
	"errors"
	"time"

	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ClientRateLimit is the API's per-client request limit, shared by every
// replica through PostgreSQL so that running more API tasks does not raise
// it. Each class allows perMinute requests in a burst and perMinute a minute
// after, the same as the per-process buckets in front of it.
type ClientRateLimit struct{ DB *pgxpool.Pool }

// Admit takes one of the client's requests in the class and returns zero, or
// takes none and returns how long until one would be admitted.
func (l ClientRateLimit) Admit(ctx context.Context, class, client string, perMinute int) (time.Duration, error) {
	interval := time.Minute.Seconds() / float64(perMinute)
	tolerance := float64(perMinute-1) * interval
	q := queries.New(l.DB)
	admitted, err := q.AdmitClientRequest(ctx, queries.AdmitClientRequestParams{
		Class: class, Client: client, IntervalSeconds: interval, ToleranceSeconds: tolerance,
	})
	if err != nil || admitted > 0 {
		return 0, err
	}
	wait, err := q.ClientRequestWait(ctx, queries.ClientRequestWaitParams{Class: class, Client: client, ToleranceSeconds: tolerance})
	if errors.Is(err, pgx.ErrNoRows) {
		// Retention deletes a row only once its wait has passed.
		return 0, nil
	}
	return time.Duration(wait * float64(time.Second)), err
}
