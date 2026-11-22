-- A team's partner must be the partner its employee is linked to. The old check
-- compared a team id with a donor id, which can never match, so it constrained
-- nothing; a composite foreign key enforces the rule in the database.
DO $$
BEGIN
 IF EXISTS (
  SELECT 1 FROM donor_partner_employees e JOIN donor_partner_teams t ON t.id = e.team_id
  WHERE t.partner_id <> e.partner_id
 ) THEN
  RAISE EXCEPTION 'donor_partner_employees has a team belonging to another partner';
 END IF;
END $$;

ALTER TABLE donor_partner_employees DROP CONSTRAINT ck_donor_partner_employees_team;
ALTER TABLE donor_partner_teams ADD CONSTRAINT uq_donor_partner_teams_id_partner UNIQUE (id, partner_id);
ALTER TABLE donor_partner_employees DROP CONSTRAINT donor_partner_employees_team_id_fkey;
ALTER TABLE donor_partner_employees ADD CONSTRAINT fk_donor_partner_employees_team
	FOREIGN KEY (team_id, partner_id) REFERENCES donor_partner_teams (id, partner_id);
