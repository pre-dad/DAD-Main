package store

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/png"
	"strings"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedPartnerInitiative creates a PUBLISHED initiative linked to partnerID with
// a committed amount, one spend category, and one verified PEOPLE metric.
func seedPartnerInitiative(t *testing.T, pool *pgxpool.Pool, partnerID pgtype.UUID, slug string, committed, spend, people int64) {
	t.Helper()
	initID, _ := NewID()
	mustExec(
		t,
		pool,
		`INSERT INTO donor_initiatives (id, slug, title, summary, type, location, region, started_on, committed_minor, partner_signup_id, publish_state, published_at)
		 VALUES ($1,$2,$3,'Summary','PROGRAM','Reno, NV','Northern Nevada', current_date, $4, $5, 'PUBLISHED', now())`,
		initID,
		slug,
		"Initiative "+slug,
		committed,
		partnerID,
	)
	spendID, _ := NewID()
	mustExec(
		t,
		pool,
		`INSERT INTO donor_initiative_spend (id, initiative_id, category, amount_minor) VALUES ($1,$2,'Meals',$3)`,
		spendID,
		initID,
		spend,
	)
	metricID, _ := NewID()
	mustExec(
		t,
		pool,
		`INSERT INTO donor_initiative_metrics (id, initiative_id, position, label, value, unit, verification_note, verified_on, publish_state)
		 VALUES ($1,$2,0,'People served',$3,'PEOPLE','audited', current_date, 'PUBLISHED')`,
		metricID,
		initID,
		people,
	)
	t.Cleanup(func() {
		cleanExec(t, "DELETE FROM donor_initiatives WHERE id=$1", initID)
	})
}

func TestPartnerDashboardAndReport(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	apps := PartnerApplicationService{DB: pool, Staff: &StaffService{DB: as(t, dbroles.API)}}
	dash := PartnerDashboardService{PartnerPortalService{DB: as(t, dbroles.API)}}
	_, superToken := staffFixture(t, pool, "SUPER_ADMIN")
	cleanPartners(t, pool)

	appID := testID(t)
	if err := apps.Apply(ctx, appID, validApplication("Delta Inc", "cfo@delta.test")); err != nil {
		t.Fatal(err)
	}
	partnerID, err := apps.Approve(ctx, superToken, appID)
	if err != nil {
		t.Fatal(err)
	}
	var adminID string
	if err := pool.QueryRow(ctx, "SELECT id FROM donor_partner_users WHERE partner_id=$1 AND role='ADMIN'", partnerID).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	adminSession := issueTestPartnerSession(t, pool, adminID)
	if err := dash.SaveLogo(ctx, adminSession, partnerTestLogo(t, 32, 16)); err != nil {
		t.Fatal(err)
	}

	// Two published initiatives: $1,000 committed / $400 deployed / 150 people,
	// and $500 committed / $100 deployed / 50 people. Tiles sum them.
	seedPartnerInitiative(t, pool, partnerID, "delta-a", 100000, 40000, 150)
	seedPartnerInitiative(t, pool, partnerID, "delta-b", 50000, 10000, 50)

	ov, err := dash.Overview(ctx, adminSession)
	if err != nil {
		t.Fatal(err)
	}
	if ov.CommittedMinor != 150000 || ov.DeployedMinor != 50000 || ov.PeopleReached != 200 {
		t.Fatalf("tiles wrong: %+v", ov)
	}
	if ov.DeployedPercent != 33 { // 50000/150000
		t.Fatalf("deployed %% wrong: %d", ov.DeployedPercent)
	}
	if ov.CompanyCode == "" || ov.UpdatedAt == nil || ov.EmployeesEnrolled != nil {
		t.Fatalf("company code %q, updated %v, enrolled %v: want a code, a last-updated time, and suppressed enrollment",
			ov.CompanyCode, ov.UpdatedAt, ov.EmployeesEnrolled)
	}

	inits, err := dash.Initiatives(ctx, adminSession)
	if err != nil || len(inits) != 2 {
		t.Fatalf("initiatives: %d %v", len(inits), err)
	}
	a := inits[0]
	if a.Title != "Initiative delta-a" || a.Status != "ACTIVE" || a.Region != "Northern Nevada" || a.DeployedMinor != 40000 ||
		len(a.Metrics) != 1 || a.Metrics[0].Value != 150 || a.Metrics[0].VerifiedOn == "" ||
		len(a.Spend) != 1 || a.Spend[0].Category != "Meals" || a.Spend[0].AmountMinor != 40000 || a.UpdatedAt.IsZero() {
		t.Fatalf("initiative detail: %+v", a)
	}
	spend, err := dash.Spend(ctx, adminSession)
	if err != nil || len(spend) != 1 || spend[0].AmountMinor != 50000 {
		t.Fatalf("spend: %+v %v", spend, err)
	}

	// Export: a valid PDF, recorded in history. Admin-only.
	pdf, err := dash.ExportReport(ctx, adminSession)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF-")) || len(pdf) < 500 {
		t.Fatalf("not a PDF: %d bytes", len(pdf))
	}
	exports, err := dash.Exports(ctx, adminSession)
	if err != nil || len(exports) != 1 || exports[0].CommittedMinor != 150000 || !exports[0].Downloadable ||
		!exports[0].PeopleReachedVerified {
		t.Fatalf("export history: %+v %v", exports, err)
	}
	var reportID pgtype.UUID
	if err := reportID.Scan(exports[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := dash.SaveProfile(ctx, adminSession, "Delta Updated", "Changed since export"); err != nil {
		t.Fatal(err)
	}
	retained, err := dash.ReportArtifact(ctx, adminSession, reportID)
	if err != nil || !bytes.Equal(retained, pdf) {
		t.Fatalf("old export was changed or regenerated: %v", err)
	}
	var snapshotName string
	if err := pool.QueryRow(ctx, "SELECT snapshot->'overview'->>'organization' FROM donor_partner_reports WHERE id=$1", reportID).Scan(&snapshotName); err != nil ||
		snapshotName != "Delta Inc" {
		t.Fatalf("snapshot changed: %q %v", snapshotName, err)
	}
	other := approvedPartner(t, pool, "Other Report Owner", "OTHER-REPORT", 0, 0)
	otherUser := mustNewID(t)
	mustExec(
		t,
		pool,
		"INSERT INTO donor_partner_users(id,partner_id,email,name,role) VALUES($1,$2,'other@report.test','Other','ADMIN')",
		otherUser,
		other,
	)
	otherSession := issueTestPartnerSession(t, pool, otherUser.String())
	t.Cleanup(func() {
		cleanExec(t, "DELETE FROM donor_partner_sessions WHERE partner_user_id=$1", otherUser)
		cleanExec(t, "DELETE FROM donor_partner_users WHERE id=$1", otherUser)
	})
	if _, err := dash.ReportArtifact(ctx, otherSession, reportID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("another organization read the export: %v", err)
	}

	// A VIEWER can read tiles but cannot export (admin-only).
	mustExec(t, pool, "DELETE FROM donor_partner_login_emails")
	if err := dash.Invite(ctx, adminSession, "analyst@delta.test", "Ana", "VIEWER"); err != nil {
		t.Fatal(err)
	}
	viewerSession, err := dash.AcceptInvite(ctx, deliverPartnerLinks(t, time.Now().UTC())["analyst@delta.test"])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dash.Overview(ctx, viewerSession); err != nil {
		t.Fatalf("viewer overview: %v", err)
	}
	if _, err := dash.ExportReport(ctx, viewerSession); !errors.Is(err, ErrPartnerForbidden) {
		t.Fatalf("viewer exported a report: %v", err)
	}
	if got, err := dash.ReportArtifact(ctx, viewerSession, reportID); err != nil || !bytes.Equal(got, pdf) {
		t.Fatalf("viewer could not read a retained export: %v", err)
	}
}

func TestPartnerReportRendersAnyNameAndPaginatesLongSpend(t *testing.T) {
	now := time.Date(2027, 3, 4, 5, 6, 0, 0, time.UTC)
	enrolled := int64(7)
	overview := PartnerOverview{
		Organization:      "Ünïcode Fondation 日本語 " + strings.Repeat("Long Name ", 20),
		CommittedVerified: true, CommittedMinor: 150000,
		DeployedVerified: true, DeployedMinor: 50000, DeployedPercent: 33,
		PeopleReachedVerified: true, PeopleReached: 200,
		EmployeesEnrolled: &enrolled, WorkforceSize: 70,
	}
	spend := make([]queries.PartnerSpendByCategoryRow, 60)
	for i := range spend {
		spend[i] = queries.PartnerSpendByCategoryRow{
			Category:    fmt.Sprintf("Category %d — %s", i, strings.Repeat("wrapped ", 25)),
			AmountMinor: int64(i+1) * 100,
		}
	}
	logo, err := cleanLogo(partnerTestLogo(t, 64, 16))
	if err != nil {
		t.Fatal(err)
	}
	pdf, err := renderPartnerReport(overview, spend, logo, now)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF-")) || bytes.Count(pdf, []byte("/Type /Page\n")) < 2 {
		t.Fatalf("report is not a multi-page PDF: %d bytes, %d pages", len(pdf), bytes.Count(pdf, []byte("/Type /Page\n")))
	}
}

// TestPartnerProfileEditedByAdminsOnly covers the §7.2 organization profile.
func TestPartnerProfileEditedByAdminsOnly(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	apps := PartnerApplicationService{DB: pool, Staff: &StaffService{DB: as(t, dbroles.API)}}
	dash := PartnerDashboardService{PartnerPortalService{DB: as(t, dbroles.API)}}
	_, superToken := staffFixture(t, pool, "SUPER_ADMIN")
	cleanPartners(t, pool)
	appID := testID(t)
	if err := apps.Apply(ctx, appID, validApplication("Zeta Ltd", "cfo@zeta.test")); err != nil {
		t.Fatal(err)
	}
	partnerID, err := apps.Approve(ctx, superToken, appID)
	if err != nil {
		t.Fatal(err)
	}
	var adminID string
	if err := pool.QueryRow(ctx, "SELECT id FROM donor_partner_users WHERE partner_id=$1 AND role='ADMIN'", partnerID).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	viewerID := testID(t)
	mustExec(
		t,
		pool,
		"INSERT INTO donor_partner_users (id, partner_id, email, name, role) VALUES ($1,$2,'viewer@zeta.test','Viewer','VIEWER')",
		viewerID,
		partnerID,
	)
	admin := issueTestPartnerSession(t, pool, adminID)
	viewer := issueTestPartnerSession(t, pool, viewerID.String())

	if err := dash.SaveProfile(ctx, admin, "  Zeta Group ", " Clean water in three counties. "); err != nil {
		t.Fatal(err)
	}
	ov, err := dash.Overview(ctx, viewer)
	if err != nil || ov.Organization != "Zeta Group" || ov.Description != "Clean water in three counties." {
		t.Fatalf("saved profile reads back as %q / %q (%v)", ov.Organization, ov.Description, err)
	}
	if ov.UserID != viewerID.String() {
		t.Fatalf("overview user id %q, want the caller's own %q", ov.UserID, viewerID.String())
	}
	if ov.CommittedVerified || ov.DeployedVerified || ov.PeopleReachedVerified {
		t.Fatalf("unknown figures were labeled verified: %+v", ov)
	}
	if err := dash.SaveProfile(ctx, viewer, "Viewer Co", ""); !errors.Is(err, ErrPartnerForbidden) {
		t.Fatalf("a viewer edited the profile: %v", err)
	}
	for _, name := range []string{" ", strings.Repeat("n", 201)} {
		if err := dash.SaveProfile(ctx, admin, name, ""); !errors.Is(err, ErrPartnerApplication) {
			t.Fatalf("name %q accepted: %v", name, err)
		}
	}
	if err := dash.SaveProfile(ctx, admin, "Zeta", strings.Repeat("d", 4001)); !errors.Is(err, ErrPartnerApplication) {
		t.Fatalf("an over-long description was accepted: %v", err)
	}
	logo := partnerTestLogo(t, 32, 16)
	if err := dash.SaveLogo(ctx, viewer, logo); !errors.Is(err, ErrPartnerForbidden) {
		t.Fatalf("viewer changed logo: %v", err)
	}
	for _, invalid := range []string{"data:image/svg+xml;base64,PHN2Zz4=", "data:image/png;base64,bad", partnerTestLogo(t, 513, 1), strings.Repeat("a", 350001)} {
		if err := dash.SaveLogo(ctx, admin, invalid); !errors.Is(err, ErrPartnerApplication) {
			t.Fatalf("unsafe logo accepted: %v", err)
		}
	}
	if err := dash.SaveLogo(ctx, admin, logo); err != nil {
		t.Fatal(err)
	}
	stored, err := dash.Logo(ctx, viewer)
	if err != nil {
		t.Fatal(err)
	}
	config, err := png.DecodeConfig(bytes.NewReader(stored))
	if err != nil || config.Width != 32 || config.Height != 16 {
		t.Fatalf("logo round trip: %+v %v", config, err)
	}
	if err := dash.SaveLogo(ctx, admin, ""); err != nil {
		t.Fatal(err)
	}
	if got, err := dash.Logo(ctx, viewer); err != nil || len(got) != 0 {
		t.Fatalf("logo removal failed: %v", err)
	}
}

func partnerTestLogo(t *testing.T, width, height int) string {
	t.Helper()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(data.Bytes())
}

// TestStaffSetPartnerLogoAtEnrollment covers staff adding an approved
// partner's logo during onboarding, with the same validation as the portal.
func TestStaffSetPartnerLogoAtEnrollment(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	_, superToken := staffFixture(t, pool, "SUPER_ADMIN")
	_, growthToken := staffFixture(t, pool, "OP_GROWTH")
	cleanPartners(t, pool)
	svc := PartnerApplicationService{DB: pool, Staff: &StaffService{DB: as(t, dbroles.API)}}
	partner := approvedPartner(t, pool, "Logo Org", "LOGOORG", 0, 0)
	logo := partnerTestLogo(t, 32, 16)

	if err := svc.SetLogo(ctx, growthToken, partner, logo); !errors.Is(err, ErrStaffForbidden) {
		t.Fatalf("an operator without partnerships access set a logo: %v", err)
	}
	if err := svc.SetLogo(ctx, superToken, partner, "data:image/svg+xml;base64,PHN2Zz4="); !errors.Is(err, ErrPartnerApplication) {
		t.Fatalf("an SVG logo: %v; want ErrPartnerApplication", err)
	}
	if err := svc.SetLogo(ctx, superToken, testID(t), logo); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a logo for no partner: %v; want ErrNoRows", err)
	}
	if err := svc.SetLogo(ctx, superToken, partner, logo); err != nil {
		t.Fatal(err)
	}
	stored, err := svc.Logo(ctx, superToken, partner)
	if err != nil {
		t.Fatal(err)
	}
	if _, format, err := image.DecodeConfig(bytes.NewReader(stored)); err != nil || format != "png" {
		t.Fatalf("stored logo is %q: %v", format, err)
	}
	detail, err := svc.Partner(ctx, superToken, partner)
	if err != nil || !detail.HasLogo {
		t.Fatalf("detail after upload: has_logo=%v err=%v", detail.HasLogo, err)
	}
	if err := svc.SetLogo(ctx, superToken, partner, ""); err != nil {
		t.Fatal(err)
	}
	if stored, err = svc.Logo(ctx, superToken, partner); err != nil || len(stored) != 0 {
		t.Fatalf("logo after removal: %d bytes, err %v", len(stored), err)
	}
	var audits int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_staff_audit WHERE target=$1 AND action IN ('partner.logo_set','partner.logo_removed')", partner.String()).Scan(&audits); err != nil ||
		audits != 2 {
		t.Fatalf("%d logo audits, err %v; want 2", audits, err)
	}
}
