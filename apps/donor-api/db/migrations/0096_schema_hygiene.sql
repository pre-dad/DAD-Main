-- Indexes that duplicate the unique constraint or prefix beside them.
DROP INDEX ix_donor_initiative_metrics_initiative;
DROP INDEX ix_donor_initiative_spend_initiative;
DROP INDEX ix_donor_partner_users_partner;

-- A donor's own reservations and guest places, and the cascade on donor delete.
CREATE INDEX ix_donor_event_reservations_donor ON donor_event_reservations (donor_id);
CREATE INDEX ix_donor_event_guests_donor ON donor_event_guests (donor_id);

-- At most one email change per donor is awaiting confirmation; an older
-- duplicate is canceled before the index is built.
UPDATE donor_email_changes e SET stage = 'CANCELED'
FROM (
 SELECT id, row_number() OVER (PARTITION BY donor_id ORDER BY created_at DESC, id DESC) AS rank
 FROM donor_email_changes WHERE stage IN ('CURRENT', 'NEW')
) ranked
WHERE ranked.id = e.id AND ranked.rank > 1;
CREATE UNIQUE INDEX ux_donor_email_changes_open ON donor_email_changes (donor_id) WHERE stage IN ('CURRENT', 'NEW');

-- A match rate is a share of the gift, so it cannot exceed 100%.
UPDATE donor_partner_signups SET match_rate_bp = 10000 WHERE match_rate_bp > 10000;
ALTER TABLE donor_partner_signups ADD CONSTRAINT ck_donor_partner_signups_match_rate_max CHECK (match_rate_bp <= 10000);
