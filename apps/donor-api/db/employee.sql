-- Employee giving (PRD §7.4). Partners only ever see totals; the caller
-- suppresses any group smaller than its minimum (EM-3). Gift totals are net of
-- succeeded refunds.

-- name: PartnerByCompanyCode :one
-- Resolve an uppercase company code to its APPROVED partner org (EM-1).
SELECT id FROM donor_partner_signups
WHERE company_code = $1 AND approved_at IS NOT NULL;

-- name: UpsertEmployeeLink :exec
-- Link a donor to a partner (EM-2). Idempotent: re-running keeps the first
-- enrolment time and does not change an existing partner (one employer).
INSERT INTO donor_partner_employees (donor_id, partner_id, enrolled_at)
VALUES ($1, $2, $3)
ON CONFLICT (donor_id) DO NOTHING;

-- name: EmployeeLink :one
SELECT donor_id, partner_id, team_id, enrolled_at FROM donor_partner_employees WHERE donor_id = $1;

-- name: SetEmployeeTeam :exec
-- Place an employee on a team (EM-4). The service checks the team belongs to
-- the employee's partner before calling this.
UPDATE donor_partner_employees SET team_id = $2 WHERE donor_id = $1;

-- name: InsertTeam :one
INSERT INTO donor_partner_teams (id, partner_id, name, created_at) VALUES ($1,$2,$3,$4)
ON CONFLICT (partner_id, name) DO NOTHING
RETURNING id;

-- name: TeamByID :one
SELECT id, partner_id, name FROM donor_partner_teams WHERE id = $1;

-- name: PartnerOurPeople :one
-- Our People (P-13): linked employees, their giving all-time and in the
-- calendar year (the matching base, including gifts from before they linked),
-- their average current streak in days, volunteer minutes from event
-- check-ins (EM-8), and the number of teams.
WITH linked AS (
  SELECT e.donor_id FROM donor_partner_employees e WHERE e.partner_id = sqlc.arg(partner_id)::uuid
), gifts AS (
  SELECT g.paid_at,
    g.amount_minor - coalesce((SELECT sum(r.amount_minor) FROM donor_refunds r
                               WHERE r.gift_id = g.id AND r.status = 'SUCCEEDED'), 0) AS net_minor
  FROM donor_gifts g WHERE g.donor_id IN (SELECT donor_id FROM linked)
)
SELECT
  (SELECT count(*) FROM linked)::bigint AS enrolled,
  coalesce((SELECT sum(net_minor) FROM gifts), 0)::bigint AS given_minor,
  coalesce((SELECT sum(net_minor) FROM gifts
            WHERE paid_at >= sqlc.arg(year_start)::timestamptz AND paid_at < sqlc.arg(year_end)::timestamptz), 0)::bigint AS year_given_minor,
  coalesce((SELECT round(avg(greatest(0,
              (sqlc.arg(as_of)::timestamptz AT TIME ZONE coalesce(d.timezone,'America/New_York'))::date
              - (m.streak_since AT TIME ZONE coalesce(d.timezone,'America/New_York'))::date)))
            FROM linked l JOIN donors d ON d.id=l.donor_id
            JOIN donor_metric_projections m ON m.donor_id=l.donor_id
            WHERE m.streak_since IS NOT NULL),0)::bigint AS avg_streak_days,
  coalesce(NOT EXISTS(SELECT 1 FROM linked l LEFT JOIN donor_metric_projections m ON m.donor_id=l.donor_id
    WHERE CASE WHEN m.donor_id IS NULL THEN EXISTS(SELECT 1 FROM donor_gifts g WHERE g.donor_id=l.donor_id)
      ELSE m.next_refresh_at<=sqlc.arg(as_of)::timestamptz OR m.refresh_error IS NOT NULL
        OR m.rules_version<sqlc.arg(rules_version)::integer END),false)::boolean AS streak_verified,
  coalesce((SELECT sum(extract(epoch FROM ev.ends_at - ev.starts_at) / 60)
            FROM donor_event_reservations r JOIN donor_events ev ON ev.id = r.event_id
            WHERE r.donor_id IN (SELECT donor_id FROM linked) AND r.checked_in_at IS NOT NULL), 0)::bigint AS volunteer_minutes,
  (SELECT count(DISTINCT e.team_id) FROM donor_partner_employees e
   WHERE e.partner_id = sqlc.arg(partner_id)::uuid AND e.team_id IS NOT NULL)::bigint AS teams,
  p.workforce_size, p.match_rate_bp, p.match_cap_minor
FROM donor_partner_signups p
WHERE p.id = sqlc.arg(partner_id)::uuid;

-- name: TeamLeaderboard :many
-- Team leaderboard (EM-5), ranked by combined active giving (the annualized
-- pledges of members whose gift is current), with average streak in days and
-- volunteer minutes as the secondary views. Teams smaller than the minimum are
-- left out.
WITH members AS (
  SELECT e.team_id, e.donor_id
  FROM donor_partner_employees e
  WHERE e.partner_id = sqlc.arg(partner_id)::uuid AND e.team_id IS NOT NULL
), volunteer AS (
  SELECT r.donor_id, sum(extract(epoch FROM ev.ends_at - ev.starts_at) / 60) AS minutes
  FROM donor_event_reservations r JOIN donor_events ev ON ev.id = r.event_id
  WHERE r.checked_in_at IS NOT NULL AND r.donor_id IN (SELECT donor_id FROM members)
  GROUP BY r.donor_id
)
SELECT t.id AS team_id, t.name,
  (count(*) FILTER (WHERE m.active))::bigint AS active_givers,
  (coalesce(sum(m.current_monthly_minor) FILTER (WHERE m.active), 0) * 12)::bigint AS annualized_minor,
  coalesce(round(avg(greatest(0,
    (sqlc.arg(as_of)::timestamptz AT TIME ZONE coalesce(d.timezone,'America/New_York'))::date
    - (m.streak_since AT TIME ZONE coalesce(d.timezone,'America/New_York'))::date))
    FILTER (WHERE m.streak_since IS NOT NULL)),0)::bigint AS avg_streak_days,
  bool_and(CASE WHEN m.donor_id IS NULL THEN NOT EXISTS(SELECT 1 FROM donor_gifts g WHERE g.donor_id=mem.donor_id)
    ELSE m.next_refresh_at>sqlc.arg(as_of)::timestamptz AND m.refresh_error IS NULL
      AND m.rules_version>=sqlc.arg(rules_version)::integer END)::boolean AS metrics_verified,
  coalesce(sum(v.minutes), 0)::bigint AS volunteer_minutes
FROM members mem
JOIN donors d ON d.id=mem.donor_id
JOIN donor_partner_teams t ON t.id = mem.team_id
LEFT JOIN donor_metric_projections m ON m.donor_id = mem.donor_id
LEFT JOIN volunteer v ON v.donor_id = mem.donor_id
GROUP BY t.id, t.name
HAVING count(*) >= sqlc.arg(minimum)::int
ORDER BY metrics_verified DESC, annualized_minor DESC, active_givers DESC, t.name, t.id;

-- name: PartnerMatchTerms :one
SELECT match_rate_bp, match_cap_minor FROM donor_partner_signups WHERE id = $1 AND approved_at IS NOT NULL;

-- name: LinkedEmployeeGiftsInYear :one
-- Linked employees' giving in a calendar year, net of refunds (EM-6). Gifts
-- from before an employee linked count too: the link attributes their whole
-- giving to the employer for matching.
SELECT coalesce(sum(g.amount_minor - coalesce((SELECT sum(r.amount_minor) FROM donor_refunds r
                                               WHERE r.gift_id = g.id AND r.status = 'SUCCEEDED'), 0)), 0)::bigint AS gifts_minor
FROM donor_gifts g
JOIN donor_partner_employees e ON e.donor_id = g.donor_id
WHERE e.partner_id = $1
  AND g.paid_at >= sqlc.arg(year_start)::timestamptz
  AND g.paid_at <  sqlc.arg(year_end)::timestamptz;

-- name: InsertMatchPayment :execrows
-- Record an offline match payment received (EM-7). No money moves in-app.
INSERT INTO donor_partner_match_payments (id, partner_id, year, amount_minor, received_at, recorded_by)
VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT(id) DO NOTHING;

-- name: MatchPaymentMatches :one
SELECT EXISTS(SELECT 1 FROM donor_partner_match_payments WHERE id=$1 AND partner_id=$2 AND year=$3 AND amount_minor=$4);

-- name: MatchPaymentsInYear :one
SELECT coalesce(sum(amount_minor), 0)::bigint AS paid_minor
FROM donor_partner_match_payments WHERE partner_id = $1 AND year = $2;

-- name: SetPartnerWorkforce :exec
-- EM-9: workforce size, admin-set and partner-updatable.
UPDATE donor_partner_signups SET workforce_size = $2 WHERE id = $1;

-- name: PartnerOrganizationByID :one
SELECT organization FROM donor_partner_signups WHERE id = $1;

-- name: TeamIDByName :one
SELECT id FROM donor_partner_teams WHERE partner_id = $1 AND name = $2;

-- name: PartnersDueTeamMonth :many
-- Approved partners with a team whose standings for the month are not yet recorded.
SELECT s.id FROM donor_partner_signups s
WHERE s.approved_at IS NOT NULL
AND EXISTS(SELECT 1 FROM donor_partner_employees e WHERE e.partner_id = s.id AND e.team_id IS NOT NULL)
AND NOT EXISTS(SELECT 1 FROM donor_team_months m WHERE m.partner_id = s.id AND m.month = sqlc.arg(month)::date)
ORDER BY s.id
LIMIT sqlc.arg(batch)::int;

-- name: CloseTeamMonth :execrows
INSERT INTO donor_team_months(partner_id, month, closed_at) VALUES($1, $2, $3)
ON CONFLICT (partner_id, month) DO NOTHING;

-- name: RecordTeamMonthStandings :many
-- Ranks the partner's teams by the annualized active giving of their members
-- as the month closes, using the same ordering as the company leaderboard.
WITH standings AS (
  SELECT e.team_id,
    count(*)::int AS members,
    (coalesce(sum(m.current_monthly_minor) FILTER (WHERE m.active), 0) * 12)::bigint AS annualized_minor,
    (count(*) FILTER (WHERE m.active))::int AS active_givers
  FROM donor_partner_employees e
  LEFT JOIN donor_metric_projections m ON m.donor_id = e.donor_id
  WHERE e.partner_id = sqlc.arg(partner_id)::uuid AND e.team_id IS NOT NULL
  GROUP BY e.team_id
  HAVING count(*) >= sqlc.arg(minimum)::int
), ranked AS (
  SELECT s.*, s.annualized_minor / s.members AS per_member_minor,
    row_number() OVER (ORDER BY s.annualized_minor DESC, s.active_givers DESC, t.name, t.id)::int AS rank
  FROM standings s JOIN donor_partner_teams t ON t.id = s.team_id
)
INSERT INTO donor_team_month_standings(partner_id, month, team_id, rank, members, annualized_minor, per_member_minor)
SELECT sqlc.arg(partner_id)::uuid, sqlc.arg(month)::date, team_id, rank, members, annualized_minor, per_member_minor FROM ranked
RETURNING team_id, rank, per_member_minor;

-- name: SetTeamMonthWinner :exec
UPDATE donor_team_months SET winner_team_id = $3 WHERE partner_id = $1 AND month = $2;

-- name: TeamMembers :many
SELECT donor_id FROM donor_partner_employees WHERE team_id = $1 ORDER BY donor_id;

-- name: PartnerTeamMetricsVerified :one
WITH eligible AS (
 SELECT team_id FROM donor_partner_employees
 WHERE partner_id=sqlc.arg(partner_id)::uuid AND team_id IS NOT NULL
 GROUP BY team_id HAVING count(*)>=sqlc.arg(minimum)::integer
)
SELECT coalesce(bool_and(CASE WHEN m.donor_id IS NULL
  THEN NOT EXISTS(SELECT 1 FROM donor_gifts g WHERE g.donor_id=e.donor_id)
  ELSE m.next_refresh_at>sqlc.arg(as_of)::timestamptz AND m.refresh_error IS NULL
    AND m.rules_version>=sqlc.arg(rules_version)::integer END),true)::boolean
FROM donor_partner_employees e JOIN eligible t ON t.team_id=e.team_id
LEFT JOIN donor_metric_projections m ON m.donor_id=e.donor_id;
