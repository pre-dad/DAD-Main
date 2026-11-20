package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

const (
	notNullViolation    = "23502"
	foreignKeyViolation = "23503"
	checkViolation      = "23514"
)

func TestSaveProfileRejectsPhoneHeldByAnotherDonor(t *testing.T) {
	s, token, _ := donorServiceFixture(t)
	pool := database(t)
	ctx := context.Background()
	const phone = "+14155550177"
	other, _ := donor(t, pool)
	mustExec(t, pool, "UPDATE donors SET phone=$2 WHERE id=$1", other, phone)
	t.Cleanup(func() { cleanExec(t, "UPDATE donors SET phone=NULL WHERE id=$1", other) })

	if _, err := s.SaveProfile(ctx, token, ProfileUpdate{Phone: phone}); !errors.Is(err, ErrPhone) {
		t.Fatalf("a number held by another donor was saved: %v", err)
	}
	mustExec(t, pool, "UPDATE donors SET phone=NULL WHERE id=$1", other)
	if _, err := s.SaveProfile(ctx, token, ProfileUpdate{Phone: phone}); err != nil {
		t.Fatalf("a released number was refused: %v", err)
	}
}

func TestPrimaryPartnerCommitmentFollowsInitiative(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	partner := approvedPartner(t, pool, "Commit Partner", "COMMIT-"+mustNewID(t).String()[:8], 0, 0)
	initiative := testID(t)
	mustExec(
		t,
		pool,
		`INSERT INTO donor_initiatives (id, slug, title, summary, type, location, region, started_on, committed_minor, partner_signup_id)
		VALUES ($1,$2,'Commit','Summary','WATER','North','North','2026-01-01',100,$3)`,
		initiative,
		"commit-"+initiative.String(),
		partner,
	)
	t.Cleanup(func() { cleanExec(t, "DELETE FROM donor_initiatives WHERE id=$1", initiative) })

	committed := func() int64 {
		var minor int64
		if err := pool.QueryRow(ctx, "SELECT committed_minor FROM donor_initiative_partners WHERE initiative_id=$1 AND partner_id=$2", initiative, partner).Scan(&minor); err != nil {
			t.Fatal(err)
		}
		return minor
	}
	if got := committed(); got != 100 {
		t.Fatalf("junction commitment %d after insert; want 100", got)
	}
	mustExec(t, pool, "UPDATE donor_initiatives SET committed_minor=250 WHERE id=$1", initiative)
	if got := committed(); got != 250 {
		t.Fatalf("junction commitment %d after edit; want 250", got)
	}
}

func TestOnlyOneEmailChangeStaysOpen(t *testing.T) {
	pool := database(t)
	id, email := donor(t, pool)
	insert := func(newEmail, hash string) error {
		changeID := testID(t)
		_, err := pool.Exec(
			context.Background(),
			`INSERT INTO donor_email_changes (id, donor_id, old_email, new_email, token_hash, encrypted_token, expires_at, stage)
			VALUES ($1,$2,$3,$4,$5,'\x00',now()+interval '1 hour','CURRENT')`,
			changeID,
			id,
			email,
			newEmail,
			hash,
		)
		return err
	}
	t.Cleanup(func() { cleanExec(t, "DELETE FROM donor_email_changes WHERE donor_id=$1", id) })
	if err := insert("a-"+email, HashToken("a"+email)); err != nil {
		t.Fatal(err)
	}
	err := insert("b-"+email, HashToken("b"+email))
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); !ok || pgErr.Code != uniqueViolation {
		t.Fatalf("second open email change: %v; want unique violation", err)
	}
}

func TestPartnerApplicationDecisionNamesItsDecider(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	staff, _ := staffFixture(t, pool, "SUPER_ADMIN")
	insert := func(status string, decidedBy any, approvedPartner any) error {
		id := testID(t)
		_, err := pool.Exec(ctx,
			`INSERT INTO donor_partner_applications(id,organization,contact_name,work_email,status,decided_by,decided_at,approved_partner_id)
			VALUES ($1,'Org','Dana',$2,$3::varchar,$4,CASE WHEN $3::varchar='PENDING' THEN NULL ELSE now() END,$5)`,
			id, id.String()+"@example.org", status, decidedBy, approvedPartner)
		t.Cleanup(func() { cleanExec(t, "DELETE FROM donor_partner_applications WHERE id=$1", id) })
		return err
	}
	for name, err := range map[string]error{
		"decided without a decider": insert("REJECTED", nil, nil),
		"pending with a decider":    insert("PENDING", staff, nil),
		"rejected with a partner":   insert("REJECTED", staff, testID(t)),
	} {
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); !ok || pgErr.Code != checkViolation {
			t.Errorf("%s: %v; want a check violation", name, err)
		}
	}
	if err := insert("REJECTED", staff, nil); err != nil {
		t.Fatalf("a rejection naming its decider was refused: %v", err)
	}
}

func TestPartnerReportFiguresCannotBeNegative(t *testing.T) {
	pool := database(t)
	partner := approvedPartner(t, pool, "Report Partner", "RPT-"+mustNewID(t).String()[:8], 0, 0)
	user := testID(t)
	mustExec(t, pool, `INSERT INTO donor_partner_users (id, partner_id, email, name, role, accepted_at, created_at)
		VALUES ($1,$2,$3,'Admin','ADMIN',now(),now())`, user, partner, "report-"+user.String()+"@x.test")
	t.Cleanup(func() {
		cleanExec(t, "DELETE FROM donor_partner_reports WHERE partner_id=$1", partner)
		cleanExec(t, "DELETE FROM donor_partner_users WHERE id=$1", user)
	})
	insert := func(committed, deployed, reached int64) error {
		_, err := pool.Exec(context.Background(),
			`INSERT INTO donor_partner_reports(id,partner_id,exported_by,committed_minor,deployed_minor,people_reached)
			VALUES ($1,$2,$3,$4,$5,$6)`, testID(t), partner, user, committed, deployed, reached)
		return err
	}
	for _, figures := range [][3]int64{{-1, 0, 0}, {0, -1, 0}, {0, 0, -1}} {
		if err := insert(figures[0], figures[1], figures[2]); err == nil {
			t.Errorf("report figures %v were accepted", figures)
		}
	}
	if err := insert(0, 0, 0); err != nil {
		t.Fatalf("zero figures were refused: %v", err)
	}
}

func TestEmployeeTeamMustBelongToTheEmployeesPartner(t *testing.T) {
	pool := database(t)
	home := approvedPartner(t, pool, "Home Co", "HOME-"+mustNewID(t).String()[:8], 0, 0)
	other := approvedPartner(t, pool, "Other Co", "OTHER-"+mustNewID(t).String()[:8], 0, 0)
	team := testID(t)
	mustExec(t, pool, "INSERT INTO donor_partner_teams(id,partner_id,name) VALUES($1,$2,'Home team')", team, home)
	member, _ := donor(t, pool)
	t.Cleanup(func() { cleanExec(t, "DELETE FROM donor_partner_employees WHERE donor_id=$1", member) })

	_, err := pool.Exec(context.Background(),
		"INSERT INTO donor_partner_employees(donor_id,partner_id,team_id) VALUES($1,$2,$3)", member, other, team)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); !ok || pgErr.Code != foreignKeyViolation {
		t.Fatalf("a team of another partner was accepted: %v", err)
	}
	mustExec(t, pool, "INSERT INTO donor_partner_employees(donor_id,partner_id,team_id) VALUES($1,$2,$3)", member, home, team)
}

func TestApprovingAPartnerCofundsItsLinkedInitiatives(t *testing.T) {
	pool := database(t)
	partner := testID(t)
	mustExec(t, pool,
		`INSERT INTO donor_partner_signups (id, organization, name, email, website, message, request_hash, consent_version)
		VALUES ($1,'Pending Co','C','c@x.test','https://x.test','',$2,'v1')`, partner, "h-"+partner.String())
	initiative := testID(t)
	mustExec(t, pool,
		`INSERT INTO donor_initiatives (id, slug, title, summary, type, location, region, started_on, committed_minor, partner_signup_id)
		VALUES ($1,$2,'Linked','Summary','WATER','North','North','2026-01-01',700,$3)`,
		initiative, "linked-"+initiative.String(), partner)
	t.Cleanup(func() {
		cleanExec(t, "DELETE FROM donor_initiative_partners WHERE initiative_id=$1", initiative)
		cleanExec(t, "DELETE FROM donor_initiatives WHERE id=$1", initiative)
		cleanExec(t, "DELETE FROM donor_partner_signups WHERE id=$1", partner)
	})
	count := func() (n int) {
		t.Helper()
		if err := pool.QueryRow(context.Background(),
			"SELECT count(*) FROM donor_initiative_partners WHERE initiative_id=$1", initiative).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(); n != 0 {
		t.Fatalf("an unapproved partner co-funds %d initiatives", n)
	}
	mustExec(t, pool, "UPDATE donor_partner_signups SET approved_at=now() WHERE id=$1", partner)
	if n := count(); n != 1 {
		t.Fatalf("approval created %d co-funder rows; want 1", n)
	}
	var minor int64
	if err := pool.QueryRow(context.Background(),
		"SELECT committed_minor FROM donor_initiative_partners WHERE initiative_id=$1 AND partner_id=$2", initiative, partner).Scan(&minor); err != nil || minor != 700 {
		t.Fatalf("co-funder commitment %d (%v); want 700", minor, err)
	}
}

func TestEventInsertMustNameItsTimeZone(t *testing.T) {
	pool := database(t)
	id := testID(t)
	_, err := pool.Exec(context.Background(),
		`INSERT INTO donor_events (id, slug, title, type, description, venue, address, city, zip, starts_at, ends_at, capacity, created_at, updated_at)
		VALUES ($1,$2,'T','VOLUNTEER','D','V','A','C','94103',now()+interval '1 day',now()+interval '2 days',5,now(),now())`,
		id, "zone-"+id.String())
	t.Cleanup(func() { cleanExec(t, "DELETE FROM donor_events WHERE id=$1", id) })
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); !ok || pgErr.Code != notNullViolation || pgErr.ColumnName != "time_zone" {
		t.Fatalf("an event without a zone was accepted: %v", err)
	}
}
