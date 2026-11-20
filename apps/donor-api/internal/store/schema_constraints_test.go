package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
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
