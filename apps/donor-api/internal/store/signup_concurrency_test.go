package store

import (
	"context"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/store/queries"
)

func TestSignupDoesNotOverwriteConcurrentlyCreatedProfile(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, _ := donor(t, pool)
	t.Cleanup(func() { cleanExec(t, "DELETE FROM donor_login_emails WHERE donor_id=$1", id) })
	// Remove the fixture inside the uncommitted transaction so the second
	// request sees no account until the first signup commits.
	first, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback(ctx)
	q := queries.New(first)
	mustExec(t, first, "DELETE FROM donors WHERE id=$1", id)
	// The address differs from the fixture donor's, so the second signup finds
	// no account until the open transaction commits.
	email := "signup-" + id.String() + "@example.org"
	if err = q.LockEmailIdentity(ctx, email); err != nil {
		t.Fatal(err)
	}
	if err = q.InsertGuest(ctx, queries.InsertGuestParams{ID: id, Email: email}); err != nil {
		t.Fatal(err)
	}
	mustExec(
		t,
		first,
		"UPDATE donors SET full_name='Original Donor', city='Boston', zip='02108', timezone='America/New_York' WHERE id=$1",
		id,
	)
	done := make(chan error, 1)
	go func() {
		done <- (PasswordService{DB: api}).Signup(ctx, SignupInput{Name: "Replacement", Email: email, City: "Chicago", Zip: "60601"})
	}()
	for {
		var blocked bool
		err = pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid)))", first.Conn().PgConn().PID()).
			Scan(&blocked)
		if err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err = <-done:
			t.Fatalf("signup did not wait for identity creation: %v", err)
		case <-time.After(time.Millisecond):
		}
	}
	if err = first.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	var name, city, zip string
	if err = pool.QueryRow(ctx, "SELECT full_name,city,zip FROM donors WHERE id=$1", id).Scan(&name, &city, &zip); err != nil {
		t.Fatal(err)
	}
	if name != "Original Donor" || city != "Boston" || zip != "02108" {
		t.Fatalf("signup overwrote the existing profile: %q %q %q", name, city, zip)
	}
}

func TestSignupCreatesProfileAndQueuesSupportedSignInDestination(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	id, _ := donor(t, pool)
	mustExec(t, pool, "DELETE FROM donors WHERE id=$1", id)
	email := "new-signup-" + id.String() + "@example.org"
	for _, table := range []string{"donor_login_emails", "donor_metric_projections"} {
		cleanup(t, "DELETE FROM "+table+" WHERE donor_id IN (SELECT id FROM donors WHERE email=$1)", email)
	}
	cleanup(t, "DELETE FROM donors WHERE email=$1", email)
	s := PasswordService{DB: as(t, dbroles.API)}
	if err := s.Signup(ctx, SignupInput{Name: "New Donor", Email: email, City: "Boston", Zip: "02108"}); err != nil {
		t.Fatal(err)
	}
	var name, path string
	if err := pool.QueryRow(ctx, "SELECT id,full_name FROM donors WHERE email=$1", email).Scan(&id, &name); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT return_path FROM donor_login_emails WHERE donor_id=$1", id).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if name != "New Donor" || !ValidReturnPath(path) {
		t.Fatalf("signup profile or destination: %q %q", name, path)
	}
	if err := s.Signup(ctx, SignupInput{Name: "Replacement", Email: email, City: "Chicago", Zip: "60601"}); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT full_name FROM donors WHERE id=$1", id).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "New Donor" {
		t.Fatalf("repeated signup replaced profile: %q", name)
	}
}
