package store

import (
	"context"
	"errors"
	"testing"

	"dad/donor-api/internal/dbroles"
)

// TestOneEmailOneRole covers PRD §3: an address signs in to one kind of
// account, and a partner login belongs to one organization.
func TestOneEmailOneRole(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	_, superToken := staffFixture(t, pool, "SUPER_ADMIN")
	staffID, _ := staffFixture(t, pool, "OP_GROWTH")
	cleanPartners(t, pool)
	partners := PartnerApplicationService{DB: pool, Staff: &StaffService{DB: as(t, dbroles.API)}}
	first := approvedPartner(t, pool, "First Org", "FIRSTONE", 0, 0)
	second := approvedPartner(t, pool, "Second Org", "SECONDONE", 0, 0)
	_, donorEmail := donor(t, pool)
	staffEmail := staffID.String() + "@example.org"
	partnerEmail := "giving-" + staffID.String() + "@first.test"

	if err := partners.InviteUser(ctx, superToken, first, partnerEmail, "Lee", "VIEWER"); err != nil {
		t.Fatal(err)
	}
	if err := partners.InviteUser(ctx, superToken, first, partnerEmail, "Lee", "ADMIN"); err != nil {
		t.Fatalf("re-inviting a login at its own organization: %v", err)
	}
	refused := func(name string, err error, role string) {
		t.Helper()
		var held EmailRoleError
		if !errors.As(err, &held) || held.Role != role {
			t.Errorf("%s: %v; want the address held by a %s account", name, err, role)
		}
	}
	for _, tc := range []struct{ email, role string }{
		{partnerEmail, identityPartner}, {donorEmail, identityDonor}, {staffEmail, identityStaff},
	} {
		refused("partner login for "+tc.role, partners.InviteUser(ctx, superToken, second, tc.email, "Lee", "VIEWER"), tc.role)
	}

	staff := &StaffService{DB: as(t, dbroles.API)}
	for _, tc := range []struct{ email, role string }{{donorEmail, identityDonor}, {partnerEmail, identityPartner}} {
		_, err := staff.Invite(ctx, superToken, tc.email, "New Person", "OP_GROWTH")
		refused("staff login for "+tc.role, err, tc.role)
	}

	checkout, request, _, _ := checkoutFixture(t)
	for _, tc := range []struct{ email, role string }{{partnerEmail, identityPartner}, {staffEmail, identityStaff}} {
		request.RequestID = testID(t)
		request.Email = tc.email
		_, err := checkout.BeginCheckout(ctx, request)
		if !errors.Is(err, ErrDenied) {
			t.Fatalf("giving as another identity: %v; want ErrDenied", err)
		}
	}
	var donors int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donors WHERE email IN ($1, $2)", partnerEmail, staffEmail).Scan(&donors); err != nil ||
		donors != 0 {
		t.Fatalf("%d donor accounts created for partner and staff addresses, err %v", donors, err)
	}

	changes, _, _, _, session, _ := changeFixture(t)
	if err := changes.Request(ctx, session, staffEmail); !errors.Is(err, ErrEmailChangeConflict) {
		t.Fatalf("moving a donor account to a staff address: %v; want ErrEmailChangeConflict", err)
	}
}
