-- name: StaffDonorSearch :many
SELECT id,email,public_name,claimed_at,disabled FROM donors
WHERE email ILIKE sqlc.arg(term)::text ESCAPE '\' AND email > sqlc.arg(after_email)::text
ORDER BY email LIMIT 51;

-- name: StaffDonorByID :one
SELECT * FROM donors WHERE id=$1;

-- name: StaffReferredPage :many
SELECT d.id,d.email,p.referral_kind,p.net_minor FROM donors d
JOIN donor_metric_projections p ON p.donor_id=d.id WHERE p.referrer_id=$1
ORDER BY d.id LIMIT 51 OFFSET sqlc.arg(page_offset)::bigint;

-- name: StaffAuditPage :many
-- Newest first, keyset-paged on (created_at DESC, id). The first page passes an
-- infinite cursor_at, so the bound is never null and the index range stays usable.
SELECT id,actor_id,action,target,created_at,details FROM donor_staff_audit
WHERE created_at <= sqlc.arg(cursor_at)::timestamptz
  AND (created_at < sqlc.arg(cursor_at)::timestamptz OR id > sqlc.arg(cursor_id)::uuid)
ORDER BY created_at DESC,id LIMIT 51;

-- name: StaffMessagePage :many
SELECT id,kind,attempts,completed_at,last_error,suppressed_reason,available_at,created_at
FROM donor_messages WHERE donor_id=sqlc.arg(donor_id)::uuid
  AND created_at <= sqlc.arg(cursor_at)::timestamptz
  AND (created_at < sqlc.arg(cursor_at)::timestamptz OR id > sqlc.arg(cursor_id)::uuid)
ORDER BY created_at DESC,id LIMIT 51;

-- name: StaffMessageByID :one
SELECT * FROM donor_messages WHERE id=$1;

-- name: MessageByDedupKey :one
SELECT * FROM donor_messages WHERE dedup_key=$1;

-- name: RecentDonorMessage :one
SELECT EXISTS(SELECT 1 FROM donor_messages WHERE donor_id=$1 AND kind=$2 AND created_at>$3);

-- name: StaffActionPage :many
SELECT id,actor_id,kind,target_id,amount_minor,period_end,reason,status,completed_at,created_at
FROM donor_staff_actions WHERE donor_id=sqlc.arg(donor_id)::uuid
  AND created_at <= sqlc.arg(cursor_at)::timestamptz
  AND (created_at < sqlc.arg(cursor_at)::timestamptz OR id > sqlc.arg(cursor_id)::uuid)
ORDER BY created_at DESC,id LIMIT 51;

-- name: StaffPendingAction :one
SELECT id,actor_id,kind,target_id,amount_minor,period_end,reason,status,completed_at,created_at
FROM donor_staff_actions
WHERE donor_id=$1 AND actor_id=$2 AND target_id=$3 AND kind=$4 AND status='AUTHORIZED'
ORDER BY created_at DESC,id LIMIT 1;

-- name: StaffFailedPayments :many
-- Unresolved failed renewals, newest first, keyset-paged on (due_at, id). The
-- grace window is the fixed 7*24h the recognition rules use, so a DST change
-- cannot move its end. state is all, in_grace or lapsed. amount_minor is 0
-- when no failure was observed (observed amounts are always positive).
SELECT p.id AS period_id, p.donor_id, p.due_at, d.email, d.public_name, d.disabled,
  (p.due_at + interval '168 hours')::timestamptz AS grace_ends_at,
  coalesce(o.amount_minor, 0)::bigint AS amount_minor
FROM donor_billing_periods p
JOIN donors d ON d.id = p.donor_id
LEFT JOIN LATERAL (
  SELECT amount_minor FROM donor_billing_observations
  WHERE period_id = p.id AND status = 'FAILED' ORDER BY occurred_at, id LIMIT 1
) o ON true
WHERE p.status = 'FAILED'
  AND (sqlc.arg(state)::text = 'all'
    OR (sqlc.arg(state)::text = 'in_grace' AND p.due_at + interval '168 hours' > sqlc.arg(as_of)::timestamptz)
    OR (sqlc.arg(state)::text = 'lapsed' AND p.due_at + interval '168 hours' <= sqlc.arg(as_of)::timestamptz))
  AND (sqlc.narg(cursor_due)::timestamptz IS NULL
    OR (p.due_at, p.id) < (sqlc.narg(cursor_due)::timestamptz, sqlc.narg(cursor_id)::uuid))
ORDER BY p.due_at DESC, p.id DESC
LIMIT 51;

-- name: StaffAccountDeletions :many
-- Self-service erasures (donor_deletions), newest first, keyset-paged.
SELECT x.donor_id, x.deleted_at,
  (SELECT count(*) FROM donor_gifts g WHERE g.donor_id = x.donor_id)::bigint AS retained_gifts
FROM donor_deletions x
WHERE sqlc.narg(cursor_at)::timestamptz IS NULL
  OR (x.deleted_at, x.donor_id) < (sqlc.narg(cursor_at)::timestamptz, sqlc.narg(cursor_id)::uuid)
ORDER BY x.deleted_at DESC, x.donor_id DESC
LIMIT 51;
