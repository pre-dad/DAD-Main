package migrations

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"dad/donor-api/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func isolatedDatabase(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	config, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil || (config.ConnConfig.Database != "donor_go" && !strings.HasPrefix(config.ConnConfig.Database, "donor_go_")) {
		t.Fatal("TEST_DATABASE_URL must name the dedicated donor_go test database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	admin, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	var suffix [8]byte
	if _, err = rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	name := "donor_migration_test_" + hex.EncodeToString(suffix[:])
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, finish := context.WithTimeout(context.Background(), 10*time.Second)
		defer finish()
		if _, err := admin.Exec(cleanup, "DROP DATABASE "+identifier+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	})
	config = config.Copy()
	config.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

func TestManifestRejectsMisnamedMigrations(t *testing.T) {
	if _, err := manifest(db.Migrations); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"1_r1.sql", "0003_Mixed.sql", "0003-dash.sql", "0003_.sql", "00003_r1.sql", "0003 r1.sql"} {
		fsys := fstest.MapFS{"migrations/0001_r1.sql": {}, "migrations/" + name: {}}
		if _, err := manifest(fsys); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
}

func TestFreshMigrationIdempotenceAndHistoryIntegrity(t *testing.T) {
	pool, ctx := isolatedDatabase(t)
	if err := Verify(ctx, pool); err == nil {
		t.Fatal("empty database reported ready")
	}
	for range 2 {
		if err := Apply(ctx, pool, false); err != nil {
			t.Fatal(err)
		}
		if err := Verify(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, "UPDATE donor_schema_migrations SET checksum='tampered'"); err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, pool, false); err == nil {
		t.Fatal("tampered checksum accepted")
	}
	if err := Verify(ctx, pool); err == nil {
		t.Fatal("tampered history reported ready")
	}
}

func TestAdoptionPreservesExistingRecordsAndRejectsDrift(t *testing.T) {
	for _, drift := range []bool{false, true} {
		t.Run(map[bool]string{false: "matching", true: "drifted"}[drift], func(t *testing.T) {
			pool, ctx := isolatedDatabase(t)
			files, err := manifest(db.Migrations)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = pool.Exec(ctx, files[0].sql); err != nil {
				t.Fatal(err)
			}
			if _, err = pool.Exec(ctx, "CREATE TABLE alembic_version(version_num text); INSERT INTO alembic_version VALUES('0030_donor_public_content'); CREATE TABLE legacy_fixture(value text); INSERT INTO legacy_fixture VALUES('preserve'); INSERT INTO donors(id,email) VALUES('00000000-0000-4000-8000-000000000001','preserve@example.org')"); err != nil {
				t.Fatal(err)
			}
			if err = Apply(ctx, pool, false); !errors.Is(err, ErrAdoptionRequired) {
				t.Fatalf("implicit adoption: %v", err)
			}
			if drift {
				if _, err = pool.Exec(ctx, "ALTER TABLE donors ADD COLUMN unexpected text"); err != nil {
					t.Fatal(err)
				}
				if err = Apply(ctx, pool, true); !errors.Is(err, ErrSchemaMismatch) {
					t.Fatalf("schema drift accepted: %v", err)
				}
				if err = Verify(ctx, pool); err == nil {
					t.Fatal("rejected adoption reported ready")
				}
			} else {
				if err = Apply(ctx, pool, true); err != nil {
					t.Fatal(err)
				}
				if err = Verify(ctx, pool); err != nil {
					t.Fatal(err)
				}
			}
			var email, legacy string
			if err = pool.QueryRow(ctx, "SELECT email FROM donors").Scan(&email); err != nil || email != "preserve@example.org" {
				t.Fatal("donor changed", err)
			}
			if err = pool.QueryRow(ctx, "SELECT value FROM legacy_fixture").Scan(&legacy); err != nil || legacy != "preserve" {
				t.Fatal("legacy data changed", err)
			}
			var leaked bool
			if err = pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname='dad_baseline_validation')").Scan(&leaked); err != nil ||
				leaked {
				t.Fatal("validation schema leaked", err)
			}
		})
	}
}

func TestHistoryReportsPendingAndUnknownMigrations(t *testing.T) {
	pool, ctx := isolatedDatabase(t)
	if err := Apply(ctx, pool, false); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM donor_schema_migrations WHERE version=(SELECT max(version) FROM donor_schema_migrations)"); err != nil {
		t.Fatal(err)
	}
	if err := Verify(ctx, pool); err == nil || !strings.Contains(err.Error(), "pending") {
		t.Fatalf("missing last migration: %v, want pending", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO donor_schema_migrations(version,checksum) VALUES('9999_unknown.sql','x')"); err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, pool, false); err == nil || !strings.Contains(err.Error(), "unknown migration: 9999_unknown.sql") {
		t.Fatalf("unknown history entry: %v", err)
	}
	if err := Verify(ctx, pool); err == nil {
		t.Fatal("unknown history entry reported ready")
	}
}

func TestAdoptionRejectsADroppedColumn(t *testing.T) {
	pool, ctx := isolatedDatabase(t)
	files, err := manifest(db.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, files[0].sql); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, "CREATE TABLE alembic_version(version_num text); INSERT INTO alembic_version VALUES('0030_donor_public_content'); ALTER TABLE donors DROP COLUMN public_name"); err != nil {
		t.Fatal(err)
	}
	if err = Apply(ctx, pool, true); !errors.Is(err, ErrSchemaMismatch) || !strings.Contains(err.Error(), "public_name") {
		t.Fatalf("dropped column accepted: %v", err)
	}
}

// applyBefore records the embedded migrations preceding name as applied, so a
// test can seed data the way that migration will find it.
func applyBefore(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) {
	t.Helper()
	files, err := manifest(db.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, "CREATE TABLE donor_schema_migrations(version text PRIMARY KEY,checksum text NOT NULL,applied_at timestamptz NOT NULL DEFAULT now())"); err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if file.name == name {
			return
		}
		if _, err = pool.Exec(ctx, file.sql); err != nil {
			t.Fatal(err)
		}
		if _, err = pool.Exec(ctx, "INSERT INTO donor_schema_migrations(version,checksum) VALUES($1,$2)", file.name, file.checksum); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatalf("no migration %s", name)
}

// Password and authenticator-code credentials end with the passkey migration:
// accounts survive, but their sessions and open invitations do not.
func TestPasskeyMigrationRetiresPasswordCredentials(t *testing.T) {
	pool, ctx := isolatedDatabase(t)
	applyBefore(t, ctx, pool, "0007_staff_passkeys.sql")
	_, err := pool.Exec(
		ctx,
		`INSERT INTO donor_staff(id,email,name,role,password_hash,totp_secret,totp_counter) VALUES('00000000-0000-4000-8000-000000000001','admin@example.org','Admin','ADMIN','$argon2id$legacy','\x00',7);
INSERT INTO donor_staff_sessions(id,staff_id,token_hash,expires_at) VALUES('00000000-0000-4000-8000-000000000002','00000000-0000-4000-8000-000000000001','session',now()+interval '1 hour');
INSERT INTO donor_staff_invites(id,staff_id,token_hash,expires_at,pending_secret) VALUES('00000000-0000-4000-8000-000000000003','00000000-0000-4000-8000-000000000001','invite',now()+interval '1 hour','\x00')`,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err = Apply(ctx, pool, false); err != nil {
		t.Fatal(err)
	}
	if err = Verify(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var staff, credentialColumns, openSessions, openInvites int
	if err = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM donor_staff WHERE email='admin@example.org' AND role='SUPER_ADMIN'),
 (SELECT count(*) FROM information_schema.columns WHERE table_name IN ('donor_staff','donor_staff_invites') AND column_name IN ('password_hash','totp_secret','totp_counter','failures','locked_until','pending_secret')),
 (SELECT count(*) FROM donor_staff_sessions WHERE revoked_at IS NULL),
 (SELECT count(*) FROM donor_staff_invites WHERE consumed_at IS NULL)`).Scan(&staff, &credentialColumns, &openSessions, &openInvites); err != nil {
		t.Fatal(err)
	}
	if staff != 1 || credentialColumns != 0 || openSessions != 0 || openInvites != 0 {
		t.Fatalf(
			"got %d accounts, %d credential columns, %d open sessions, %d open invitations",
			staff,
			credentialColumns,
			openSessions,
			openInvites,
		)
	}
}

// A subscription synced before views read stored state keeps its row, with no
// billing anchor until its next sync and no change held by the processor.
func TestSubscriptionSyncMigrationKeepsSyncedRows(t *testing.T) {
	pool, ctx := isolatedDatabase(t)
	applyBefore(t, ctx, pool, "0009_subscription_sync.sql")
	if _, err := pool.Exec(ctx, `INSERT INTO donors(id,email) VALUES('00000000-0000-4000-8000-000000000001','synced@example.org');
INSERT INTO donor_checkouts(id,donor_id,amount_minor,frequency) VALUES('00000000-0000-4000-8000-000000000002','00000000-0000-4000-8000-000000000001',3000,'MONTHLY');
INSERT INTO donor_stripe_checkouts(id,checkout_id,request_hash,request_params,stripe_subscription_id,status) VALUES('00000000-0000-4000-8000-000000000003','00000000-0000-4000-8000-000000000002','hash','{}','sub_synced','COMPLETE');
INSERT INTO donor_subscriptions(id,attempt_id,donor_id,status,period_end,observed_at) VALUES('00000000-0000-4000-8000-000000000004','00000000-0000-4000-8000-000000000003','00000000-0000-4000-8000-000000000001','active','2027-02-01T00:00:00Z','2027-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, pool, false); err != nil {
		t.Fatal(err)
	}
	if err := Verify(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var status string
	var anchored, external bool
	var observed time.Time
	if err := pool.QueryRow(ctx, "SELECT status,billing_anchor IS NOT NULL,external_change,observed_at FROM donor_subscriptions").Scan(&status, &anchored, &external, &observed); err != nil {
		t.Fatal(err)
	}
	if status != "active" || anchored || external || !observed.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("got %s, anchored %v, external %v, observed %s", status, anchored, external, observed)
	}
}
