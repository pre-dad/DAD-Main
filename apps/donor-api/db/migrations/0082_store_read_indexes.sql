-- Staff reads that sorted or scanned a whole table on every page.
--
-- The audit log pages newest first, and every donor view, search and export
-- writes to it, so it is the fastest-growing table the back office reads.
CREATE INDEX ix_donor_staff_audit_created ON donor_staff_audit (created_at DESC, id);

-- The account-deletions queue pages newest first by (deleted_at, donor_id).
CREATE INDEX ix_donor_deletions_deleted ON donor_deletions (deleted_at DESC, donor_id DESC);

-- Donor search matches any part of an address (ILIKE '%term%'), which a btree
-- cannot serve. Trigrams let it use an index instead of scanning every donor.
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE INDEX ix_donors_email_trgm ON donors USING gin (email gin_trgm_ops);
