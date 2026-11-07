-- Lookups on the sign-in, staff and monitoring paths that read their whole
-- table, found by EXPLAIN against 500,000 donors, and the indexes credential
-- retention needs to delete in bounded batches.

-- Phone sign-in finds the donor by number.
CREATE INDEX ix_donors_phone ON donors (phone) WHERE phone IS NOT NULL;

-- Every sign-up and sign-in checks whether a partner login holds the address.
CREATE INDEX ix_donor_partner_users_email ON donor_partner_users (email);

-- The audit log pages newest first.
CREATE INDEX ix_donor_staff_audit_created ON donor_staff_audit (created_at DESC, id);

-- The involuntary-churn report looks up the gift that paid each failed period.
CREATE INDEX ix_donor_gifts_period ON donor_gifts (period_id) WHERE period_id IS NOT NULL;

-- The queue monitor counts every unfinished item, parked ones included, which
-- the delivery indexes (attempts < 5) cannot serve, so it read whole tables.
CREATE INDEX ix_donor_messages_open ON donor_messages (available_at) WHERE completed_at IS NULL;
CREATE INDEX ix_donor_login_emails_open ON donor_login_emails (available_at) WHERE completed_at IS NULL;
CREATE INDEX ix_donor_push_notifications_open ON donor_push_notifications (available_at) WHERE completed_at IS NULL;
CREATE INDEX ix_donor_partner_push_open ON donor_partner_push_notifications (available_at) WHERE completed_at IS NULL;
CREATE INDEX ix_donor_subscriptions_unsynced ON donor_subscriptions (period_end)
	WHERE status NOT IN ('canceled', 'incomplete_expired') AND observed_at < period_end;

-- Credential retention: sessions are deleted once their last credential has
-- lapsed, and the audit rows that named them are kept without the session.
CREATE INDEX ix_donor_sessions_lapsed ON donor_sessions ((coalesce(refresh_expires_at, expires_at)), id);
CREATE INDEX ix_donor_session_events_session ON donor_session_events (session_id);
ALTER TABLE donor_session_events
	ALTER COLUMN session_id DROP NOT NULL,
	DROP CONSTRAINT donor_session_events_session_id_fkey,
	ADD FOREIGN KEY (session_id) REFERENCES donor_sessions (id) ON DELETE SET NULL;
CREATE INDEX ix_donor_login_links_expires ON donor_login_links (expires_at, id);
CREATE INDEX ix_donor_login_emails_completed ON donor_login_emails (completed_at, id) WHERE completed_at IS NOT NULL;
