-- name: InsertFinancialMessage :exec
INSERT INTO donor_messages (id,donor_id,kind,dedup_key,context,available_at)
VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (dedup_key) DO NOTHING;

-- name: QueueFinancialIntents :exec
WITH gifts AS MATERIALIZED (
    SELECT g.id,g.paid_at FROM donor_gifts g WHERE g.donor_id=sqlc.arg(donor_id)
    AND g.paid_at<=sqlc.arg(as_of)::timestamptz
), years AS (
    SELECT DISTINCT extract(year FROM paid_at AT TIME ZONE 'America/New_York')::integer AS year FROM gifts
), candidates AS (
    SELECT sqlc.arg(donor_id)::uuid AS donor_id,'RECEIPT' AS kind,'receipt:'||id::text AS dedup_key,
        json_build_object('gift_id',id::text) AS payload,sqlc.arg(as_of)::timestamptz AS available_at FROM gifts
    UNION ALL
    SELECT sqlc.arg(donor_id)::uuid,'ANNUAL','annual:'||sqlc.arg(donor_id)::text||':'||year::text,
        json_build_object('year',year),sqlc.arg(as_of)::timestamptz FROM years
    WHERE year>=2026 AND year<extract(year FROM sqlc.arg(as_of)::timestamptz AT TIME ZONE 'America/New_York')
    UNION ALL
    SELECT p.referrer_id,'REFERRAL','referral:'||p.donor_id::text||':'||p.referrer_id::text||':'||p.referral_kind,
        json_build_object('referred_id',p.donor_id::text,'kind',coalesce(p.referral_kind,'')),sqlc.arg(as_of)::timestamptz
    FROM donor_metric_projections p WHERE p.donor_id=sqlc.arg(donor_id) AND p.referrer_id IS NOT NULL AND p.net_minor>0
    UNION ALL
    SELECT p.donor_id,'RECOVERY','recovery:'||p.id::text||':'||days.day::text,
        json_build_object('period_id',p.id::text,'day',days.day),p.due_at+days.day*interval '24 hours'
    FROM donor_billing_periods p CROSS JOIN (VALUES(0),(5)) AS days(day)
    WHERE p.donor_id=sqlc.arg(donor_id) AND p.status='FAILED'
    UNION ALL
    -- Streak lapsed email (§6.11): the failure stayed unresolved through the
    -- 7-day grace, so the run ended. Keyed by the period, available at grace
    -- end, so it fires once and not before the lapse is real.
    SELECT p.donor_id,'LAPSED','lapsed:'||p.id::text,
        json_build_object('period_id',p.id::text),p.due_at+interval '7 days'
    FROM donor_billing_periods p
    WHERE p.donor_id=sqlc.arg(donor_id) AND p.status='FAILED'
)
INSERT INTO donor_messages(id,donor_id,kind,dedup_key,context,available_at)
SELECT gen_random_uuid(),c.donor_id,c.kind,c.dedup_key,c.payload,c.available_at FROM candidates c
WHERE NOT EXISTS(SELECT 1 FROM donor_messages m WHERE m.dedup_key=c.dedup_key)
AND (c.kind<>'REFERRAL' OR NOT EXISTS(
    SELECT 1 FROM donor_messages m WHERE m.kind='REFERRAL' AND m.donor_id=c.donor_id
    AND m.context->>'referred_id'=c.payload->>'referred_id' AND m.context->>'kind'=c.payload->>'kind'))
ON CONFLICT(dedup_key) DO NOTHING;

-- name: QueueLoginEmail :exec
-- At most one sign-in link a minute and five an hour per donor, so requests from
-- many addresses cannot flood an inbox.
INSERT INTO donor_login_emails (id,donor_id,available_at,created_at,return_path)
SELECT $1,$2,$3,$3,sqlc.narg(return_path) WHERE NOT EXISTS
(SELECT 1 FROM donor_login_emails WHERE donor_id=$2 AND created_at>$3::timestamptz-interval '1 minute')
AND (SELECT count(*) FROM donor_login_emails WHERE donor_id=$2 AND created_at>$3::timestamptz-interval '1 hour')<5;

-- name: DueProjectionDonors :many
-- Batch claim of the due-soonest donors; LIMIT bounds one step's work so
-- replicas claim disjoint donors and scale linearly instead of one per txn.
SELECT d.id FROM donor_metric_projections p JOIN donors d ON d.id=p.donor_id
WHERE p.next_refresh_at<=$1 ORDER BY p.next_refresh_at
LIMIT sqlc.arg(batch)::int FOR NO KEY UPDATE OF d, p SKIP LOCKED;

-- name: OutdatedProjectionDonors :many
-- Batch claim of donors computed under earlier rules; LIMIT bounds one step's
-- work so replicas claim disjoint donors and scale linearly. The projection is
-- locked too, so a claim re-checks its filter against a refresh that committed
-- after the statement began instead of refreshing that donor again.
SELECT d.id FROM donor_metric_projections p JOIN donors d ON d.id=p.donor_id
WHERE p.rules_version<$1 AND p.computed_at<=$2
LIMIT sqlc.arg(batch)::int FOR NO KEY UPDATE OF d, p SKIP LOCKED;

-- name: SeedProjection :exec
INSERT INTO donor_metric_projections (id,donor_id,computed_at,next_refresh_at)
VALUES ($1,$2,$3,$3) ON CONFLICT (donor_id) DO NOTHING;

-- name: DeferProjection :exec
-- A deferred projection takes the current rules version, so it waits out its
-- backoff instead of being picked up again at once as outdated.
UPDATE donor_metric_projections SET refresh_failures=refresh_failures+1,refresh_error=$2,rules_version=sqlc.arg(rules_version),
next_refresh_at=sqlc.arg(as_of)::timestamptz+least(power(2,refresh_failures)*interval '5 minutes',interval '1 day')
WHERE donor_id=$1;
