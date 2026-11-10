package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/oidc"
	"dad/donor-api/internal/sms"
)

func TestPhoneSignIn(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	id, _ := donor(t, pool)
	ctx := context.Background()
	phone := "+14155550123"
	if _, err := pool.Exec(ctx, "UPDATE donors SET phone=$2 WHERE id=$1", id, phone); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Exec(ctx, "DELETE FROM donor_phone_codes WHERE phone=$1", phone) })

	sender := &sms.StubSender{}
	sends := &sync.WaitGroup{}
	svc := PhoneService{DB: api, Sender: sender, sends: sends}

	if err := (PhoneService{DB: api}).BeginSignIn(ctx, phone); !errors.Is(err, ErrSignInUnavailable) {
		t.Fatalf("unconfigured BeginSignIn = %v; want ErrSignInUnavailable", err)
	}

	// A code is sent for a known phone.
	if err := svc.BeginSignIn(ctx, phone); err != nil {
		t.Fatal(err)
	}
	sends.Wait()
	msg, ok := sender.Last(phone)
	if !ok {
		t.Fatal("no code sent")
	}
	code := extractCode(t, msg)

	// A wrong code is rejected and counts an attempt.
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	if _, err := svc.FinishSignIn(ctx, phone, wrong, Client{}); !errors.Is(err, ErrSignInCode) {
		t.Fatalf("wrong code = %v; want ErrSignInCode", err)
	}
	var attempts int
	if err := pool.QueryRow(ctx, "SELECT attempts FROM donor_phone_codes WHERE phone=$1", phone).Scan(&attempts); err != nil ||
		attempts != 1 {
		t.Fatalf("attempts = %d, err %v; want 1", attempts, err)
	}

	// The right code signs in and consumes the code (single use).
	tokens, err := svc.FinishSignIn(ctx, phone, code, Client{})
	if err != nil {
		t.Fatal(err)
	}
	if tokens.Access == "" {
		t.Fatal("no session issued")
	}
	var remaining int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_phone_codes WHERE phone=$1", phone).Scan(&remaining); err != nil ||
		remaining != 0 {
		t.Fatalf("code not consumed: count=%d err=%v", remaining, err)
	}

	// An unknown phone sends nothing but reports success (no enumeration).
	if err := svc.BeginSignIn(ctx, "+19998887777"); err != nil {
		t.Fatal(err)
	}
	sends.Wait()
	if _, ok := sender.Last("+19998887777"); ok {
		t.Fatal("code sent to a phone with no account")
	}
}

func TestPhoneSignInSerializesGuessesAndResends(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	id, _ := donor(t, pool)
	const phone = "+14155550125"
	mustExec(t, pool, "UPDATE donors SET phone=$2 WHERE id=$1", id, phone)
	t.Cleanup(func() { cleanExec(t, "DELETE FROM donor_phone_codes WHERE phone=$1", phone) })
	sender := &sms.StubSender{}
	sends := &sync.WaitGroup{}
	svc := PhoneService{DB: as(t, dbroles.API), Sender: sender, sends: sends}
	if err := svc.BeginSignIn(ctx, phone); err != nil {
		t.Fatal(err)
	}
	sends.Wait()
	message, _ := sender.Last(phone)
	code := extractCode(t, message)
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	start := make(chan struct{})
	results := make(chan error, 32)
	for range cap(results) {
		go func() {
			<-start
			_, err := svc.FinishSignIn(ctx, phone, wrong, Client{})
			results <- err
		}()
	}
	close(start)
	for range cap(results) {
		if err := <-results; !errors.Is(err, ErrSignInCode) {
			t.Errorf("wrong concurrent guess: %v", err)
		}
	}
	var attempts int
	if err := pool.QueryRow(ctx, "SELECT attempts FROM donor_phone_codes WHERE phone=$1", phone).Scan(&attempts); err != nil ||
		attempts != phoneCodeMaxTries {
		t.Fatalf("attempts = %d (%v); want exactly %d", attempts, err, phoneCodeMaxTries)
	}
	if err := svc.BeginSignIn(ctx, phone); err != nil {
		t.Fatal(err)
	}
	sends.Wait()
	var hash string
	if err := pool.QueryRow(ctx, "SELECT attempts,code_hash FROM donor_phone_codes WHERE phone=$1", phone).Scan(&attempts, &hash); err != nil ||
		attempts != phoneCodeMaxTries ||
		hash != HashToken(code) {
		t.Fatalf("immediate resend reset the code or guesses: %d %v", attempts, err)
	}
	if _, err := svc.FinishSignIn(ctx, phone, code, Client{}); !errors.Is(err, ErrSignInCode) {
		t.Fatalf("exhausted code signed in: %v", err)
	}
	mustExec(t, pool, "UPDATE donor_phone_codes SET created_at=now()-interval '2 minutes' WHERE phone=$1", phone)
	if err := svc.BeginSignIn(ctx, phone); err != nil {
		t.Fatal(err)
	}
	sends.Wait()
	message, _ = sender.Last(phone)
	if _, err := svc.FinishSignIn(ctx, phone, extractCode(t, message), Client{}); err != nil {
		t.Fatalf("replacement code after cooldown: %v", err)
	}
}

func TestPhoneGuessesAreBoundedAcrossResends(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	id, _ := donor(t, pool)
	const phone = "+14155550127"
	mustExec(t, pool, "UPDATE donors SET phone=$2 WHERE id=$1", id, phone)
	t.Cleanup(func() { cleanExec(t, "DELETE FROM donor_phone_codes WHERE phone=$1", phone) })
	sender := &sms.StubSender{}
	sends := &sync.WaitGroup{}
	svc := PhoneService{DB: as(t, dbroles.API), Sender: sender, sends: sends}
	resend := func() string {
		t.Helper()
		mustExec(t, pool, "UPDATE donor_phone_codes SET created_at=now()-interval '2 minutes' WHERE phone=$1", phone)
		if err := svc.BeginSignIn(ctx, phone); err != nil {
			t.Fatal(err)
		}
		sends.Wait()
		message, _ := sender.Last(phone)
		return extractCode(t, message)
	}
	wrongFor := func(code string) string {
		if code == "000000" {
			return "111111"
		}
		return "000000"
	}

	code := resend()
	for range phoneWindowMaxTries / phoneCodeMaxTries {
		for range phoneCodeMaxTries {
			if _, err := svc.FinishSignIn(ctx, phone, wrongFor(code), Client{}); !errors.Is(err, ErrSignInCode) {
				t.Fatalf("wrong guess: %v", err)
			}
		}
		code = resend()
	}
	if _, err := svc.FinishSignIn(ctx, phone, code, Client{}); !errors.Is(err, ErrSignInCode) {
		t.Fatalf("a code issued after the day's guesses were spent signed in: %v", err)
	}
	hash := func() (h string) {
		t.Helper()
		if err := pool.QueryRow(ctx, "SELECT code_hash FROM donor_phone_codes WHERE phone=$1", phone).Scan(&h); err != nil {
			t.Fatal(err)
		}
		return h
	}
	spent := hash()
	resend()
	if hash() != spent {
		t.Fatal("a code was issued after the day's guesses were spent")
	}

	mustExec(t, pool, "UPDATE donor_phone_codes SET window_started_at=now()-interval '25 hours' WHERE phone=$1", phone)
	if _, err := svc.FinishSignIn(ctx, phone, resend(), Client{}); err != nil {
		t.Fatalf("a fresh code after the window rolled over: %v", err)
	}
}

func TestPhoneChangeInvalidatesPendingCode(t *testing.T) {
	s, token, id := donorServiceFixture(t)
	pool := database(t)
	ctx := context.Background()
	const phone = "+14155550126"
	mustExec(t, pool, "UPDATE donors SET phone=$2 WHERE id=$1", id, phone)
	mustExec(
		t,
		pool,
		"INSERT INTO donor_phone_codes(phone,code_hash,expires_at) VALUES($1,$2,now()+interval '10 minutes')",
		phone,
		HashToken("123456"),
	)
	t.Cleanup(func() { cleanExec(t, "DELETE FROM donor_phone_codes WHERE phone=$1", phone) })
	if _, err := s.SaveProfile(ctx, token, ProfileUpdate{}); err != nil {
		t.Fatal(err)
	}
	other, _ := donor(t, pool)
	mustExec(t, pool, "UPDATE donors SET phone=$2 WHERE id=$1", other, phone)
	phoneSignIn := PhoneService{DB: as(t, dbroles.API), Sender: &sms.StubSender{}}
	if _, err := phoneSignIn.FinishSignIn(ctx, phone, "123456", Client{}); !errors.Is(err, ErrSignInCode) {
		t.Fatalf("a detached phone's code authenticated another donor: %v", err)
	}
}

func TestOIDCSignInLinksAndAuthenticates(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	id, email := donor(t, pool)
	ctx := context.Background()
	t.Cleanup(func() { pool.Exec(ctx, "DELETE FROM donor_oidc_identities WHERE donor_id=$1", id) })

	settings := &oidc.Settings{Mode: "stub", StubKey: "secret", ClientIDs: map[oidc.Provider][]string{
		oidc.Apple: {"com.dad.app"}, oidc.Google: {"g.apps"},
	}}
	svc := OIDCService{DB: api, Verifier: oidc.NewVerifier(settings), Settings: settings}

	empty := OIDCService{DB: api, Verifier: nil, Settings: &oidc.Settings{}}
	if _, err := empty.SignIn(ctx, oidc.Google, "x", "", Client{}); !errors.Is(err, ErrSignInUnavailable) {
		t.Fatalf("unconfigured = %v; want ErrSignInUnavailable", err)
	}

	// First sign-in: no prior link, matched by verified email -> links + signs in.
	token := oidc.SignStubToken("secret", oidc.Google, "goog-sub-1", "g.apps", email, time.Now().Add(time.Hour))
	tokens, err := svc.SignIn(ctx, oidc.Google, token, "", Client{})
	if err != nil {
		t.Fatal(err)
	}
	if tokens.Access == "" {
		t.Fatal("no session issued on first sign-in")
	}
	if profile, err := (DonorService{DB: api}).Profile(ctx, tokens.Access); err != nil || profile.Email != email {
		t.Fatalf("social sign-in did not claim a usable account: %+v %v", profile, err)
	}
	var linked int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_oidc_identities WHERE donor_id=$1 AND provider='GOOGLE' AND subject='goog-sub-1'", id).Scan(&linked); err != nil ||
		linked != 1 {
		t.Fatalf("identity not linked: count=%d err=%v", linked, err)
	}

	// Second sign-in: resolved by the existing link (email no longer needed).
	token2 := oidc.SignStubToken("secret", oidc.Google, "goog-sub-1", "g.apps", "", time.Now().Add(time.Hour))
	if _, err := svc.SignIn(ctx, oidc.Google, token2, "", Client{}); err != nil {
		t.Fatalf("second sign-in via link failed: %v", err)
	}

	// A verified identity can create a new donor account.
	orphan := oidc.SignStubToken(
		"secret",
		oidc.Apple,
		"apple-sub-"+testID(t).String(),
		"com.dad.app",
		"new-"+testID(t).String()+"@example.org",
		time.Now().Add(time.Hour),
	)
	newTokens, err := svc.SignIn(ctx, oidc.Apple, orphan, "", Client{})
	if err != nil {
		t.Fatal(err)
	}
	newProfile, err := (DonorService{DB: api}).Profile(ctx, newTokens.Access)
	if err != nil || !strings.HasPrefix(newProfile.Email, "new-") {
		t.Fatalf("new verified identity: %+v %v", newProfile, err)
	}

	if _, err := svc.SignIn(ctx, oidc.Google, "bad.token.sig", "", Client{}); !errors.Is(err, ErrSignInCode) {
		t.Fatalf("bad token = %v; want ErrSignInCode", err)
	}
}

// TestOIDCNonceBinding covers the nonce policy end to end through the store:
// raw and SHA-256 forms match, anything else is the uniform sign-in failure, and
// requiring a nonce rejects a sign-in that supplies none.
func TestOIDCNonceBinding(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	id, email := donor(t, pool)
	ctx := context.Background()
	t.Cleanup(func() { pool.Exec(ctx, "DELETE FROM donor_oidc_identities WHERE donor_id=$1", id) })
	settings := &oidc.Settings{Mode: "stub", StubKey: "secret", ClientIDs: map[oidc.Provider][]string{oidc.Google: {"g.apps"}}}
	svc := OIDCService{DB: api, Verifier: oidc.NewVerifier(settings), Settings: settings}
	required := *settings
	required.RequireNonce = true
	strict := OIDCService{DB: api, Verifier: oidc.NewVerifier(&required), Settings: &required}

	const raw = "client-nonce-0123456789"
	sum := sha256.Sum256([]byte(raw))
	hashed := hex.EncodeToString(sum[:])
	token := func(claim string) string {
		return oidc.SignStubTokenWithNonce(
			"secret",
			oidc.Google,
			"nonce-sub-"+id.String(),
			"g.apps",
			email,
			claim,
			time.Now().Add(time.Hour),
		)
	}
	for _, tc := range []struct {
		name    string
		service OIDCService
		claim   string
		sent    string
		ok      bool
	}{
		{"raw form", svc, raw, raw, true},
		{"hash form", svc, hashed, raw, true},
		{"strict raw form", strict, raw, raw, true},
		{"no nonce sent, optional mode", svc, "", "", true},
		{"no nonce sent, token has one, optional mode", svc, raw, "", true},
		{"no nonce sent, required mode", strict, raw, "", false},
		{"replay with a different nonce", svc, raw, "another-nonce-0123456789", false},
		{"token lacks the claim", svc, "", raw, false},
		{"uppercase hash is not the lowercase form", svc, strings.ToUpper(hashed), raw, false},
		{"hash of a different nonce", svc, hashed, "another-nonce-0123456789", false},
	} {
		_, err := tc.service.SignIn(ctx, oidc.Google, token(tc.claim), tc.sent, Client{})
		if tc.ok != (err == nil) || (!tc.ok && !errors.Is(err, ErrSignInCode)) {
			t.Errorf("%s: err = %v, want success=%v with ErrSignInCode on failure", tc.name, err, tc.ok)
		}
	}
}

func TestOIDCLinkRechecksEmailAfterDonorLock(t *testing.T) {
	pool := database(t)
	id, email := donor(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t.Cleanup(func() { cleanExec(t, "DELETE FROM donor_oidc_identities WHERE donor_id=$1", id) })
	settings := &oidc.Settings{Mode: "stub", StubKey: "fixture", ClientIDs: map[oidc.Provider][]string{oidc.Google: {"g.apps"}}}
	svc := OIDCService{DB: as(t, dbroles.API), Verifier: oidc.NewVerifier(settings), Settings: settings}
	lock, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(ctx)
	mustExec(t, lock, "UPDATE donors SET email=$2 WHERE id=$1", id, "changed-"+email)
	result := make(chan error, 1)
	go func() {
		token := oidc.SignStubToken("fixture", oidc.Google, id.String(), "g.apps", email, time.Now().Add(time.Hour))
		_, err := svc.SignIn(ctx, oidc.Google, token, "", Client{})
		result <- err
	}()
	awaitPartnerWaiters(t, pool, 1)
	if err := lock.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, ErrSignInCode) {
		t.Fatalf("sign-in linked a stale email: %v", err)
	}
	var identities, sessions int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM donor_oidc_identities WHERE donor_id=$1),
		(SELECT count(*) FROM donor_sessions WHERE donor_id=$1)`, id).Scan(&identities, &sessions); err != nil {
		t.Fatal(err)
	}
	if identities != 0 || sessions != 0 {
		t.Fatalf("stale email created %d identities and %d sessions", identities, sessions)
	}
}

// extractCode pulls the 6-digit code out of the stub SMS message.
func extractCode(t *testing.T, msg string) string {
	t.Helper()
	var code string
	for _, f := range []byte(msg) {
		if f >= '0' && f <= '9' {
			code += string(f)
		}
	}
	// The message contains only the code's digits and the "10 minutes" literal;
	// take the first run of 6 digits.
	for i := 0; i+6 <= len(code); i++ {
		return code[i : i+6]
	}
	t.Fatalf("no code in %q", msg)
	return ""
}

type unreachableProvider struct{}

func (unreachableProvider) Verify(context.Context, oidc.Provider, string) (oidc.Identity, error) {
	return oidc.Identity{}, oidc.ErrKeys
}

// TestOIDCProviderOutageIsNotABadToken keeps a provider outage from telling the
// donor their sign-in was wrong.
func TestOIDCProviderOutageIsNotABadToken(t *testing.T) {
	settings := &oidc.Settings{Mode: "live", ClientIDs: map[oidc.Provider][]string{oidc.Google: {"g.apps"}}}
	svc := OIDCService{DB: as(t, dbroles.API), Verifier: unreachableProvider{}, Settings: settings}
	_, err := svc.SignIn(context.Background(), oidc.Google, "token", "", Client{})
	if !errors.Is(err, oidc.ErrKeys) || errors.Is(err, ErrSignInCode) {
		t.Fatalf("SignIn during a provider outage = %v; want ErrKeys", err)
	}
}

type failingSender struct{}

func (failingSender) Send(context.Context, string, string) error { return sms.ErrUnavailable }

// TestPhoneSignInHidesAFailedSend keeps a gateway outage from revealing which
// numbers have accounts: a known number answers like an unknown one.
func TestPhoneSignInHidesAFailedSend(t *testing.T) {
	pool := database(t)
	id, _ := donor(t, pool)
	ctx := context.Background()
	phone := "+14155550124"
	mustExec(t, pool, "UPDATE donors SET phone=$2 WHERE id=$1", id, phone)
	t.Cleanup(func() { cleanExec(t, "DELETE FROM donor_phone_codes WHERE phone=$1", phone) })
	sends := &sync.WaitGroup{}
	svc := PhoneService{DB: as(t, dbroles.API), Sender: failingSender{}, sends: sends}
	for _, number := range []string{phone, "+19998887776"} {
		if err := svc.BeginSignIn(ctx, number); err != nil {
			t.Errorf("BeginSignIn(%s) = %v; want the same success for every number", number, err)
		}
	}
	sends.Wait()
}
