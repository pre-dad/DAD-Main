package store

import (
	"context"
	"errors"
	"testing"

	"dad/donor-api/internal/dbroles"
	"github.com/jackc/pgx/v5/pgxpool"
)

func cleanPartners(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		for _, s := range []string{
			"DELETE FROM donor_partner_reports",
			"DELETE FROM donor_partner_login_emails",
			"DELETE FROM donor_partner_messages",
			"DELETE FROM donor_partner_users",
			"UPDATE donor_initiatives SET partner_signup_id=NULL WHERE partner_signup_id IS NOT NULL",
			"DELETE FROM donor_partner_applications",
			"DELETE FROM donor_partner_signups s WHERE approved_at IS NOT NULL" +
				" AND NOT EXISTS (SELECT 1 FROM donor_partner_employees e WHERE e.partner_id=s.id)",
		} {
			if _, err := pool.Exec(ctx, s); err != nil {
				t.Errorf("%s: %v", s, err)
			}
		}
	})
}

func validApplication(org, email string) PartnerApplicationInput {
	return PartnerApplicationInput{
		Organization: org, Website: "https://example.org", Type: "Corporate",
		ContactName: "Dana Partner", Title: "Head of Giving", WorkEmail: email,
		Phone: "+14155550123", WorkforceSize: 500,
	}
}

func TestPartnerApplicationRetryPreservesTheSubmittedFields(t *testing.T) {
	pool := database(t)
	cleanPartners(t, pool)
	svc := PartnerApplicationService{DB: as(t, dbroles.API)}
	ctx := context.Background()
	id := testID(t)
	original := validApplication("Retry Corp", "retry@partner.test")
	if err := svc.Apply(ctx, id, original); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*PartnerApplicationInput){
		"organization": func(in *PartnerApplicationInput) { in.Organization = "Another organization" },
		"website":      func(in *PartnerApplicationInput) { in.Website = "https://another.example.org" },
		"type":         func(in *PartnerApplicationInput) { in.Type = "Foundation" },
		"contact":      func(in *PartnerApplicationInput) { in.ContactName = "Another contact" },
		"title":        func(in *PartnerApplicationInput) { in.Title = "Another title" },
		"email":        func(in *PartnerApplicationInput) { in.WorkEmail = "another@partner.test" },
		"phone":        func(in *PartnerApplicationInput) { in.Phone = "+14155550124" },
		"workforce":    func(in *PartnerApplicationInput) { in.WorkforceSize++ },
	} {
		t.Run(name, func(t *testing.T) {
			changed := original
			change(&changed)
			if err := svc.Apply(ctx, id, changed); !errors.Is(err, ErrPartnerApplication) {
				t.Fatalf("changed %s acknowledged as the original application: %v", name, err)
			}
		})
	}
	original.Organization = " Retry Corp "
	original.WorkEmail = "RETRY@partner.test"
	if err := svc.Apply(ctx, id, original); err != nil {
		t.Fatalf("equivalent normalized retry: %v", err)
	}
}

func TestPartnerApplicationApprovalStateMachine(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	svc := PartnerApplicationService{DB: pool, Staff: &StaffService{DB: as(t, dbroles.API)}}
	_, superToken := staffFixture(t, pool, "SUPER_ADMIN")
	_, growthToken := staffFixture(t, pool, "OP_GROWTH")
	// Registered AFTER the staff fixtures so (LIFO) it runs BEFORE their
	// deletion, clearing donor_partner_applications.decided_by's FK first.
	cleanPartners(t, pool)

	// Apply (public) creates a PENDING application; a bad website is refused.
	bad := validApplication("Acme", "bad@acme.test")
	bad.Website = "http://insecure.example"
	if err := svc.Apply(ctx, testID(t), bad); !errors.Is(err, ErrPartnerApplication) {
		t.Fatalf("insecure website accepted: %v", err)
	}
	appID := testID(t)
	if err := svc.Apply(ctx, appID, validApplication("Acme Corp", "giving@acme.test")); err != nil {
		t.Fatal(err)
	}
	// Idempotent retry with the same id.
	if err := svc.Apply(ctx, appID, validApplication("Acme Corp", "giving@acme.test")); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	// A second PENDING application for the same work email is refused.
	if err := svc.Apply(ctx, testID(t), validApplication("Acme Corp", "giving@acme.test")); !errors.Is(err, ErrPartnerApplication) {
		t.Fatalf("duplicate pending accepted: %v", err)
	}

	// An operator WITHOUT partner.manage cannot decide (OP_GROWTH lacks it).
	if _, err := svc.Approve(ctx, growthToken, appID); !errors.Is(err, ErrStaffForbidden) {
		t.Fatalf("OP_GROWTH approved a partner: %v", err)
	}

	// Super admin approves -> partner org created, signer is first ADMIN.
	partnerID, err := svc.Approve(ctx, superToken, appID)
	if err != nil {
		t.Fatal(err)
	}
	var orgApproved bool
	if err := pool.QueryRow(ctx, "SELECT approved_at IS NOT NULL FROM donor_partner_signups WHERE id=$1", partnerID).Scan(&orgApproved); err != nil ||
		!orgApproved {
		t.Fatalf("partner org not approved: %v", err)
	}
	var adminEmail, adminRole string
	if err := pool.QueryRow(ctx, "SELECT email, role FROM donor_partner_users WHERE partner_id=$1", partnerID).Scan(&adminEmail, &adminRole); err != nil ||
		adminEmail != "giving@acme.test" ||
		adminRole != "ADMIN" {
		t.Fatalf("signer not made partner admin: %q %q %v", adminEmail, adminRole, err)
	}

	// Re-deciding an already-approved application is refused.
	if _, err := svc.Approve(ctx, superToken, appID); !errors.Is(err, ErrPartnerDecision) {
		t.Fatalf("re-approve accepted: %v", err)
	}

	// After rejection a partner may re-apply (the pending uniqueness is released).
	rejID := testID(t)
	if err := svc.Apply(ctx, rejID, validApplication("Beta LLC", "ops@beta.test")); err != nil {
		t.Fatal(err)
	}
	if err := svc.Reject(ctx, superToken, rejID, "incomplete details"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Apply(ctx, testID(t), validApplication("Beta LLC", "ops@beta.test")); err != nil {
		t.Fatalf("re-apply after rejection refused: %v", err)
	}

	// SetMatch + LinkInitiative require an approved partner.
	if err := svc.SetMatch(ctx, superToken, partnerID, 600, 5000, 1_000_00); err != nil {
		t.Fatal(err)
	}
	var rate int32
	if err := pool.QueryRow(ctx, "SELECT match_rate_bp FROM donor_partner_signups WHERE id=$1", partnerID).Scan(&rate); err != nil ||
		rate != 5000 {
		t.Fatalf("match rate not set: %d %v", rate, err)
	}
	if err := svc.SetMatch(ctx, superToken, partnerID, 600, 10001, 1_000_00); !errors.Is(err, ErrPartnerApplication) {
		t.Fatalf("match rate above 100%% accepted: %v", err)
	}
}
