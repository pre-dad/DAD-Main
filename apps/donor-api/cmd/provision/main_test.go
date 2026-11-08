package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/url"
	"os"
	"strings"
	"testing"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/dbroles/dbrolestest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func secret(t *testing.T) string {
	t.Helper()
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(bytes[:])
}

// environment names a fresh login for every role in the test database.
// Logins are cluster-wide, so each test drops the ones it created.
func environment(t *testing.T) map[string]string {
	t.Helper()
	owner := os.Getenv("TEST_DATABASE_URL")
	dbrolestest.URL(t, dbroles.API)
	prefix := "provision_test_" + secret(t)[:8] + "_"
	env := map[string]string{dbroles.OwnerVariable: owner}
	for _, role := range dbroles.Roles {
		location, err := url.Parse(owner)
		if err != nil {
			t.Fatal(err)
		}
		location.User = url.UserPassword(prefix+strings.TrimPrefix(role.Name, "dad_"), secret(t))
		env[role.Variable()] = location.String()
	}
	t.Cleanup(func() {
		conn, err := pgx.Connect(context.Background(), owner)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close(context.Background())
		rows, err := conn.Query(context.Background(), "SELECT rolname FROM pg_roles WHERE starts_with(rolname,$1)", prefix)
		if err != nil {
			t.Error(err)
			return
		}
		names, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Error(err)
		}
		for _, name := range names {
			if _, err = conn.Exec(context.Background(), "DROP ROLE "+pgx.Identifier{name}.Sanitize()); err != nil {
				t.Error(err)
			}
		}
	})
	return env
}

// connect opens a pool as the process would.
func connect(t *testing.T, dsn string) (*pgxpool.Pool, error) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		return nil, err
	}
	t.Cleanup(pool.Close)
	return pool, pool.Ping(context.Background())
}

func TestProvisionCreatesALoginPerRole(t *testing.T) {
	env := environment(t)
	getenv := func(name string) string { return env[name] }
	if err := run(nil, getenv, io.Discard); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, role := range dbroles.Roles {
		conn, err := connect(t, env[role.Variable()])
		if err != nil {
			t.Fatalf("%s: %v", role.Name, err)
		}
		var limit int32
		if err = conn.QueryRow(ctx, "SELECT rolconnlimit FROM pg_roles WHERE rolname=current_user").Scan(&limit); err != nil ||
			limit != role.Pool*dbroles.DefaultReplicas(role) {
			t.Errorf("%s: connection limit %d, want %d (%v)", role.Name, limit, role.Pool*dbroles.DefaultReplicas(role), err)
		}
		if err = role.Check(ctx, conn, false); err != nil {
			t.Errorf("%s: %v", role.Name, err)
		}
	}
	monitor, _ := connect(t, env[dbroles.Monitor.Variable()])
	if _, err := monitor.Exec(ctx, "SELECT email FROM donors LIMIT 1"); err == nil {
		t.Error("the monitor's login read donor addresses")
	}
	if err := dbroles.API.Check(ctx, monitor, false); err == nil || !strings.Contains(err.Error(), "not a member of dad_api") {
		t.Errorf("the monitor's login passed as the API's: %v", err)
	}

	previous := env[dbroles.API.Variable()]
	location, _ := url.Parse(previous)
	location.User = url.UserPassword(location.User.Username(), secret(t))
	location.RawQuery = "pool_max_conns=12"
	env[dbroles.ReplicasVariable] = "api=3"
	env[dbroles.API.Variable()] = location.String()
	if err := run(nil, getenv, io.Discard); err != nil {
		t.Fatalf("provisioning again: %v", err)
	}
	if _, err := connect(t, previous); err == nil {
		t.Error(
			"the replaced password still signs in; the server must authenticate by password (a trust rule in pg_hba.conf, as in a default local install, accepts any), so use the container in the README",
		)
	}
	api, err := connect(t, env[dbroles.API.Variable()])
	if err != nil {
		t.Fatal(err)
	}
	var limit int32
	if err = api.QueryRow(ctx, "SELECT rolconnlimit FROM pg_roles WHERE rolname=current_user").Scan(&limit); err != nil || limit != 36 {
		t.Errorf("connection limit %d after pool_max_conns=12 and api=3 (%v)", limit, err)
	}
}

func TestProvisionRefusesARoleHeldByAnotherUser(t *testing.T) {
	env := environment(t)
	getenv := func(name string) string { return env[name] }
	if err := run(nil, getenv, io.Discard); err != nil {
		t.Fatal(err)
	}
	stale, _ := url.Parse(env[dbroles.Monitor.Variable()])
	location, _ := url.Parse(env[dbroles.Monitor.Variable()])
	location.User = url.UserPassword(stale.User.Username()+"_new", secret(t))
	env[dbroles.Monitor.Variable()] = location.String()
	err := run(nil, getenv, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "dad_monitor's privileges also reach "+stale.User.Username()) {
		t.Fatalf("got %v, want the stale login named", err)
	}
	if _, err = connect(t, location.String()); err == nil {
		t.Error("a refused provisioning left its new login behind")
	}
}

func TestProvisionRefusesUnsafeLogins(t *testing.T) {
	owner := os.Getenv("TEST_DATABASE_URL")
	location, err := url.Parse(owner)
	if err != nil {
		t.Fatal(err)
	}
	database := location.Path
	for _, tc := range []struct {
		name, variable, url, want string
	}{
		{"group role", dbroles.Monitor.Variable(), "postgresql://dad_api:0123456789abcdef0123@db" + database, "must name its own login user"},
		{"reserved name", dbroles.Monitor.Variable(), "postgresql://pg_monitor:0123456789abcdef0123@db" + database, "must name its own login user"},
		{"short password", dbroles.Monitor.Variable(), "postgresql://monitor:short@db" + database, "at least 16 printable ASCII"},
		{"other database", dbroles.Monitor.Variable(), "postgresql://monitor:0123456789abcdef0123@db/elsewhere", `names database "elsewhere"`},
		{"pool size", dbroles.Monitor.Variable(), "postgresql://monitor:0123456789abcdef0123@db" + database + "?pool_max_conns=0", "invalid pool_max_conns"},
		{"missing", dbroles.Rekey.Variable(), "", "set DONOR_REKEY_DATABASE_URL"},
	} {
		env := map[string]string{dbroles.OwnerVariable: owner}
		for _, role := range dbroles.Roles {
			env[role.Variable()] = "postgresql://" + strings.TrimPrefix(role.Name, "dad_") + "_login:0123456789abcdef0123@db" + database
		}
		env[tc.variable] = tc.url
		err := run(nil, func(name string) string { return env[name] }, io.Discard)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestProvisionRefusesLimitsOverTheBudgetBeforeChangingAnything(t *testing.T) {
	env := environment(t)
	env[dbroles.BudgetVariable] = "100"
	err := run(nil, func(name string) string { return env[name] }, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "over the budget of 100") {
		t.Fatalf("got %v", err)
	}
	if _, err = connect(t, env[dbroles.Monitor.Variable()]); err == nil {
		t.Error("a refused provisioning left a login behind")
	}
	env[dbroles.BudgetVariable] = ""
	env[dbroles.ReplicasVariable] = "nobody=2"
	if err = run(nil, func(name string) string { return env[name] }, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "unknown role") {
		t.Errorf("got %v", err)
	}
}
