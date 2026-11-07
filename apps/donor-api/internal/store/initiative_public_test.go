package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"dad/donor-api/internal/dbroles"
	"github.com/jackc/pgx/v5/pgtype"
)

// TestPublicInitiativesShowOnlyPublished covers the public initiative pages:
// they list published initiatives with their approved partner's name, and a
// draft reads as not found.
func TestPublicInitiativesShowOnlyPublished(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	_, admin := staffFixture(t, pool, "SUPER_ADMIN")
	s := InitiativeService{Staff: &StaffService{DB: as(t, dbroles.API)}}
	partner := approvedPartner(t, pool, "Public Wells", "PUBWELLS", 0, 0)
	cleanInitiatives(t, pool)
	in := withBoardApproval(t, pool, validInitiative("public-wells"))
	in.PartnerSignupID = partner.String()
	published, err := s.Create(ctx, admin, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Publish(ctx, admin, published); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, admin, validInitiative("draft-wells")); err != nil {
		t.Fatal(err)
	}

	reader := InitiativeReader{DB: as(t, dbroles.API)}
	all, err := reader.Published(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found *PublicInitiative
	for i := range all {
		if all[i].Slug == "draft-wells" {
			t.Fatal("a draft initiative is listed publicly")
		}
		if all[i].Slug == "public-wells" {
			found = &all[i]
		}
	}
	if found == nil || found.ID != published.String() || found.Partner == nil || *found.Partner != "Public Wells" ||
		found.StartedOn != "2026-03-01" {
		t.Fatalf("published initiative %+v", found)
	}
	if one, err := reader.PublishedBySlug(ctx, "public-wells"); err != nil || one.ID != found.ID || one.Partner == nil ||
		*one.Partner != *found.Partner {
		t.Fatalf("by slug: %+v, %v", one, err)
	}
	if _, err := reader.PublishedBySlug(ctx, "draft-wells"); !errors.Is(err, ErrInitiativeNotPublished) {
		t.Fatalf("draft by slug: %v; want ErrInitiativeNotPublished", err)
	}
}

// TestContentInitiativesMoveToTheEditor runs the migrations that copy the
// spreadsheet's published initiatives into donor_initiatives and carry over
// their board approvals, inside a transaction it rolls back.
func TestContentInitiativesMoveToTheEditor(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	migration, err := os.ReadFile("../../db/migrations/0061_initiatives_from_content.sql")
	if err != nil {
		t.Fatal(err)
	}
	approvals, err := os.ReadFile("../../db/migrations/0063_initiative_board_approval.sql")
	if err != nil {
		t.Fatal(err)
	}
	_, backfill, found := strings.Cut(string(approvals), ";\n\nUPDATE")
	if !found {
		t.Fatal("0063 has no approval backfill")
	}
	actor, _ := staffFixture(t, pool, "SUPER_ADMIN")
	approvedPartner(t, pool, "Sheet Partner", "SHEETPART", 0, 0)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	publication, source := testID(t), testID(t)
	payload := `{"initiatives":[` +
		`{"slug":"sheet-wells","title":"Sheet Wells","summary":"Wells.","partner":"sheet partner","planned_start":"2027-04-01",` +
		`"board_approval_source_id":"` + source.String() + `","approved_on":"2026-02-03"},` +
		`{"slug":"sheet-farms","title":"Sheet Farms","summary":"Farms.","partner":"Nobody Yet","planned_start":"2027-05-01"}` +
		`],"ledger":[],"ledger_as_of":"2026-10-01","cadence":"monthly"}`
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		// Migration 0077 dropped these tables; recreate them for this transaction
		// only so the historical copy migrations can still be exercised.
		{`CREATE TABLE donor_content_publications (
			id UUID PRIMARY KEY, actor_id UUID NOT NULL REFERENCES donor_staff (id),
			payload JSON NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now())`, nil},
		{`CREATE TABLE donor_content_head (
			id VARCHAR(16) PRIMARY KEY, created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			publication_id UUID REFERENCES donor_content_publications (id))`, nil},
		{"INSERT INTO donor_content_sources(id,actor_id,name,sha256,encrypted_data) VALUES($1,$2,'approval.pdf',repeat('0',64),'x')", []any{source, actor}},
		{"INSERT INTO donor_content_publications(id,actor_id,payload,created_at) VALUES($1,$2,$3,now())", []any{publication, actor, payload}},
		{"INSERT INTO donor_content_head(id,publication_id) VALUES('r1',$1) ON CONFLICT(id) DO UPDATE SET publication_id=EXCLUDED.publication_id", []any{publication}},
		{"DELETE FROM donor_initiatives WHERE slug IN ('sheet-wells','sheet-farms')", nil},
	} {
		if _, err := tx.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "UPDATE"+backfill); err != nil {
		t.Fatal(err)
	}
	var approval pgtype.UUID
	var approvedOn string
	if err := tx.QueryRow(ctx, "SELECT board_approval_source_id, board_approved_on::text FROM donor_initiatives WHERE slug='sheet-wells'").
		Scan(&approval, &approvedOn); err != nil || approval != source || approvedOn != "2026-02-03" {
		t.Fatalf("carried approval %v on %s, err %v", approval, approvedOn, err)
	}
	reader := InitiativeReader{DB: tx}
	wells, err := reader.PublishedBySlug(ctx, "sheet-wells")
	if err != nil || wells.Partner == nil || *wells.Partner != "Sheet Partner" || wells.StartedOn != "2027-04-01" ||
		wells.Location != "To be announced" {
		t.Fatalf("copied initiative %+v, %v", wells, err)
	}
	farms, err := reader.PublishedBySlug(ctx, "sheet-farms")
	if err != nil || farms.Partner != nil {
		t.Fatalf("an initiative whose partner is not approved: %+v, %v", farms, err)
	}
	var rotating int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM donor_initiatives WHERE slug IN ('sheet-wells','sheet-farms') AND in_rotation").Scan(&rotating); err != nil ||
		rotating != 0 {
		t.Fatalf("%d planned initiatives joined the daily rotation, err %v", rotating, err)
	}
}

// TestOverallSpendSumsCategoryTotalsAcrossPublishedInitiatives covers the
// public spend page (PRD §5.2): category totals only, summed over published
// initiatives, and a draft initiative's spend never counts.
func TestOverallSpendSumsCategoryTotalsAcrossPublishedInitiatives(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	_, admin := staffFixture(t, pool, "SUPER_ADMIN")
	s := InitiativeService{Staff: &StaffService{DB: as(t, dbroles.API)}}
	cleanInitiatives(t, pool)
	reader := InitiativeReader{DB: as(t, dbroles.API)}

	empty, err := reader.OverallSpend(ctx)
	if err != nil || empty.TotalMinor != 0 || empty.Categories == nil || len(empty.Categories) != 0 {
		t.Fatalf("no published initiatives: %+v, %v", empty, err)
	}

	for _, c := range []struct {
		slug    string
		publish bool
		spend   map[string]int64
	}{
		{"overall-a", true, map[string]int64{"Direct aid": 100_00, "Logistics": 20_00}},
		{"overall-b", true, map[string]int64{"Direct aid": 50_00}},
		{"overall-draft", false, map[string]int64{"Direct aid": 999_00, "Fundraising": 1_00}},
	} {
		id, err := s.Create(ctx, admin, withBoardApproval(t, pool, validInitiative(c.slug)))
		if err != nil {
			t.Fatal(err)
		}
		for category, amount := range c.spend {
			in := SpendInput{Category: category, AmountMinor: amount, VerificationNote: "Verified", VerifiedOn: "2026-10-01"}
			if err = s.SetSpend(ctx, admin, id, in); err != nil {
				t.Fatal(err)
			}
			if c.publish {
				if err = s.PublishSpend(ctx, admin, id, category); err != nil {
					t.Fatal(err)
				}
			}
		}
		if c.publish {
			if err = s.Publish(ctx, admin, id); err != nil {
				t.Fatal(err)
			}
		}
	}

	got, err := reader.OverallSpend(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []SpendCategory{{Category: "Direct aid", AmountMinor: 150_00}, {Category: "Logistics", AmountMinor: 20_00}}
	if got.TotalMinor != 170_00 || len(got.Categories) != len(want) {
		t.Fatalf("overall spend = %+v, want total 17000 over %v", got, want)
	}
	for i := range want {
		if got.Categories[i] != want[i] {
			t.Fatalf("category %d = %+v, want %+v", i, got.Categories[i], want[i])
		}
	}
}

func TestOverallSpendIgnoresUnpublishedSpendLinesOfPublishedInitiatives(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	_, admin := staffFixture(t, pool, "SUPER_ADMIN")
	s := InitiativeService{Staff: &StaffService{DB: as(t, dbroles.API)}}
	cleanInitiatives(t, pool)
	id, err := s.Create(ctx, admin, withBoardApproval(t, pool, validInitiative("spend-draft-line")))
	if err != nil {
		t.Fatal(err)
	}
	line := func(category string, amount int64) SpendInput {
		return SpendInput{Category: category, AmountMinor: amount, VerificationNote: "Verified", VerifiedOn: "2026-10-01"}
	}
	if err = s.SetSpend(ctx, admin, id, line("Direct aid", 40_00)); err != nil {
		t.Fatal(err)
	}
	if err = s.PublishSpend(ctx, admin, id, "Direct aid"); err != nil {
		t.Fatal(err)
	}
	if err = s.Publish(ctx, admin, id); err != nil {
		t.Fatal(err)
	}
	if err = s.SetSpend(ctx, admin, id, line("Logistics", 900_00)); err != nil {
		t.Fatal(err)
	}
	got, err := (InitiativeReader{DB: as(t, dbroles.API)}).OverallSpend(ctx)
	if err != nil || got.TotalMinor != 40_00 || len(got.Categories) != 1 || got.Categories[0].Category != "Direct aid" {
		t.Fatalf("overall spend counted an unpublished line: %+v, %v", got, err)
	}
}
