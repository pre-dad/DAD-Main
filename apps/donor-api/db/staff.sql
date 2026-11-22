-- name: AuthenticatedStaff :one
SELECT s.* FROM donor_staff s JOIN donor_staff_sessions x ON x.staff_id=s.id
WHERE x.token_hash=$1 AND x.revoked_at IS NULL AND x.expires_at>$2 AND NOT s.disabled;

-- name: StaffByID :one
SELECT * FROM donor_staff WHERE id=$1 FOR UPDATE;

-- name: LockStaffAccounts :many
SELECT sqlc.embed(s), EXISTS(SELECT 1 FROM donor_staff_passkeys p WHERE p.staff_id=s.id) AS active
FROM donor_staff s ORDER BY s.id FOR UPDATE OF s;

-- name: InsertStaff :one
INSERT INTO donor_staff(id,email,name,role) VALUES($1,$2,$3,$4) RETURNING *;

-- name: InsertStaffAudit :exec
INSERT INTO donor_staff_audit(id,actor_id,action,target,details,created_at) VALUES($1,$2,$3,$4,$5,$6);

-- name: InsertStaffSession :exec
INSERT INTO donor_staff_sessions(id,staff_id,token_hash,expires_at) VALUES($1,$2,$3,$4);

-- name: RevokeStaffSession :exec
UPDATE donor_staff_sessions SET revoked_at=$2 WHERE token_hash=$1 AND revoked_at IS NULL;

-- name: RevokeStaffSessions :exec
UPDATE donor_staff_sessions SET revoked_at=$2 WHERE staff_id=$1 AND revoked_at IS NULL;

-- name: InsertStaffInvite :exec
INSERT INTO donor_staff_invites(id,staff_id,token_hash,expires_at) VALUES($1,$2,$3,$4);

-- name: StaffInviteByToken :one
SELECT * FROM donor_staff_invites WHERE token_hash=$1 FOR UPDATE;

-- name: StaffInviteByID :one
SELECT * FROM donor_staff_invites WHERE id=$1 FOR UPDATE;

-- name: StaffInviteOwner :one
SELECT staff_id FROM donor_staff_invites WHERE token_hash=$1;

-- name: SaveStaffInvite :exec
UPDATE donor_staff_invites SET attempts=$2,consumed_at=$3 WHERE id=$1;

-- name: InvalidateStaffInvites :exec
UPDATE donor_staff_invites SET consumed_at=$2 WHERE staff_id=$1 AND consumed_at IS NULL;

-- name: SetStaffAccess :exec
UPDATE donor_staff SET disabled=$2,role=$3 WHERE id=$1;

-- name: StaffPasskeys :many
SELECT * FROM donor_staff_passkeys WHERE staff_id=$1 ORDER BY id FOR UPDATE;

-- name: StaffPasskeyOwner :one
SELECT staff_id FROM donor_staff_passkeys WHERE credential_id=$1;

-- name: InsertStaffPasskey :exec
INSERT INTO donor_staff_passkeys(id,staff_id,credential_id,public_key,sign_count,transports,aaguid,attestation_format,user_verified,backup_eligible,backup_state)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11);

-- name: RecordStaffPasskeyUse :exec
UPDATE donor_staff_passkeys SET sign_count=$2,user_verified=$3,backup_state=$4,last_used_at=$5 WHERE credential_id=$1;

-- name: DeleteStaffPasskeys :execrows
DELETE FROM donor_staff_passkeys WHERE staff_id=$1;

-- name: InsertStaffCeremony :exec
INSERT INTO donor_staff_ceremonies(token_hash,kind,invite_id,session,expires_at) VALUES($1,$2,$3,$4,$5);

-- name: TakeStaffCeremony :one
DELETE FROM donor_staff_ceremonies WHERE token_hash=$1 AND kind=$2 AND expires_at>clock_timestamp()
RETURNING invite_id,session,expires_at;
