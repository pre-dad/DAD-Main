-- Partner status mail is delivered per recipient. A partner message stays one
-- org-level intent (deduped, rendered once); a short fan-out step turns it into
-- one delivery row per accepted user, and each delivery is claimed, leased,
-- retried and parked on its own. A large organisation can no longer outlast one
-- lease, and a failure for one recipient never resends to the others.
--
-- fanned_out_at is NULL until an intent has been expanded. Intents already
-- queued under the old shape have it NULL, so the worker expands them the same
-- way; the unique (message_id, partner_user_id) makes that idempotent.
ALTER TABLE donor_partner_messages ADD COLUMN fanned_out_at timestamptz;

CREATE TABLE donor_partner_message_deliveries (
    id uuid PRIMARY KEY,
    message_id uuid NOT NULL REFERENCES donor_partner_messages (id) ON DELETE CASCADE,
    partner_user_id uuid NOT NULL REFERENCES donor_partner_users (id) ON DELETE CASCADE,
    ordinal integer NOT NULL,
    available_at timestamptz NOT NULL,
    completed_at timestamptz,
    attempts integer NOT NULL DEFAULT 0,
    last_error varchar(80),
    suppressed_reason varchar(80),
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT uq_donor_partner_message_deliveries_recipient UNIQUE (message_id, partner_user_id)
);
CREATE INDEX ix_donor_partner_message_deliveries_pending
    ON donor_partner_message_deliveries (available_at, id)
    WHERE completed_at IS NULL AND attempts < 5;
CREATE INDEX ix_donor_partner_message_deliveries_user
    ON donor_partner_message_deliveries (partner_user_id);

-- Intents are claimed for fan-out only while unexpanded.
DROP INDEX ix_donor_partner_messages_pending;
CREATE INDEX ix_donor_partner_messages_pending ON donor_partner_messages (available_at, id)
    WHERE completed_at IS NULL AND fanned_out_at IS NULL AND attempts < 5;
