package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func staffFixture(t *testing.T, pool *pgxpool.Pool, role string) (pgtype.UUID, string) {
	t.Helper()
	id, session := testID(t), testID(t)
	token := "staff-test-" + session.String()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "INSERT INTO donor_staff(id,email,name,role) VALUES($1,$2,'Test staff',$3)", id, id.String()+"@example.org", role); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, sql := range []string{"DELETE FROM donor_staff_audit WHERE actor_id=$1", "DELETE FROM donor_staff_sessions WHERE staff_id=$1", "DELETE FROM donor_staff WHERE id=$1"} {
			if _, err := pool.Exec(ctx, sql, id); err != nil {
				t.Error(err)
			}
		}
	})
	if _, err := pool.Exec(ctx, "INSERT INTO donor_staff_sessions(id,staff_id,token_hash,expires_at) VALUES($1,$2,$3,$4)", session, id, HashToken(token), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return id, token
}

func TestStaffWriteHoldsActorLockAndRechecksRevocation(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	id, token := staffFixture(t, pool, "SUPER_ADMIN")
	s := &StaffService{DB: api}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := staffWrite(ctx, s, token, "content.publish", func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (bool, error) {
		probe, err := pool.Begin(ctx)
		if err != nil {
			return false, err
		}
		defer probe.Rollback(ctx)
		var locked pgtype.UUID
		err = probe.QueryRow(ctx, "SELECT id FROM donor_staff WHERE id=$1 FOR UPDATE SKIP LOCKED", id).Scan(&locked)
		if !errors.Is(err, pgx.ErrNoRows) {
			return false, errors.New("staff write did not lock its actor")
		}
		return true, auditStaff(ctx, queries.New(tx), actor.ID, "test.mutation", id.String(), nil, now)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SignOut(ctx, token); err != nil {
		t.Fatal(err)
	}
	_, err = staffWrite(ctx, s, token, "content.publish", func(pgx.Tx, queries.DonorStaff, time.Time) (bool, error) {
		t.Error("revoked session reached the write")
		return true, nil
	})
	if !errors.Is(err, ErrStaffDenied) {
		t.Fatalf("revoked session: got %v, want ErrStaffDenied", err)
	}
}

func TestStaffPermissionMatrixAndExpiredSessions(t *testing.T) {
	pool := database(t)
	ctx := context.Background()

	// Every permission any role carries, plus one that no role has, so the
	// matrix also proves unknown permissions are denied.
	allPermissions := []string{
		"audit.read", "badge.manage", "content.publish", "data.export", "donor.read", "donor.recompute",
		"event.manage", "gift.cancel", "gift.refund", "initiative.manage", "message.resend",
		"partner.manage", "report.read", "staff.manage",
		"nonexistent.permission",
	}
	// Independent oracle: the permissions each role is expected to hold, spelled
	// out here rather than read back from staffPermissions, so a mistake in the
	// production catalog (rolePermissions) is caught instead of moving both sides
	// of the assertion together. Keep in sync with PRD §8 deliberately, by hand.
	wantPermissions := map[string]map[string]bool{
		"SUPER_ADMIN": {
			"audit.read": true, "badge.manage": true, "content.publish": true, "data.export": true,
			"donor.read": true, "donor.recompute": true, "event.manage": true, "gift.cancel": true, "gift.refund": true,
			"initiative.manage": true, "message.resend": true, "partner.manage": true,
			"report.read": true, "staff.manage": true,
		},
		"OP_PARTNERSHIPS": {"partner.manage": true, "report.read": true},
		"OP_GROWTH":       {"event.manage": true, "report.read": true},
		"OP_IMPACT":       {"initiative.manage": true, "report.read": true},
	}
	// super-admin-only powers (PRD §8): no operator may ever hold these.
	superAdminOnly := map[string]bool{
		"gift.refund": true, "gift.cancel": true, "data.export": true,
		"badge.manage": true, "staff.manage": true, "donor.recompute": true,
	}

	for _, role := range []string{"SUPER_ADMIN", "OP_PARTNERSHIPS", "OP_GROWTH", "OP_IMPACT"} {
		id, token := staffFixture(t, pool, role)
		for _, permission := range allPermissions {
			allowed := wantPermissions[role][permission]
			// §8 invariant: an operator never holds a super-admin-only power.
			if role != superAdminRole && superAdminOnly[permission] && allowed {
				t.Fatalf("%s must not hold super-admin-only permission %s", role, permission)
			}
			_, err := requireStaff(ctx, queries.New(pool), token, permission, time.Now())
			if (allowed && err != nil) || (!allowed && !errors.Is(err, ErrStaffForbidden)) {
				t.Fatalf("%s / %s: allowed=%v err=%v", role, permission, allowed, err)
			}
		}
		if _, err := requireStaff(ctx, queries.New(pool), token, "donor.read", time.Now().Add(2*time.Hour)); !errors.Is(
			err,
			ErrStaffDenied,
		) {
			t.Fatal("expired session accepted", err)
		}
		if _, err := pool.Exec(ctx, "UPDATE donor_staff SET disabled=true WHERE id=$1", id); err != nil {
			t.Fatal(err)
		}
		if _, err := requireStaff(ctx, queries.New(pool), token, "donor.read", time.Now()); !errors.Is(err, ErrStaffDenied) {
			t.Fatal("disabled staff accepted", err)
		}
	}
}

func TestRecoveryReplacesPasskeysAndRevokesSessions(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	s := staffService(t, dbroles.API)
	_, adminKey := activatedStaff(t, pool, s, "SUPER_ADMIN")
	admin, err := signIn(t, s, adminKey)
	if err != nil {
		t.Fatal(err)
	}
	target, targetKey := activatedStaff(t, pool, s, "SUPER_ADMIN")
	session, err := signIn(t, s, targetKey)
	if err != nil {
		t.Fatal(err)
	}
	change, err := s.ChangeAccess(ctx, admin, target, "recovery", "")
	if err != nil || change.Token == "" {
		t.Fatal("recovery invitation not issued", err)
	}
	if _, err = s.Me(ctx, session); !errors.Is(err, ErrStaffDenied) {
		t.Fatal("recovery left a session usable", err)
	}
	if _, err = signIn(t, s, targetKey); !errors.Is(err, ErrStaffDenied) {
		t.Fatal("recovery left a passkey usable", err)
	}
	accounts, err := s.Accounts(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range accounts {
		if account.ID == target.String() && account.Active {
			t.Fatal("recovered account still reported active")
		}
	}
	var details string
	if err = pool.QueryRow(ctx, "SELECT details::text FROM donor_staff_audit WHERE target=$1 AND action='staff.recovery_invited'", target.String()).Scan(&details); err != nil ||
		details != `{"previously_active":true,"previously_disabled":false}` {
		t.Fatal("recovery audit missing", details, err)
	}
	invitation, err := s.BeginActivation(ctx, change.Token)
	if err != nil {
		t.Fatal(err)
	}
	replacement := newAuthenticator(t, staffTestOrigin, staffTestRPID)
	if _, err = s.Activate(ctx, invitation.ID, replacement.register(t, invitation.Options)); err != nil {
		t.Fatal(err)
	}
	if _, err = signIn(t, s, replacement); err != nil {
		t.Fatal("replacement passkey rejected", err)
	}
}

func TestAdministratorInvitesStaffWhoAreInactiveUntilTheyEnrol(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	_, admin := staffFixture(t, pool, "SUPER_ADMIN")
	email := testID(t).String() + "@example.org"
	t.Cleanup(func() {
		for _, sql := range []string{
			"DELETE FROM donor_staff_audit WHERE target IN (SELECT id::text FROM donor_staff WHERE email=$1)",
			"DELETE FROM donor_staff_ceremonies WHERE invite_id IN (SELECT i.id FROM donor_staff_invites i JOIN donor_staff s ON s.id=i.staff_id WHERE s.email=$1)",
			"DELETE FROM donor_staff_invites WHERE staff_id IN (SELECT id FROM donor_staff WHERE email=$1)",
			"DELETE FROM donor_staff WHERE email=$1",
		} {
			mustExec(t, pool, sql, email)
		}
	})
	s := staffService(t, dbroles.API)
	token, err := s.Invite(ctx, admin, email, "New Person", "OP_PARTNERSHIPS")
	if err != nil || token == "" {
		t.Fatalf("invitation: %v", err)
	}
	accounts, err := s.Accounts(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	invited := slices.IndexFunc(accounts, func(a StaffView) bool { return a.Email == email })
	if invited < 0 || accounts[invited].Active || accounts[invited].Role != "OP_PARTNERSHIPS" {
		t.Fatalf("invited account missing or active: %+v", accounts)
	}
	if _, err = s.BeginActivation(ctx, token); err != nil {
		t.Fatalf("the invitation does not start enrolment: %v", err)
	}
}

func TestBreakGlassRecoveryReplacesOnlyAnEnabledAdministratorsPasskeys(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	s := staffService(t, dbroles.API)
	operator := staffService(t, dbroles.StaffCLI)
	admin, key := activatedStaff(t, pool, s, "SUPER_ADMIN")
	session, err := signIn(t, s, key)
	if err != nil {
		t.Fatal(err)
	}
	support, _ := invitedStaff(t, pool, "OP_PARTNERSHIPS")
	disabled, _ := activatedStaff(t, pool, s, "SUPER_ADMIN")
	mustExec(t, pool, "UPDATE donor_staff SET disabled=true WHERE id=$1", disabled)
	for _, other := range []string{support.String() + "@example.org", disabled.String() + "@example.org", "nobody@example.org"} {
		if _, err := operator.Recover(ctx, other); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("break-glass recovery of %s: got %v, want no such administrator", other, err)
		}
	}
	token, err := operator.Recover(ctx, admin.String()+"@example.org")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Me(ctx, session); !errors.Is(err, ErrStaffDenied) {
		t.Fatal("break-glass recovery left a session usable", err)
	}
	if _, err = signIn(t, s, key); !errors.Is(err, ErrStaffDenied) {
		t.Fatal("break-glass recovery left a passkey usable", err)
	}
	var actor pgtype.UUID
	if err = pool.QueryRow(ctx, "SELECT actor_id FROM donor_staff_audit WHERE target=$1 AND action='staff.recovery_invited'", admin.String()).Scan(&actor); err != nil ||
		actor.Valid {
		t.Fatal("break-glass recovery not audited as an operator action", err)
	}
	invitation, err := s.BeginActivation(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Activate(ctx, invitation.ID, key.register(t, invitation.Options)); err != nil {
		t.Fatal(err)
	}
}

// Operators act only in their own area (§8): Growth runs events, Impact Ops
// runs initiatives, Partnerships runs partners, and none of them reads donors.
func TestOperatorsActOnlyInTheirArea(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	ctx := context.Background()
	cleanEvents(t, pool)
	cleanInitiatives(t, pool)
	staff := &StaffService{DB: api}
	events := EventService{Staff: staff}
	initiatives := InitiativeService{Staff: staff}
	donorID, _ := donor(t, pool)
	area := map[string]struct{ event, initiative, donors bool }{
		"OP_GROWTH":       {event: true},
		"OP_IMPACT":       {initiative: true},
		"OP_PARTNERSHIPS": {},
		"SUPER_ADMIN":     {event: true, initiative: true, donors: true},
	}
	for role, may := range area {
		_, token := staffFixture(t, pool, role)
		slug := "area-" + strings.ReplaceAll(strings.ToLower(role), "_", "-")
		_, err := events.Create(ctx, token, eventInput(slug, "San Francisco", "94103", "VOLUNTEER", 48*time.Hour))
		if allowed := !errors.Is(err, ErrStaffForbidden); allowed != may.event || (may.event && err != nil) {
			t.Errorf("%s creating an event: %v", role, err)
		}
		_, err = initiatives.Create(ctx, token, validInitiative(slug))
		if allowed := !errors.Is(err, ErrStaffForbidden); allowed != may.initiative || (may.initiative && err != nil) {
			t.Errorf("%s creating an initiative: %v", role, err)
		}
		_, err = staff.DonorDetail(ctx, token, donorID, 0, "")
		if allowed := !errors.Is(err, ErrStaffForbidden); allowed != may.donors || (may.donors && err != nil) {
			t.Errorf("%s reading a donor: %v", role, err)
		}
	}
}
