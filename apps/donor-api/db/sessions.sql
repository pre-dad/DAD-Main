-- name: AuthenticatedDonor :one
SELECT sqlc.embed(d), s.id AS session_id, s.last_used_at FROM donors d JOIN donor_sessions s ON s.donor_id = d.id
WHERE s.token_hash = $1 AND s.revoked_at IS NULL AND s.expires_at > $2
AND d.disabled = false AND d.claimed_at IS NOT NULL;

-- name: TouchSession :exec
UPDATE donor_sessions SET last_used_at = $2 WHERE id = $1 AND last_used_at < $2;

-- name: InsertSession :exec
INSERT INTO donor_sessions (id, donor_id, token_hash, expires_at, client, device_name, platform,
 refresh_family_hash, refresh_hash, refresh_expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: RefreshingSession :one
SELECT sqlc.embed(s), d.disabled FROM donor_sessions s JOIN donors d ON d.id = s.donor_id
WHERE s.refresh_family_hash = $1 FOR UPDATE OF s;

-- name: RotateSession :exec
UPDATE donor_sessions SET token_hash = $2, expires_at = $3, refresh_hash = $4, refresh_previous_hash = $5,
 refreshed_at = $6, refresh_expires_at = $7, last_used_at = $8
WHERE id = $1;

-- name: DonorSessions :many
-- Lists the sessions a donor can still use, refreshing included, marking the
-- one whose credential hashes to current_hash.
SELECT id, client, device_name, platform, created_at, last_used_at, (token_hash = sqlc.arg(current_hash))::boolean AS current
FROM donor_sessions
WHERE donor_id = $1 AND revoked_at IS NULL AND coalesce(refresh_expires_at, expires_at) > sqlc.arg(as_of)
ORDER BY current DESC, last_used_at DESC, id LIMIT 50;

-- name: DonorSessionCredential :one
SELECT token_hash FROM donor_sessions WHERE id = $1 AND donor_id = $2;

-- name: RevokeSession :exec
-- Every revocation clears the session's push token, which a check constraint
-- requires, so a signed-out installation receives no further notifications.
UPDATE donor_sessions SET revoked_at = $2, push_token = NULL, push_environment = NULL WHERE token_hash = $1 AND revoked_at IS NULL;

-- name: RevokeSessionByID :execrows
UPDATE donor_sessions SET revoked_at = $2, push_token = NULL, push_environment = NULL WHERE id = $1 AND revoked_at IS NULL;

-- name: RevokeOtherSessions :exec
UPDATE donor_sessions SET revoked_at = $3, push_token = NULL, push_environment = NULL WHERE donor_id = $1 AND token_hash <> $2 AND revoked_at IS NULL;

-- name: RevokeAllSessions :exec
UPDATE donor_sessions SET revoked_at=$2, push_token=NULL, push_environment=NULL WHERE donor_id=$1 AND revoked_at IS NULL;

-- name: RecordSessionEvent :exec
INSERT INTO donor_session_events (id, donor_id, session_id, action) VALUES ($1, $2, $3, $4);

-- name: DatabaseNow :one
SELECT clock_timestamp()::timestamptz AS now;
