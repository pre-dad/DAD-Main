-- name: InsertPartnerApplication :execrows
-- P-1: a new partner application. A repeated id inserts nothing, so a retry
-- that races its original reads the stored row. The partial unique index
-- rejects a second PENDING application for the same work email.
INSERT INTO donor_partner_applications
  (id, organization, website, type, contact_name, title, work_email, phone, workforce_size, created_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10)
ON CONFLICT (id) DO NOTHING;

-- name: PartnerApplicationByID :one
SELECT * FROM donor_partner_applications WHERE id=$1 FOR UPDATE;

-- name: PendingPartnerApplications :many
-- §8 Partnerships approval queue, oldest first.
SELECT * FROM donor_partner_applications
WHERE status='PENDING' AND (created_at, id) > (sqlc.arg(cursor_at)::timestamptz, sqlc.arg(cursor_id)::uuid)
ORDER BY created_at ASC, id
LIMIT 51;

-- name: DecidePartnerApplication :exec
-- Records an approve/reject decision (status + decider + time + reason) and, on
-- approval, the partner-org row it produced.
UPDATE donor_partner_applications
SET status=$2, decided_by=$3, decided_at=$4, decision_reason=$5, approved_partner_id=$6, updated_at=$4
WHERE id=$1 AND status='PENDING';

-- name: PromotePartnerOrg :exec
-- Promotes an approved application's organization into the partner-org anchor
-- (donor_partner_signups) that initiatives reference. Reuses the signups row so
-- the existing initiative FK is unchanged; sets the approved + §8 settings.
INSERT INTO donor_partner_signups
  (id, organization, name, email, website, message, request_hash, consent_version,
   approved_at, partner_type, workforce_size, company_code)
VALUES ($1,$2,$3,$4,$5,'','','r1-partner-approved-v1',$6,$7,$8,$9);

-- name: PartnerOrgByID :one
SELECT * FROM donor_partner_signups WHERE id=$1;

-- name: LockPartnerOrg :one
SELECT id FROM donor_partner_signups WHERE id=$1 AND approved_at IS NOT NULL FOR UPDATE;

-- name: StaffPartnersPage :many
-- §8 Partnerships: approved partner orgs with their active users and linked
-- initiatives. Rows without approved_at are R1 contact-form inquiries.
SELECT p.id, p.organization, p.partner_type, p.company_code, p.workforce_size, p.match_rate_bp, p.match_cap_minor, p.approved_at,
  (SELECT count(*) FROM donor_partner_users u WHERE u.partner_id=p.id AND NOT u.disabled)::bigint AS user_count,
  (SELECT count(*) FROM donor_initiative_partners ip WHERE ip.partner_id=p.id)::bigint AS initiative_count
FROM donor_partner_signups p
WHERE p.approved_at IS NOT NULL
ORDER BY p.organization, p.id
LIMIT 51 OFFSET sqlc.arg(page_offset)::bigint;

-- name: StaffPartnerInitiatives :many
SELECT i.id, i.title, i.status, i.publish_state, ip.committed_minor
FROM donor_initiatives i JOIN donor_initiative_partners ip ON ip.initiative_id=i.id
WHERE ip.partner_id=$1
ORDER BY title, id;

-- name: SetPartnerMatch :execrows
-- §8 Partnerships: set workforce size, match rate (basis points) and cap.
UPDATE donor_partner_signups
SET workforce_size=$2, match_rate_bp=$3, match_cap_minor=$4
WHERE id=$1 AND approved_at IS NOT NULL;

-- name: UpdatePartnerProfile :execrows
-- Partner admin edits the org profile (§7.2).
UPDATE donor_partner_signups
SET organization=$2, description=$3
WHERE id=$1 AND approved_at IS NOT NULL;

-- name: LinkInitiativeToPartner :execrows
-- §8/P-3: link an initiative to its partner org.
INSERT INTO donor_initiative_partners(initiative_id,partner_id,committed_minor,updated_at)
SELECT id, sqlc.arg(partner_id)::uuid, coalesce(sqlc.narg(committed_minor)::bigint, committed_minor), sqlc.arg(updated_at)::timestamptz
FROM donor_initiatives WHERE id=sqlc.arg(initiative_id)::uuid
ON CONFLICT(initiative_id,partner_id) DO UPDATE SET
 committed_minor=coalesce(sqlc.narg(committed_minor)::bigint,donor_initiative_partners.committed_minor),updated_at=EXCLUDED.updated_at;

-- name: PartnerUsersForPartner :many
SELECT * FROM donor_partner_users WHERE partner_id=$1 AND NOT disabled ORDER BY created_at, id;

-- name: DisablePartnerUser :execrows
-- P-4: a partner admin removes a user (soft delete).
UPDATE donor_partner_users SET disabled=true,invite_token_hash=NULL,invite_expires_at=NULL WHERE partner_id=$1 AND id=$2;

-- name: RevokePartnerUserSessions :exec
UPDATE donor_partner_sessions SET revoked_at=$2,push_token=NULL,push_environment=NULL WHERE partner_user_id=$1 AND revoked_at IS NULL;

-- name: InvitePartnerUser :one
-- P-4: a partner admin invites a VIEWER (or ADMIN) by email; the signer of an
-- approved application is invited as the first ADMIN. Re-inviting refreshes
-- the role and re-enables a removed user. The emailed link is minted on delivery.
INSERT INTO donor_partner_users (id, partner_id, email, name, role, invited_by, created_at)
VALUES ($1,$2,$3,$4,$5,$6,$7)
ON CONFLICT (partner_id, email) DO UPDATE
SET role=EXCLUDED.role, disabled=false
RETURNING id;

-- name: PartnerUserByInvite :one
SELECT * FROM donor_partner_users
WHERE invite_token_hash=$1 AND NOT disabled AND invite_expires_at>clock_timestamp()
FOR UPDATE;

-- name: AcceptPartnerInvite :exec
-- Spends the sign-in link; the first one also marks the user accepted.
UPDATE donor_partner_users
SET accepted_at=coalesce(accepted_at, $2), invite_token_hash=NULL, invite_expires_at=NULL
WHERE id=$1;

-- name: InsertPartnerSession :exec
INSERT INTO donor_partner_sessions (id, partner_user_id, token_hash, expires_at, created_at, app)
VALUES ($1,$2,$3,$4,$5,$6);

-- name: AuthenticatedPartnerUser :one
-- Resolves a live partner session to its user + org, rejecting expired/revoked
-- sessions and disabled users.
SELECT u.*, p.organization, p.approved_at
FROM donor_partner_sessions s
JOIN donor_partner_users u ON u.id = s.partner_user_id
JOIN donor_partner_signups p ON p.id = u.partner_id
WHERE s.token_hash=$1 AND s.revoked_at IS NULL AND s.expires_at > sqlc.arg(as_of)::timestamptz
  AND NOT u.disabled AND p.approved_at IS NOT NULL;

-- name: RevokePartnerSession :exec
UPDATE donor_partner_sessions SET revoked_at=$2,push_token=NULL,push_environment=NULL WHERE token_hash=$1 AND revoked_at IS NULL;

-- name: PartnerSpendByCategory :many
-- P-12: spend by category across a partner's linked initiatives (totals only).
SELECT s.category, sum(s.amount_minor)::bigint AS amount_minor
FROM donor_initiative_spend s
JOIN donor_initiatives i ON i.id = s.initiative_id
WHERE EXISTS(SELECT 1 FROM donor_initiative_partners ip WHERE ip.initiative_id=i.id AND ip.partner_id=$1) AND i.publish_state='PUBLISHED'
GROUP BY s.category
ORDER BY s.category;

-- name: PartnerOverviewTotals :one
-- P-10 tiles over the partner's published initiatives: committed, deployed
-- (their spend), people reached (published verified PEOPLE metrics, never an
-- estimate, §7.3), linked employees, and when any of those figures last changed.
SELECT
  p.company_code,
  p.workforce_size,
  p.description,
  (p.logo_png IS NOT NULL)::boolean AS has_logo,
  EXISTS(SELECT 1 FROM donor_initiatives i WHERE EXISTS(SELECT 1 FROM donor_initiative_partners ip WHERE ip.initiative_id=i.id AND ip.partner_id=p.id) AND i.publish_state='PUBLISHED') AS committed_verified,
  EXISTS(SELECT 1 FROM donor_initiative_spend s JOIN donor_initiatives i ON i.id=s.initiative_id
    WHERE EXISTS(SELECT 1 FROM donor_initiative_partners ip WHERE ip.initiative_id=i.id AND ip.partner_id=p.id) AND i.publish_state='PUBLISHED') AS deployed_verified,
  EXISTS(SELECT 1 FROM donor_initiative_metrics m JOIN donor_initiatives i ON i.id=m.initiative_id
    WHERE EXISTS(SELECT 1 FROM donor_initiative_partners ip WHERE ip.initiative_id=i.id AND ip.partner_id=p.id) AND i.publish_state='PUBLISHED' AND m.publish_state='PUBLISHED' AND m.unit='PEOPLE') AS people_reached_verified,
  coalesce((SELECT sum(ip.committed_minor) FROM donor_initiatives i JOIN donor_initiative_partners ip ON ip.initiative_id=i.id AND ip.partner_id=p.id
            WHERE EXISTS(SELECT 1 FROM donor_initiative_partners ip WHERE ip.initiative_id=i.id AND ip.partner_id=p.id) AND i.publish_state='PUBLISHED'), 0)::bigint AS committed_minor,
  coalesce((SELECT sum(s.amount_minor) FROM donor_initiative_spend s
            JOIN donor_initiatives i ON i.id=s.initiative_id
            WHERE EXISTS(SELECT 1 FROM donor_initiative_partners ip WHERE ip.initiative_id=i.id AND ip.partner_id=p.id) AND i.publish_state='PUBLISHED'), 0)::bigint AS deployed_minor,
  coalesce((SELECT sum(m.value) FROM donor_initiative_metrics m
            JOIN donor_initiatives i ON i.id=m.initiative_id
            WHERE EXISTS(SELECT 1 FROM donor_initiative_partners ip WHERE ip.initiative_id=i.id AND ip.partner_id=p.id) AND i.publish_state='PUBLISHED'
              AND m.publish_state='PUBLISHED' AND m.unit='PEOPLE'), 0)::bigint AS people_reached,
  (SELECT count(*) FROM donor_partner_employees e WHERE e.partner_id=p.id)::bigint AS employees_enrolled,
  (SELECT greatest(max(i.updated_at), max(m.updated_at), max(s.updated_at))
   FROM donor_initiatives i
   LEFT JOIN donor_initiative_metrics m ON m.initiative_id=i.id AND m.publish_state='PUBLISHED'
   LEFT JOIN donor_initiative_spend s ON s.initiative_id=i.id
   WHERE EXISTS(SELECT 1 FROM donor_initiative_partners ip WHERE ip.initiative_id=i.id AND ip.partner_id=p.id) AND i.publish_state='PUBLISHED')::timestamptz AS updated_at
FROM donor_partner_signups p
WHERE p.id=$1;

-- name: InsertPartnerReport :exec
-- P-14: record one dashboard PDF export for the export-history list.
INSERT INTO donor_partner_reports
  (id, partner_id, exported_by, committed_minor, deployed_minor, people_reached, created_at, pdf, snapshot)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9);

-- name: PartnerReports :many
-- P-14: previous exports for a partner, newest first, with the exporter name.
SELECT r.id, r.committed_minor, r.deployed_minor, r.people_reached, r.created_at, u.name AS exported_by_name,
  (r.pdf IS NOT NULL)::boolean AS downloadable,
  coalesce((r.snapshot->'overview'->>'people_reached_verified')::boolean,false)::boolean AS people_reached_verified
FROM donor_partner_reports r
JOIN donor_partner_users u ON u.id = r.exported_by
WHERE r.partner_id=$1
ORDER BY r.created_at DESC
LIMIT 50;

-- name: PartnerReportArtifact :one
SELECT pdf FROM donor_partner_reports WHERE id=$1 AND partner_id=$2 AND pdf IS NOT NULL;

-- name: PartnerLogo :one
SELECT logo_png FROM donor_partner_signups WHERE id=$1;

-- name: SetPartnerLogo :exec
UPDATE donor_partner_signups SET logo_png=$2 WHERE id=$1 AND approved_at IS NOT NULL;

-- name: PartnerInitiativesList :many
-- P-11: the partner's published initiatives with committed and deployed totals
-- and when any of their figures last changed.
SELECT i.id, i.title, i.summary, i.status, i.region, cardinality(i.counties)::int AS county_count, (SELECT ip.committed_minor FROM donor_initiative_partners ip WHERE ip.initiative_id=i.id AND ip.partner_id=$1)::bigint AS committed_minor,
  coalesce((SELECT sum(s.amount_minor) FROM donor_initiative_spend s WHERE s.initiative_id=i.id), 0)::bigint AS deployed_minor,
  greatest(i.updated_at,
    (SELECT max(m.updated_at) FROM donor_initiative_metrics m WHERE m.initiative_id=i.id AND m.publish_state='PUBLISHED'),
    (SELECT max(s.updated_at) FROM donor_initiative_spend s WHERE s.initiative_id=i.id))::timestamptz AS updated_at
FROM donor_initiatives i
WHERE EXISTS(SELECT 1 FROM donor_initiative_partners ip WHERE ip.initiative_id=i.id AND ip.partner_id=$1) AND i.publish_state='PUBLISHED'
ORDER BY i.status, i.title;

-- name: PartnerInitiativeMetrics :many
-- P-11 detail: every published headline metric of the partner's initiatives.
SELECT m.initiative_id, m.label, m.value, m.unit, m.verified_on
FROM donor_initiative_metrics m
JOIN donor_initiatives i ON i.id = m.initiative_id
WHERE EXISTS(SELECT 1 FROM donor_initiative_partners ip WHERE ip.initiative_id=i.id AND ip.partner_id=$1) AND i.publish_state='PUBLISHED' AND m.publish_state='PUBLISHED'
ORDER BY m.initiative_id, m.position;

-- name: PartnerInitiativeSpend :many
-- P-11/P-12: spend by category for each of the partner's initiatives.
SELECT s.initiative_id, s.category, s.amount_minor
FROM donor_initiative_spend s
JOIN donor_initiatives i ON i.id = s.initiative_id
WHERE EXISTS(SELECT 1 FROM donor_initiative_partners ip WHERE ip.initiative_id=i.id AND ip.partner_id=$1) AND i.publish_state='PUBLISHED'
ORDER BY s.initiative_id, s.category;

-- name: QueuePartnerMessage :exec
-- Queues one org-level partner email intent, deduped by key so the same event
-- enqueues at most once. Delivery fans out to the org's users (PartnerMessageRecipients).
INSERT INTO donor_partner_messages (id,partner_id,kind,dedup_key,context,available_at)
VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (dedup_key) DO NOTHING;

-- name: NextPartnerMessage :one
SELECT * FROM donor_partner_messages WHERE completed_at IS NULL AND attempts<5 AND available_at<=$1
ORDER BY available_at,id LIMIT 1 FOR UPDATE SKIP LOCKED;

-- name: RenderPartnerMessage :exec
UPDATE donor_partner_messages SET subject=$2,body=$3 WHERE id=$1;

-- name: ClaimPartnerMessage :exec
UPDATE donor_partner_messages SET available_at=$2 WHERE id=$1;

-- name: CompletePartnerMessage :exec
UPDATE donor_partner_messages SET completed_at=$2,suppressed_reason=$3 WHERE id=$1;

-- name: FailPartnerMessage :exec
UPDATE donor_partner_messages SET attempts=attempts+1,last_error=$2,available_at=$3 WHERE id=$1;

-- name: PartnerMessageRecipients :many
-- The org's users who can receive mail: accepted (completed first sign-in) and
-- not disabled. A pending invitee has their own invite email and is not yet a
-- recipient of status notices.
SELECT email,name FROM donor_partner_users
WHERE partner_id=$1 AND disabled=false AND accepted_at IS NOT NULL
ORDER BY email;

-- name: PartnerForMessage :one
SELECT id,organization,approved_at FROM donor_partner_signups WHERE id=$1;

-- name: PartnersDueMonthlyReport :many
-- Approved partners with at least one published initiative that have not had a
-- MONTHLY_REPORT queued for the given year-month. Drives the monthly sweep;
-- the dedup_key carries the period so each partner is mailed once per month.
SELECT s.id FROM donor_partner_signups s
WHERE s.approved_at IS NOT NULL
AND EXISTS(SELECT 1 FROM donor_initiatives i WHERE EXISTS(SELECT 1 FROM donor_initiative_partners ip WHERE ip.initiative_id=i.id AND ip.partner_id=s.id) AND i.publish_state='PUBLISHED')
AND NOT EXISTS(
    SELECT 1 FROM donor_partner_messages m
    WHERE m.partner_id=s.id AND m.kind='MONTHLY_REPORT'
    AND m.dedup_key='partner-monthly:'||s.id::text||':'||sqlc.arg(period)::text
)
ORDER BY s.id
LIMIT sqlc.arg(batch)::int;

-- name: QueuePartnerLoginEmail :exec
-- An approval or invitation always queues; a requested sign-in link is limited
-- to one a minute and five an hour per user, so requests from many addresses
-- cannot flood an inbox.
INSERT INTO donor_partner_login_emails (id, partner_user_id, kind, available_at, created_at)
SELECT sqlc.arg(id)::uuid, sqlc.arg(partner_user_id)::uuid, sqlc.arg(kind)::text,
  sqlc.arg(available_at)::timestamptz, sqlc.arg(available_at)::timestamptz
WHERE sqlc.arg(kind)::text <> 'SIGN_IN' OR (
  NOT EXISTS (SELECT 1 FROM donor_partner_login_emails
              WHERE partner_user_id=sqlc.arg(partner_user_id)::uuid AND kind='SIGN_IN'
                AND created_at > sqlc.arg(available_at)::timestamptz - interval '1 minute')
  AND (SELECT count(*) FROM donor_partner_login_emails
       WHERE partner_user_id=sqlc.arg(partner_user_id)::uuid AND kind='SIGN_IN'
         AND created_at > sqlc.arg(available_at)::timestamptz - interval '1 hour') < 5
);

-- name: PartnerUsersForSignIn :many
-- The enabled users of approved partners who sign in with this email.
SELECT u.id FROM donor_partner_users u
JOIN donor_partner_signups p ON p.id = u.partner_id
WHERE u.email=$1 AND NOT u.disabled AND p.approved_at IS NOT NULL
ORDER BY u.id;

-- name: NextPartnerLoginEmail :one
SELECT * FROM donor_partner_login_emails WHERE completed_at IS NULL AND attempts<5 AND available_at<=$1
ORDER BY available_at,id LIMIT 1 FOR UPDATE SKIP LOCKED;

-- name: LockPartnerLoginRecipient :one
SELECT u.id, u.email, u.name, u.role, u.disabled, u.invite_token_hash, p.organization, p.approved_at
FROM donor_partner_users u
JOIN donor_partner_signups p ON p.id = u.partner_id
WHERE u.id=$1
FOR UPDATE OF u;

-- name: SetPartnerUserLink :exec
UPDATE donor_partner_users SET invite_token_hash=$2, invite_expires_at=$3 WHERE id=$1;

-- name: PreparePartnerLoginEmail :exec
UPDATE donor_partner_login_emails SET encrypted_token=$2, expires_at=$3 WHERE id=$1;

-- name: ClaimPartnerLoginEmail :exec
UPDATE donor_partner_login_emails SET attempts=attempts+1, available_at=$2 WHERE id=$1;

-- name: CompletePartnerLoginEmail :exec
UPDATE donor_partner_login_emails SET completed_at=$2, encrypted_token=NULL, last_error=NULL WHERE id=$1;

-- name: FailPartnerLoginEmail :exec
UPDATE donor_partner_login_emails SET last_error=$2, available_at=$3 WHERE id=$1 AND completed_at IS NULL;

-- name: PartnerCompanyCode :one
SELECT company_code FROM donor_partner_signups WHERE id=$1;

-- name: AttachApplicationReceipt :execrows
UPDATE donor_partner_applications SET receipt_hash=$2,receipt_expires_at=$3
WHERE id=$1 AND (receipt_hash IS NULL OR receipt_hash=$2);

-- name: ApplicationStatusByReceipt :one
SELECT organization,status FROM donor_partner_applications
WHERE receipt_hash=$1 AND receipt_expires_at>clock_timestamp();

-- name: InitiativePartners :many
SELECT partner_id,committed_minor FROM donor_initiative_partners WHERE initiative_id=$1 ORDER BY partner_id;
