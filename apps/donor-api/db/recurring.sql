-- name: OwnedMonthlyAttempt :one
SELECT a.* FROM donor_stripe_checkouts a JOIN donor_checkouts d ON d.id=a.checkout_id
WHERE d.donor_id=$1 AND d.frequency='MONTHLY' AND a.id=$2 FOR UPDATE OF a;

-- name: LatestMonthlyAttempt :one
SELECT a.* FROM donor_stripe_checkouts a JOIN donor_checkouts d ON d.id=a.checkout_id
WHERE d.donor_id=$1 AND d.frequency='MONTHLY' AND a.status<>'EXPIRED'
ORDER BY a.created_at DESC,a.id DESC LIMIT 1 FOR UPDATE OF a;

-- name: LatestMonthlyAttemptSnapshot :one
SELECT a.* FROM donor_stripe_checkouts a JOIN donor_checkouts d ON d.id=a.checkout_id
WHERE d.donor_id=$1 AND d.frequency='MONTHLY' AND a.status<>'EXPIRED'
ORDER BY a.created_at DESC,a.id DESC LIMIT 1;

-- name: SubscriptionObservedAt :one
SELECT observed_at FROM donor_subscriptions WHERE attempt_id=$1;

-- name: LatestConfirmedAmount :one
SELECT amount_minor FROM donor_amount_changes WHERE checkout_id=$1 AND confirmed_at IS NOT NULL ORDER BY created_at DESC,id DESC LIMIT 1;

-- name: CancellationByID :one
SELECT * FROM donor_cancellations WHERE id=$1;

-- name: InsertCancellation :exec
INSERT INTO donor_cancellations(id,donor_id,attempt_id,confirmed_period_end,created_at) VALUES($1,$2,$3,$4,$5);

-- name: CompleteCancellation :exec
UPDATE donor_cancellations SET completed_at=$2 WHERE id=$1;

-- name: AmountChangeByID :one
SELECT * FROM donor_amount_changes WHERE id=$1;

-- name: InsertAmountChange :exec
INSERT INTO donor_amount_changes(id,donor_id,checkout_id,previous_minor,amount_minor,effective_at,item_id,product_id,created_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9);

-- name: LatestPaymentUpdate :one
SELECT * FROM donor_payment_updates WHERE donor_id=$1 ORDER BY created_at DESC,id DESC LIMIT 1;

-- name: UnfinishedPaymentUpdate :one
SELECT * FROM donor_payment_updates WHERE donor_id=$1 AND status NOT IN ('APPLIED','EXPIRED','CANCELLED') ORDER BY created_at DESC,id DESC LIMIT 1;

-- name: InsertPaymentUpdate :exec
INSERT INTO donor_payment_updates(id,donor_id,attempt_id,customer_id,request_params,status,created_at)
VALUES($1,$2,$3,$4,$5,'PENDING',$6);

-- name: AcquireSubscriptionLease :execrows
INSERT INTO donor_subscription_leases(attempt_id,donor_id,command_id,expires_at)
VALUES(sqlc.arg(attempt_id),sqlc.arg(donor_id),sqlc.arg(command_id),sqlc.arg(expires_at))
ON CONFLICT (attempt_id) DO UPDATE SET command_id=EXCLUDED.command_id,expires_at=EXCLUDED.expires_at
WHERE donor_subscription_leases.expires_at<=sqlc.arg(now);

-- name: ReleaseSubscriptionLease :exec
DELETE FROM donor_subscription_leases WHERE attempt_id=$1 AND command_id=$2;
