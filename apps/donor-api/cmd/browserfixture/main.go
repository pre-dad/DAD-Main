// Command browserfixture prepares an account for the browser journeys and
// prints the single-use sign-in link that opens it, as the emailed link would.
// Browser tests run without email, so this stands in for the inbox; the
// journey itself signs in through the real claim or invitation page.
//
//	browserfixture -kind donor|partner -origin http://127.0.0.1:3000
//
// It writes only to the dedicated donor_go test database.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"dad/donor-api/internal/store"
	"dad/donor-api/internal/store/queries"
)

func main() {
	if err := run(os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "browserfixture:", err)
		os.Exit(1)
	}
}

func run(args []string, getenv func(string) string, out io.Writer) error {
	flags := flag.NewFlagSet("browserfixture", flag.ContinueOnError)
	kind := flags.String("kind", "", "account to prepare: donor or partner")
	origin := flags.String("origin", "", "public origin the link opens")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *kind != "donor" && *kind != "partner" {
		return errors.New("-kind must be donor or partner")
	}
	if *origin == "" {
		return errors.New("-origin is required")
	}
	dsn := getenv("DONOR_GO_DATABASE_URL")
	if parsed, err := url.Parse(dsn); err != nil || parsed.Path != "/donor_go" ||
		!slices.Contains([]string{"localhost", "127.0.0.1", "::1"}, parsed.Hostname()) {
		return errors.New("DONOR_GO_DATABASE_URL must name the dedicated donor_go test database on localhost")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := store.Open(ctx, dsn, 2)
	if err != nil {
		return err
	}
	defer pool.Close()
	return prepare(ctx, pool, *kind, *origin, out)
}

func prepare(ctx context.Context, pool *pgxpool.Pool, kind, origin string, out io.Writer) error {
	id, err := store.NewID()
	if err != nil {
		return err
	}
	token, err := store.NewID()
	if err != nil {
		return err
	}
	secret := store.HashToken(token.String())
	expires := time.Now().Add(15 * time.Minute)
	email := "browser-" + id.String() + "@example.org"
	if kind == "donor" {
		if err = queries.New(pool).InsertGuest(ctx, queries.InsertGuestParams{ID: id, Email: email}); err != nil {
			return err
		}
		if _, err = pool.Exec(ctx, "INSERT INTO donor_login_links (id,donor_id,token_hash,expires_at) VALUES ($1,$1,$2,$3)",
			id, store.HashToken(secret), expires); err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "%s/account/claim#token=%s\n", origin, secret)
		return err
	}
	if _, err = pool.Exec(ctx,
		`INSERT INTO donor_partner_signups (id,organization,name,email,website,message,request_hash,consent_version,approved_at,company_code)
		 VALUES ($1,'Browser Journey Co','Avery Partner',$2,'https://example.org','',$3,'v1',now(),$4)`,
		id, email, "browser-"+id.String(), "BJ"+strings.ToUpper(id.String()[:8])); err != nil {
		return err
	}
	if _, err = pool.Exec(ctx,
		`INSERT INTO donor_partner_users (id,partner_id,email,name,role,invite_token_hash,invite_expires_at)
		 VALUES ($1,$1,$2,'Avery Partner','ADMIN',$3,$4)`,
		id, email, store.HashToken(secret), expires); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s/partner/accept#token=%s\n", origin, secret)
	return err
}
