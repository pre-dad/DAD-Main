-- name: InsertExportJob :one
INSERT INTO donor_export_jobs(id,actor_id,dataset,requested_at,expires_at)
VALUES(sqlc.arg(id),sqlc.arg(actor_id),sqlc.arg(dataset),sqlc.arg(requested_at),sqlc.arg(expires_at))
RETURNING *;

-- name: OpenExportJob :one
-- The actor's job for the dataset that is still being built, so a second click
-- follows it rather than queueing a duplicate.
SELECT * FROM donor_export_jobs
WHERE actor_id=sqlc.arg(actor_id) AND dataset=sqlc.arg(dataset) AND state IN ('queued','running')
ORDER BY requested_at DESC LIMIT 1;

-- name: CountOpenExportJobs :one
SELECT count(*)::bigint FROM donor_export_jobs WHERE actor_id=sqlc.arg(actor_id) AND state IN ('queued','running');

-- name: ExportJobForActor :one
SELECT * FROM donor_export_jobs WHERE id=sqlc.arg(id) AND actor_id=sqlc.arg(actor_id) AND expires_at>sqlc.arg(as_of)::timestamptz;

-- name: ListExportJobs :many
SELECT * FROM donor_export_jobs
WHERE actor_id=sqlc.arg(actor_id) AND expires_at>sqlc.arg(as_of)::timestamptz
ORDER BY requested_at DESC,id LIMIT 20;

-- name: ClaimExportJob :one
-- The oldest unfinished job no other worker holds.
SELECT * FROM donor_export_jobs
WHERE state IN ('queued','running') AND expires_at>sqlc.arg(as_of)::timestamptz
ORDER BY requested_at,id LIMIT 1 FOR UPDATE SKIP LOCKED;

-- name: ExportJobStaff :one
SELECT role,disabled FROM donor_staff WHERE id=sqlc.arg(id);

-- name: AdvanceExportJob :exec
UPDATE donor_export_jobs SET state='running',cursor=sqlc.arg(cursor),row_count=row_count+sqlc.arg(rows)::bigint,
 chunk_count=chunk_count+1,stored_bytes=stored_bytes+sqlc.arg(bytes)::bigint,attempts=0
WHERE id=sqlc.arg(id);

-- name: CompleteExportJob :exec
UPDATE donor_export_jobs SET state='complete',cursor=sqlc.arg(cursor),row_count=row_count+sqlc.arg(rows)::bigint,
 chunk_count=chunk_count+1,stored_bytes=stored_bytes+sqlc.arg(bytes)::bigint,attempts=0,
 completed_at=sqlc.arg(completed_at),expires_at=sqlc.arg(expires_at)
WHERE id=sqlc.arg(id);

-- name: FailExportJob :exec
UPDATE donor_export_jobs SET state='failed',failure=sqlc.arg(failure),completed_at=sqlc.arg(completed_at),expires_at=sqlc.arg(expires_at)
WHERE id=sqlc.arg(id);

-- name: RecordExportJobFailure :one
-- Counts a step that failed, and fails the job once it has failed too often
-- for a retry to help.
UPDATE donor_export_jobs SET attempts=attempts+1,
 state=CASE WHEN attempts+1>=sqlc.arg(limit_attempts)::integer THEN 'failed' ELSE state END,
 failure=CASE WHEN attempts+1>=sqlc.arg(limit_attempts)::integer THEN 'error' ELSE failure END,
 completed_at=CASE WHEN attempts+1>=sqlc.arg(limit_attempts)::integer THEN sqlc.arg(as_of)::timestamptz ELSE completed_at END
WHERE id=sqlc.arg(id) AND state IN ('queued','running')
RETURNING *;

-- name: InsertExportChunk :exec
INSERT INTO donor_export_chunks(job_id,seq,data) VALUES(sqlc.arg(job_id),sqlc.arg(seq),sqlc.arg(data));

-- name: ExportChunk :one
SELECT data FROM donor_export_chunks WHERE job_id=sqlc.arg(job_id) AND seq=sqlc.arg(seq);

-- name: DeleteExportChunks :exec
DELETE FROM donor_export_chunks WHERE job_id=sqlc.arg(job_id);

-- name: PurgeExpiredExports :execrows
-- One bounded batch of jobs, with their chunks, whose retention has ended.
DELETE FROM donor_export_jobs WHERE id=ANY(ARRAY(
 SELECT id FROM donor_export_jobs WHERE expires_at<sqlc.arg(expired_before)::timestamptz
 ORDER BY expires_at,id LIMIT sqlc.arg(batch)
));
