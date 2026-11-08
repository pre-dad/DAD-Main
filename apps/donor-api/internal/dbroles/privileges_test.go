package dbroles_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/dbroles/dbrolestest"
	"dad/donor-api/internal/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// owner connects as the test user, which owns the schema.
func owner(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbrolestest.URL(t, dbroles.API)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = migrations.Verify(ctx, pool); err != nil {
		t.Fatalf("TEST_DATABASE_URL is not migrated to this checkout (%v); run `make migrate` (or go run ./cmd/migrate with "+
			"DONOR_GO_DATABASE_URL set to it) on a database created for this checkout", err)
	}
	return pool
}

func TestCatalogMatchesTheDeclaration(t *testing.T) {
	pool := owner(t)
	ctx := context.Background()
	drift, err := dbroles.Drift(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(drift) > 0 {
		t.Errorf("privileges differ from the declaration; migrations would run:\n%s", strings.Join(drift, "\n"))
	}
	undeclared, err := dbroles.Undeclared(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, relation := range undeclared {
		t.Errorf("%s has no declared privileges; list it in grants with every role that uses it", relation)
	}
}

func TestNewRelationsNeedDeclaredPrivileges(t *testing.T) {
	ctx := context.Background()
	tx, err := owner(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "CREATE SCHEMA recipient_probe; CREATE TABLE recipient_probe.cases (id uuid PRIMARY KEY); CREATE TABLE donor_probe (id uuid PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	undeclared, err := dbroles.Undeclared(ctx, tx)
	if err != nil || !slices.Equal(undeclared, []string{"donor_probe", "recipient_probe.cases"}) {
		t.Fatalf("undeclared %v, %v", undeclared, err)
	}
}

func TestRolesHoldOnlyTheirPrivileges(t *testing.T) {
	pool := owner(t)
	for _, role := range dbroles.Roles {
		var attributes, owned, schemas, memberships int
		err := pool.QueryRow(context.Background(), `
SELECT (SELECT count(*) FROM pg_roles WHERE oid=r.oid AND (rolcanlogin OR rolsuper OR rolcreaterole OR rolcreatedb OR rolreplication OR rolbypassrls)),
 (SELECT count(*) FROM pg_class WHERE relowner=r.oid)+(SELECT count(*) FROM pg_namespace WHERE nspowner=r.oid)
  +(SELECT count(*) FROM pg_proc WHERE proowner=r.oid)+(SELECT count(*) FROM pg_type WHERE typowner=r.oid),
 (SELECT count(*) FROM pg_namespace WHERE has_schema_privilege(r.oid,oid,'CREATE'))+has_database_privilege(r.oid,current_database(),'CREATE')::int,
 (SELECT count(*) FROM pg_auth_members WHERE member=r.oid)
FROM pg_roles r WHERE r.rolname=$1`, role.Name).Scan(&attributes, &owned, &schemas, &memberships)
		if err != nil {
			t.Fatalf("%s: %v", role.Name, err)
		}
		if attributes+owned+schemas+memberships > 0 {
			t.Errorf(
				"%s: attributes %d, owned objects %d, schemas or databases it may create in %d, memberships %d; want none",
				role.Name,
				attributes,
				owned,
				schemas,
				memberships,
			)
		}
	}
}

func TestMigrationsRestoreTheDeclaration(t *testing.T) {
	ctx := context.Background()
	tx, err := owner(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for _, statement := range []string{
		"GRANT DELETE, TRUNCATE ON donor_gifts TO dad_api",
		"REVOKE SELECT ON donor_gifts FROM dad_worker_messages",
		"GRANT UPDATE (email) ON donors TO dad_worker_push",
		"GRANT SELECT ON donor_messages TO PUBLIC",
		"GRANT CREATE ON SCHEMA public TO dad_monitor, PUBLIC",
	} {
		if _, err = tx.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	drift, err := dbroles.Drift(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`REVOKE ALL ON TABLE "donor_gifts" FROM "dad_api"`,
		`GRANT SELECT, UPDATE ("id") ON TABLE "donor_gifts" TO "dad_api"`,
		`GRANT SELECT ON TABLE "donor_gifts" TO "dad_worker_messages"`,
		`GRANT SELECT, UPDATE ("id") ON TABLE "donors" TO "dad_worker_push"`,
		`REVOKE ALL ON TABLE "donor_messages" FROM PUBLIC`,
		`REVOKE CREATE ON SCHEMA "public" FROM PUBLIC`,
		`GRANT USAGE ON SCHEMA "public" TO "dad_monitor"`,
	} {
		if !slices.Contains(drift, want) {
			t.Errorf("drift lacks %s:\n%s", want, strings.Join(drift, "\n"))
		}
	}
	if err = dbroles.Reconcile(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if drift, err = dbroles.Drift(ctx, tx); err != nil || len(drift) > 0 {
		t.Fatalf("drift after reconciling: %v %v", drift, err)
	}
}

// TestRolesAreRefusedWhatTheyDoNotNeed checks a sample of the boundaries the
// declaration draws, each in a transaction that is rolled back.
func TestRolesAreRefusedWhatTheyDoNotNeed(t *testing.T) {
	for _, tc := range []struct {
		role       dbroles.Role
		statements []string
	}{
		{dbroles.API, []string{
			"UPDATE donor_referral_corrections SET rules_version=0", "DELETE FROM donor_referral_corrections",
			"UPDATE donor_export_jobs SET state='complete'", "DELETE FROM donor_export_jobs", "INSERT INTO donor_export_chunks (job_id, seq, data) VALUES (gen_random_uuid(), 0, ''::bytea)",
			"DROP TABLE donor_gifts", "ALTER TABLE donors ADD COLUMN extra integer", "CREATE TABLE extra (id integer)",
			"TRUNCATE donor_messages", "UPDATE donor_gifts SET amount_minor=1", "DELETE FROM donors", "CREATE ROLE extra",
		}},
		{dbroles.ProjectionWorker, []string{
			"UPDATE donor_referral_corrections SET rules_version=0", "DELETE FROM donor_referral_corrections",
			"SELECT encrypted_token FROM donor_login_emails", "SELECT encrypted_token FROM donor_email_changes",
			"UPDATE donors SET email='taken@example.org'", "UPDATE donor_staff SET role='SUPER_ADMIN'", "SELECT token_hash FROM donor_staff_sessions",
			"INSERT INTO donor_gifts (id) VALUES (gen_random_uuid())", "SELECT encrypted_data FROM donor_content_sources",
			"UPDATE donor_staff_audit SET action='x'", "DELETE FROM donor_staff_audit", "INSERT INTO donor_export_jobs (id) VALUES (gen_random_uuid())",
		}},
		{dbroles.Monitor, []string{
			"SELECT body FROM donor_messages", "SELECT email FROM donors", "SELECT data FROM donor_stripe_events",
			"SELECT new_email FROM donor_email_changes", "UPDATE donor_messages SET attempts=0", "DELETE FROM donor_push_notifications",
			"SELECT data FROM donor_export_chunks", "SELECT actor_id FROM donor_export_jobs",
		}},
		{dbroles.Rekey, []string{"SELECT new_email FROM donor_email_changes", "SELECT donor_id FROM donor_login_emails", "SELECT name FROM donor_content_sources"}},
		{dbroles.MessageWorker, []string{"SELECT encrypted_data FROM donor_content_sources", "SELECT token_hash FROM donor_sessions", "INSERT INTO donor_messages (id) VALUES (gen_random_uuid())"}},
		{dbroles.StaffCLI, []string{"SELECT id FROM donors", "SELECT id FROM donor_gifts", "SELECT details FROM donor_staff_audit"}},
		{dbroles.StripeWorker, []string{"SELECT id FROM donor_staff", "UPDATE donors SET email='taken@example.org'", "DELETE FROM donor_gifts", "UPDATE donor_referral_corrections SET rules_version=0", "DELETE FROM donor_referral_corrections"}},
	} {
		pool := dbrolestest.Open(t, tc.role)
		for _, statement := range tc.statements {
			err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
				_, err := tx.Exec(context.Background(), statement)
				return err
			})
			if pgErr, ok := errors.AsType[*pgconn.PgError](err); !ok || pgErr.Code != "42501" {
				t.Errorf("%s: %s: got %v, want insufficient privilege", tc.role.Name, statement, err)
			}
		}
	}
}

func TestCheckRefusesOwnersAndOtherRoles(t *testing.T) {
	ctx := context.Background()
	pool := owner(t)
	if err := dbroles.API.Check(ctx, pool, false); err == nil ||
		!strings.Contains(err.Error(), "DONOR_API_DATABASE_URL connects as a user that can change the schema") {
		t.Errorf("owner: got %v", err)
	}
	if err := dbroles.API.Check(ctx, pool, true); err != nil {
		t.Errorf("owner allowed: %v", err)
	}
	// The test superuser assumes each role, and could reset it.
	for _, role := range dbroles.Roles {
		assumed := dbrolestest.Open(t, role)
		if err := role.Check(ctx, assumed, false); err == nil || !strings.Contains(err.Error(), "can change the schema") {
			t.Errorf("%s assumed by the owner: got %v", role.Name, err)
		}
		if err := role.Check(ctx, assumed, true); err != nil {
			t.Errorf("%s assumed on a local machine: %v", role.Name, err)
		}
	}
	if err := dbroles.API.Check(ctx, dbrolestest.Open(t, dbroles.Monitor), true); err == nil ||
		!strings.Contains(err.Error(), "not a member of dad_api") {
		t.Errorf("another role: got %v", err)
	}
}

func TestEachProcessReadsItsOwnURL(t *testing.T) {
	env := map[string]string{
		"DONOR_WORKER_LOGIN_EMAIL_DATABASE_URL": "postgresql://login@db/app",
		dbroles.OwnerVariable:                   "postgresql://owner@db/app",
	}
	getenv := func(name string) string { return env[name] }
	if got := dbroles.Workers["login-email"].URL(getenv); got != "postgresql://login@db/app" {
		t.Errorf("own URL: %q", got)
	}
	if got := dbroles.API.URL(getenv); got != "" {
		t.Errorf("owner URL without permission: %q", got)
	}
	env[dbroles.AllowOwnerVariable] = "true"
	if got := dbroles.API.URL(getenv); got != "postgresql://owner@db/app" {
		t.Errorf("owner URL with permission: %q", got)
	}
	if got := dbroles.StaffCLI.Variable(); got != "DONOR_STAFF_CLI_DATABASE_URL" {
		t.Errorf("variable %q", got)
	}
}
