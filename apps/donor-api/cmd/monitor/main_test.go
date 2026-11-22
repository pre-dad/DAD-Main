package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/dbroles/dbrolestest"
	"dad/donor-api/internal/migrations"
	"dad/donor-api/internal/store"
)

func TestRunRejectsInvalidInvocationBeforeTheDatabase(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		dsn  string
		want string
	}{
		{"zero age", []string{"--max-age=0"}, "postgresql://unreachable.invalid/donor", "invalid monitor arguments"},
		{"negative age", []string{"--max-age=-1m"}, "postgresql://unreachable.invalid/donor", "invalid monitor arguments"},
		{"stray argument", []string{"queues"}, "postgresql://unreachable.invalid/donor", "invalid monitor arguments"},
		{"no database", nil, "", "is required"},
	} {
		err := run(context.Background(), tc.args, func(string) string { return tc.dsn }, io.Discard)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error containing %q", tc.name, err, tc.want)
		}
	}
}

func TestRunReportsEveryQueue(t *testing.T) {
	dsn := dbrolestest.URL(t, dbroles.Monitor)
	var out bytes.Buffer
	// Other tests' queue rows may need attention; only the report is checked.
	env := map[string]string{dbroles.Monitor.Variable(): dsn, dbroles.AllowOwnerVariable: "true"}
	if err := run(context.Background(), nil, func(name string) string { return env[name] }, &out); err != nil &&
		err.Error() != "queue requires attention" {
		t.Fatal(err)
	}
	var rows []struct{ Queue string }
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("report %q: %v", out.String(), err)
	}
	var queues []string
	for _, row := range rows {
		queues = append(queues, row.Queue)
	}
	if want := []string{"email-changes", "exports", "login-email", "messages", "projections", "push", "reversals", "stripe", "subscriptions"}; !slices.Equal(
		queues,
		want,
	) {
		t.Fatalf("queues %v, want %v", queues, want)
	}
}

func TestRunRefusesTheSchemaOwner(t *testing.T) {
	owner := os.Getenv("TEST_DATABASE_URL")
	dbrolestest.URL(t, dbroles.Monitor)
	err := run(
		context.Background(),
		nil,
		func(name string) string { return map[string]string{dbroles.Monitor.Variable(): owner}[name] },
		io.Discard,
	)
	if err == nil || !strings.Contains(err.Error(), "can change the schema") {
		t.Fatalf("got %v, want the owner refused", err)
	}
}

// monitorDatabase returns the owner pool and the monitor role's URL for a
// migrated database no other test writes to, so the queues start empty.
func monitorDatabase(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	dbrolestest.URL(t, dbroles.Monitor)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	config, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	id, err := store.NewID()
	if err != nil {
		t.Fatal(err)
	}
	name := "donor_go_monitor_" + strings.ReplaceAll(id.String(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
			t.Error(err)
		}
	})
	config = config.Copy()
	config.ConnConfig.Database = name
	owner, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	if err := migrations.Apply(ctx, owner, false); err != nil {
		t.Fatal(err)
	}
	location, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	location.Path = "/" + name
	return owner, dbrolestest.As(t, location.String(), dbroles.Monitor)
}

func TestRunAlertsOnParkedOrOverdueItems(t *testing.T) {
	ctx := context.Background()
	owner, dsn := monitorDatabase(t)
	env := map[string]string{dbroles.Monitor.Variable(): dsn, dbroles.AllowOwnerVariable: "true"}
	monitor := func(maxAge string) error {
		return run(ctx, []string{"--max-age=" + maxAge}, func(name string) string { return env[name] }, io.Discard)
	}
	donor, err := store.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, "INSERT INTO donors(id,email) VALUES($1,$2)", donor, donor.String()+"@monitor.invalid"); err != nil {
		t.Fatal(err)
	}
	queue := func(due time.Time, attempts int) {
		t.Helper()
		id, err := store.NewID()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Exec(ctx, "DELETE FROM donor_login_emails"); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Exec(
			ctx,
			"INSERT INTO donor_login_emails(id,donor_id,available_at,attempts) VALUES($1,$2,$3,$4)", id, donor, due, attempts,
		); err != nil {
			t.Fatal(err)
		}
	}

	if err := monitor("1h"); err != nil {
		t.Fatalf("empty queues: got %v, want none", err)
	}
	queue(time.Now().Add(-10*time.Minute), 0)
	if err := monitor("1h"); err != nil {
		t.Fatalf("an item younger than the maximum age: got %v, want none", err)
	}
	if err := monitor("1m"); err == nil || err.Error() != "queue requires attention" {
		t.Fatalf("an overdue item: got %v, want an alert", err)
	}
	queue(time.Now(), 5)
	if err := monitor("1h"); err == nil || err.Error() != "queue requires attention" {
		t.Fatalf("a parked item: got %v, want an alert", err)
	}
}
