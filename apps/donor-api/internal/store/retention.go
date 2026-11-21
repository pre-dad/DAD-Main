package store

import (
	"context"
	"errors"
	"time"

	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// touchRetentionMonths is the product owner's retention period for referral
	// touches that led to no attributed gift, counted from capture. Touches
	// that did are kept for good.
	touchRetentionMonths = 13
	touchPurgeInterval   = time.Hour
	// touchPurgeBatch bounds each delete transaction's locks and WAL.
	touchPurgeBatch = 2000
	// analyticsRetentionMonths is how long analytics events are kept.
	analyticsRetentionMonths = 13
	analyticsPurgeBatch      = 5000
	// Credentials are useless once lapsed, so they are kept only long enough to
	// investigate a report: sessions for a quarter, sign-in links and the
	// emails that carried them for a month, and the phone number of an expired
	// code for a day.
	sessionRetention     = 90 * 24 * time.Hour
	loginRetention       = 30 * 24 * time.Hour
	phoneCodeRetention   = 24 * time.Hour
	credentialPurgeBatch = 2000
	rateLimitPurgeBatch  = 5000
)

// TouchRetention deletes analytics events past retention, lapsed credentials,
// expired staff sign-in ceremonies, spent rate limits, company-code touches
// once they expire (an expired one can never bind), and referral touches past
// retention. A pass starts once an interval and deletes one batch per call
// until a batch comes up short. Concurrent passes skip each other's rows, so
// every worker may run one.
type TouchRetention struct {
	DB     *pgxpool.Pool
	next   time.Time
	cursor *queries.PurgeReferralTouchesRow
}

// purgeStage deletes at most size rows per call and reports how many it deleted.
type purgeStage struct {
	size  int32
	purge func(batch int32) (int64, error)
}

// runPurgeStages runs each stage once. A failing stage is recorded and does
// not stop the stages after it. A stage that deleted a full batch may have
// more to delete, so the run ends there and reports full.
func runPurgeStages(stages []purgeStage) (full bool, err error) {
	var errs []error
	for _, stage := range stages {
		deleted, e := stage.purge(stage.size)
		if e != nil {
			errs = append(errs, e)
			continue
		}
		if deleted == int64(stage.size) {
			return true, errors.Join(errs...)
		}
	}
	return false, errors.Join(errs...)
}

// PurgeNext deletes the next batch if a pass is due or under way, reporting
// whether it deleted anything. A failure ends the pass until the next
// interval, so it cannot hold up the work sharing the worker.
func (r *TouchRetention) PurgeNext(ctx context.Context, now time.Time) (bool, error) {
	var failed error
	if r.cursor == nil {
		if now.Before(r.next) {
			return false, nil
		}
		full, err := runPurgeStages(retentionStages(ctx, queries.New(r.DB), now))
		if full {
			if err != nil {
				r.next = now.Add(touchPurgeInterval)
			}
			return true, err
		}
		failed = err
		r.cursor = &queries.PurgeReferralTouchesRow{}
	}
	deleted, err := purgeTouches(ctx, r.DB, now, *r.cursor, touchPurgeBatch)
	failed = errors.Join(failed, err)
	if failed != nil || len(deleted) < touchPurgeBatch {
		r.cursor, r.next = nil, now.Add(touchPurgeInterval)
	} else {
		r.cursor = &deleted[len(deleted)-1]
	}
	return len(deleted) > 0, failed
}

func retentionStages(ctx context.Context, q *queries.Queries, now time.Time) []purgeStage {
	return []purgeStage{
		{analyticsPurgeBatch, func(batch int32) (int64, error) {
			return q.PurgeAnalytics(ctx, queries.PurgeAnalyticsParams{
				ReceivedBefore: timestamp(now.AddDate(0, -analyticsRetentionMonths, 0)),
				Batch:          batch,
			})
		}},
		{credentialPurgeBatch, func(batch int32) (int64, error) {
			return q.PurgeDonorSessions(ctx, queries.PurgeDonorSessionsParams{
				LapsedBefore: timestamp(now.Add(-sessionRetention)),
				Batch:        batch,
			})
		}},
		{credentialPurgeBatch, func(batch int32) (int64, error) {
			return q.PurgeLoginLinks(ctx, queries.PurgeLoginLinksParams{
				ExpiredBefore: timestamp(now.Add(-loginRetention)),
				Batch:         batch,
			})
		}},
		{credentialPurgeBatch, func(batch int32) (int64, error) {
			return q.PurgeLoginEmails(ctx, queries.PurgeLoginEmailsParams{
				CompletedBefore: timestamp(now.Add(-loginRetention)),
				Batch:           batch,
			})
		}},
		{credentialPurgeBatch, func(batch int32) (int64, error) {
			return q.PurgePartnerSessions(ctx, queries.PurgePartnerSessionsParams{
				ExpiredBefore: timestamp(now.Add(-sessionRetention)),
				Batch:         batch,
			})
		}},
		{credentialPurgeBatch, func(batch int32) (int64, error) {
			return q.PurgePartnerLoginEmails(ctx, queries.PurgePartnerLoginEmailsParams{
				CompletedBefore: timestamp(now.Add(-loginRetention)),
				Batch:           batch,
			})
		}},
		{credentialPurgeBatch, func(batch int32) (int64, error) {
			return q.PurgePhoneCodes(ctx, queries.PurgePhoneCodesParams{
				ExpiredBefore: timestamp(now.Add(-phoneCodeRetention)),
				Batch:         batch,
			})
		}},
		{credentialPurgeBatch, func(batch int32) (int64, error) {
			return q.PurgeStaffCeremonies(ctx, queries.PurgeStaffCeremoniesParams{
				ExpiredBefore: timestamp(now),
				Batch:         batch,
			})
		}},
		{rateLimitPurgeBatch, func(batch int32) (int64, error) { return q.PurgeRateLimits(ctx, batch) }},
		{1, func(int32) (int64, error) { return 0, q.PurgeCompanyTouches(ctx, timestamp(now)) }},
	}
}

// purgeTouches deletes up to batch touches past retention that follow after,
// in capture order, and returns their keys in that order.
func purgeTouches(
	ctx context.Context,
	db *pgxpool.Pool,
	now time.Time,
	after queries.PurgeReferralTouchesRow,
	batch int32,
) ([]queries.PurgeReferralTouchesRow, error) {
	return queries.New(db).PurgeReferralTouches(ctx, queries.PurgeReferralTouchesParams{
		AfterTouchedAt: pgtype.Timestamptz{Time: after.TouchedAt.Time, Valid: true},
		AfterID:        pgtype.UUID{Bytes: after.ID.Bytes, Valid: true},
		TouchedBefore:  timestamp(now.AddDate(0, -touchRetentionMonths, 0)),
		AsOf:           timestamp(now),
		Batch:          batch,
	})
}
