-- name: ClaimStripeEvent :one
-- Leases the next due event to one attempt: the attempt is counted and the event
-- is not due again until lease_until, so a crashed worker's event is retried
-- after the lease expires. The returned attempts and available_at identify the
-- attempt in the statements below.
UPDATE donor_stripe_events e SET attempts = e.attempts + 1, available_at = sqlc.arg(lease_until)::timestamptz
WHERE e.id = (
    SELECT due.id FROM donor_stripe_events due
    WHERE due.processed_at IS NULL AND due.attempts < 10 AND due.available_at <= sqlc.arg(as_of)::timestamptz
    ORDER BY due.available_at, due.id
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING *;

-- name: LockClaimedStripeEvent :one
-- Fails with no rows when the attempt no longer owns the event: another worker
-- reclaimed it after the lease expired, an operator reset it, or it is done.
SELECT id FROM donor_stripe_events
WHERE id = $1 AND attempts = $2 AND available_at = $3 AND processed_at IS NULL
FOR UPDATE;

-- name: CompleteStripeEvent :exec
UPDATE donor_stripe_events SET processed_at = $2, last_error = NULL WHERE id = $1;

-- name: FailStripeEvent :execrows
UPDATE donor_stripe_events SET last_error = $2,
available_at = now() + least(power(2, attempts) * interval '30 seconds', interval '1 hour')
WHERE id = $1 AND attempts = $3 AND available_at = $4 AND processed_at IS NULL;

-- name: ReleaseStripeEvent :execrows
-- Gives back a claim whose attempt never reached a verdict, so the interrupted
-- attempt is not counted against the event.
UPDATE donor_stripe_events SET attempts = attempts - 1, available_at = sqlc.arg(as_of)::timestamptz
WHERE id = $1 AND attempts = $2 AND available_at = $3 AND processed_at IS NULL;

-- name: CheckoutDraft :one
SELECT * FROM donor_checkouts WHERE id=$1;

-- name: AttemptForCheckout :one
SELECT * FROM donor_stripe_checkouts WHERE checkout_id=$1 FOR UPDATE;

-- name: AttemptSnapshotForCheckout :one
SELECT * FROM donor_stripe_checkouts WHERE checkout_id=$1;

-- name: AttemptSnapshotByID :one
SELECT * FROM donor_stripe_checkouts WHERE id=$1;

-- name: GiftSnapshotByPayment :one
SELECT * FROM donor_gifts WHERE stripe_payment_intent_id=$1;

-- name: PaymentUpdateSnapshot :one
SELECT * FROM donor_payment_updates WHERE id=$1;

-- name: BindSubscription :exec
UPDATE donor_stripe_checkouts SET stripe_subscription_id=$2 WHERE id=$1;

-- name: RecordCheckoutSession :exec
UPDATE donor_stripe_checkouts SET stripe_session_id=$2,status=$3 WHERE id=$1;

-- name: ReleaseReservation :exec
DELETE FROM donor_recurring_reservations WHERE attempt_id=$1;

-- name: BillingPeriodByInvoice :one
SELECT * FROM donor_billing_periods WHERE stripe_invoice_id=$1;

-- name: InsertBillingPeriod :exec
INSERT INTO donor_billing_periods (id,donor_id,checkout_id,stripe_invoice_id,stripe_subscription_id,due_at,ends_at,status)
VALUES ($1,$2,$3,$4,$5,$6,$7,'FAILED') ON CONFLICT (stripe_invoice_id) DO NOTHING;

-- name: MarkPeriodPaid :exec
UPDATE donor_billing_periods SET status='PAID' WHERE id=$1;

-- name: InsertBillingObservation :exec
INSERT INTO donor_billing_observations
(id,period_id,source_event_id,status,amount_minor,currency,billing_reason,occurred_at,event_created_at,recorded_at)
VALUES ($1,$2,$3,$4,$5,'USD',$6,$7,$8,$9)
ON CONFLICT (source_event_id) DO NOTHING;

-- name: HasGift :one
SELECT EXISTS(SELECT 1 FROM donor_gifts WHERE donor_id=$1)::boolean;

-- name: InsertGift :exec
INSERT INTO donor_gifts (id,donor_id,checkout_id,stripe_object_id,amount_minor,currency,frequency,paid_at,stripe_payment_intent_id,period_id,referrer_id,referral_kind)
VALUES ($1,$2,$3,$4,$5,'USD',$6,$7,$8,$9,$10,$11) ON CONFLICT (stripe_object_id) DO NOTHING;

-- name: GiftByStripeObject :one
SELECT * FROM donor_gifts WHERE stripe_object_id=$1 FOR UPDATE;

-- name: EnrichGift :exec
UPDATE donor_gifts SET stripe_payment_intent_id=$2,period_id=$3 WHERE id=$1;

-- name: InvoiceAmountChange :one
SELECT amount_minor FROM donor_amount_changes WHERE checkout_id=$1 AND confirmed_at IS NOT NULL
AND effective_at<=$2 ORDER BY effective_at DESC,created_at DESC LIMIT 1;

-- name: PendingAmountChange :one
SELECT (confirmed_at IS NULL)::boolean AS pending FROM donor_amount_changes WHERE checkout_id=$1
ORDER BY created_at DESC LIMIT 1;

-- name: GiftByPayment :one
SELECT * FROM donor_gifts WHERE stripe_payment_intent_id=$1 FOR UPDATE;

-- name: RefundByStripeID :one
SELECT * FROM donor_refunds WHERE stripe_refund_id=$1;

-- name: InsertRefund :exec
INSERT INTO donor_refunds (id,gift_id,stripe_refund_id,amount_minor,currency,status,observed_at)
VALUES ($1,$2,$3,$4,'USD',$5,$6) ON CONFLICT (stripe_refund_id) DO NOTHING;

-- name: UpdateRefund :exec
UPDATE donor_refunds SET status=$2,observed_at=$3 WHERE id=$1;

-- name: SucceededRefundAmounts :many
SELECT amount_minor FROM donor_refunds WHERE gift_id=$1 AND status='SUCCEEDED';

-- name: InsertRefundObservation :exec
INSERT INTO donor_refund_observations (id,refund_id,source_event_id,source_action_id,status,observed_at)
VALUES ($1,$2,$3,$4,$5,$6);

-- name: LatestAmountChange :one
SELECT * FROM donor_amount_changes WHERE checkout_id=$1 ORDER BY created_at DESC LIMIT 1;

-- name: ConfirmAmountChange :exec
UPDATE donor_amount_changes SET confirmed_at=COALESCE(confirmed_at,$2) WHERE id=$1;

-- name: SaveSubscription :one
INSERT INTO donor_subscriptions (id,attempt_id,donor_id,status,period_end,stops_at,billing_anchor,external_change,observed_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
ON CONFLICT (attempt_id) DO UPDATE SET status=EXCLUDED.status,period_end=EXCLUDED.period_end,
stops_at=EXCLUDED.stops_at,billing_anchor=EXCLUDED.billing_anchor,external_change=EXCLUDED.external_change,
observed_at=EXCLUDED.observed_at RETURNING *;

-- name: CancelAttempt :exec
UPDATE donor_stripe_checkouts SET status='CANCELLED' WHERE id=$1;

-- name: PaymentUpdateByID :one
SELECT * FROM donor_payment_updates WHERE id=$1 FOR UPDATE;

-- name: SavePaymentUpdate :exec
UPDATE donor_payment_updates SET stripe_session_id=$2,status=$3,hosted_url=$4,completed_at=$5 WHERE id=$1;

-- name: RetryStripeEvent :execrows
UPDATE donor_stripe_events SET attempts=0,last_error=NULL,available_at=now() WHERE stripe_event_id=$1 AND processed_at IS NULL;

-- name: NextSubscriptionWithoutAnchor :one
-- Subscriptions synced before the billing anchor was stored, in key order
-- after the cursor. Ended ones need no anchor.
SELECT a.id,a.checkout_id,a.stripe_subscription_id,s.donor_id FROM donor_subscriptions s
JOIN donor_stripe_checkouts a ON a.id=s.attempt_id
WHERE s.billing_anchor IS NULL AND s.status NOT IN ('canceled','incomplete_expired') AND a.id>sqlc.arg(after)::uuid
ORDER BY a.id LIMIT 1;

-- name: QueueDeletedAccountRefunds :exec
-- Gifts on this checkout paid after its donor deleted the account.
INSERT INTO donor_deleted_account_reversals (id, donor_id, kind, target, amount_minor, available_at, created_at)
SELECT gen_random_uuid(), g.donor_id, 'REFUND', g.stripe_payment_intent_id, g.amount_minor,
       sqlc.arg(as_of)::timestamptz, sqlc.arg(as_of)::timestamptz
FROM donor_gifts g
JOIN donor_deletions x ON x.donor_id = g.donor_id
WHERE g.checkout_id = sqlc.arg(checkout_id) AND g.paid_at > x.deleted_at AND g.stripe_payment_intent_id IS NOT NULL
ON CONFLICT (kind, target) DO NOTHING;

-- name: QueueDeletedAccountCancellation :exec
-- The checkout's subscription, unless Stripe has already ended it.
INSERT INTO donor_deleted_account_reversals (id, donor_id, kind, target, available_at, created_at)
SELECT gen_random_uuid(), c.donor_id, 'CANCEL', a.stripe_subscription_id,
       sqlc.arg(as_of)::timestamptz, sqlc.arg(as_of)::timestamptz
FROM donor_stripe_checkouts a
JOIN donor_checkouts c ON c.id = a.checkout_id
JOIN donor_deletions x ON x.donor_id = c.donor_id
WHERE a.checkout_id = sqlc.arg(checkout_id) AND a.stripe_subscription_id IS NOT NULL
  AND NOT EXISTS (
    SELECT 1 FROM donor_subscriptions s
    WHERE s.attempt_id = a.id AND s.status IN ('canceled', 'incomplete_expired')
  )
ON CONFLICT (kind, target) DO NOTHING;

-- name: ClaimDeletedAccountReversal :one
UPDATE donor_deleted_account_reversals r SET attempts = r.attempts + 1, available_at = sqlc.arg(lease_until)::timestamptz
WHERE r.id = (
    SELECT due.id FROM donor_deleted_account_reversals due
    WHERE due.completed_at IS NULL AND due.available_at <= sqlc.arg(as_of)::timestamptz AND due.attempts < 10
    ORDER BY due.available_at, due.id
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING *;

-- name: CompleteDeletedAccountReversal :exec
UPDATE donor_deleted_account_reversals SET completed_at = $2, last_error = NULL WHERE id = $1;

-- name: FailDeletedAccountReversal :exec
UPDATE donor_deleted_account_reversals SET available_at = $2, last_error = $3 WHERE id = $1;
