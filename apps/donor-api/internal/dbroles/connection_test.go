package dbroles

import (
	"bytes"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestConnectionURLPrecedenceAndParts(t *testing.T) {
	const secret = "s3cr3t/p@ss:word#1"
	for name, tc := range map[string]struct {
		env  map[string]string
		want func(*testing.T, *pgx.ConnConfig)
		none bool
		fail string
	}{
		"full URL alone is unchanged": {
			env: map[string]string{"DONOR_API_DATABASE_URL": "postgresql://dad_api_login:inline-password-1@db:5432/dad?sslmode=require"},
			want: func(t *testing.T, c *pgx.ConnConfig) {
				if c.Password != "inline-password-1" || c.Host != "db" || c.Database != "dad" {
					t.Fatalf("got %s@%s/%s", c.Password, c.Host, c.Database)
				}
			},
		},
		"separate password replaces the URL's": {
			env: map[string]string{
				"DONOR_API_DATABASE_URL":      "postgresql://dad_api_login@db:5432/dad",
				"DONOR_API_DATABASE_PASSWORD": secret,
			},
			want: func(t *testing.T, c *pgx.ConnConfig) {
				if c.Password != secret || c.User != "dad_api_login" || c.Host != "db" {
					t.Fatalf("got user=%s host=%s", c.User, c.Host)
				}
			},
		},
		"parts build a URL": {
			env: map[string]string{
				"DONOR_WORKER_STRIPE_DATABASE_HOST":     "db.internal",
				"DONOR_WORKER_STRIPE_DATABASE_PORT":     "6432",
				"DONOR_WORKER_STRIPE_DATABASE_NAME":     "dad",
				"DONOR_WORKER_STRIPE_DATABASE_USER":     "stripe_login",
				"DONOR_WORKER_STRIPE_DATABASE_PASSWORD": secret,
				"DONOR_WORKER_STRIPE_DATABASE_SSLMODE":  "require",
			},
			want: func(t *testing.T, c *pgx.ConnConfig) {
				if c.Host != "db.internal" || c.Port != 6432 || c.Database != "dad" || c.User != "stripe_login" || c.Password != secret {
					t.Fatalf("got %s:%d/%s %s", c.Host, c.Port, c.Database, c.User)
				}
			},
		},
		"URL wins over parts": {
			env: map[string]string{
				"DONOR_API_DATABASE_URL":  "postgresql://u:pw-pw-pw-pw-pw-1@urlhost/dad",
				"DONOR_API_DATABASE_HOST": "parthost",
			},
			want: func(t *testing.T, c *pgx.ConnConfig) {
				if c.Host != "urlhost" {
					t.Fatalf("host %s", c.Host)
				}
			},
		},
		"default port": {
			env: map[string]string{"DONOR_API_DATABASE_HOST": "h", "DONOR_API_DATABASE_NAME": "d", "DONOR_API_DATABASE_USER": "u", "DONOR_API_DATABASE_PASSWORD": "p"},
			want: func(t *testing.T, c *pgx.ConnConfig) {
				if c.Port != 5432 {
					t.Fatalf("port %d", c.Port)
				}
			},
		},
		"nothing configured": {env: map[string]string{"DONOR_API_DATABASE_PASSWORD": "alone"}, none: true},
		"password with a URL that has no user": {
			env:  map[string]string{"DONOR_API_DATABASE_URL": "postgresql://db/dad", "DONOR_API_DATABASE_PASSWORD": secret},
			fail: "DONOR_API_DATABASE_URL must be a URL with a user",
		},
		"password with an unparseable URL": {
			env:  map[string]string{"DONOR_API_DATABASE_URL": "postgresql://u@db:bad/dad", "DONOR_API_DATABASE_PASSWORD": secret},
			fail: "DONOR_API_DATABASE_URL must be a URL with a user",
		},
		"parts without a user": {
			env:  map[string]string{"DONOR_API_DATABASE_HOST": "h", "DONOR_API_DATABASE_NAME": "d", "DONOR_API_DATABASE_PASSWORD": secret},
			fail: "DONOR_API_DATABASE_USER and DONOR_API_DATABASE_NAME are required",
		},
		"parts without a database name": {
			env:  map[string]string{"DONOR_API_DATABASE_HOST": "h", "DONOR_API_DATABASE_USER": "u", "DONOR_API_DATABASE_PASSWORD": secret},
			fail: "DONOR_API_DATABASE_USER and DONOR_API_DATABASE_NAME are required",
		},
	} {
		t.Run(name, func(t *testing.T) {
			variable := "DONOR_API_DATABASE_URL"
			if name == "parts build a URL" {
				variable = "DONOR_WORKER_STRIPE_DATABASE_URL"
			}
			got, err := ConnectionURL(variable, env(tc.env))
			if tc.fail != "" {
				if err == nil || !strings.Contains(err.Error(), tc.fail) || strings.Contains(err.Error(), secret) {
					t.Fatalf("got %q, %v; want error containing %q and no password", got, err, tc.fail)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.none {
				if got != "" {
					t.Fatalf("got %q", got)
				}
				return
			}
			c, err := pgx.ParseConfig(got)
			if err != nil {
				t.Fatal("unparseable")
			}
			tc.want(t, c)
		})
	}
}

func TestRoleURLAssemblesPartsAndOwnerFallback(t *testing.T) {
	parts := env(map[string]string{
		"DONOR_API_DATABASE_HOST": "h", "DONOR_API_DATABASE_NAME": "d", "DONOR_API_DATABASE_USER": "u", "DONOR_API_DATABASE_PASSWORD": "pw",
	})
	if got, err := API.URL(parts); err != nil {
		t.Fatal(err)
	} else if u, err := url.Parse(got); err != nil || u.Host != "h:5432" {
		t.Fatalf("API.URL = %v %v", u, err)
	}
	owner := env(map[string]string{
		AllowOwnerVariable:           "true",
		"DONOR_GO_DATABASE_HOST":     "oh",
		"DONOR_GO_DATABASE_NAME":     "d",
		"DONOR_GO_DATABASE_USER":     "o",
		"DONOR_GO_DATABASE_PASSWORD": "pw",
	})
	if got, err := API.URL(owner); err != nil {
		t.Fatal(err)
	} else if u, err := url.Parse(got); err != nil || u.Host != "oh:5432" {
		t.Fatalf("owner fallback = %v %v", u, err)
	}
	if got, err := API.URL(env(map[string]string{AllowOwnerVariable: "false", "DONOR_GO_DATABASE_HOST": "oh"})); err != nil || got != "" {
		t.Fatalf("owner connection used without opt-in: %q, %v", got, err)
	}
	if _, err := API.URL(env(map[string]string{"DONOR_API_DATABASE_HOST": "h"})); err == nil {
		t.Fatal("incomplete role parts were accepted")
	}
}

func TestPasswordNeverReachesCheckErrorsOrLogs(t *testing.T) {
	const secret = "never-log-this-password-9"
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	getenv := env(map[string]string{
		"DONOR_API_DATABASE_HOST": "h", "DONOR_API_DATABASE_NAME": "d", "DONOR_API_DATABASE_USER": "u", "DONOR_API_DATABASE_PASSWORD": secret,
	})
	dsn, err := API.URL(getenv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseLogin(API, strings.Replace(dsn, "5432", "bad port", 1)); err != nil {
		log.Error("parse", "err", err)
	}
	if _, err := ParseLogin(API, dsn[:len("postgresql://u:")]+"short@h/d"); err != nil {
		log.Error("parse", "err", err)
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatalf("password leaked into logs: %s", logs.String())
	}
}
