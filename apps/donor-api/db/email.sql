-- name: NextLoginEmail :one
SELECT * FROM donor_login_emails WHERE completed_at IS NULL AND attempts<sqlc.arg(max_attempts)::int AND available_at<=$1
ORDER BY available_at,id LIMIT 1 FOR UPDATE SKIP LOCKED;

-- name: LoginEmailByID :one
SELECT * FROM donor_login_emails WHERE id=$1 FOR UPDATE;

-- name: CompleteLoginEmail :exec
UPDATE donor_login_emails SET completed_at=$2,encrypted_token=NULL,last_error=NULL WHERE id=$1;

-- name: PrepareLoginEmail :exec
UPDATE donor_login_emails SET encrypted_token=$2,expires_at=$3 WHERE id=$1;

-- name: InsertLoginLink :exec
INSERT INTO donor_login_links (id,donor_id,token_hash,expires_at) VALUES ($1,$2,$3,$4);

-- name: FailLoginEmail :exec
UPDATE donor_login_emails SET last_error=$2,available_at=$3 WHERE id=$1 AND completed_at IS NULL;

-- name: ClaimLoginEmail :exec
UPDATE donor_login_emails SET attempts=attempts+1,available_at=$2 WHERE id=$1;

-- name: ReceiptGift :one
SELECT * FROM donor_gifts WHERE id=$1 AND donor_id=$2 AND paid_at<=$3;

-- name: ReceiptRefunds :many
SELECT * FROM donor_refunds WHERE gift_id=$1 ORDER BY observed_at,id;

-- name: AnnualGifts :many
SELECT * FROM donor_gifts WHERE donor_id=$1 AND paid_at>=sqlc.arg(starts_at)
AND paid_at<sqlc.arg(ends_at) AND paid_at<=sqlc.arg(as_of) ORDER BY paid_at,id;

-- name: AnnualRefunds :many
SELECT r.* FROM donor_refunds r JOIN donor_gifts g ON g.id=r.gift_id
WHERE g.donor_id=$1 AND g.paid_at>=sqlc.arg(starts_at)
AND g.paid_at<sqlc.arg(ends_at) AND g.paid_at<=sqlc.arg(as_of)
ORDER BY r.gift_id,r.observed_at,r.id;

-- name: NextMessage :one
SELECT * FROM donor_messages WHERE completed_at IS NULL AND attempts<sqlc.arg(max_attempts)::int AND available_at<=$1
ORDER BY available_at,id LIMIT 1 FOR UPDATE SKIP LOCKED;

-- name: MessageByID :one
SELECT * FROM donor_messages WHERE id=$1 FOR UPDATE;

-- name: CompleteMessage :exec
UPDATE donor_messages SET completed_at=$2,last_error=NULL,suppressed_reason=$3 WHERE id=$1;

-- name: RenderMessage :exec
UPDATE donor_messages SET subject=$2,body=$3 WHERE id=$1;

-- name: IncrementMessageAttempt :exec
UPDATE donor_messages SET attempts=attempts+1 WHERE id=$1;

-- name: FailMessage :exec
UPDATE donor_messages SET last_error=$2,available_at=$3 WHERE id=$1 AND completed_at IS NULL;

-- name: ClaimMessage :exec
UPDATE donor_messages SET attempts=attempts+1,available_at=$2 WHERE id=$1;

-- name: DonorPreference :one
SELECT * FROM donor_preferences WHERE donor_id=$1;

-- name: MessagePeriod :one
SELECT * FROM donor_billing_periods WHERE id=$1 AND donor_id=$2;

-- name: SubscriptionForCheckout :one
SELECT s.* FROM donor_subscriptions s JOIN donor_stripe_checkouts a ON a.id=s.attempt_id WHERE a.checkout_id=$1;
