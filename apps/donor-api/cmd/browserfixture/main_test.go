package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/dbroles/dbrolestest"
	"dad/donor-api/internal/store"
)

func TestRefusesAnythingButTheBrowserTestDatabase(t *testing.T) {
	for _, tc := range []struct {
		args []string
		dsn  string
		want string
	}{
		{[]string{"-kind", "staff", "-origin", "http://127.0.0.1:3000"}, "postgresql://u@localhost/donor_go", "-kind"},
		{[]string{"-kind", "donor"}, "postgresql://u@localhost/donor_go", "-origin"},
		{[]string{"-kind", "donor", "-origin", "http://127.0.0.1:3000"}, "postgresql://u@localhost/dad", "donor_go"},
		{[]string{"-kind", "partner", "-origin", "http://127.0.0.1:3000"}, "", "donor_go"},
		{[]string{"-kind", "donor", "-origin", "http://127.0.0.1:3000"}, "postgresql://u@db.example.org/donor_go", "localhost"},
	} {
		getenv := func(name string) string {
			if name == "DONOR_GO_DATABASE_URL" {
				return tc.dsn
			}
			return ""
		}
		if err := run(tc.args, getenv, io.Discard); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v with %q: got %v, want an error naming %s", tc.args, tc.dsn, err, tc.want)
		}
	}
}

func TestPreparedAccountsMatchTheLinkTheyPrint(t *testing.T) {
	dbrolestest.URL(t, dbroles.API)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	origin := "http://127.0.0.1:3000"

	for _, tc := range []struct{ kind, path, table, column, hashColumn string }{
		{"donor", "/account/claim#token=", "donor_login_links", "donor_id", "token_hash"},
		{"partner", "/partner/accept#token=", "donor_partner_users", "partner_id", "invite_token_hash"},
	} {
		var out bytes.Buffer
		if err := prepare(ctx, pool, tc.kind, origin, &out); err != nil {
			t.Fatalf("%s: %v", tc.kind, err)
		}
		link := strings.TrimSpace(out.String())
		secret, found := strings.CutPrefix(link, origin+tc.path)
		if !found || secret == "" {
			t.Fatalf("%s: link %q does not have the shape %s%s<secret>", tc.kind, link, origin, tc.path)
		}
		var id string
		if err := pool.QueryRow(ctx, "SELECT "+tc.column+"::text FROM "+tc.table+" WHERE "+tc.hashColumn+"=$1", store.HashToken(secret)).Scan(&id); err != nil {
			t.Fatalf("%s: the printed secret matches no stored hash: %v", tc.kind, err)
		}
		t.Cleanup(func() {
			statements := []string{"DELETE FROM donor_login_links WHERE donor_id=$1", "DELETE FROM donors WHERE id=$1"}
			if tc.kind == "partner" {
				statements = []string{
					"DELETE FROM donor_partner_users WHERE partner_id=$1",
					"DELETE FROM donor_partner_signups WHERE id=$1",
				}
			}
			for _, statement := range statements {
				if _, err := pool.Exec(ctx, statement, id); err != nil {
					t.Error(err)
				}
			}
		})
	}
}
