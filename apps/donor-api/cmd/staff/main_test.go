package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/dbroles/dbrolestest"
	"dad/donor-api/internal/migrations"
)

func TestRunRejectsInvalidInvocationBeforeTheDatabase(t *testing.T) {
	env := map[string]string{
		"DONOR_STAFF_ORIGIN":           "https://staff.dad.example.org",
		"DONOR_STAFF_CLI_DATABASE_URL": "postgresql://unreachable.invalid/donor",
	}
	for _, tc := range []struct {
		name  string
		args  []string
		unset string
		want  string
	}{
		{"missing name", []string{"--email=admin@example.org"}, "", "--email and --name are required"},
		{"missing email", []string{"--name=Admin"}, "", "--email and --name are required"},
		{"stray argument", []string{"--email=admin@example.org", "--name=Admin", "extra"}, "", "--email and --name are required"},
		{"recovery with a name", []string{"--recover", "--email=admin@example.org", "--name=Admin"}, "", "--email and --name are required"},
		{"recovery without an email", []string{"--recover"}, "", "--email and --name are required"},
		{"no staff origin", []string{"--email=admin@example.org", "--name=Admin"}, "DONOR_STAFF_ORIGIN", "DONOR_STAFF_ORIGIN"},
		{"no database", []string{"--email=admin@example.org", "--name=Admin"}, "DONOR_STAFF_CLI_DATABASE_URL", "DONOR_STAFF_CLI_DATABASE_URL is required"},
	} {
		getenv := func(name string) string {
			if name == tc.unset {
				return ""
			}
			return env[name]
		}
		if err := run(tc.args, getenv, io.Discard); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error containing %q", tc.name, err, tc.want)
		}
	}
}

func TestRunRefusesTheSchemaOwner(t *testing.T) {
	dbrolestest.URL(t, dbroles.StaffCLI)
	env := map[string]string{
		"DONOR_STAFF_ORIGIN":        "https://staff.dad.example.org",
		dbroles.StaffCLI.Variable(): os.Getenv("TEST_DATABASE_URL"),
	}
	err := run([]string{"--recover", "--email=nobody@example.org"}, func(name string) string { return env[name] }, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "can change the schema") {
		t.Fatalf("got %v, want the owner refused", err)
	}
}

// TestRunInvitesTheFirstAdministratorAndRecoversThem runs both uses of the
// command as its role, on a database of its own: the first invitation needs
// one without staff.
func TestRunInvitesTheFirstAdministratorAndRecoversThem(t *testing.T) {
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	location, err := url.Parse(dbrolestest.URL(t, dbroles.StaffCLI))
	if err != nil {
		t.Fatal(err)
	}
	var suffix [8]byte
	if _, err = rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	name := "donor_staff_test_" + hex.EncodeToString(suffix[:])
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	})
	location.Path = "/" + name
	owner := *location
	owner.RawQuery = ""
	pool, err := pgxpool.New(ctx, owner.String())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = migrations.Apply(ctx, pool, false); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"DONOR_STAFF_ORIGIN":        "https://staff.dad.example.org",
		dbroles.StaffCLI.Variable(): location.String(),
		dbroles.AllowOwnerVariable:  "true",
	}
	getenv := func(name string) string { return env[name] }
	for _, args := range [][]string{{"--email=first@example.org", "--name=First Administrator"}, {"--recover", "--email=first@example.org"}} {
		var out bytes.Buffer
		if err = run(args, getenv, &out); err != nil ||
			!strings.Contains(out.String(), "https://staff.dad.example.org/staff/activate#token=") {
			t.Fatalf("%v: got %q, %v", args, out.String(), err)
		}
	}
	if err = run([]string{"--email=second@example.org", "--name=Second"}, getenv, io.Discard); err == nil ||
		err.Error() != "staff already exists; use an administrator account" {
		t.Fatalf("second bootstrap: %v", err)
	}
}
