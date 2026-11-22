-- name: GiftHistory :many
SELECT g.stripe_object_id,g.donor_id,g.amount_minor,g.currency,g.frequency,g.paid_at,g.period_id,
g.referrer_id,g.referral_kind,c.referral_expires_at
FROM donor_gifts g JOIN donor_checkouts c ON c.id=g.checkout_id WHERE g.donor_id=$1;

-- name: RefundHistory :many
SELECT r.*, g.stripe_object_id FROM donor_refunds r
JOIN donor_gifts g ON g.id=r.gift_id WHERE g.donor_id=$1;

-- name: PeriodHistory :many
SELECT * FROM donor_billing_periods WHERE donor_id=$1;

-- name: RefundObservations :many
SELECT r.stripe_refund_id, o.status, o.observed_at
FROM donor_refund_observations o JOIN donor_refunds r ON r.id=o.refund_id
JOIN donor_gifts g ON g.id=r.gift_id
WHERE g.donor_id=$1 AND o.observed_at <= $2 ORDER BY o.observed_at, o.id;

-- name: StoppedPeriods :many
SELECT stops_at FROM donor_subscriptions WHERE donor_id=$1 AND stops_at IS NOT NULL;

-- name: Projection :one
SELECT * FROM donor_metric_projections WHERE donor_id=$1;

-- name: LockProjection :one
-- Referral deltas lock this row without taking its donor lock.
SELECT * FROM donor_metric_projections WHERE donor_id=$1 FOR NO KEY UPDATE;

-- name: SaveProjection :exec
-- The caller locks the projection before aggregating its referred rows.
INSERT INTO donor_metric_projections
(id,donor_id,referrer_id,referral_kind,first_gift_id,net_minor,founding_awarded_at,
 best_streak,current_streak,at_risk,grace_ends_at,computed_at,next_refresh_at,rules_version,
 referred_link_count,referred_irl_count,referred_link_minor,referred_irl_minor,streak_since,
 current_monthly_minor,active)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)
ON CONFLICT (donor_id) DO UPDATE SET
referrer_id=EXCLUDED.referrer_id,referral_kind=EXCLUDED.referral_kind,first_gift_id=EXCLUDED.first_gift_id,
net_minor=EXCLUDED.net_minor,founding_awarded_at=EXCLUDED.founding_awarded_at,
best_streak=EXCLUDED.best_streak,current_streak=EXCLUDED.current_streak,at_risk=EXCLUDED.at_risk,
grace_ends_at=EXCLUDED.grace_ends_at,computed_at=EXCLUDED.computed_at,next_refresh_at=EXCLUDED.next_refresh_at,
rules_version=EXCLUDED.rules_version,refresh_failures=0,refresh_error=NULL,
referred_link_count=EXCLUDED.referred_link_count,referred_irl_count=EXCLUDED.referred_irl_count,
referred_link_minor=EXCLUDED.referred_link_minor,referred_irl_minor=EXCLUDED.referred_irl_minor,
streak_since=EXCLUDED.streak_since,current_monthly_minor=EXCLUDED.current_monthly_minor,active=EXCLUDED.active;

-- name: ReferralTotalsFromReferred :one
-- Reconcile under LockProjection so concurrent deltas cannot be overwritten.
SELECT
count(*) FILTER (WHERE referral_kind='LINK' AND net_minor>0)::bigint AS link_count,
count(*) FILTER (WHERE referral_kind='IRL' AND net_minor>0)::bigint AS irl_count,
coalesce(sum(net_minor) FILTER (WHERE referral_kind='LINK'),0)::bigint AS link_minor,
coalesce(sum(net_minor) FILTER (WHERE referral_kind='IRL'),0)::bigint AS irl_minor
FROM donor_metric_projections WHERE referrer_id=$1;

-- name: ApplyReferralDelta :one
-- The caller ensures the target row exists before applying the delta.
UPDATE donor_metric_projections SET
referred_link_count=referred_link_count+$2,
referred_irl_count=referred_irl_count+$3,
referred_link_minor=referred_link_minor+$4,
referred_irl_minor=referred_irl_minor+$5
WHERE donor_id=$1
RETURNING referred_link_count, referred_irl_count;

-- name: ReferralTotals :one
-- The totals are maintained incrementally on the referrer's own row (see
-- ApplyReferralDelta), so they are read directly. Readiness is a live freshness
-- check over the referred rows, since it flips when any one's next_refresh_at
-- moves. dashboard() reads the referrer's own row first and returns
-- "preparing" before
-- calling this, so own-row always exists here.
SELECT own.referred_link_count::bigint AS link_count,
own.referred_irl_count::bigint AS irl_count,
own.referred_link_minor::numeric AS link_minor,
own.referred_irl_minor::numeric AS irl_minor,
coalesce((SELECT bool_and(coalesce(ref.net_minor>=0 AND ref.next_refresh_at>$2 AND ref.referral_kind IN ('LINK','IRL'),false))
FROM donor_metric_projections ref WHERE ref.referrer_id=$1),true)::boolean AS ready
FROM donor_metric_projections own WHERE own.donor_id=$1;

-- name: MissingReferredProjection :one
-- Work stays proportional to the referrer's checkouts: a gift's donor is its
-- checkout's, and the scalar subquery (NULL for a missing row or a missing
-- net) is probed per checkout, where a join lets the planner scan every
-- gift or projection once a tree is large.
SELECT EXISTS(SELECT 1 FROM donor_checkouts c WHERE c.referrer_id=$1
AND (SELECT p.net_minor FROM donor_metric_projections p WHERE p.donor_id=c.donor_id) IS NULL
AND EXISTS(SELECT 1 FROM donor_gifts g WHERE g.checkout_id=c.id))::boolean;

-- name: InsertReferralCorrection :exec
INSERT INTO donor_referral_corrections
    (donor_id,old_first_gift_id,new_first_gift_id,old_referrer_id,new_referrer_id,old_kind,new_kind,recorded_at,rules_version)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9);

-- name: SaveRetentionMilestone :exec
INSERT INTO donor_retention_milestones(donor_id,days,cohort_at,checkpoint_at,retained,computed_at)
VALUES($1,$2,$3,$4,$5,$6)
ON CONFLICT(donor_id,days) DO UPDATE SET cohort_at=EXCLUDED.cohort_at,
checkpoint_at=EXCLUDED.checkpoint_at,retained=EXCLUDED.retained,computed_at=EXCLUDED.computed_at;

-- name: FoundingClaimed :one
-- Whether the donor holds a waitlist grant (D-6).
SELECT EXISTS(SELECT 1 FROM donor_founding_grants WHERE donor_id = $1)::boolean;

-- name: GrantFoundingClaim :exec
-- Grants the donor Founding Donor if their address hash has a claim.
INSERT INTO donor_founding_grants (donor_id, granted_at)
SELECT sqlc.arg(donor_id)::uuid, sqlc.arg(now)::timestamptz
WHERE EXISTS(SELECT 1 FROM donor_founding_claims
             WHERE email_hash = sqlc.arg(email_hash)::text)
ON CONFLICT (donor_id) DO NOTHING;

-- name: GrantFoundingClaimsByEmail :exec
-- Grants Founding Donor to the donors who already hold imported addresses.
INSERT INTO donor_founding_grants (donor_id, granted_at)
SELECT id, sqlc.arg(now)::timestamptz FROM donors WHERE email = ANY(sqlc.arg(emails)::text[])
ON CONFLICT (donor_id) DO NOTHING;

-- name: ImportFoundingClaims :execrows
-- An address listed more than once keeps its earliest sign-up.
INSERT INTO donor_founding_claims(email_hash, signed_up_on, source, imported_at)
SELECT unnest(sqlc.arg(hashes)::text[]), unnest(sqlc.arg(dates)::date[]), unnest(sqlc.arg(sources)::text[]),
  sqlc.arg(imported_at)::timestamptz
ON CONFLICT (email_hash) DO UPDATE SET
  signed_up_on = least(donor_founding_claims.signed_up_on, EXCLUDED.signed_up_on),
  source = CASE WHEN EXCLUDED.signed_up_on < donor_founding_claims.signed_up_on THEN EXCLUDED.source ELSE donor_founding_claims.source END
WHERE EXCLUDED.signed_up_on < donor_founding_claims.signed_up_on;
