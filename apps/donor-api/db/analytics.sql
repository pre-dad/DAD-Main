-- name: InsertAnalyticsEvents :copyfrom
INSERT INTO analytics_events (id, name, donor_id, source, properties, occurred_at)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: InsertObservedEvent :exec
INSERT INTO analytics_events(id,name,donor_id,source,properties,occurred_at,journey_id,dedup_key)
VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING;
-- name: InsertObservedEvents :exec
INSERT INTO analytics_events(id,name,donor_id,source,properties,occurred_at,journey_id)
SELECT unnest(sqlc.arg(ids)::uuid[]),unnest(sqlc.arg(names)::text[]),sqlc.narg(donor_id)::uuid,sqlc.arg(source)::text,
 unnest(sqlc.arg(properties)::jsonb[]),unnest(sqlc.arg(occurred_at)::timestamptz[]),unnest(sqlc.arg(journeys)::uuid[])
ON CONFLICT DO NOTHING;
-- name: AttachCheckoutJourney :exec
UPDATE donor_checkouts SET journey_id=$2 WHERE id=$1 AND journey_id IS NULL;
-- name: DeleteDonorAnalytics :exec
DELETE FROM analytics_events WHERE donor_id=$1;
-- name: PurgeAnalytics :exec
DELETE FROM analytics_events WHERE received_at<$1;
-- name: BindAnalyticsJourney :exec
UPDATE analytics_events SET donor_id=$2 WHERE journey_id=$1 AND donor_id IS NULL;
-- name: JourneyReport :one
WITH landings AS (
 SELECT journey_id,min(occurred_at) AS landed FROM analytics_events
 WHERE name='app_opened' AND journey_id IS NOT NULL AND occurred_at>=sqlc.arg(start_at)::timestamptz AND occurred_at<sqlc.arg(end_at)::timestamptz GROUP BY journey_id
), gifts AS (
 SELECT donor_id,min(paid_at) AS paid FROM donor_gifts GROUP BY donor_id
), timed AS (
 SELECT g.donor_id,g.paid,min(l.landed) AS landed FROM gifts g
 JOIN donor_checkouts c ON c.donor_id=g.donor_id
 JOIN landings l ON l.journey_id=c.journey_id AND l.landed<=g.paid
 WHERE g.paid<sqlc.arg(end_at)::timestamptz GROUP BY g.donor_id,g.paid
)
SELECT (SELECT count(*) FROM landings)::bigint AS landings,
 count(*)::bigint AS sample,
 coalesce(round((percentile_cont(0.5) WITHIN GROUP(ORDER BY extract(epoch FROM paid-landed)))::numeric),0)::bigint AS median_seconds FROM timed;
-- name: SevenDayShareReport :one
WITH first_gifts AS (SELECT donor_id,min(paid_at) AS joined FROM donor_gifts GROUP BY donor_id),
cohort AS (
 SELECT f.* FROM first_gifts f WHERE f.joined>=sqlc.arg(start_at)::timestamptz AND f.joined<sqlc.arg(end_at)::timestamptz-interval '7 days'
 AND EXISTS(SELECT 1 FROM analytics_events e WHERE e.donor_id=f.donor_id AND e.name='app_opened' AND e.occurred_at>=f.joined AND e.occurred_at<f.joined+interval '7 days')
)
SELECT count(*)::bigint AS eligible,
 count(*) FILTER(WHERE EXISTS(SELECT 1 FROM analytics_events e WHERE e.donor_id=c.donor_id AND e.name IN ('invite_shared','irl_code_shown')
 AND e.occurred_at>=c.joined AND e.occurred_at<c.joined+interval '7 days'))::bigint AS shared FROM cohort c;
