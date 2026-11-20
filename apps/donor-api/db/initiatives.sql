-- name: InsertInitiative :exec
INSERT INTO donor_initiatives
  (id, slug, title, summary, type, location, region, counties, started_on, status, committed_minor, partner_signup_id,
   board_approval_source_id, board_approved_on, created_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$15);

-- name: UpdateInitiative :exec
UPDATE donor_initiatives
SET title=$2, summary=$3, type=$4, location=$5, region=$6, counties=$7,
    started_on=$8, status=$9, committed_minor=$10, partner_signup_id=$11,
    board_approval_source_id=$12, board_approved_on=$13, updated_at=$14
WHERE id=$1;

-- name: PublishInitiative :exec
UPDATE donor_initiatives
SET publish_state='PUBLISHED', published_at=$2, updated_at=$2
WHERE id=$1;

-- name: InitiativeByID :one
SELECT * FROM donor_initiatives WHERE id=$1;

-- name: LockInitiative :one
SELECT * FROM donor_initiatives WHERE id=$1 FOR UPDATE;

-- name: StaffInitiativesPage :many
SELECT * FROM donor_initiatives
ORDER BY created_at DESC, id
LIMIT 51 OFFSET sqlc.arg(page_offset)::bigint;

-- name: SetInitiativeRotation :execrows
UPDATE donor_initiatives SET in_rotation=$2, updated_at=$3 WHERE id=$1;

-- name: UpsertInitiativeMetric :exec
INSERT INTO donor_initiative_metrics
  (id, initiative_id, position, label, value, unit, verification_note, verified_on, created_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$9)
ON CONFLICT (initiative_id, position) DO UPDATE
SET publish_state='DRAFT', label=EXCLUDED.label, value=EXCLUDED.value, unit=EXCLUDED.unit,
    verification_note=EXCLUDED.verification_note, verified_on=EXCLUDED.verified_on,
    updated_at=EXCLUDED.updated_at;

-- name: PublishInitiativeMetric :exec
UPDATE donor_initiative_metrics
SET publish_state='PUBLISHED', updated_at=$3
WHERE initiative_id=$1 AND position=$2;

-- name: DeleteInitiativeMetric :exec
DELETE FROM donor_initiative_metrics WHERE initiative_id=$1 AND position=$2;

-- name: InitiativeMetrics :many
SELECT * FROM donor_initiative_metrics
WHERE initiative_id=$1
ORDER BY position;

-- name: PublishedInitiativeMetrics :many
SELECT * FROM donor_initiative_metrics
WHERE initiative_id=$1 AND publish_state='PUBLISHED'
ORDER BY position;

-- name: UpsertInitiativeSpend :exec
INSERT INTO donor_initiative_spend (id, initiative_id, category, amount_minor, created_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$5)
ON CONFLICT (initiative_id, category) DO UPDATE
SET amount_minor=EXCLUDED.amount_minor, updated_at=EXCLUDED.updated_at;

-- name: DeleteInitiativeSpend :exec
DELETE FROM donor_initiative_spend WHERE initiative_id=$1 AND category=$2;

-- name: InitiativeSpend :many
SELECT category, amount_minor FROM donor_initiative_spend
WHERE initiative_id=$1
ORDER BY category;

-- name: PublishedSpendByCategory :many
-- Overall spend: each category's total across every published initiative.
SELECT s.category, SUM(s.amount_minor)::bigint AS amount_minor
FROM donor_initiative_spend s
JOIN donor_initiatives i ON i.id = s.initiative_id
WHERE i.publish_state = 'PUBLISHED'
GROUP BY s.category
ORDER BY s.category;

-- name: RotatableInitiatives :many
-- The published, active set the daily rotation chooses from. Ordered stably by
-- (started_on, id) so the deterministic index the application computes is stable
-- across requests. Served by ix_donor_initiatives_rotatable.
SELECT * FROM donor_initiatives
WHERE publish_state='PUBLISHED' AND status='ACTIVE' AND in_rotation
ORDER BY started_on, id;

-- name: RecordInitiativeExposure :exec
INSERT INTO donor_initiative_exposures (donor_id, initiative_id, first_shown_on, last_shown_on)
VALUES ($1,$2,$3,$3)
ON CONFLICT (donor_id, initiative_id) DO UPDATE SET last_shown_on=EXCLUDED.last_shown_on;

-- name: DistinctInitiativeExposures :one
SELECT count(*)::bigint AS shown FROM donor_initiative_exposures WHERE donor_id=$1;

-- name: PublishedInitiatives :many
SELECT i.id, i.slug, i.title, i.summary, i.type, i.location, i.status, i.started_on, coalesce((SELECT string_agg(p.organization, ', ' ORDER BY p.organization) FROM donor_initiative_partners ip JOIN donor_partner_signups p ON p.id=ip.partner_id WHERE ip.initiative_id=i.id AND p.approved_at IS NOT NULL),'')::text AS partner
FROM donor_initiatives i
WHERE i.publish_state = 'PUBLISHED'
ORDER BY i.status, i.started_on, i.title
-- Hard cap: published initiatives beyond 500 are silently dropped from the list.
LIMIT 500;

-- name: PublishedInitiativeBySlug :one
SELECT i.id, i.slug, i.title, i.summary, i.type, i.location, i.status, i.started_on, coalesce((SELECT string_agg(p.organization, ', ' ORDER BY p.organization) FROM donor_initiative_partners ip JOIN donor_partner_signups p ON p.id=ip.partner_id WHERE ip.initiative_id=i.id AND p.approved_at IS NOT NULL),'')::text AS partner
FROM donor_initiatives i
WHERE i.publish_state = 'PUBLISHED' AND i.slug = $1;

-- name: SaveSpendDraft :exec
INSERT INTO donor_initiative_spend_drafts(initiative_id,category,amount_minor,verification_note,verified_on,updated_at)
VALUES($1,$2,$3,$4,$5,$6)
ON CONFLICT(initiative_id,category) DO UPDATE SET amount_minor=EXCLUDED.amount_minor,
 verification_note=EXCLUDED.verification_note,verified_on=EXCLUDED.verified_on,updated_at=EXCLUDED.updated_at;

-- name: StaffSpend :many
SELECT d.category,d.amount_minor,d.verification_note,d.verified_on,'DRAFT'::text AS publish_state FROM donor_initiative_spend_drafts d WHERE d.initiative_id=$1
UNION ALL
SELECT s.category,s.amount_minor,s.verification_note,s.verified_on,'PUBLISHED'::text FROM donor_initiative_spend s
WHERE s.initiative_id=$1 AND NOT EXISTS(SELECT 1 FROM donor_initiative_spend_drafts d WHERE d.initiative_id=s.initiative_id AND d.category=s.category)
ORDER BY category;

-- name: PublishSpendDraft :execrows
INSERT INTO donor_initiative_spend(id,initiative_id,category,amount_minor,created_at,updated_at,verification_note,verified_on)
SELECT sqlc.arg(id)::uuid,d.initiative_id,d.category,d.amount_minor,sqlc.arg(as_of)::timestamptz,sqlc.arg(as_of)::timestamptz,d.verification_note,d.verified_on
FROM donor_initiative_spend_drafts d WHERE d.initiative_id=sqlc.arg(initiative_id)::uuid AND d.category=sqlc.arg(category)::text
 AND verified_on IS NOT NULL AND verification_note<>''
ON CONFLICT(initiative_id,category) DO UPDATE SET amount_minor=EXCLUDED.amount_minor,updated_at=EXCLUDED.updated_at,verification_note=EXCLUDED.verification_note,verified_on=EXCLUDED.verified_on;

-- name: DeleteSpendDraft :exec
DELETE FROM donor_initiative_spend_drafts WHERE initiative_id=$1 AND category=$2;
