package dbroles

import (
	"slices"
	"strings"
	"testing"
)

func TestPrivilegeListsAreCanonical(t *testing.T) {
	got := parsePrivileges("UPDATE (encrypted_token), SELECT (id, encrypted_token), INSERT,SELECT")
	want := privileges{"INSERT", "SELECT", "SELECT (encrypted_token)", "SELECT (id)", "UPDATE (encrypted_token)"}
	if !slices.Equal(got, want) {
		t.Fatalf("parsed %q, want %q", got, want)
	}
	if sql := got.sql(); sql != `INSERT, SELECT, SELECT ("encrypted_token", "id"), UPDATE ("encrypted_token")` {
		t.Fatalf("rendered %s", sql)
	}
	if got := parsePrivileges(""); len(got) != 0 {
		t.Fatalf("empty list parsed as %q", got)
	}
}

func TestEveryDeclaredRoleIsAProcessRole(t *testing.T) {
	for relation, byRole := range grants {
		for role, list := range byRole {
			if !slices.Contains(Roles, role) || len(parsePrivileges(list)) == 0 {
				t.Errorf("%s: %+v holds %q", relation, role, list)
			}
		}
	}
}

func TestParsePrivilegesNamesAnUnbalancedList(t *testing.T) {
	defer func() {
		got, _ := recover().(string)
		if !strings.Contains(got, `"(id"`) {
			t.Fatalf("panic = %q; want it to quote the unclosed list", got)
		}
	}()
	parsePrivileges("SELECT (id")
}
