-- A decided partner application names its decider, and only an approved one
-- names the partner it produced. The service has always written both together.
ALTER TABLE donor_partner_applications ADD CONSTRAINT ck_donor_partner_applications_decider
	CHECK ((status = 'PENDING') = (decided_by IS NULL));
ALTER TABLE donor_partner_applications ADD CONSTRAINT ck_donor_partner_applications_approved_partner
	CHECK (approved_partner_id IS NULL OR status = 'APPROVED');

-- The report figures are stored with each export row (0043's header predates
-- that); none of them can be negative.
ALTER TABLE donor_partner_reports ADD CONSTRAINT ck_donor_partner_reports_figures
	CHECK (committed_minor >= 0 AND deployed_minor >= 0 AND people_reached >= 0);

-- Retention purges lapsed partner sessions and spent login emails in bounded
-- batches, oldest first, so each needs an index in that order.
CREATE INDEX ix_donor_partner_sessions_lapsed ON donor_partner_sessions (expires_at, id);
CREATE INDEX ix_donor_partner_login_emails_completed ON donor_partner_login_emails (completed_at, id) WHERE completed_at IS NOT NULL;
