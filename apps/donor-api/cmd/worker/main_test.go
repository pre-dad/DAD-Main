package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/dbroles/dbrolestest"
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
	for _, kind := range []string{"projections", "push"} {
		role := dbroles.Workers[kind]
		env := map[string]string{
			"DONOR_GO_WORKERS_ENABLED": "true",
			role.Variable():            dbrolestest.URL(t, role),
			dbroles.AllowOwnerVariable: "true",
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
