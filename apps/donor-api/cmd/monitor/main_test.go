package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/dbroles/dbrolestest"
)

func TestRunRejectsInvalidInvocationBeforeTheDatabase(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		dsn  string
	}{
		{"zero age", []string{"--max-age=0"}, "postgresql://unreachable.invalid/donor"},
		{"negative age", []string{"--max-age=-1m"}, "postgresql://unreachable.invalid/donor"},
		{"stray argument", []string{"queues"}, "postgresql://unreachable.invalid/donor"},
		{"no database", nil, ""},
	} {
		err := run(context.Background(), tc.args, func(string) string { return tc.dsn }, io.Discard)
		if err == nil || err.Error() == "monitor database unavailable" {
			t.Errorf("%s: got %v, want an invocation error", tc.name, err)
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
