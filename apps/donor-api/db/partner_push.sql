-- name: ReleasePartnerPushToken :exec
UPDATE donor_partner_sessions SET push_token=NULL,push_environment=NULL WHERE push_token=$1;

-- name: SetPartnerPushToken :one
UPDATE donor_partner_sessions s SET platform=$2,push_token=$3,push_environment=$4
WHERE s.token_hash=$1 AND s.app AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
RETURNING id;

-- name: ClearPartnerPushToken :exec
UPDATE donor_partner_sessions SET push_token=NULL,push_environment=NULL WHERE token_hash=$1;

-- name: DropPartnerPushToken :exec
UPDATE donor_partner_sessions SET push_token=NULL,push_environment=NULL WHERE id=$1 AND push_token=$2;

-- name: QueuePartnerPush :exec
INSERT INTO donor_partner_push_notifications(session_id,partner_id,kind,dedup_key,available_at,expires_at)
SELECT s.id,u.partner_id,sqlc.arg(kind)::text,sqlc.arg(dedup_key)::text||':'||s.id::text,
    sqlc.arg(as_of)::timestamptz,least(s.expires_at,sqlc.arg(as_of)::timestamptz+interval '24 hours')
FROM donor_partner_sessions s
JOIN donor_partner_users u ON u.id=s.partner_user_id
WHERE u.partner_id=sqlc.arg(partner_id)::uuid AND NOT u.disabled AND u.accepted_at IS NOT NULL
    AND s.push_token IS NOT NULL AND s.revoked_at IS NULL AND s.expires_at>sqlc.arg(as_of)::timestamptz
ON CONFLICT(dedup_key) DO NOTHING;

-- name: NextPartnerPush :one
SELECT * FROM donor_partner_push_notifications
WHERE completed_at IS NULL AND attempts<sqlc.arg(max_attempts)::int AND available_at<=$1
ORDER BY available_at,id LIMIT 1 FOR UPDATE SKIP LOCKED;

-- name: PartnerPushTarget :one
SELECT s.platform,s.push_token,s.push_environment FROM donor_partner_sessions s
JOIN donor_partner_users u ON u.id=s.partner_user_id
JOIN donor_partner_signups p ON p.id=u.partner_id
WHERE s.id=$1 AND u.partner_id=$2 AND NOT u.disabled AND p.approved_at IS NOT NULL
    AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp() AND s.push_token IS NOT NULL;

-- name: ClaimPartnerPush :exec
UPDATE donor_partner_push_notifications SET attempts=attempts+1,available_at=$2 WHERE id=$1;

-- name: CompletePartnerPush :exec
UPDATE donor_partner_push_notifications SET completed_at=$2,last_error=NULL,suppressed_reason=$3 WHERE id=$1;

-- name: FailPartnerPush :exec
UPDATE donor_partner_push_notifications SET last_error=$2,available_at=$3,attempts=$4 WHERE id=$1 AND completed_at IS NULL;

-- name: NativePartnerSession :one
SELECT id FROM donor_partner_sessions WHERE token_hash=$1 AND app AND revoked_at IS NULL AND expires_at>clock_timestamp();
