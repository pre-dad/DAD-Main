package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5/pgxpool"
)

func awaitPartnerWaiters(t *testing.T, pool *pgxpool.Pool, count int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		var blocked int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname=current_database() AND cardinality(pg_blocking_pids(pid))>0`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked >= count {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("partner requests did not reach the held lock")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestPartnerInviteIsSingleUseUnderConcurrency(t *testing.T) {
	pool := database(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cleanPartners(t, pool)
	partner := approvedPartner(t, pool, "Concurrent Portal", "CONCURRENT-PORTAL", 0, 0)
	user := mustNewID(t)
	proof := "invite-" + user.String()
	mustExec(t, pool, `INSERT INTO donor_partner_users(id,partner_id,email,name,role,invite_token_hash,invite_expires_at)
		VALUES($1,$2,'single-use@partner.test','Partner','ADMIN',$3,now()+interval '1 hour')`, user, partner, HashToken(proof))
	lock, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(ctx)
	mustExec(t, lock, "SELECT id FROM donor_partner_users WHERE id=$1 FOR UPDATE", user)
	portal := PartnerPortalService{DB: as(t, dbroles.API)}
	const requests = 4
	results := make(chan error, requests)
	for range requests {
		go func() {
			_, err := portal.AcceptInvite(ctx, proof)
			results <- err
		}()
	}
	awaitPartnerWaiters(t, pool, requests)
	if err := lock.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	successes := 0
	for range requests {
		if err := <-results; err == nil {
			successes++
		} else if !errors.Is(err, ErrPartnerInvite) {
			t.Errorf("redemption: %v", err)
		}
	}
	var sessions int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_partner_sessions WHERE partner_user_id=$1", user).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if successes != 1 || sessions != 1 {
		t.Fatalf("one proof created %d successful redemptions and %d sessions", successes, sessions)
	}
}

func TestPartnerApplicationRetryIsIdempotentUnderConcurrency(t *testing.T) {
	pool := database(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cleanPartners(t, pool)
	apps := PartnerApplicationService{DB: as(t, dbroles.API)}
	application := mustNewID(t)
	input := validApplication("Double Submit", "double@partner.test")
	lock, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(ctx)
	mustExec(
		t,
		lock,
		`INSERT INTO donor_partner_applications(id,organization,website,type,contact_name,title,work_email,phone,workforce_size)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		application,
		input.Organization,
		input.Website,
		input.Type,
		input.ContactName,
		input.Title,
		input.WorkEmail,
		input.Phone,
		input.WorkforceSize,
	)
	done := make(chan error, 1)
	go func() { done <- apps.Apply(ctx, application, input) }()
	awaitPartnerWaiters(t, pool, 1)
	if err := lock.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("retry that raced the original submission: %v", err)
	}
	var stored int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_partner_applications WHERE work_email=$1", input.WorkEmail).Scan(&stored); err != nil ||
		stored != 1 {
		t.Fatalf("%d applications stored (%v), want 1", stored, err)
	}
}

func TestPartnerApprovalIsSingleDecisionUnderConcurrency(t *testing.T) {
	pool := database(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, first := staffFixture(t, pool, "SUPER_ADMIN")
	_, second := staffFixture(t, pool, "OP_PARTNERSHIPS")
	cleanPartners(t, pool)
	apps := PartnerApplicationService{DB: as(t, dbroles.API), Staff: &StaffService{DB: as(t, dbroles.API)}}
	application := mustNewID(t)
	if err := apps.Apply(ctx, application, validApplication("Single Decision", "decision@partner.test")); err != nil {
		t.Fatal(err)
	}
	lock, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(ctx)
	mustExec(t, lock, "SELECT id FROM donor_partner_applications WHERE id=$1 FOR UPDATE", application)
	results := make(chan error, 2)
	for _, token := range []string{first, second} {
		go func() {
			_, err := apps.Approve(ctx, token, application)
			results <- err
		}()
	}
	awaitPartnerWaiters(t, pool, 2)
	if err := lock.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	successes := 0
	for range 2 {
		if err := <-results; err == nil {
			successes++
		} else if !errors.Is(err, ErrPartnerDecision) {
			t.Errorf("approval: %v", err)
		}
	}
	var organizations int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_partner_signups WHERE email='decision@partner.test'").Scan(&organizations); err != nil {
		t.Fatal(err)
	}
	if successes != 1 || organizations != 1 {
		t.Fatalf("one application created %d approvals and %d organizations", successes, organizations)
	}
}

func TestPartnerAdminRechecksAccessAfterOrganizationLock(t *testing.T) {
	pool := database(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cleanPartners(t, pool)
	partner := approvedPartner(t, pool, "Revocation", "REVOKED-PARTNER", 0, 0)
	user := mustNewID(t)
	mustExec(t, pool, `INSERT INTO donor_partner_users(id,partner_id,email,name,role,accepted_at)
		VALUES($1,$2,'revoked@partner.test','Partner','ADMIN',now())`, user, partner)
	token := issueTestPartnerSession(t, pool, user.String())
	lock, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(ctx)
	mustExec(t, lock, "SELECT id FROM donor_partner_signups WHERE id=$1 FOR UPDATE", partner)
	portal := PartnerPortalService{DB: as(t, dbroles.API)}
	done := make(chan error, 1)
	go func() { done <- portal.SaveProfile(ctx, token, "Unauthorized change", "") }()
	awaitPartnerWaiters(t, pool, 1)
	if _, err := queries.New(lock).DisablePartnerUser(ctx, queries.DisablePartnerUserParams{PartnerID: partner, ID: user}); err != nil {
		t.Fatal(err)
	}
	if err := lock.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrPartnerDenied) {
		t.Fatalf("revoked partner changed its organization: %v", err)
	}
	var organization string
	if err := pool.QueryRow(ctx, "SELECT organization FROM donor_partner_signups WHERE id=$1", partner).Scan(&organization); err != nil ||
		organization != "Revocation" {
		t.Fatalf("organization = %q (%v)", organization, err)
	}
}
