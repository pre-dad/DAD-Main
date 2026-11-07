package oidc

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type provider struct {
	t       *testing.T
	key     *rsa.PrivateKey
	kid     string
	fetches atomic.Int32
	down    atomic.Bool
	server  *httptest.Server
}

func newProvider(t *testing.T) *provider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &provider{t: t, key: key, kid: "k1"}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		p.fetches.Add(1)
		if p.down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": p.kid, "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(p.key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(p.key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(p.server.Close)
	return p
}

func (p *provider) verifier(now time.Time) *liveVerifier {
	v := newLiveVerifier(map[Provider][]string{Google: {"g.apps"}, Apple: {"com.dad.app"}})
	v.keyURLs = map[Provider]string{Google: p.server.URL, Apple: p.server.URL}
	v.client = p.server.Client()
	v.now = func() time.Time { return now }
	return v
}

func (p *provider) sign(header, claims map[string]any) string {
	p.t.Helper()
	segment := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			p.t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	signed := segment(header) + "." + segment(claims)
	digest := sha256.Sum256([]byte(signed))
	signature, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, digest[:])
	if err != nil {
		p.t.Fatal(err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(signature)
}

var issued = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func googleClaims() map[string]any {
	return map[string]any{
		"iss": "https://accounts.google.com", "sub": "g-123", "aud": "g.apps",
		"iat": issued.Unix(), "exp": issued.Add(time.Hour).Unix(),
		"email": "d@example.org", "email_verified": true, "hd": "example.org",
	}
}

func rs256(kid string) map[string]any { return map[string]any{"alg": "RS256", "kid": kid} }

func TestLiveVerifierAcceptsProviderTokens(t *testing.T) {
	p := newProvider(t)
	v := p.verifier(issued.Add(time.Minute))
	id, err := v.Verify(context.Background(), Google, p.sign(rs256("k1"), googleClaims()))
	if err != nil || id != (Identity{Provider: Google, Subject: "g-123", Email: "d@example.org"}) {
		t.Fatalf("Google token: %+v %v", id, err)
	}
	if _, err = v.Verify(context.Background(), Google, p.sign(rs256("k1"), googleClaims())); err != nil || p.fetches.Load() != 1 {
		t.Fatalf("second Google token: %v after %d fetches; want the cached keys", err, p.fetches.Load())
	}
	apple := map[string]any{
		"iss": "https://appleid.apple.com", "sub": "a-1", "aud": []string{"com.dad.app"},
		"iat": issued.Unix(), "exp": issued.Add(time.Hour).Unix(),
		"email": "relay@privaterelay.appleid.com", "email_verified": "true",
	}
	id, err = v.Verify(context.Background(), Apple, p.sign(rs256("k1"), apple))
	if err != nil || id.Subject != "a-1" || id.Email != "relay@privaterelay.appleid.com" {
		t.Fatalf("Apple token: %+v %v", id, err)
	}
	if p.fetches.Load() != 2 {
		t.Fatalf("fetched the keys %d times; want once per provider", p.fetches.Load())
	}
}

func TestLiveVerifierDropsUnverifiedEmail(t *testing.T) {
	for _, verified := range []any{false, "false", nil} {
		p := newProvider(t)
		claims := googleClaims()
		claims["email_verified"] = verified
		id, err := p.verifier(issued).Verify(context.Background(), Google, p.sign(rs256("k1"), claims))
		if err != nil || id.Subject != "g-123" || id.Email != "" {
			t.Errorf("email_verified %v: %+v %v; want the subject without the email", verified, id, err)
		}
	}
}

func TestLiveVerifierRejections(t *testing.T) {
	p := newProvider(t)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	with := func(key string, value any) map[string]any {
		claims := googleClaims()
		claims[key] = value
		return claims
	}
	forged := p.sign(rs256("k1"), googleClaims())
	p.key, other = other, p.key
	forgedBy := p.sign(rs256("k1"), googleClaims())
	p.key = other
	tampered := p.sign(rs256("k1"), googleClaims())
	tampered = tampered[:len(tampered)-4] + "AAAA"
	for name, token := range map[string]string{
		"wrong signer":    forgedBy,
		"bad signature":   tampered,
		"wrong issuer":    p.sign(rs256("k1"), with("iss", "https://evil.example")),
		"wrong audience":  p.sign(rs256("k1"), with("aud", "someone-else")),
		"audience list":   p.sign(rs256("k1"), with("aud", []string{"someone-else"})),
		"expired":         p.sign(rs256("k1"), with("exp", issued.Add(-2*time.Minute).Unix())),
		"issued later":    p.sign(rs256("k1"), with("iat", issued.Add(10*time.Minute).Unix())),
		"no subject":      p.sign(rs256("k1"), with("sub", "")),
		"no expiry":       p.sign(rs256("k1"), with("exp", 0)),
		"no issued time":  p.sign(rs256("k1"), with("iat", 0)),
		"expiry at issue": p.sign(rs256("k1"), with("exp", issued.Unix())),
		"wrong presenter": p.sign(rs256("k1"), with("azp", "another.apps")),
		"no presenter":    p.sign(rs256("k1"), with("aud", []string{"g.apps", "another.apps"})),
		"long subject":    p.sign(rs256("k1"), with("sub", strings.Repeat("s", 256))),
		"NUL subject":     p.sign(rs256("k1"), with("sub", "s\x00")),
		"alg none":        p.sign(map[string]any{"alg": "none", "kid": "k1"}, googleClaims()),
		"HS256":           p.sign(map[string]any{"alg": "HS256", "kid": "k1"}, googleClaims()),
		"no kid":          p.sign(map[string]any{"alg": "RS256"}, googleClaims()),
		"unknown kid":     p.sign(rs256("k9"), googleClaims()),
		"two segments":    "a.b",
		"garbage":         "not-a-token",
		"other provider":  forged,
		"undecodable sig": forged[:len(forged)-2] + "!!",
	} {
		provider := Google
		if name == "other provider" {
			provider = Apple
		}
		if _, err := p.verifier(issued).Verify(context.Background(), provider, token); !errors.Is(err, ErrToken) {
			t.Errorf("%s: err = %v; want ErrToken", name, err)
		}
	}
}

func TestGoogleEmailAuthority(t *testing.T) {
	p := newProvider(t)
	v := p.verifier(issued)
	for _, tc := range []struct {
		name, email, domain string
		trusted             bool
	}{
		{"Gmail", "d@gmail.com", "", true},
		{"Workspace", "d@example.org", "example.org", true},
		{"external mailbox", "d@example.org", "", false},
		{"Gmail lookalike", "d@gmail.com.example.org", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := googleClaims()
			claims["email"], claims["hd"] = tc.email, tc.domain
			id, err := v.Verify(context.Background(), Google, p.sign(rs256("k1"), claims))
			if err != nil || id.Subject != "g-123" || (id.Email != "") != tc.trusted {
				t.Fatalf("identity = %+v, %v; trusted email: %v", id, err, tc.trusted)
			}
		})
	}
}

func TestLiveVerifierAcceptsConfiguredPresenter(t *testing.T) {
	p := newProvider(t)
	v := p.verifier(issued)
	v.clientIDs[Google] = []string{"g.apps", "g.android"}
	claims := googleClaims()
	claims["aud"], claims["azp"] = []string{"g.apps", "g.android"}, "g.android"
	if _, err := v.Verify(context.Background(), Google, p.sign(rs256("k1"), claims)); err != nil {
		t.Fatal(err)
	}
}

func TestLiveVerifierRefetchesRotatedKeys(t *testing.T) {
	p := newProvider(t)
	now := issued
	v := p.verifier(now)
	v.now = func() time.Time { return now }
	if _, err := v.Verify(context.Background(), Google, p.sign(rs256("k1"), googleClaims())); err != nil {
		t.Fatal(err)
	}
	p.kid = "k2"
	rotated := p.sign(rs256("k2"), googleClaims())
	if _, err := v.Verify(context.Background(), Google, rotated); !errors.Is(err, ErrToken) || p.fetches.Load() != 1 {
		t.Fatalf("an unknown key right after a fetch: %v after %d fetches; want ErrToken without refetching", err, p.fetches.Load())
	}
	now = now.Add(2 * time.Minute)
	if _, err := v.Verify(context.Background(), Google, rotated); err != nil || p.fetches.Load() != 2 {
		t.Fatalf("a rotated key: %v after %d fetches; want it found by one refetch", err, p.fetches.Load())
	}
}

func TestLiveVerifierProviderOutage(t *testing.T) {
	p := newProvider(t)
	now := issued
	v := p.verifier(now)
	v.now = func() time.Time { return now }
	token := p.sign(rs256("k1"), googleClaims())
	if _, err := v.Verify(context.Background(), Google, token); err != nil {
		t.Fatal(err)
	}
	p.down.Store(true)
	now = now.Add(2 * time.Hour)
	claims := googleClaims()
	claims["exp"] = now.Add(time.Hour).Unix()
	if _, err := v.Verify(context.Background(), Google, p.sign(rs256("k1"), claims)); err != nil {
		t.Fatalf("a known key during an outage: %v; want it still accepted", err)
	}
	cold := p.verifier(now)
	if _, err := cold.Verify(context.Background(), Google, p.sign(rs256("k1"), claims)); !errors.Is(err, ErrKeys) {
		t.Fatalf("no keys and an outage: %v; want ErrKeys", err)
	}
}

func TestLiveVerifierBacksOffFailedFetches(t *testing.T) {
	p := newProvider(t)
	p.down.Store(true)
	now := issued
	v := p.verifier(now)
	v.now = func() time.Time { return now }
	token := p.sign(rs256("k1"), googleClaims())
	for range 3 {
		if _, err := v.Verify(context.Background(), Google, token); !errors.Is(err, ErrKeys) {
			t.Fatalf("an outage: %v; want ErrKeys", err)
		}
	}
	if p.fetches.Load() != 1 {
		t.Fatalf("%d fetches during one backoff window; want 1", p.fetches.Load())
	}
	now = now.Add(2 * keysRefetchGap)
	p.down.Store(false)
	if _, err := v.Verify(context.Background(), Google, token); err != nil || p.fetches.Load() != 2 {
		t.Fatalf("after the window: %v with %d fetches; want a successful refetch", err, p.fetches.Load())
	}
}

func TestLiveVerifierAcceptsEveryConfiguredAudience(t *testing.T) {
	p := newProvider(t)
	v := p.verifier(issued)
	v.clientIDs[Apple] = []string{"com.dad.app", "com.dad.web"}
	claims := googleClaims()
	claims["iss"], claims["aud"] = "https://appleid.apple.com", "com.dad.web"
	if _, err := v.Verify(context.Background(), Apple, p.sign(rs256("k1"), claims)); err != nil {
		t.Fatalf("the website's audience: %v", err)
	}
}
