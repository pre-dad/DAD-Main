package store

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/store/queries"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	staffTestOrigin = "https://staff.example.org"
	staffTestRPID   = "staff.example.org"
)

// staffService runs staff operations as role: the API, or the staff command
// for break-glass recovery.
func staffService(t *testing.T, role dbroles.Role) *StaffService {
	t.Helper()
	s, err := NewStaffService(as(t, role), staffTestOrigin)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// invitedStaff creates an account with an open invitation, removing every
// record its passkey journey leaves behind.
func invitedStaff(t *testing.T, pool *pgxpool.Pool, role string) (pgtype.UUID, string) {
	t.Helper()
	id, _ := staffFixture(t, pool, role)
	t.Cleanup(func() {
		ctx := context.Background()
		for _, sql := range []string{
			"DELETE FROM donor_staff_ceremonies WHERE invite_id IN (SELECT id FROM donor_staff_invites WHERE staff_id=$1)",
			"DELETE FROM donor_staff_invites WHERE staff_id=$1",
			"DELETE FROM donor_staff_passkeys WHERE staff_id=$1",
			"DELETE FROM donor_staff_audit WHERE target=$1::text",
		} {
			if _, err := pool.Exec(ctx, sql, id); err != nil {
				t.Error(err)
			}
		}
	})
	invite, err := createStaffInvite(context.Background(), queries.New(pool), id, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return id, invite
}

// activatedStaff enrols a software passkey for a new account.
func activatedStaff(t *testing.T, pool *pgxpool.Pool, s *StaffService, role string) (pgtype.UUID, *authenticator) {
	t.Helper()
	id, invite := invitedStaff(t, pool, role)
	key := newAuthenticator(t, staffTestOrigin, staffTestRPID)
	if _, err := activate(t, s, invite, key); err != nil {
		t.Fatal(err)
	}
	return id, key
}

// activate answers an invitation with the passkey.
func activate(t *testing.T, s *StaffService, invite string, key *authenticator) (string, error) {
	t.Helper()
	invitation, err := s.BeginActivation(context.Background(), invite)
	if err != nil {
		t.Fatal(err)
	}
	return s.Activate(context.Background(), invitation.ID, key.register(t, invitation.Options))
}

func signIn(t *testing.T, s *StaffService, key *authenticator) (string, error) {
	t.Helper()
	ceremony, err := s.BeginSignIn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s.SignIn(context.Background(), ceremony.ID, key.assert(t, ceremony.Options))
}

func auditCount(t *testing.T, pool *pgxpool.Pool, id pgtype.UUID, action string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM donor_staff_audit WHERE target=$1 AND action=$2", id.String(), action).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// auditReasons counts an account's audit records of one action by reason.
func auditReasons(t *testing.T, pool *pgxpool.Pool, id pgtype.UUID, action string) map[string]int {
	t.Helper()
	var counts map[string]int
	if err := pool.QueryRow(context.Background(), `SELECT coalesce(jsonb_object_agg(reason, n), '{}') FROM
		(SELECT details->>'reason' AS reason, count(*) AS n FROM donor_staff_audit WHERE target=$1 AND action=$2 GROUP BY 1) r`, id.String(), action).Scan(&counts); err != nil {
		t.Fatal(err)
	}
	return counts
}

func TestStaffPasskeyActivationSignsInOnceAndSignInIsSingleUse(t *testing.T) {
	pool := database(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := staffService(t, dbroles.API)
	id, invite := invitedStaff(t, pool, "OP_PARTNERSHIPS")
	invitation, err := s.BeginActivation(ctx, invite)
	if err != nil {
		t.Fatal(err)
	}
	creation, ok := invitation.Options.(protocol.PublicKeyCredentialCreationOptions)
	if !ok {
		t.Fatalf("invitation options are %T, want creation options", invitation.Options)
	}
	selection := creation.AuthenticatorSelection
	if invitation.Email != id.String()+"@example.org" || invitation.Name != "Test staff" || creation.RelyingParty.ID != staffTestRPID ||
		selection.ResidentKey != protocol.ResidentKeyRequirementRequired || selection.UserVerification != protocol.VerificationRequired ||
		string(creation.User.ID.(protocol.URLEncodedBase64)) != string(id.Bytes[:]) {
		t.Fatalf("registration does not require a discoverable, verified passkey for this account: %+v", invitation)
	}
	key := newAuthenticator(t, staffTestOrigin, staffTestRPID)
	attestation := key.register(t, invitation.Options)
	session, err := s.Activate(ctx, invitation.ID, attestation)
	if err != nil {
		t.Fatal(err)
	}
	if me, err := s.Me(ctx, session); err != nil || !me.Active || me.ID != id.String() {
		t.Fatal("activation did not sign the staff member in", me, err)
	}
	if _, err = s.Activate(ctx, invitation.ID, attestation); !errors.Is(err, ErrStaffDenied) {
		t.Fatal("activation ceremony replay accepted", err)
	}
	if _, err = s.BeginActivation(ctx, invite); !errors.Is(err, ErrStaffDenied) {
		t.Fatal("consumed invitation reused", err)
	}

	ceremony, err := s.BeginSignIn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	request, ok := ceremony.Options.(protocol.PublicKeyCredentialRequestOptions)
	if !ok {
		t.Fatalf("sign-in options are %T, want request options", ceremony.Options)
	}
	if request.RelyingPartyID != staffTestRPID || request.UserVerification != protocol.VerificationRequired ||
		len(request.AllowedCredentials) != 0 {
		t.Fatalf("sign-in is not a usernameless, verified request: %+v", request)
	}
	assertion := key.assert(t, ceremony.Options)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	tokens := make(chan string, 2)
	for range 2 {
		wg.Go(func() {
			token, err := s.SignIn(ctx, ceremony.ID, assertion)
			if err == nil {
				tokens <- token
			}
			results <- err
		})
	}
	wg.Wait()
	close(results)
	close(tokens)
	successes, denied := 0, 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrStaffDenied):
			denied++
		default:
			t.Fatal(err)
		}
	}
	if successes != 1 || denied != 1 {
		t.Fatalf("challenge replay: %d successes, %d denied", successes, denied)
	}
	for token := range tokens {
		if _, err = s.Me(ctx, token); err != nil {
			t.Fatal(err)
		}
		if err = s.SignOut(ctx, token); err != nil {
			t.Fatal(err)
		}
		if _, err = s.Me(ctx, token); !errors.Is(err, ErrStaffDenied) {
			t.Fatal("signed-out session accepted", err)
		}
	}
	var count int64
	var used pgtype.Timestamptz
	if err = pool.QueryRow(ctx, "SELECT sign_count,last_used_at FROM donor_staff_passkeys WHERE staff_id=$1", id).Scan(&count, &used); err != nil ||
		count != 1 ||
		!used.Valid {
		t.Fatal("passkey use not recorded", count, used, err)
	}
	if auditCount(t, pool, id, "staff.activated") != 1 || auditCount(t, pool, id, "staff.signed_in") != 2 {
		t.Fatal("activation or sign-in audit missing or duplicated")
	}
}

func TestStaffSignInRejectsForgedOrUnusableAssertions(t *testing.T) {
	pool := database(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := staffService(t, dbroles.API)
	id, key := activatedStaff(t, pool, s, "OP_PARTNERSHIPS")
	for range 2 {
		if _, err := signIn(t, s, key); err != nil {
			t.Fatal(err)
		}
	}
	failures := map[string]int{}
	for _, tc := range []struct {
		name, reason string
		forge        func(*authenticator)
		repair       func()
	}{
		{"wrong origin", "invalid", func(a *authenticator) { a.origin = "https://donors.example.org" }, nil},
		{"wrong relying party", "invalid", func(a *authenticator) { a.rpID = "donors.example.org" }, nil},
		{"user not verified", "invalid", func(a *authenticator) { a.flags &^= flagUV }, nil},
		{"counter regression", "counter", func(a *authenticator) { a.count -= 2 }, nil},
		{"counter not advancing", "counter", func(a *authenticator) { a.step = 0 }, nil},
		{"disabled account", "disabled", func(*authenticator) { mustExec(t, pool, "UPDATE donor_staff SET disabled=true WHERE id=$1", id) },
			func() { mustExec(t, pool, "UPDATE donor_staff SET disabled=false WHERE id=$1", id) }},
	} {
		forged := *key
		tc.forge(&forged)
		if _, err := signIn(t, s, &forged); !errors.Is(err, ErrStaffDenied) {
			t.Fatalf("%s: got %v, want ErrStaffDenied", tc.name, err)
		}
		if tc.repair != nil {
			tc.repair()
		}
		failures[tc.reason]++
		if got := auditReasons(t, pool, id, "staff.sign_in_failed"); !maps.Equal(got, failures) {
			t.Fatalf("%s: failed sign-ins audited as %v, want %v", tc.name, got, failures)
		}
	}
	ceremony, err := s.BeginSignIn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SignIn(ctx, ceremony.ID, newAuthenticator(t, staffTestOrigin, staffTestRPID).assert(t, ceremony.Options)); !errors.Is(
		err,
		ErrStaffDenied,
	) {
		t.Fatal("unknown passkey accepted", err)
	}
	if _, err = s.SignIn(ctx, ceremony.ID, key.assert(t, ceremony.Options)); !errors.Is(err, ErrStaffDenied) {
		t.Fatal("ceremony answered by an unknown passkey was not consumed", err)
	}
	if ceremony, err = s.BeginSignIn(ctx); err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, "UPDATE donor_staff_ceremonies SET expires_at=now() WHERE token_hash=$1", HashToken(ceremony.ID))
	if _, err = s.SignIn(ctx, ceremony.ID, key.assert(t, ceremony.Options)); !errors.Is(err, ErrStaffDenied) {
		t.Fatal("expired challenge accepted", err)
	}
	if _, err = s.SignIn(ctx, ceremony.ID, []byte(`{"id":"x"}`)); !errors.Is(err, ErrStaffInput) {
		t.Fatal("malformed credential not rejected as input", err)
	}
	if _, err = signIn(t, s, key); err != nil {
		t.Fatal("genuine passkey rejected after forged attempts", err)
	}
}

func TestStaffActivationCountsAttemptsAndRejectsForgedAttestations(t *testing.T) {
	pool := database(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := staffService(t, dbroles.API)
	id, invite := invitedStaff(t, pool, "SUPER_ADMIN")
	for _, forge := range []func(*authenticator){
		func(a *authenticator) { a.origin = "https://donors.example.org" },
		func(a *authenticator) { a.rpID = "example.org" },
		func(a *authenticator) { a.flags &^= flagUV },
	} {
		invitation, err := s.BeginActivation(ctx, invite)
		if err != nil {
			t.Fatal(err)
		}
		key := newAuthenticator(t, staffTestOrigin, staffTestRPID)
		forge(key)
		if _, err = s.Activate(ctx, invitation.ID, key.register(t, invitation.Options)); !errors.Is(err, ErrStaffDenied) {
			t.Fatal("forged attestation accepted", err)
		}
	}
	var passkeys, attempts int
	if err := pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM donor_staff_passkeys WHERE staff_id=$1), (SELECT attempts FROM donor_staff_invites WHERE staff_id=$1)", id).Scan(&passkeys, &attempts); err != nil ||
		passkeys != 0 ||
		attempts != 3 {
		t.Fatal("rejected attestations stored a passkey or went uncounted", passkeys, attempts, err)
	}
	if got := auditReasons(t, pool, id, "staff.activation_failed"); !maps.Equal(got, map[string]int{"invalid": 3}) {
		t.Fatal("rejected attestations audited as", got)
	}
	mustExec(t, pool, "UPDATE donor_staff_invites SET attempts=5 WHERE staff_id=$1", id)
	if _, err := s.BeginActivation(ctx, invite); !errors.Is(err, ErrStaffDenied) {
		t.Fatal("exhausted invitation accepted", err)
	}
	mustExec(t, pool, "UPDATE donor_staff_invites SET attempts=0, expires_at=now() WHERE staff_id=$1", id)
	if _, err := s.BeginActivation(ctx, invite); !errors.Is(err, ErrStaffDenied) {
		t.Fatal("expired invitation accepted", err)
	}
}

func TestStaffSyncedPasskeySignsInWithACounterOfZero(t *testing.T) {
	pool := database(t)
	s := staffService(t, dbroles.API)
	id, invite := invitedStaff(t, pool, "OP_PARTNERSHIPS")
	key := newAuthenticator(t, staffTestOrigin, staffTestRPID)
	key.step = 0
	if _, err := activate(t, s, invite, key); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := signIn(t, s, key); err != nil {
			t.Fatal("synced passkey refused", err)
		}
	}
	var count int64
	var eligible, backedUp bool
	if err := pool.QueryRow(context.Background(), "SELECT sign_count,backup_eligible,backup_state FROM donor_staff_passkeys WHERE staff_id=$1", id).Scan(&count, &eligible, &backedUp); err != nil ||
		count != 0 ||
		!eligible ||
		!backedUp ||
		auditCount(t, pool, id, "staff.sign_in_failed") != 0 {
		t.Fatal("synced passkey not recorded as one", count, eligible, backedUp, err)
	}
}

func TestStaffActivationRefusesAPasskeyRegisteredToAnotherAccount(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	s := staffService(t, dbroles.API)
	owner, key := activatedStaff(t, pool, s, "SUPER_ADMIN")
	other, invite := invitedStaff(t, pool, "OP_PARTNERSHIPS")
	reused := *key
	if _, err := activate(t, s, invite, &reused); !errors.Is(err, ErrStaffDenied) {
		t.Fatal("one passkey registered to two accounts", err)
	}
	var passkeys, attempts int
	if err := pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM donor_staff_passkeys WHERE staff_id=$1), (SELECT attempts FROM donor_staff_invites WHERE staff_id=$1)", other).Scan(&passkeys, &attempts); err != nil ||
		passkeys != 0 ||
		attempts != 1 {
		t.Fatal("refused passkey stored or the attempt went uncounted", passkeys, attempts, err)
	}
	if got := auditReasons(t, pool, other, "staff.activation_failed"); !maps.Equal(got, map[string]int{"credential_in_use": 1}) {
		t.Fatal("refused passkey audited as", got)
	}
	session, err := signIn(t, s, key)
	if err != nil {
		t.Fatal(err)
	}
	if me, err := s.Me(ctx, session); err != nil || me.ID != owner.String() {
		t.Fatal("passkey no longer signs its owner in", me, err)
	}
}

func TestStaffPasskeyKeepsOnlyKnownTransports(t *testing.T) {
	pool := database(t)
	s := staffService(t, dbroles.API)
	id, invite := invitedStaff(t, pool, "OP_PARTNERSHIPS")
	key := newAuthenticator(t, staffTestOrigin, staffTestRPID)
	key.transports = []string{"internal", strings.Repeat("x", 64), "usb", "cable"}
	if _, err := activate(t, s, invite, key); err != nil {
		t.Fatal("unknown transport refused the passkey", err)
	}
	var transports []string
	if err := pool.QueryRow(context.Background(), "SELECT transports FROM donor_staff_passkeys WHERE staff_id=$1", id).Scan(&transports); err != nil ||
		!slices.Equal(transports, []string{"internal", "usb", "hybrid"}) {
		t.Fatal("stored transports", transports, err)
	}
	if _, err := signIn(t, s, key); err != nil {
		t.Fatal(err)
	}
}

func TestStaffCeremoniesAnswerOnlyTheirOwnKind(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	s := staffService(t, dbroles.API)
	_, signer := activatedStaff(t, pool, s, "OP_PARTNERSHIPS")
	id, invite := invitedStaff(t, pool, "OP_PARTNERSHIPS")
	invitation, err := s.BeginActivation(ctx, invite)
	if err != nil {
		t.Fatal(err)
	}
	ceremony, err := s.BeginSignIn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	key := newAuthenticator(t, staffTestOrigin, staffTestRPID)
	if _, err = s.Activate(ctx, ceremony.ID, key.register(t, invitation.Options)); !errors.Is(err, ErrStaffDenied) {
		t.Fatal("sign-in ceremony activated a passkey", err)
	}
	if _, err = s.SignIn(ctx, invitation.ID, signer.assert(t, ceremony.Options)); !errors.Is(err, ErrStaffDenied) {
		t.Fatal("activation ceremony signed a passkey in", err)
	}
	if _, err = s.Activate(ctx, invitation.ID, key.register(t, invitation.Options)); err != nil {
		t.Fatal("misdirected answer spent the activation ceremony", err)
	}
	if auditCount(t, pool, id, "staff.activated") != 1 {
		t.Fatal("activation not audited")
	}
}

func TestStaffActivationRefusesADisabledAccount(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	s := staffService(t, dbroles.API)
	id, invite := invitedStaff(t, pool, "SUPER_ADMIN")
	invitation, err := s.BeginActivation(ctx, invite)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, "UPDATE donor_staff SET disabled=true WHERE id=$1", id)
	if _, err = s.Activate(ctx, invitation.ID, newAuthenticator(t, staffTestOrigin, staffTestRPID).register(t, invitation.Options)); !errors.Is(
		err,
		ErrStaffDenied,
	) {
		t.Fatal("disabled account activated", err)
	}
	if _, err = s.BeginActivation(ctx, invite); !errors.Is(err, ErrStaffDenied) {
		t.Fatal("disabled account offered activation", err)
	}
	var passkeys int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM donor_staff_passkeys WHERE staff_id=$1", id).Scan(&passkeys); err != nil ||
		passkeys != 0 {
		t.Fatal("disabled account holds a passkey", passkeys, err)
	}
}

// Serial, because every pass purges every expired staff ceremony in the shared database.
func TestRetentionPurgesExpiredStaffCeremoniesOnly(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	s := staffService(t, dbroles.API)
	stale, err := s.BeginSignIn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	live, err := s.BeginSignIn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, "UPDATE donor_staff_ceremonies SET expires_at=now()-interval '1 second' WHERE token_hash=$1", HashToken(stale.ID))
	t.Cleanup(func() { cleanExec(t, "DELETE FROM donor_staff_ceremonies WHERE token_hash=$1", HashToken(live.ID)) })

	retention := &TouchRetention{DB: as(t, dbroles.ProjectionWorker)}
	if _, err = retention.PurgeNext(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	present := func(id string) (found bool) {
		t.Helper()
		if err := pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM donor_staff_ceremonies WHERE token_hash=$1)", HashToken(id)).Scan(&found); err != nil {
			t.Fatal(err)
		}
		return found
	}
	if present(stale.ID) || !present(live.ID) {
		t.Fatalf("after a pass: expired ceremony present=%t, live ceremony present=%t", present(stale.ID), present(live.ID))
	}
}
