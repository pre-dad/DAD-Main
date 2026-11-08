-- Staff CSV exports run in the projections worker instead of inside the HTTP
-- request. A job walks its dataset in bounded keyset steps; each step appends
-- one gzip member to donor_export_chunks and advances the cursor in the same
-- transaction, so a crashed step leaves nothing behind and the next one resumes.
-- A finished file is kept until expires_at, then purged with its chunks.
CREATE TABLE donor_export_jobs (
 id UUID PRIMARY KEY,
 actor_id UUID NOT NULL REFERENCES donor_staff (id),
 dataset VARCHAR(40) NOT NULL,
 state VARCHAR(10) NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'running', 'complete', 'failed')),
 failure VARCHAR(20) CHECK (failure IN ('too_large', 'forbidden', 'error')),
 requested_at TIMESTAMPTZ NOT NULL,
 expires_at TIMESTAMPTZ NOT NULL,
 completed_at TIMESTAMPTZ,
 cursor UUID,
 row_count BIGINT NOT NULL DEFAULT 0 CHECK (row_count >= 0),
 chunk_count INTEGER NOT NULL DEFAULT 0 CHECK (chunk_count >= 0),
 stored_bytes BIGINT NOT NULL DEFAULT 0 CHECK (stored_bytes >= 0),
 attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
 CHECK ((state = 'failed') = (failure IS NOT NULL)),
 CHECK ((state IN ('complete', 'failed')) = (completed_at IS NOT NULL))
);

CREATE INDEX ix_donor_export_jobs_open ON donor_export_jobs (requested_at, id) WHERE state IN ('queued', 'running');
CREATE INDEX ix_donor_export_jobs_actor ON donor_export_jobs (actor_id, requested_at DESC);
CREATE INDEX ix_donor_export_jobs_expires_at ON donor_export_jobs (expires_at, id);

CREATE TABLE donor_export_chunks (
 job_id UUID NOT NULL REFERENCES donor_export_jobs (id) ON DELETE CASCADE,
 seq INTEGER NOT NULL CHECK (seq >= 0),
 data BYTEA NOT NULL,
 PRIMARY KEY (job_id, seq)
);
