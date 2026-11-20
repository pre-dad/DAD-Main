-- The staff form still edits donor_initiatives.committed_minor and
-- partner_signup_id while every reader uses donor_initiative_partners, so an
-- edited commitment must reach the primary partner's junction row.
CREATE OR REPLACE FUNCTION donor_sync_primary_partner() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.partner_signup_id IS NOT NULL AND EXISTS(SELECT 1 FROM donor_partner_signups WHERE id=NEW.partner_signup_id AND approved_at IS NOT NULL) THEN
  INSERT INTO donor_initiative_partners(initiative_id,partner_id,committed_minor,updated_at)
  VALUES(NEW.id,NEW.partner_signup_id,NEW.committed_minor,NEW.updated_at)
  ON CONFLICT(initiative_id,partner_id) DO UPDATE
  SET committed_minor=EXCLUDED.committed_minor,updated_at=EXCLUDED.updated_at;
 END IF;
 RETURN NEW;
END $$;

DROP TRIGGER donor_primary_partner ON donor_initiatives;
CREATE TRIGGER donor_primary_partner_insert AFTER INSERT ON donor_initiatives
FOR EACH ROW EXECUTE FUNCTION donor_sync_primary_partner();
CREATE TRIGGER donor_primary_partner_update AFTER UPDATE OF partner_signup_id, committed_minor ON donor_initiatives
FOR EACH ROW WHEN (OLD.partner_signup_id IS DISTINCT FROM NEW.partner_signup_id OR OLD.committed_minor IS DISTINCT FROM NEW.committed_minor)
EXECUTE FUNCTION donor_sync_primary_partner();
