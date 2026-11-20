-- R1 staff reporting (PRD §4 success metrics / docs/analytics.md).
--
-- These queries use committed financial facts. Consented journey and share
-- observations are queried separately in analytics.sql; missing observations
-- are unavailable, never fabricated zeros.
--
-- Attribution is the FROZEN source on the donor's own projection row
-- (referrer_id / referral_kind, set at the earliest verified payment), not the
-- browser campaign parameters or the candidate stored on the gift row, which
-- recognition may have withheld. DIRECT = no referrer. A donor's "first gift" is
-- the projection's first_gift_id, which references donor_gifts.stripe_object_id.
--
-- Reporting periods are half-open [start, end) on gift paid_at / period due_at,
-- in UTC. The range queries take $1=start, $2=end; the as-of queries take the
-- named cutoff, start_at and end_at. The service echoes the cutoff and
-- timezone alongside the numbers.

-- name: ReportFoundingDonors :one
-- Distinct donors whose permanent founding award was granted (projection
-- founding_awarded_at). A later refund/cancellation does not remove it. Scoped
-- to awards recorded in the period so the report is period-attributable.
SELECT count(*)::bigint AS founding
FROM donor_metric_projections
WHERE founding_awarded_at >= $1 AND founding_awarded_at < $2;

-- name: ReportFirstGiftBySource :many
-- First-gift conversions grouped by frozen source (DIRECT/LINK/IRL). A "first
-- gift" is the gift the projection points at (first_gift_id); its paid_at must
-- fall in the period. gift mix (frequency) rides along so first-gift donor mix
-- is reported without confusing gifts with donors.
SELECT
	CASE WHEN p.referral_kind IS NULL THEN 'DIRECT' ELSE p.referral_kind END AS source,
	g.frequency,
	count(*)::bigint AS donors,
	coalesce(sum(g.amount_minor), 0)::bigint AS gross_minor
FROM donor_metric_projections p
JOIN donor_gifts g ON g.stripe_object_id = p.first_gift_id
WHERE p.first_gift_id IS NOT NULL
	AND g.paid_at >= $1 AND g.paid_at < $2
GROUP BY source, g.frequency;

-- name: ReportGiftMix :many
-- Successful gift count and collected dollars by frequency over the period.
-- Counts every gift (not just first gifts), so recurring volume is visible.
SELECT g.frequency, count(*)::bigint AS gifts, coalesce(sum(g.amount_minor), 0)::bigint AS collected_minor
FROM donor_gifts g
WHERE g.paid_at >= $1 AND g.paid_at < $2
GROUP BY g.frequency;

-- name: ReportViralCoefficient :one
WITH first_gifts AS MATERIALIZED (
  SELECT DISTINCT ON (g.donor_id) g.donor_id, g.paid_at, p.referrer_id
  FROM donor_gifts g LEFT JOIN donor_metric_projections p ON p.donor_id=g.donor_id
  WHERE g.paid_at < sqlc.arg(cutoff)::timestamptz
  ORDER BY g.donor_id, g.paid_at, g.stripe_object_id
), cohort AS (
  SELECT donor_id FROM first_gifts WHERE paid_at >= sqlc.arg(start_at)::timestamptz
    AND paid_at < sqlc.arg(end_at)::timestamptz
), conversions AS (
  SELECT f.donor_id FROM first_gifts f JOIN cohort c ON c.donor_id=f.referrer_id
)
SELECT (SELECT count(*) FROM cohort)::bigint AS cohort,
  count(*)::bigint AS gross_conversions,
  count(*) FILTER (WHERE EXISTS (
    SELECT 1 FROM donor_gifts g WHERE g.donor_id=conversions.donor_id AND g.paid_at < sqlc.arg(cutoff)::timestamptz
    AND g.amount_minor > coalesce((SELECT sum(r.amount_minor) FROM donor_refunds r WHERE r.gift_id=g.id
      AND coalesce((SELECT o.status FROM donor_refund_observations o WHERE o.refund_id=r.id
        AND o.observed_at < sqlc.arg(cutoff)::timestamptz ORDER BY o.observed_at DESC,o.id DESC LIMIT 1),
        CASE WHEN r.observed_at < sqlc.arg(cutoff)::timestamptz THEN r.status END)='SUCCEEDED'),0)
  ))::bigint AS qualified_conversions
FROM conversions;

-- name: ReportReferralDollars :one
WITH first_gifts AS (
  SELECT DISTINCT ON (g.donor_id) g.donor_id,g.paid_at,p.referrer_id,p.referral_kind
  FROM donor_gifts g LEFT JOIN donor_metric_projections p ON p.donor_id=g.donor_id
  WHERE g.paid_at < sqlc.arg(cutoff)::timestamptz
  ORDER BY g.donor_id,g.paid_at,g.stripe_object_id
), net AS (
  SELECT f.referral_kind,g.amount_minor-coalesce((SELECT sum(r.amount_minor) FROM donor_refunds r WHERE r.gift_id=g.id
    AND coalesce((SELECT o.status FROM donor_refund_observations o WHERE o.refund_id=r.id
      AND o.observed_at < sqlc.arg(cutoff)::timestamptz ORDER BY o.observed_at DESC,o.id DESC LIMIT 1),
      CASE WHEN r.observed_at < sqlc.arg(cutoff)::timestamptz THEN r.status END)='SUCCEEDED'),0) AS amount
  FROM first_gifts f JOIN donor_gifts g ON g.donor_id=f.donor_id
  WHERE f.referrer_id IS NOT NULL AND f.paid_at >= sqlc.arg(start_at)::timestamptz
    AND f.paid_at < sqlc.arg(end_at)::timestamptz AND g.paid_at < sqlc.arg(cutoff)::timestamptz
)
SELECT coalesce(sum(amount) FILTER(WHERE referral_kind='LINK'),0)::bigint AS link_minor,
  coalesce(sum(amount) FILTER(WHERE referral_kind='IRL'),0)::bigint AS irl_minor FROM net;

-- name: ReportInvoluntaryChurn :one
WITH failures AS (
  SELECT DISTINCT ON (o.period_id) o.period_id, o.amount_minor
  FROM donor_billing_observations o JOIN donor_billing_periods p ON p.id=o.period_id
  WHERE o.status='FAILED' AND o.billing_reason='subscription_cycle'
    AND o.occurred_at < sqlc.arg(cutoff)::timestamptz
    AND p.due_at >= sqlc.arg(start_at)::timestamptz AND p.due_at < sqlc.arg(end_at)::timestamptz
  ORDER BY o.period_id,o.occurred_at,o.id
), periods AS (
  SELECT f.*, p.due_at, paid.paid_at, paid.amount_minor AS paid_minor
  FROM failures f JOIN donor_billing_periods p ON p.id=f.period_id
  LEFT JOIN LATERAL (SELECT g.paid_at,g.amount_minor FROM donor_gifts g WHERE g.period_id=p.id
    AND g.paid_at < sqlc.arg(cutoff)::timestamptz ORDER BY g.paid_at,g.stripe_object_id LIMIT 1) paid ON true
)
SELECT count(*)::bigint AS failed_periods,
  coalesce(sum(amount_minor) FILTER (WHERE due_at+interval '7 days' <= sqlc.arg(cutoff)::timestamptz
    AND (paid_at IS NULL OR paid_at >= due_at+interval '7 days')),0)::bigint AS lost_minor,
  -- A payment after the grace window counts in both lost_minor and recovered_periods.
  count(*) FILTER (WHERE paid_at IS NOT NULL)::bigint AS recovered_periods,
  coalesce(sum(paid_minor),0)::bigint AS recovered_minor,
  count(*) FILTER (WHERE paid_at < due_at+interval '7 days')::bigint AS recovered_within_grace,
  count(*) FILTER (WHERE paid_minor IS NOT NULL AND paid_minor<>amount_minor)::bigint AS inconsistent_periods,
  (SELECT count(*) FROM donor_billing_periods p WHERE p.due_at >= sqlc.arg(start_at)::timestamptz
    AND p.due_at < sqlc.arg(end_at)::timestamptz AND p.status IN ('PAID','FAILED')
    AND NOT EXISTS(SELECT 1 FROM donor_billing_observations o WHERE o.period_id=p.id
      AND o.occurred_at < sqlc.arg(cutoff)::timestamptz))::bigint AS missing_history
FROM periods;

-- name: ReportRetention :many
SELECT days,count(*)::bigint AS eligible,
  count(*) FILTER (WHERE retained)::bigint AS retained,
  count(*) FILTER (WHERE retained IS NULL)::bigint AS unknown
FROM donor_retention_milestones
WHERE cohort_at >= sqlc.arg(start_at)::timestamptz AND cohort_at < sqlc.arg(end_at)::timestamptz
  AND checkpoint_at <= sqlc.arg(cutoff)::timestamptz
GROUP BY days ORDER BY days;

-- name: ReportRetentionMissing :one
SELECT EXISTS(SELECT 1 FROM donor_gifts g WHERE g.frequency='MONTHLY'
  AND g.paid_at >= $1 AND g.paid_at < $2 AND NOT EXISTS (
    SELECT 1 FROM donor_retention_milestones r WHERE r.donor_id=g.donor_id));
