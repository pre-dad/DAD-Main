package store

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	donorPasskeyTestOrigin = "https://example.org"
	donorPasskeyTestRPID   = "example.org"
)

func donorPasskeyService(t *testing.T) *DonorPasskeyService {
	t.Helper()
	s, err := NewDonorPasskeyService(as(t, dbroles.API), donorPasskeyTestOrigin)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// passkeyDonor creates a signed-in donor, removing every record its passkey
// journey leaves behind, and returns the donor and their session token.
func passkeyDonor(t *testing.T, pool *pgxpool.Pool) (pgtype.UUID, string) {
	t.Helper()
	id, _ := donor(t, pool)
	return passkeyDonorFor(t, pool, id)
}

func passkeyDonorFor(t *testing.T, pool *pgxpool.Pool, id pgtype.UUID) (pgtype.UUID, string) {
	t.Helper()
	t.Cleanup(func() {
		for _, sql := range []string{"DELETE FROM donor_ceremonies WHERE donor_id=$1", "DELETE FROM donor_passkeys WHERE donor_id=$1", "DELETE FROM donor_messages WHERE donor_id=$1"} {
			if _, err := pool.Exec(context.Background(), sql, id); err != nil {
				t.Error(err)
			}
		}
	})
	return id, claimSession(t, pool, id, Client{}).Access
}

func registerPasskey(t *testing.T, s *DonorPasskeyService, session string, key *authenticator, name string) error {
	t.Helper()
	ceremony, err := s.BeginRegister(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.FinishRegister(context.Background(), session, ceremony.ID, key.register(t, ceremony.Options), name)
	return err
}

func donorSignIn(t *testing.T, s *DonorPasskeyService, key *authenticator, client Client) (Tokens, error) {
	t.Helper()
	ceremony, err := s.BeginSignIn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s.FinishSignIn(context.Background(), ceremony.ID, key.assert(t, ceremony.Options), client)
}

func TestDonorPasskeyRegistersSignsInAndIsDeleted(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	s := donorPasskeyService(t)
	id, session := passkeyDonor(t, pool)
	key := newAuthenticator(t, donorPasskeyTestOrigin, donorPasskeyTestRPID)
	if err := registerPasskey(t, s, session, key, "My phone"); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListPasskeys(ctx, session)
	if err != nil || len(list.Passkeys) != 1 || list.Passkeys[0].Name == nil || *list.Passkeys[0].Name != "My phone" ||
		list.Passkeys[0].LastUsedAt != nil {
		t.Fatal("registered passkey listed as", list, err)
	}
	tokens, err := donorSignIn(t, s, key, Client{})
	if err != nil || !signedInAs(t, pool, tokens.Access) || tokens.Refresh != "" {
		t.Fatal("passkey did not sign the browser in", tokens, err)
	}
	if sessionEvents(t, pool, id, "SIGNED_IN") != 2 {
		t.Fatal("passkey sign-in not recorded as a session event")
	}
	if list, err = s.ListPasskeys(ctx, session); err != nil || list.Passkeys[0].LastUsedAt == nil {
		t.Fatal("passkey use not recorded", list, err)
	}
	if list, err = s.DeletePasskey(ctx, session, list.Passkeys[0].ID); err != nil || len(list.Passkeys) != 0 {
		t.Fatal("deleted passkey still listed", list, err)
	}
	if _, err = donorSignIn(t, s, key, Client{}); !errors.Is(err, ErrPasskeyDenied) {
		t.Fatal("deleted passkey signed in", err)
	}
}

func TestDonorPasskeySignsTheAppInWithARefreshToken(t *testing.T) {
	pool := database(t)
	s := donorPasskeyService(t)
	_, session := passkeyDonor(t, pool)
	key := newAuthenticator(t, donorPasskeyTestOrigin, donorPasskeyTestRPID)
	if err := registerPasskey(t, s, session, key, ""); err != nil {
		t.Fatal(err)
	}
	tokens, err := donorSignIn(t, s, key, app)
	if err != nil || tokens.Refresh == "" || !signedInAs(t, pool, tokens.Access) {
		t.Fatal("passkey did not sign the app in", tokens, err)
	}
}

func TestDonorPasskeyCeremonyIsSingleUse(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	s := donorPasskeyService(t)
	_, session := passkeyDonor(t, pool)
	key := newAuthenticator(t, donorPasskeyTestOrigin, donorPasskeyTestRPID)
	ceremony, err := s.BeginRegister(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinishRegister(ctx, session, ceremony.ID, key.register(t, ceremony.Options), ""); err != nil {
		t.Fatal(err)
	}
	other := newAuthenticator(t, donorPasskeyTestOrigin, donorPasskeyTestRPID)
	if _, err = s.FinishRegister(ctx, session, ceremony.ID, other.register(t, ceremony.Options), ""); !errors.Is(err, ErrPasskeyDenied) {
		t.Fatal("registration ceremony reused", err)
	}
	signIn, err := s.BeginSignIn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinishSignIn(ctx, signIn.ID, key.assert(t, signIn.Options), Client{}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinishSignIn(ctx, signIn.ID, key.assert(t, signIn.Options), Client{}); !errors.Is(err, ErrPasskeyDenied) {
		t.Fatal("sign-in ceremony reused", err)
	}
}

func TestDonorPasskeyRefusedAnswerSpendsItsCeremony(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	s := donorPasskeyService(t)
	_, session := passkeyDonor(t, pool)
	key := newAuthenticator(t, donorPasskeyTestOrigin, donorPasskeyTestRPID)
	if err := registerPasskey(t, s, session, key, ""); err != nil {
		t.Fatal(err)
	}
	stranger := newAuthenticator(t, donorPasskeyTestOrigin, donorPasskeyTestRPID)
	signIn, err := s.BeginSignIn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinishSignIn(ctx, signIn.ID, stranger.assert(t, signIn.Options), Client{}); !errors.Is(err, ErrPasskeyDenied) {
		t.Fatal("unregistered passkey signed in", err)
	}
	if _, err = s.FinishSignIn(ctx, signIn.ID, key.assert(t, signIn.Options), Client{}); !errors.Is(err, ErrPasskeyDenied) {
		t.Fatal("sign-in ceremony answered again after a refusal", err)
	}
	register, err := s.BeginRegister(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	reused := *key
	if _, err = s.FinishRegister(ctx, session, register.ID, reused.register(t, register.Options), ""); !errors.Is(err, ErrPasskeyDenied) {
		t.Fatal("held passkey registered twice", err)
	}
	if _, err = s.FinishRegister(ctx, session, register.ID, stranger.register(t, register.Options), ""); !errors.Is(err, ErrPasskeyDenied) {
		t.Fatal("registration ceremony answered again after a refusal", err)
	}
}

func TestDonorPasskeyCeremoniesAnswerOnlyTheirOwnKind(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	s := donorPasskeyService(t)
	_, session := passkeyDonor(t, pool)
	signer := newAuthenticator(t, donorPasskeyTestOrigin, donorPasskeyTestRPID)
	if err := registerPasskey(t, s, session, signer, ""); err != nil {
		t.Fatal(err)
	}
	register, err := s.BeginRegister(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	signIn, err := s.BeginSignIn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	key := newAuthenticator(t, donorPasskeyTestOrigin, donorPasskeyTestRPID)
	if _, err = s.FinishRegister(ctx, session, signIn.ID, key.register(t, register.Options), ""); !errors.Is(err, ErrPasskeyDenied) {
		t.Fatal("sign-in ceremony registered a passkey", err)
	}
	if _, err = s.FinishSignIn(ctx, register.ID, signer.assert(t, signIn.Options), Client{}); !errors.Is(err, ErrPasskeyDenied) {
		t.Fatal("registration ceremony signed a passkey in", err)
	}
	if list, err := s.FinishRegister(ctx, session, register.ID, key.register(t, register.Options), ""); err != nil ||
		len(list.Passkeys) != 2 {
		t.Fatal("misdirected answer spent the registration ceremony", list, err)
	}
}

func TestDonorPasskeyRegistrationBelongsToItsDonor(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	s := donorPasskeyService(t)
	_, owner := passkeyDonor(t, pool)
	other, intruder := passkeyDonor(t, pool)
	ceremony, err := s.BeginRegister(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	key := newAuthenticator(t, donorPasskeyTestOrigin, donorPasskeyTestRPID)
	if _, err = s.FinishRegister(ctx, intruder, ceremony.ID, key.register(t, ceremony.Options), ""); !errors.Is(err, ErrPasskeyDenied) {
		t.Fatal("another donor finished the registration", err)
	}
	var passkeys int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM donor_passkeys WHERE donor_id=$1", other).Scan(&passkeys); err != nil ||
		passkeys != 0 {
		t.Fatal("passkey stored for the wrong donor", passkeys, err)
	}
	if _, err = s.BeginRegister(ctx, "not-a-session"); !errors.Is(err, ErrDenied) {
		t.Fatal("registration offered without a session", err)
	}
}

func TestDonorPasskeyRegistrationBelongsToItsSession(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	s := donorPasskeyService(t)
	id, first := passkeyDonor(t, pool)
	_, second := passkeyDonorFor(t, pool, id)
	ceremony, err := s.BeginRegister(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	key := newAuthenticator(t, donorPasskeyTestOrigin, donorPasskeyTestRPID)
	response := key.register(t, ceremony.Options)
	if _, err = s.FinishRegister(ctx, second, ceremony.ID, response, ""); !errors.Is(err, ErrPasskeyDenied) {
		t.Fatal("another session finished the registration", err)
	}
	if _, err = s.FinishRegister(ctx, first, ceremony.ID, response, ""); !errors.Is(err, ErrPasskeyDenied) {
		t.Fatal("refused ceremony was not spent", err)
	}
	if list, err := s.ListPasskeys(ctx, first); err != nil || len(list.Passkeys) != 0 {
		t.Fatal("refused registration stored a passkey", list, err)
	}
}

func TestMalformedDonorPasskeyAnswerSpendsItsCeremony(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	s := donorPasskeyService(t)
	_, session := passkeyDonor(t, pool)
	key := newAuthenticator(t, donorPasskeyTestOrigin, donorPasskeyTestRPID)
	if err := registerPasskey(t, s, session, key, ""); err != nil {
		t.Fatal(err)
	}
	signIn, err := s.BeginSignIn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinishSignIn(ctx, signIn.ID, []byte(`{}`), Client{}); !errors.Is(err, ErrPasskeyDenied) {
		t.Fatal("malformed assertion accepted", err)
	}
	if _, err = s.FinishSignIn(ctx, signIn.ID, key.assert(t, signIn.Options), Client{}); !errors.Is(err, ErrPasskeyDenied) {
		t.Fatal("malformed assertion did not spend the ceremony", err)
	}
	registration, err := s.BeginRegister(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinishRegister(ctx, session, registration.ID, []byte(`{}`), ""); !errors.Is(err, ErrPasskeyDenied) {
		t.Fatal("malformed attestation accepted", err)
	}
	other := newAuthenticator(t, donorPasskeyTestOrigin, donorPasskeyTestRPID)
	if _, err = s.FinishRegister(ctx, session, registration.ID, other.register(t, registration.Options), ""); !errors.Is(
		err,
		ErrPasskeyDenied,
	) {
		t.Fatal("malformed attestation did not spend the ceremony", err)
	}
}

func TestDonorPasskeyRefusesACredentialRegisteredToAnotherDonor(t *testing.T) {
	pool := database(t)
	s := donorPasskeyService(t)
	owner, first := passkeyDonor(t, pool)
	_, second := passkeyDonor(t, pool)
	key := newAuthenticator(t, donorPasskeyTestOrigin, donorPasskeyTestRPID)
	if err := registerPasskey(t, s, first, key, ""); err != nil {
		t.Fatal(err)
	}
	reused := *key
	if err := registerPasskey(t, s, second, &reused, ""); !errors.Is(err, ErrPasskeyDenied) {
		t.Fatal("one passkey registered to two donors", err)
	}
	tokens, err := donorSignIn(t, s, key, Client{})
	if err != nil {
		t.Fatal(err)
	}
	if !signedInAs(t, pool, tokens.Access) || sessionEvents(t, pool, owner, "SIGNED_IN") != 2 {
		t.Fatal("passkey no longer signs its owner in")
	}
}

func TestDonorPasskeySignInRefusesUnknownKeysAndDisabledDonors(t *testing.T) {
	pool := database(t)
	s := donorPasskeyService(t)
	if _, err := donorSignIn(t, s, newAuthenticator(t, donorPasskeyTestOrigin, donorPasskeyTestRPID), Client{}); !errors.Is(
		err,
		ErrPasskeyDenied,
	) {
		t.Fatal("unregistered passkey signed in", err)
	}
	id, session := passkeyDonor(t, pool)
	key := newAuthenticator(t, donorPasskeyTestOrigin, donorPasskeyTestRPID)
	if err := registerPasskey(t, s, session, key, ""); err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, "UPDATE donors SET disabled=true WHERE id=$1", id)
	if _, err := donorSignIn(t, s, key, Client{}); !errors.Is(err, ErrPasskeyDenied) {
		t.Fatal("disabled donor signed in", err)
	}
}

func TestDonorPasskeyIsDeletedOnlyByItsDonor(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	s := donorPasskeyService(t)
	_, owner := passkeyDonor(t, pool)
	_, other := passkeyDonor(t, pool)
	if err := registerPasskey(t, s, owner, newAuthenticator(t, donorPasskeyTestOrigin, donorPasskeyTestRPID), ""); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListPasskeys(ctx, owner)
	if err != nil || len(list.Passkeys) != 1 {
		t.Fatal(list, err)
	}
	if _, err = s.DeletePasskey(ctx, other, list.Passkeys[0].ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("another donor deleted the passkey", err)
	}
	if list, err = s.ListPasskeys(ctx, owner); err != nil || len(list.Passkeys) != 1 {
		t.Fatal("passkey gone after a refused delete", list, err)
	}
	if others, err := s.ListPasskeys(ctx, other); err != nil || len(others.Passkeys) != 0 {
		t.Fatal("passkey listed to another donor", others, err)
	}
}

func TestDonorPasskeyRegistrationExcludesHeldPasskeys(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	s := donorPasskeyService(t)
	_, session := passkeyDonor(t, pool)
	key := newAuthenticator(t, donorPasskeyTestOrigin, donorPasskeyTestRPID)
	if err := registerPasskey(t, s, session, key, ""); err != nil {
		t.Fatal(err)
	}
	ceremony, err := s.BeginRegister(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	excluded := ceremony.Options.(protocol.PublicKeyCredentialCreationOptions).CredentialExcludeList
	if len(excluded) != 1 || !bytes.Equal(excluded[0].CredentialID, key.id) {
		t.Fatal("held passkey not excluded", excluded)
	}
}

func TestDonorPasskeyAddedSendsAnEssentialNotice(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	s := donorPasskeyService(t)
	id, email := donor(t, pool)
	_, session := passkeyDonorFor(t, pool, id)
	if err := registerPasskey(t, s, session, newAuthenticator(t, donorPasskeyTestOrigin, donorPasskeyTestRPID), "Work laptop"); err != nil {
		t.Fatal(err)
	}
	unnamed := newAuthenticator(t, donorPasskeyTestOrigin, donorPasskeyTestRPID)
	if err := registerPasskey(t, s, session, unnamed, ""); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListPasskeys(ctx, session)
	if err != nil || len(list.Passkeys) != 2 {
		t.Fatal(list, err)
	}
	for _, passkey := range list.Passkeys {
		if _, err = s.DeletePasskey(ctx, session, passkey.ID); err != nil {
			t.Fatal(err)
		}
	}
	transport := &recordingMail{}
	w := notificationWorker(t, transport)
	for range 2 {
		if worked, err := w.DeliverNext(ctx, time.Now().Add(time.Second)); err != nil || !worked {
			t.Fatal("removing the passkey withdrew its notice", worked, err)
		}
	}
	if len(transport.messages) != 2 {
		t.Fatal("notices sent", len(transport.messages))
	}
	bodies := transport.messages[0].Body + transport.messages[1].Body
	for _, m := range transport.messages {
		if m.To != email || m.Subject != "A passkey was added to your DAD account" ||
			!strings.Contains(m.Body, "https://donor.example.org/account/profile") {
			t.Fatal("notice", m)
		}
	}
	if !strings.Contains(bodies, `A passkey named "Work laptop" was added`) || !strings.Contains(bodies, "A passkey was added") {
		t.Fatal("notices do not describe each passkey", bodies)
	}
}

func TestDonorPasskeyNamesAreBounded(t *testing.T) {
	for name, want := range map[string]bool{"": true, "Work laptop": true, strings.Repeat("é", MaxPasskeyNameRunes): true, strings.Repeat("a", MaxPasskeyNameRunes+1): false, "line\nbreak": false} {
		if ValidPasskeyName(name) != want {
			t.Errorf("ValidPasskeyName(%q) = %v", name, !want)
		}
	}
}

func TestDonorPasskeyExpiredCeremoniesAreRefusedAndSpent(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	s := donorPasskeyService(t)
	_, session := passkeyDonor(t, pool)
	key := newAuthenticator(t, donorPasskeyTestOrigin, donorPasskeyTestRPID)
	if err := registerPasskey(t, s, session, key, ""); err != nil {
		t.Fatal(err)
	}
	expire := func(id string) {
		t.Helper()
		mustExec(t, pool, "UPDATE donor_ceremonies SET expires_at=now()-interval '1 second' WHERE token_hash=$1", HashToken(id))
	}
	signIn, err := s.BeginSignIn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	answer := key.assert(t, signIn.Options)
	expire(signIn.ID)
	if _, err = s.FinishSignIn(ctx, signIn.ID, answer, Client{}); !errors.Is(err, ErrPasskeyDenied) {
		t.Fatal("expired sign-in ceremony answered", err)
	}
	register, err := s.BeginRegister(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	other := newAuthenticator(t, donorPasskeyTestOrigin, donorPasskeyTestRPID)
	response := other.register(t, register.Options)
	expire(register.ID)
	if _, err = s.FinishRegister(ctx, session, register.ID, response, ""); !errors.Is(err, ErrPasskeyDenied) {
		t.Fatal("expired registration ceremony answered", err)
	}
	// An expired ceremony is only refused when answered; the next ceremony
	// begun reaps it.
	if _, err = s.BeginSignIn(ctx); err != nil {
		t.Fatal(err)
	}
	var left int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM donor_ceremonies WHERE token_hash IN ($1,$2)", HashToken(signIn.ID), HashToken(register.ID)).Scan(&left); err != nil ||
		left != 0 {
		t.Fatal("expired ceremonies survived the sweep", left, err)
	}
	if list, err := s.ListPasskeys(ctx, session); err != nil || len(list.Passkeys) != 1 {
		t.Fatal("expired registration stored a passkey", list, err)
	}
}

func TestDonorPasskeyClonedCounterIsDeniedAndNotRecorded(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	s := donorPasskeyService(t)
	_, session := passkeyDonor(t, pool)
	key := newAuthenticator(t, donorPasskeyTestOrigin, donorPasskeyTestRPID)
	if err := registerPasskey(t, s, session, key, ""); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := donorSignIn(t, s, key, Client{}); err != nil {
			t.Fatal(err)
		}
	}
	stored := func() (count int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, "SELECT max(sign_count) FROM donor_passkeys WHERE credential_id=$1", key.id).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	if stored() != 2 {
		t.Fatal("authenticator counter not stored", stored())
	}
	key.count = 0
	if _, err := donorSignIn(t, s, key, Client{}); !errors.Is(err, ErrPasskeyDenied) {
		t.Fatal("a passkey whose counter went backwards signed in", err)
	}
	if stored() != 2 {
		t.Fatal("cloned assertion lowered the stored counter", stored())
	}
	key.count = 2
	if _, err := donorSignIn(t, s, key, Client{}); err != nil {
		t.Fatal("the genuine passkey, counting on from its last use, was refused", err)
	}
}

func TestDonorPasskeyRefusedNameLeavesTheCeremonyUsable(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	s := donorPasskeyService(t)
	_, session := passkeyDonor(t, pool)
	ceremony, err := s.BeginRegister(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	key := newAuthenticator(t, donorPasskeyTestOrigin, donorPasskeyTestRPID)
	response := key.register(t, ceremony.Options)
	for _, name := range []string{"line\nbreak", strings.Repeat("a", MaxPasskeyNameRunes+1)} {
		if _, err = s.FinishRegister(ctx, session, ceremony.ID, response, name); !errors.Is(err, ErrPasskeyDenied) {
			t.Fatalf("name %q accepted: %v", name, err)
		}
	}
	list, err := s.FinishRegister(ctx, session, ceremony.ID, response, strings.Repeat("é", MaxPasskeyNameRunes))
	if err != nil || len(list.Passkeys) != 1 {
		t.Fatal("a refused name spent the ceremony", list, err)
	}
}

func TestDonorPasskeyDisabledDonorCannotRegister(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	s := donorPasskeyService(t)
	id, session := passkeyDonor(t, pool)
	mustExec(t, pool, "UPDATE donors SET disabled=true WHERE id=$1", id)
	if _, err := s.BeginRegister(ctx, session); !errors.Is(err, ErrDenied) {
		t.Fatal("registration offered to a disabled donor", err)
	}
}
