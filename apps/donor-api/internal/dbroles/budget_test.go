package dbroles_test

import (
	"slices"
	"strings"
	"testing"

	"dad/donor-api/internal/dbroles"
)

func logins() []dbroles.Login {
	var all []dbroles.Login
	for _, role := range dbroles.Roles {
		all = append(all, dbroles.Login{Role: role, Pool: role.Pool})
	}
	return all
}

func TestDefaultLimitsCoverReplicasWithinTheBudget(t *testing.T) {
	all := logins()
	if err := dbroles.Plan(all, nil, dbroles.DefaultBudget); err != nil {
		t.Fatal(err)
	}
	want := map[string]int32{"dad_api": 640}
	var total int32
	for _, login := range all {
		expected, set := want[login.Role.Name]
		if !set {
			expected = 4
		}
		if got := login.ConnectionLimit(); got != expected {
			t.Errorf("%s: limit %d, want %d", login.Role.Name, got, expected)
		}
		total += login.ConnectionLimit()
	}
	if total != 680 {
		t.Errorf("total %d, want 680", total)
	}
}

func TestReplicasOverrideTheDefaultAndMultiplyThePoolSize(t *testing.T) {
	replicas, err := dbroles.ParseReplicas("api=12, push=5")
	if err != nil {
		t.Fatal(err)
	}
	all := logins()
	if err = dbroles.Plan(all, replicas, dbroles.DefaultBudget); err != nil {
		t.Fatal(err)
	}
	for _, login := range all {
		switch login.Role.Name {
		case "dad_api":
			if login.ConnectionLimit() != 480 {
				t.Errorf("api limit %d, want 480", login.ConnectionLimit())
			}
		case "dad_worker_push":
			if login.ConnectionLimit() != 10 {
				t.Errorf("push limit %d, want 10", login.ConnectionLimit())
			}
		}
	}
}

func TestPlanRefusesLimitsOverTheBudget(t *testing.T) {
	err := dbroles.Plan(logins(), nil, 679)
	if err == nil || !strings.Contains(err.Error(), "total 680, over the budget of 679") {
		t.Fatalf("got %v", err)
	}
	if err = dbroles.Plan(logins(), nil, 680); err != nil {
		t.Fatalf("a total equal to the budget: %v", err)
	}
}

func TestParseReplicasRefusesBadLists(t *testing.T) {
	for list, want := range map[string]string{
		"api":           "role=count",
		"api=0":         "at least 1",
		"api=x":         "at least 1",
		"nobody=2":      `unknown role "nobody"`,
		"api=2,api=3":   `"api" twice`,
		"dad_api=2":     "unknown role",
		"staff-cli=2,,": "role=count",
	} {
		if _, err := dbroles.ParseReplicas(list); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", list, err, want)
		}
	}
	if replicas, err := dbroles.ParseReplicas(""); err != nil || len(replicas) != 0 {
		t.Errorf("empty list: %v %v", replicas, err)
	}
}

func TestParseBudget(t *testing.T) {
	if n, err := dbroles.ParseBudget(""); err != nil || n != dbroles.DefaultBudget {
		t.Errorf("default: %d %v", n, err)
	}
	if n, err := dbroles.ParseBudget("1500"); err != nil || n != 1500 {
		t.Errorf("1500: %d %v", n, err)
	}
	for _, bad := range []string{"0", "-3", "many"} {
		if _, err := dbroles.ParseBudget(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestEveryRoleHasAUniqueKey(t *testing.T) {
	var keys []string
	for _, role := range dbroles.Roles {
		keys = append(keys, role.Key())
	}
	slices.Sort(keys)
	if len(slices.Compact(slices.Clone(keys))) != len(keys) {
		t.Fatalf("duplicate keys in %v", keys)
	}
	if !slices.Contains(keys, "email-changes") || !slices.Contains(keys, "staff-cli") || !slices.Contains(keys, "api") {
		t.Errorf("unexpected keys %v", keys)
	}
}
