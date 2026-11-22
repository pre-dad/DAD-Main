-- name: InsertInsightSpec :exec
INSERT INTO donor_insight_specs(id,spec,catalog_version,state,author_id,created_at,updated_at)
VALUES(sqlc.arg(id),sqlc.arg(spec),sqlc.arg(catalog_version),'draft',sqlc.arg(author_id),sqlc.arg(at),sqlc.arg(at));

-- name: InsightSpecForUpdate :one
SELECT id,state,author_id FROM donor_insight_specs WHERE id=$1 FOR UPDATE;

-- name: InsightSpecByID :one
SELECT s.id,s.spec,s.catalog_version,s.state,s.author_id,a.name AS author_name,s.approver_id,p.name AS approver_name,
  s.reject_reason,s.created_at,s.updated_at,s.submitted_at,s.decided_at,s.retired_at
FROM donor_insight_specs s
JOIN donor_staff a ON a.id=s.author_id
LEFT JOIN donor_staff p ON p.id=s.approver_id
WHERE s.id=$1;

-- name: InsightSpecsPage :many
-- Newest first, keyset-paged on (created_at DESC, id). The first page passes an
-- infinite cursor_at; an empty state lists every state.
SELECT s.id,s.catalog_version,s.state,s.author_id,a.name AS author_name,s.approver_id,p.name AS approver_name,
  s.reject_reason,s.created_at,s.updated_at,s.submitted_at,s.decided_at,s.retired_at
FROM donor_insight_specs s
JOIN donor_staff a ON a.id=s.author_id
LEFT JOIN donor_staff p ON p.id=s.approver_id
WHERE (sqlc.arg(state)::text='' OR s.state=sqlc.arg(state)::text)
  AND s.created_at <= sqlc.arg(cursor_at)::timestamptz
  AND (s.created_at < sqlc.arg(cursor_at)::timestamptz OR s.id > sqlc.arg(cursor_id)::uuid)
ORDER BY s.created_at DESC,s.id LIMIT 51;

-- name: UpdateInsightDraft :exec
UPDATE donor_insight_specs SET spec=$2,catalog_version=$3,updated_at=$4 WHERE id=$1 AND state='draft';

-- name: SubmitInsightSpec :exec
UPDATE donor_insight_specs SET state='pending',submitted_at=$2,updated_at=$2 WHERE id=$1 AND state='draft';

-- name: RetireLiveInsightSpec :one
-- Returns the id of the spec it retired; no row when none was live.
UPDATE donor_insight_specs SET state='retired',retired_by=sqlc.arg(actor_id),retired_at=sqlc.arg(at),updated_at=sqlc.arg(at)
WHERE state='live' RETURNING id;

-- name: ApproveInsightSpec :exec
UPDATE donor_insight_specs SET state='live',approver_id=sqlc.arg(actor_id),decided_at=sqlc.arg(at),updated_at=sqlc.arg(at)
WHERE id=sqlc.arg(id) AND state='pending';

-- name: RejectInsightSpec :exec
UPDATE donor_insight_specs SET state='rejected',approver_id=sqlc.arg(actor_id),decided_at=sqlc.arg(at),updated_at=sqlc.arg(at),
  reject_reason=sqlc.arg(reason)
WHERE id=sqlc.arg(id) AND state='pending';

-- name: RetireInsightSpec :exec
UPDATE donor_insight_specs SET state='retired',retired_by=sqlc.arg(actor_id),retired_at=sqlc.arg(at),updated_at=sqlc.arg(at)
WHERE id=sqlc.arg(id) AND state='live';

-- name: LiveInsightSpec :one
SELECT id,spec,catalog_version FROM donor_insight_specs WHERE state='live';
