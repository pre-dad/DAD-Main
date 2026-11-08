-- name: DonorBadges :many
SELECT code, source, earned_at, revoked_at, city FROM donor_badges
WHERE donor_id=$1
ORDER BY earned_at, code;

-- name: EarnBadge :execrows
-- Records an auto-earned badge. Permanent: if the row already exists it is left
-- as-is (first-earned time wins); a previously revoked badge is NOT silently
-- re-earned here (a revoke is an admin decision, honoured until an admin grants).
-- Returns rows affected so the caller fires the badge-earned push only for a
-- genuinely new earn.
-- city is the city a city badge was earned in, NULL for any other badge.
INSERT INTO donor_badges (donor_id, code, source, earned_at, city)
VALUES ($1, $2, 'AUTO', $3, sqlc.narg(city)::varchar)
ON CONFLICT (donor_id, code) DO NOTHING;

-- name: BadgeEarned :one
-- True if the donor currently holds this badge (earned and not revoked), for
-- rechecking a badge-earned push at send time (an admin may revoke in between).
SELECT EXISTS(
    SELECT 1 FROM donor_badges WHERE donor_id=$1 AND code=$2 AND revoked_at IS NULL
);

-- name: QueueBadgePush :exec
-- Queues a badge-earned push (§6.11) for the donor, keyed so it fires at most
-- once per badge, and only while the donor has a live app device so nothing
-- accumulates for a donor without the app.
INSERT INTO donor_push_notifications(id,donor_id,category,dedup_key,context,available_at,expires_at)
SELECT gen_random_uuid(),sqlc.arg(donor_id)::uuid,'BADGE_EARNED',
    'push:badge:'||sqlc.arg(donor_id)::text||':'||sqlc.arg(code)::text,
    json_build_object('code',sqlc.arg(code)::text),
    sqlc.arg(as_of)::timestamptz,sqlc.arg(expires_at)::timestamptz
WHERE EXISTS(SELECT 1 FROM donor_sessions s WHERE s.donor_id=sqlc.arg(donor_id)::uuid AND s.push_token IS NOT NULL
    AND coalesce(s.refresh_expires_at,s.expires_at)>sqlc.arg(as_of)::timestamptz)
ON CONFLICT(dedup_key) DO NOTHING;

-- name: GrantBadge :exec
-- Admin grant (B-4). Clears any revocation and marks the source as GRANT.
INSERT INTO donor_badges (donor_id, code, source, earned_at)
VALUES ($1, $2, 'GRANT', $3)
ON CONFLICT (donor_id, code) DO UPDATE SET source='GRANT', revoked_at=NULL;

-- name: RevokeBadge :exec
-- Admin revoke (B-4): tombstone the row rather than delete it, for audit.
UPDATE donor_badges SET revoked_at=$3 WHERE donor_id=$1 AND code=$2;

-- name: DonorRaisedAmount :one
-- True if the donor has ever confirmed an amount increase (Level Up badge).
SELECT EXISTS (
  SELECT 1 FROM donor_amount_changes
  WHERE donor_id=$1 AND confirmed_at IS NOT NULL AND amount_minor > previous_minor
)::boolean AS raised;

-- name: DonorFirstGiftAt :one
-- When the donor first gave, to tell a restarted run from their first (Comeback).
SELECT min(paid_at)::timestamptz AS first_paid_at FROM donor_gifts WHERE donor_id=$1;

-- name: RecognitionWording :many
SELECT kind,key,title,body FROM donor_recognition_wording WHERE kind=$1;

-- name: SetRecognitionWording :exec
INSERT INTO donor_recognition_wording(kind,key,title,body,actor_id,updated_at) VALUES($1,$2,$3,$4,$5,$6)
ON CONFLICT(kind,key) DO UPDATE SET title=EXCLUDED.title,body=EXCLUDED.body,actor_id=EXCLUDED.actor_id,updated_at=EXCLUDED.updated_at;

-- name: ResetRecognitionWording :exec
DELETE FROM donor_recognition_wording WHERE kind=$1 AND key=$2;

-- name: MilestoneWording :one
SELECT title,body FROM donor_recognition_wording WHERE kind='MILESTONE' AND key=$1;

-- name: LatestStreakReset :one
SELECT max(reset_at)::timestamptz AS reset_at FROM donor_streak_resets WHERE donor_id=$1 AND reset_at<=sqlc.arg(as_of)::timestamptz;
-- name: InsertStreakReset :exec
INSERT INTO donor_streak_resets(id,donor_id,reset_at,actor_id,reason) VALUES($1,$2,$3,$4,$5);
-- name: BadgeRules :many
SELECT code,signal,threshold,min_population,artwork FROM donor_badge_rules ORDER BY code;
-- name: SetBadgeRule :exec
INSERT INTO donor_badge_rules(code,signal,threshold,min_population,artwork,updated_at,actor_id) VALUES($1,$2,$3,$4,$5,$6,$7)
ON CONFLICT(code) DO UPDATE SET signal=EXCLUDED.signal,threshold=EXCLUDED.threshold,min_population=EXCLUDED.min_population,artwork=EXCLUDED.artwork,updated_at=EXCLUDED.updated_at,actor_id=EXCLUDED.actor_id;
-- name: MilestoneRules :many
SELECT days FROM donor_milestone_rules;
-- name: SetMilestoneRules :exec
INSERT INTO donor_milestone_rules(singleton,days,updated_at,actor_id) VALUES(true,$1,$2,$3)
ON CONFLICT(singleton) DO UPDATE SET days=EXCLUDED.days,updated_at=EXCLUDED.updated_at,actor_id=EXCLUDED.actor_id;

-- name: RecognitionEpoch :one
SELECT epoch FROM donor_recognition_epoch;
-- name: BumpRecognitionEpoch :exec
UPDATE donor_recognition_epoch SET epoch=epoch+1;
