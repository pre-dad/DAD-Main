-- name: CancelEmailChanges :exec
UPDATE donor_email_changes SET stage='CANCELED',encrypted_token=NULL
WHERE donor_id=$1 AND stage IN ('CURRENT','NEW');

-- name: InsertEmailChange :exec
INSERT INTO donor_email_changes(id,donor_id,old_email,new_email,stage,token_hash,encrypted_token,expires_at)
VALUES($1,$2,$3,$4,'CURRENT',$5,$6,$7);

-- name: EmailChangeOwner :one
SELECT donor_id FROM donor_email_changes WHERE token_hash=$1;

-- name: EmailChangeByToken :one
SELECT * FROM donor_email_changes WHERE token_hash=$1 FOR UPDATE;

-- name: AdvanceEmailChange :exec
UPDATE donor_email_changes SET stage='NEW',token_hash=$2,encrypted_token=$3,expires_at=$4,
sent_at=NULL,attempts=0,available_at=now() WHERE id=$1;

-- name: CompleteEmailChange :exec
UPDATE donor_email_changes SET stage='COMPLETED',encrypted_token=NULL,completed_at=$2 WHERE id=$1;

-- name: SetDonorEmail :exec
UPDATE donors SET email=$2 WHERE id=$1;

-- name: RecordIdentityEvent :exec
INSERT INTO donor_identity_events(id,donor_id,email_change_id,action) VALUES($1,$2,$3,$4);

-- name: EmailChangeDeliveryOwner :one
SELECT d.id FROM donors d JOIN donor_email_changes c ON c.donor_id=d.id
WHERE c.stage IN ('CURRENT','NEW') AND c.sent_at IS NULL AND c.attempts<sqlc.arg(max_attempts)::int AND c.available_at<=$1
ORDER BY c.available_at,c.id LIMIT 1 FOR UPDATE OF d SKIP LOCKED;

-- name: NextEmailChangeDelivery :one
SELECT * FROM donor_email_changes WHERE donor_id=$1 AND stage IN ('CURRENT','NEW')
AND sent_at IS NULL AND attempts<sqlc.arg(max_attempts)::int AND available_at<=$2 ORDER BY available_at,id LIMIT 1 FOR UPDATE;

-- name: EmailChangeDelivered :exec
UPDATE donor_email_changes SET sent_at=$2,encrypted_token=NULL WHERE id=$1 AND stage=$3;

-- name: ClaimEmailChangeDelivery :exec
UPDATE donor_email_changes SET attempts=attempts+1,available_at=$2 WHERE id=$1;

-- name: RetryEmailChangeDelivery :exec
UPDATE donor_email_changes SET available_at=$2 WHERE id=$1 AND stage=$3 AND sent_at IS NULL;

-- name: EmailChangeDeliveryFailed :exec
UPDATE donor_email_changes SET attempts=attempts+1,available_at=$2 WHERE id=$1;
