package dbroles_test

import (
	"context"
	"strings"
	"testing"

	"dad/donor-api/internal/dbroles"
)

func TestProvisionRefusesTheOwnerAsALogin(t *testing.T) {
	ctx := context.Background()
	tx, err := owner(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var user string
	if err = tx.QueryRow(ctx, "SELECT current_user").Scan(&user); err != nil {
		t.Fatal(err)
	}
	err = dbroles.Provision(ctx, tx, []dbroles.Login{{Role: dbroles.Rekey, User: user, Password: "0123456789abcdef", Pool: 2}})
	if err == nil || !strings.Contains(err.Error(), "DONOR_REKEY_DATABASE_URL names the schema owner") {
		t.Fatalf("got %v", err)
	}
}
