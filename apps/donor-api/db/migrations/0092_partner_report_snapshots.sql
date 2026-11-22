ALTER TABLE donor_partner_reports ADD COLUMN snapshot_digest bytea
  CHECK (snapshot_digest IS NULL OR octet_length(snapshot_digest)=32);
CREATE UNIQUE INDEX donor_partner_reports_snapshot_key
  ON donor_partner_reports(partner_id,snapshot_digest) WHERE snapshot_digest IS NOT NULL;
