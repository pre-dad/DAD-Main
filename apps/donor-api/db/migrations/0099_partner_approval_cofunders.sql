-- An initiative linked to a partner before the partner was approved gets its
-- co-funder row at the moment of approval, as it would on a later edit.
CREATE FUNCTION donor_sync_partner_approval() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 INSERT INTO donor_initiative_partners(initiative_id,partner_id,committed_minor,updated_at)
 SELECT i.id,NEW.id,i.committed_minor,i.updated_at FROM donor_initiatives i WHERE i.partner_signup_id=NEW.id
 ON CONFLICT(initiative_id,partner_id) DO NOTHING;
 RETURN NEW;
END $$;

CREATE TRIGGER donor_partner_approval AFTER UPDATE OF approved_at ON donor_partner_signups
FOR EACH ROW WHEN (OLD.approved_at IS NULL AND NEW.approved_at IS NOT NULL)
EXECUTE FUNCTION donor_sync_partner_approval();
