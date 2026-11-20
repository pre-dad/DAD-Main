package main

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/dbroles/dbrolestest"
	"dad/donor-api/internal/store"
)

func TestRunRejectsInvalidInvocationBeforeTheDatabase(t *testing.T) {
	env := map[string]string{
		"DONOR_GO_WORKERS_ENABLED":         "true",
		"DONOR_WORKER_STRIPE_DATABASE_URL": "postgresql://unreachable.invalid/donor",
	}
	for _, tc := range []struct {
		name  string
		args  []string
		unset string
	}{
		{"no kind", nil, ""},
		{"unknown kind", []string{"--kind=refunds"}, ""},
		{"stray argument", []string{"--kind=stripe", "extra"}, ""},
		{"retry for another kind", []string{"--kind=messages", "--retry-event=evt_123"}, ""},
		{"retry without event id", []string{"--kind=stripe", "--retry-event=123"}, ""},
		{"concurrency on a sequential kind", []string{"--kind=stripe", "--concurrency=2"}, ""},
		{"concurrency above the limit", []string{"--kind=messages", "--concurrency=99"}, ""},
		{"concurrency below one", []string{"--kind=messages", "--concurrency=0"}, ""},
		{"workers disabled", []string{"--kind=stripe"}, "DONOR_GO_WORKERS_ENABLED"},
		{"no database", []string{"--kind=stripe"}, "DONOR_WORKER_STRIPE_DATABASE_URL"},
	} {
		getenv := func(name string) string {
			if name == tc.unset {
				return ""
			}
			return env[name]
		}
		err := run(context.Background(), tc.args, getenv)
		if !isConfigError(err) || err.Error() == "worker database is unreachable" {
			t.Errorf("%s: got %v, want a configuration error", tc.name, err)
		}
	}
}

func TestPushWorkerRunsWithoutProvidersButRefusesPartialOnes(t *testing.T) {
	if step, err := stepFor("push", nil, func(string) string { return "" }); err != nil || step == nil {
		t.Fatalf("unconfigured push worker: %v", err)
	}
	partial := func(name string) string {
		if name == "DONOR_PUSH_APNS_KEY_ID" {
			return "ABC123DEFG"
		}
		return ""
	}
	if _, err := stepFor("push", nil, partial); !isConfigError(err) {
		t.Fatalf("partial APNs settings: got %v, want a configuration error", err)
	}
}

func TestStripeWorkerRefusesAnInvalidRequestCeiling(t *testing.T) {
	env := map[string]string{"STRIPE_SECRET_KEY": "sk_test_123", "DONOR_STRIPE_REQUESTS_PER_SECOND": "0"}
	if _, err := stepFor("stripe", nil, func(name string) string { return env[name] }); !isConfigError(err) {
		t.Fatalf("got %v, want a configuration error", err)
	}
}

func isConfigError(err error) bool {
	_, ok := errors.AsType[configError](err)
	return ok
}

func TestWorkersRunAsTheirRoles(t *testing.T) {
	for _, kind := range []string{"projections", "push", "messages", "login-email", "email-changes"} {
		role := dbroles.Workers[kind]
		env := map[string]string{
			"DONOR_GO_WORKERS_ENABLED":   "true",
			role.Variable():              dbrolestest.URL(t, role),
			dbroles.AllowOwnerVariable:   "true",
			"DONOR_SMTP_HOST":            "smtp.example.org",
			"DONOR_EMAIL_FROM":           "DAD <sender@example.org>",
			"DONOR_EMAIL_ENCRYPTION_KEY": base64.URLEncoding.EncodeToString(make([]byte, 32)),
			"DONOR_STAFF_ORIGIN":         "https://staff.dad.example.org",
			"DONOR_PUBLIC_ORIGIN":        "https://dad.example.org",
		}
		if err := run(context.Background(), []string{"--kind=" + kind, "--once"}, func(name string) string { return env[name] }); err != nil {
			t.Errorf("%s: %v", kind, err)
		}
	}
}

func TestRunRefusesTheSchemaOwner(t *testing.T) {
	dbrolestest.URL(t, dbroles.ProjectionWorker)
	env := map[string]string{"DONOR_GO_WORKERS_ENABLED": "true", "DONOR_WORKER_PROJECTIONS_DATABASE_URL": os.Getenv("TEST_DATABASE_URL")}
	err := run(context.Background(), []string{"--kind=projections", "--once"}, func(name string) string { return env[name] })
	if !isConfigError(err) || !strings.Contains(err.Error(), "can change the schema") {
		t.Fatalf("got %v, want the owner refused", err)
	}
	env[dbroles.AllowOwnerVariable] = "true"
	if err = run(context.Background(), []string{"--kind=projections", "--once"}, func(name string) string { return env[name] }); err != nil {
		t.Fatalf("owner allowed: %v", err)
	}
}

// A projections step builds a queued staff export, so exports need no worker
// of their own.
func TestProjectionsWorkerBuildsQueuedExports(t *testing.T) {
	ctx := context.Background()
	owner, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close(ctx) })
	staff, err := store.NewID()
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Exec(ctx, "INSERT INTO donor_staff(id,email,name,role) VALUES($1,$1::uuid::text||'@example.org','Export test','SUPER_ADMIN')", staff); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, statement := range []string{
			"DELETE FROM donor_export_jobs WHERE actor_id=$1", "DELETE FROM donor_staff_audit WHERE actor_id=$1", "DELETE FROM donor_staff WHERE id=$1",
		} {
			if _, err := owner.Exec(ctx, statement, staff); err != nil {
				t.Error(err)
			}
		}
	})
	if _, err = owner.Exec(ctx, "INSERT INTO donor_export_jobs(id,actor_id,dataset,requested_at,expires_at) VALUES($1,$2,'staff',now(),now()+interval '1 day')", job, staff); err != nil {
		t.Fatal(err)
	}
	role := dbroles.ProjectionWorker
	env := map[string]string{
		"DONOR_GO_WORKERS_ENABLED": "true",
		role.Variable():            dbrolestest.URL(t, role),
		dbroles.AllowOwnerVariable: "true",
	}
	if err = run(ctx, []string{"--kind=projections", "--once"}, func(name string) string { return env[name] }); err != nil {
		t.Fatal(err)
	}
	var state string
	var rows int
	if err = owner.QueryRow(ctx, "SELECT state,row_count FROM donor_export_jobs WHERE id=$1", job).Scan(&state, &rows); err != nil ||
		state != "complete" || rows < 1 {
		t.Fatalf("export %s with %d rows: %v", state, rows, err)
	}
}

type stubQueue struct {
	worked bool
	err    error
	calls  int
}

func (q *stubQueue) DeliverNext(context.Context, time.Time) (bool, error) {
	q.calls++
	return q.worked, q.err
}

func TestRunAllRunsEveryStepAndJoinsErrors(t *testing.T) {
	failing, working := errors.New("first"), errors.New("second")
	var ran []string
	step := func(name string, worked bool, err error) func(context.Context) (bool, error) {
		return func(context.Context) (bool, error) {
			ran = append(ran, name)
			return worked, err
		}
	}
	worked, err := runAll(context.Background(), step("a", false, failing), step("b", true, working), step("c", false, nil))
	if !worked || !errors.Is(err, failing) || !errors.Is(err, working) || strings.Join(ran, "") != "abc" {
		t.Fatalf("worked=%v err=%v ran=%v", worked, err, ran)
	}
}

func TestRotatingQueuesDoNotStarveEachOther(t *testing.T) {
	broken := &stubQueue{err: errors.New("down")}
	busy := &stubQueue{worked: true}
	step := rotating([]deliverer{broken, busy})
	for range 4 {
		if _, err := step(context.Background()); err != nil && !errors.Is(err, broken.err) {
			t.Fatal(err)
		}
	}
	if broken.calls == 0 || busy.calls == 0 {
		t.Fatalf("a queue never ran: broken=%d busy=%d", broken.calls, busy.calls)
	}
	idle := &stubQueue{}
	worked, err := rotating([]deliverer{broken, idle})(context.Background())
	if worked || !errors.Is(err, broken.err) || idle.calls != 1 {
		t.Fatalf("worked=%v err=%v idle calls=%d", worked, err, idle.calls)
	}
}
