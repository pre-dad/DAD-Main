package store

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestContactLeadsAreDeleted runs the migration that deletes the retired
// contact form's leads, inside a transaction it rolls back. Approved partners
// stay, and an initiative linked to a lead loses only the link.
func TestContactLeadsAreDeleted(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	migration, err := os.ReadFile("../../db/migrations/0062_delete_contact_leads.sql")
	if err != nil {
		t.Fatal(err)
	}
	partner := approvedPartner(t, pool, "Kept Partner", "KEPT-"+strings.ToUpper(mustNewID(t).String()[:8]), 0, 0)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	lead, linked, initiative := testID(t), testID(t), testID(t)
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO donor_partner_signups (id, organization, name, email, website, message, request_hash, consent_version)
			VALUES ($1,'Lead','L','lead@x.test','https://lead.test','Hello',$2,'v1'), ($3,'Linked','L','linked@x.test','https://linked.test','',$4,'v1')`,
			[]any{lead, "h-" + lead.String(), linked, "h-" + linked.String()}},
		{`INSERT INTO donor_initiatives (id, slug, title, summary, type, location, region, started_on, partner_signup_id)
			VALUES ($1,'lead-linked','Linked','Summary','WATER','North','North','2026-01-01',$2)`, []any{initiative, linked}},
	} {
		if _, err := tx.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	var leads, partners int
	var unlinked bool
	if err := tx.QueryRow(ctx, `SELECT
  (SELECT count(*) FROM donor_partner_signups WHERE approved_at IS NULL),
  (SELECT count(*) FROM donor_partner_signups WHERE id=$1),
  (SELECT partner_signup_id IS NULL FROM donor_initiatives WHERE id=$2)`, partner, initiative).Scan(&leads, &partners, &unlinked); err != nil {
		t.Fatal(err)
	}
	if leads != 0 || partners != 1 || !unlinked {
		t.Fatalf("%d leads kept, approved partner kept %d, initiative unlinked %v", leads, partners, unlinked)
	}
}
