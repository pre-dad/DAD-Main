package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"github.com/jackc/pgx/v5"
)

// reinviteFixture approves a partner, signs its admin in, and invites a viewer
// whose invitation email has been delivered but not accepted.
type reinviteFixture struct {
	apps     PartnerApplicationService
	portal   PartnerPortalService
	partner  string
	admin    string
	viewerID string
	invite   string
	staff    string
}

func newReinviteFixture(t *testing.T, org, domain string) reinviteFixture {
	t.Helper()
	pool := database(t)
	ctx := context.Background()
	f := reinviteFixture{
		apps:   PartnerApplicationService{DB: pool, Staff: &StaffService{DB: as(t, dbroles.API)}},
		portal: PartnerPortalService{DB: as(t, dbroles.API)},
	}
	_, f.staff = staffFixture(t, pool, "SUPER_ADMIN")
	cleanPartners(t, pool)
	mustExec(t, pool, "DELETE FROM donor_partner_login_emails")

	appID := testID(t)
	if err := f.apps.Apply(ctx, appID, validApplication(org, "lead@"+domain)); err != nil {
		t.Fatal(err)
	}
	partnerID, err := f.apps.Approve(ctx, f.staff, appID)
	if err != nil {
		t.Fatal(err)
	}
	f.partner = partnerID.String()
	if f.admin, err = f.portal.AcceptInvite(ctx, deliverPartnerLinks(t, time.Now().UTC())["lead@"+domain], false); err != nil {
		t.Fatal(err)
	}
	if err := f.portal.Invite(ctx, f.admin, "viewer@"+domain, "Val Viewer", "VIEWER"); err != nil {
		t.Fatal(err)
	}
	f.invite = deliverPartnerLinks(t, time.Now().UTC())["viewer@"+domain]
	if f.invite == "" {
		t.Fatal("invitation was not delivered")
	}
	if err := pool.QueryRow(ctx, "SELECT id FROM donor_partner_users WHERE email=$1", "viewer@"+domain).Scan(&f.viewerID); err != nil {
		t.Fatal(err)
	}
	return f
}

// ageInvites moves the user's queued emails back so the resend throttle has
// elapsed.
func ageInvites(t *testing.T, userID string, by time.Duration) {
	t.Helper()
	mustExec(t, database(t), "UPDATE donor_partner_login_emails SET created_at=created_at-$2::interval WHERE partner_user_id=$1",
		mustUUID(t, userID), by.String())
}

// A public sign-in request names only an email, so it must never touch the
// live link of a user who has not accepted their invitation.
func TestPartnerSignInRequestLeavesUnacceptedInviteIntact(t *testing.T) {
	f := newReinviteFixture(t, "Reinvite Public Co", "public.test")
	ctx := context.Background()

	if err := f.portal.RequestSignIn(ctx, "viewer@public.test"); err != nil {
		t.Fatal(err)
	}
	if sent := deliverPartnerLinks(t, time.Now().UTC()); len(sent) != 0 {
		t.Fatalf("a public request mailed an unaccepted user: %v", sent)
	}
	var queued int
	if err := database(t).QueryRow(ctx, "SELECT count(*) FROM donor_partner_login_emails WHERE kind='SIGN_IN'").Scan(&queued); err != nil ||
		queued != 0 {
		t.Fatalf("a public request queued %d sign-in emails for an unaccepted user: %v", queued, err)
	}
	if _, err := f.portal.AcceptInvite(ctx, f.invite, false); err != nil {
		t.Fatalf("the invitation was invalidated by a public request: %v", err)
	}
	if err := f.portal.RequestSignIn(ctx, "viewer@public.test"); err != nil {
		t.Fatal(err)
	}
	if sent := deliverPartnerLinks(t, time.Now().UTC()); sent["viewer@public.test"] == "" {
		t.Fatalf("an accepted user cannot request a sign-in link: %v", sent)
	}
}

// A sign-in email queued before the restriction existed must not replace an
// unaccepted user's invitation when it is delivered.
func TestPartnerSignInDeliveryDropsUnacceptedRecipient(t *testing.T) {
	f := newReinviteFixture(t, "Reinvite Queue Co", "queue.test")
	ctx := context.Background()
	mustExec(t, database(t), `INSERT INTO donor_partner_login_emails (id, partner_user_id, kind, available_at, created_at)
		VALUES ($1,$2,'SIGN_IN',now(),now())`, mustNewID(t), mustUUID(t, f.viewerID))
	if sent := deliverPartnerLinks(t, time.Now().UTC()); len(sent) != 0 {
		t.Fatalf("a queued sign-in email reached an unaccepted user: %v", sent)
	}
	if _, err := f.portal.AcceptInvite(ctx, f.invite, false); err != nil {
		t.Fatalf("the invitation was replaced by a queued sign-in email: %v", err)
	}
}

func TestPartnerAdminReinvitesUnacceptedUser(t *testing.T) {
	f := newReinviteFixture(t, "Reinvite Admin Co", "admin.test")
	pool := database(t)
	ctx := context.Background()
	viewer := mustUUID(t, f.viewerID)

	if err := f.portal.Reinvite(ctx, f.admin, viewer); !errors.Is(err, ErrPartnerInviteRate) {
		t.Fatalf("an immediate resend was allowed: %v", err)
	}
	var live bool
	if err := pool.QueryRow(ctx, "SELECT invite_token_hash=$2 FROM donor_partner_users WHERE id=$1", viewer, HashToken(f.invite)).
		Scan(&live); err != nil || !live {
		t.Fatalf("a refused resend replaced the live invitation: %v %v", live, err)
	}
	mustExec(t, pool, "UPDATE donor_partner_users SET invite_expires_at=now()-interval '1 day' WHERE id=$1", viewer)
	ageInvites(t, f.viewerID, 2*time.Minute)

	if err := f.portal.Reinvite(ctx, f.admin, viewer); err != nil {
		t.Fatalf("resend: %v", err)
	}
	if _, err := f.portal.AcceptInvite(ctx, f.invite, false); !errors.Is(err, ErrPartnerInvite) {
		t.Fatalf("the old invitation still works after a resend: %v", err)
	}
	fresh := deliverPartnerLinks(t, time.Now().UTC())["viewer@admin.test"]
	if fresh == "" || fresh == f.invite {
		t.Fatalf("resend delivered %q, want a new link", fresh)
	}
	var days float64
	if err := pool.QueryRow(ctx, "SELECT extract(epoch FROM invite_expires_at-now())/86400 FROM donor_partner_users WHERE id=$1", viewer).
		Scan(&days); err != nil || days < 6.9 || days > 7.1 {
		t.Fatalf("resent link lives %.2f days (%v), want 7", days, err)
	}
	if _, err := f.portal.AcceptInvite(ctx, fresh, false); err != nil {
		t.Fatalf("the resent invitation did not sign the user in: %v", err)
	}
	if err := f.portal.Reinvite(ctx, f.admin, viewer); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("resent to a user who already accepted: %v", err)
	}
}

func TestPartnerReinviteDropsQueuedEmailsAndRespectsLimits(t *testing.T) {
	f := newReinviteFixture(t, "Reinvite Limits Co", "limits.test")
	pool := database(t)
	ctx := context.Background()
	viewer := mustUUID(t, f.viewerID)
	mustExec(t, pool, "UPDATE donor_partner_users SET accepted_at=NULL WHERE id=$1", viewer)

	// An invitation still waiting to be mailed is dropped by the resend, so only
	// the new one is delivered.
	if err := f.portal.Invite(ctx, f.admin, "viewer@limits.test", "Val Viewer", "VIEWER"); err != nil {
		t.Fatal(err)
	}
	ageInvites(t, f.viewerID, 2*time.Minute)
	if err := f.portal.Reinvite(ctx, f.admin, viewer); err != nil {
		t.Fatal(err)
	}
	if sent := deliverPartnerLinks(t, time.Now().UTC()); len(sent) != 1 || sent["viewer@limits.test"] == "" {
		t.Fatalf("delivered %v, want exactly one email", sent)
	}

	// Five invitations in an hour is the cap, however far apart they were.
	mustExec(t, pool, "UPDATE donor_partner_login_emails SET created_at=now()-interval '30 minutes' WHERE partner_user_id=$1", viewer)
	for range partnerReinvitesPerHour {
		mustExec(t, pool, `INSERT INTO donor_partner_login_emails (id, partner_user_id, kind, available_at, created_at, completed_at)
			VALUES ($1,$2,'INVITED',now(),now()-interval '10 minutes',now())`, mustNewID(t), viewer)
	}
	if err := f.portal.Reinvite(ctx, f.admin, viewer); !errors.Is(err, ErrPartnerInviteRate) {
		t.Fatalf("hourly limit not enforced: %v", err)
	}
	mustExec(t, pool, "UPDATE donor_partner_login_emails SET created_at=now()-interval '2 hours' WHERE partner_user_id=$1", viewer)
	if err := f.portal.Reinvite(ctx, f.admin, viewer); err != nil {
		t.Fatalf("resend after the hour: %v", err)
	}
}

func TestPartnerReinviteAuthorization(t *testing.T) {
	f := newReinviteFixture(t, "Reinvite Authz Co", "authz.test")
	pool := database(t)
	ctx := context.Background()
	viewer := mustUUID(t, f.viewerID)
	ageInvites(t, f.viewerID, 2*time.Minute)

	if err := f.portal.Reinvite(ctx, "no-such-session", viewer); !errors.Is(err, ErrPartnerDenied) {
		t.Fatalf("signed-out resend: %v", err)
	}
	other := approvedPartner(t, pool, "Other org", "OTHERREINV", 0, 0)
	_, otherAdmin := partnerPushSession(t, pool, other, false)
	if err := f.portal.Reinvite(ctx, otherAdmin, viewer); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("an admin of another organization resent this invitation: %v", err)
	}
	viewerSession := issueTestPartnerSession(t, pool, f.viewerID)
	if err := f.portal.Reinvite(ctx, viewerSession, viewer); !errors.Is(err, ErrPartnerForbidden) {
		t.Fatalf("a viewer resent an invitation: %v", err)
	}
	mustExec(t, pool, "UPDATE donor_partner_users SET disabled=true WHERE id=$1", viewer)
	if err := f.portal.Reinvite(ctx, f.admin, viewer); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("resent to a removed user: %v", err)
	}
	if err := f.portal.Reinvite(ctx, f.admin, mustNewID(t)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("resent to an unknown user: %v", err)
	}
}

func TestStaffReinvitesPartnerUser(t *testing.T) {
	f := newReinviteFixture(t, "Reinvite Staff Co", "staff.test")
	pool := database(t)
	ctx := context.Background()
	partner, viewer := mustUUID(t, f.partner), mustUUID(t, f.viewerID)
	_, growth := staffFixture(t, pool, "OP_GROWTH")
	ageInvites(t, f.viewerID, 2*time.Minute)

	if err := f.apps.ReinviteUser(ctx, "no-such-session", partner, viewer); err == nil {
		t.Fatal("resend without a staff session")
	}
	if err := f.apps.ReinviteUser(ctx, growth, partner, viewer); err == nil {
		t.Fatal("resend by staff without partner.manage")
	}
	if err := f.apps.ReinviteUser(ctx, f.staff, partner, mustNewID(t)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("resent to an unknown user: %v", err)
	}
	other := approvedPartner(t, pool, "Other staff org", "OTHERSTAFF", 0, 0)
	if err := f.apps.ReinviteUser(ctx, f.staff, other, viewer); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("resent through another organization: %v", err)
	}
	if err := f.apps.ReinviteUser(ctx, f.staff, partner, viewer); err != nil {
		t.Fatal(err)
	}
	if err := f.apps.ReinviteUser(ctx, f.staff, partner, viewer); !errors.Is(err, ErrPartnerInviteRate) {
		t.Fatalf("an immediate second resend was allowed: %v", err)
	}
	if _, err := f.portal.AcceptInvite(ctx, f.invite, false); !errors.Is(err, ErrPartnerInvite) {
		t.Fatalf("the old invitation survived a staff resend: %v", err)
	}
	if fresh := deliverPartnerLinks(t, time.Now().UTC())["viewer@staff.test"]; fresh == "" {
		t.Fatal("staff resend did not email a new link")
	}
	var audited int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_staff_audit WHERE action='partner.user_reinvited' AND target=$1", f.partner).
		Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("audited %d resends: %v", audited, err)
	}
}
