-- name: ReleasePushToken :exec
UPDATE donor_sessions SET push_token=NULL,push_environment=NULL WHERE push_token=$1 AND id<>$2;

-- name: SetPushToken :execrows
UPDATE donor_sessions SET platform=$2,push_token=$3,push_environment=$4 WHERE id=$1 AND client='APP';

-- name: DropPushToken :exec
-- Clears a session's token only if it is still the one presented, so a
-- token the app has since replaced survives a late provider rejection.
UPDATE donor_sessions SET push_token=NULL,push_environment=NULL WHERE id=$1 AND push_token=$2;

-- name: QueuePushIntents :exec
-- Push intents follow the facts the projection just saved: a referral
-- conversion (the referral notice's own condition) whose first gift was paid
-- or recorded since recent_since, so a referrer who installs the app later is
-- not told of an old conversion as news, a failed latest renewal
-- while the streak it threatens is at risk, and a streak milestone the caller
-- computed, keyed by the run it belongs to. Only donors with a live device
-- get one, so nothing accumulates for donors without the app.
WITH projection AS (
    SELECT * FROM donor_metric_projections m WHERE m.donor_id=sqlc.arg(donor_id)::uuid
), candidates AS (
    SELECT p.referrer_id AS donor_id,'REFERRAL_CONVERTED' AS category,'push:referral:'||p.donor_id::text||':'||p.referrer_id::text||':'||p.referral_kind AS dedup_key,
        json_build_object('referred_id',p.donor_id::text,'kind',p.referral_kind) AS context,
        sqlc.arg(as_of)::timestamptz AS available_at
    FROM projection p WHERE p.referrer_id IS NOT NULL AND p.net_minor>0
    AND EXISTS(SELECT 1 FROM donor_gifts g WHERE g.stripe_object_id=p.first_gift_id
        AND greatest(g.paid_at,g.created_at)>sqlc.arg(recent_since)::timestamptz)
    UNION ALL
    SELECT p.donor_id,'STREAK_AT_RISK',
        'push:streak-at-risk:'||b.id::text||CASE WHEN days.day=0 THEN '' ELSE ':5' END,
        json_build_object('period_id',b.id::text,'day',days.day),
        greatest(b.due_at+days.day*interval '24 hours',sqlc.arg(as_of)::timestamptz)
    FROM projection p CROSS JOIN LATERAL (
        SELECT id,status,due_at FROM donor_billing_periods WHERE donor_id=p.donor_id AND due_at<=sqlc.arg(as_of)::timestamptz
        ORDER BY due_at DESC LIMIT 1
    ) b CROSS JOIN (VALUES(0),(5)) AS days(day) WHERE p.at_risk AND b.status='FAILED'
    UNION ALL
    -- Streak lapsed (§6.11): the latest renewal failed and its 7-day grace has
    -- fully elapsed with no recovery, so the run ended. Keyed by that period so
    -- the lapse fires once; distinct from the at-risk reminders inside grace.
    SELECT p.donor_id,'STREAK_LAPSED','push:streak-lapsed:'||b.id::text,json_build_object('period_id',b.id::text),sqlc.arg(as_of)::timestamptz
    FROM projection p CROSS JOIN LATERAL (
        SELECT id,status,due_at FROM donor_billing_periods WHERE donor_id=p.donor_id AND due_at<=sqlc.arg(as_of)::timestamptz
        ORDER BY due_at DESC LIMIT 1
    ) b WHERE b.status='FAILED' AND b.due_at + interval '7 days' <= sqlc.arg(as_of)::timestamptz
    UNION ALL
    SELECT sqlc.arg(donor_id)::uuid,'STREAK_MILESTONE',
        'push:streak-milestone:'||sqlc.arg(donor_id)::text||':'||floor(extract(epoch FROM sqlc.narg(run_started_at)::timestamptz))::bigint::text||':'||sqlc.narg(milestone)::integer::text,
        json_build_object('days',sqlc.narg(milestone)::integer),sqlc.arg(as_of)::timestamptz
    WHERE sqlc.narg(milestone)::integer IS NOT NULL AND sqlc.narg(run_started_at)::timestamptz IS NOT NULL
    UNION ALL
    SELECT g.donor_id,'GIFT_PROCESSED','push:gift:'||g.id::text,
        json_build_object('gift_id',g.id::text),sqlc.arg(as_of)::timestamptz
    FROM donor_gifts g WHERE g.donor_id=sqlc.arg(donor_id)::uuid
      AND g.paid_at<=sqlc.arg(as_of)::timestamptz
      AND greatest(g.paid_at,g.created_at)>sqlc.arg(recent_since)::timestamptz
)
-- The lifetime is a fixed span of seconds: a bare timestamptz difference
-- normalises to an interval of whole days, and adding days back across a DST
-- boundary shifts the instant by the offset, so a push scheduled past a
-- transition would expire an hour early or late.
INSERT INTO donor_push_notifications(id,donor_id,category,dedup_key,context,available_at,expires_at)
SELECT gen_random_uuid(),c.donor_id,c.category,c.dedup_key,c.context,c.available_at,c.available_at+make_interval(secs=>extract(epoch FROM sqlc.arg(expires_at)::timestamptz-sqlc.arg(as_of)::timestamptz))
FROM candidates c
WHERE EXISTS(SELECT 1 FROM donor_sessions s WHERE s.donor_id=c.donor_id AND s.push_token IS NOT NULL
    AND coalesce(s.refresh_expires_at,s.expires_at)>sqlc.arg(as_of)::timestamptz)
AND (c.category<>'REFERRAL_CONVERTED' OR NOT EXISTS(
    SELECT 1 FROM donor_push_notifications n WHERE n.category='REFERRAL_CONVERTED' AND n.donor_id=c.donor_id
    AND n.context->>'referred_id'=c.context->>'referred_id' AND n.context->>'kind'=c.context->>'kind'))
ON CONFLICT(dedup_key) DO NOTHING;

-- name: NextPushNotification :one
SELECT * FROM donor_push_notifications WHERE completed_at IS NULL AND attempts<sqlc.arg(max_attempts)::int AND available_at<=$1
ORDER BY available_at,id LIMIT 1 FOR UPDATE SKIP LOCKED;

-- name: PushTargets :many
-- Revoking a session clears its token; one that lapsed keeps it, unused,
-- until the installation signs in again and registers it anew.
SELECT id,platform,push_token,push_environment FROM donor_sessions
WHERE donor_id=$1 AND push_token IS NOT NULL AND coalesce(refresh_expires_at,expires_at)>sqlc.arg(as_of)
AND id<>ALL(sqlc.arg(sent_to)::uuid[])
ORDER BY id;

-- name: ClaimPushNotification :exec
UPDATE donor_push_notifications SET attempts=attempts+1,available_at=$2 WHERE id=$1;

-- name: RecordPushSent :exec
UPDATE donor_push_notifications SET sent_to=sent_to||sqlc.arg(sessions)::uuid[] WHERE id=$1;

-- name: CompletePushNotification :exec
UPDATE donor_push_notifications SET completed_at=$2,last_error=NULL,suppressed_reason=$3 WHERE id=$1;

-- name: FailPushNotification :exec
UPDATE donor_push_notifications SET last_error=$2,available_at=$3,attempts=greatest(attempts,sqlc.arg(attempts))
WHERE id=$1 AND completed_at IS NULL;

-- name: ReferralConversions :many
-- A conversion is the referred donor's first gift, while it still counts,
-- as on the dashboard. Fetches one row past a page.
SELECT g.paid_at AS converted_at,p.referral_kind::text AS kind
FROM donor_metric_projections p JOIN donor_gifts g ON g.stripe_object_id=p.first_gift_id
WHERE p.referrer_id=$1 AND p.net_minor>0 AND (sqlc.narg(since)::timestamptz IS NULL OR g.paid_at>sqlc.narg(since))
ORDER BY g.paid_at DESC,p.donor_id DESC LIMIT 21;

-- name: ClearPushToken :exec
UPDATE donor_sessions SET push_token=NULL,push_environment=NULL WHERE id=$1;
