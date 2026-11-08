package oidc

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestFromEnvironmentUnconfigured(t *testing.T) {
	s, err := FromEnvironment(env(nil))
	if s != nil || err != nil {
		t.Fatalf("unconfigured = %v, %v; want nil, nil", s, err)
	}
}

func TestFromEnvironmentStub(t *testing.T) {
	s, err := FromEnvironment(env(map[string]string{
		"DONOR_OIDC_MODE": "stub", "DONOR_OIDC_STUB_KEY": "k",
		"DONOR_OIDC_APPLE_CLIENT_ID": "com.dad.app", "DONOR_OIDC_GOOGLE_CLIENT_ID": "g.apps",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !s.Configured(Apple) || !s.Configured(Google) {
		t.Fatalf("providers not configured: %+v", s)
	}
	if NewVerifier(s) == nil {
		t.Fatal("NewVerifier(stub) = nil")
	}
}

func TestFromEnvironmentInvalid(t *testing.T) {
	for _, m := range []map[string]string{
		{"DONOR_OIDC_MODE": "stub", "DONOR_OIDC_APPLE_CLIENT_ID": "a"}, // missing stub key
		{"DONOR_OIDC_MODE": "stub", "DONOR_OIDC_STUB_KEY": "k"},        // no client ids
		{"DONOR_OIDC_MODE": "live"},                                    // no client ids
		{"DONOR_OIDC_MODE": "nope", "DONOR_OIDC_APPLE_CLIENT_ID": "a", "DONOR_OIDC_STUB_KEY": "k"},
	} {
		if _, err := FromEnvironment(env(m)); !errors.Is(err, ErrConfiguration) {
			t.Errorf("FromEnvironment(%v) error = %v; want ErrConfiguration", m, err)
		}
	}
}

func TestFromEnvironmentLive(t *testing.T) {
	s, err := FromEnvironment(env(map[string]string{"DONOR_OIDC_MODE": "live", "DONOR_OIDC_GOOGLE_CLIENT_ID": "g.apps"}))
	if err != nil || s == nil || !s.Configured(Google) || s.Configured(Apple) {
		t.Fatalf("live settings: %+v %v", s, err)
	}
	if _, ok := NewVerifier(s).(*liveVerifier); !ok {
		t.Fatal("NewVerifier(live) is not the live verifier")
	}
}

func stubSettings() *Settings {
	return &Settings{Mode: "stub", StubKey: "secret", ClientIDs: map[Provider][]string{
		Apple: {"com.dad.app"}, Google: {"g.apps"},
	}}
}

func TestStubVerifierValid(t *testing.T) {
	v := NewVerifier(stubSettings())
	token := SignStubToken("secret", Google, "sub-123", "g.apps", "d@example.org", time.Now().Add(time.Hour))
	id, err := v.Verify(context.Background(), Google, token)
	if err != nil {
		t.Fatal(err)
	}
	if id.Provider != Google || id.Subject != "sub-123" || id.Email != "d@example.org" {
		t.Fatalf("identity = %+v", id)
	}
}

func TestStubVerifierRejections(t *testing.T) {
	v := NewVerifier(stubSettings())
	ctx := context.Background()

	// Wrong signing key.
	bad := SignStubToken("wrong", Apple, "s", "com.dad.app", "", time.Now().Add(time.Hour))
	if _, err := v.Verify(ctx, Apple, bad); !errors.Is(err, ErrToken) {
		t.Errorf("bad signature error = %v; want ErrToken", err)
	}
	// Wrong audience (client id).
	wrongAud := SignStubToken("secret", Apple, "s", "someone.else", "", time.Now().Add(time.Hour))
	if _, err := v.Verify(ctx, Apple, wrongAud); !errors.Is(err, ErrToken) {
		t.Errorf("wrong aud error = %v; want ErrToken", err)
	}
	// Expired.
	expired := SignStubToken("secret", Apple, "s", "com.dad.app", "", time.Now().Add(-time.Hour))
	if _, err := v.Verify(ctx, Apple, expired); !errors.Is(err, ErrToken) {
		t.Errorf("expired error = %v; want ErrToken", err)
	}
	// Token for one provider presented to another (issuer mismatch).
	appleTok := SignStubToken("secret", Apple, "s", "com.dad.app", "", time.Now().Add(time.Hour))
	if _, err := v.Verify(ctx, Google, appleTok); !errors.Is(err, ErrToken) {
		t.Errorf("provider mismatch error = %v; want ErrToken", err)
	}
	// Malformed.
	if _, err := v.Verify(ctx, Apple, "not.a.token.at.all"); !errors.Is(err, ErrToken) {
		t.Errorf("malformed error = %v; want ErrToken", err)
	}
}

func TestFromEnvironmentAcceptsSeveralClientIDs(t *testing.T) {
	s, err := FromEnvironment(env(map[string]string{
		"DONOR_OIDC_MODE": "live", "DONOR_OIDC_APPLE_CLIENT_ID": " org.dad.app , org.dad.web,, ",
	}))
	if err != nil || !slices.Equal(s.ClientIDs[Apple], []string{"org.dad.app", "org.dad.web"}) {
		t.Fatalf("client ids: %+v %v", s, err)
	}
	if _, err := FromEnvironment(env(map[string]string{"DONOR_OIDC_MODE": "live", "DONOR_OIDC_APPLE_CLIENT_ID": " , "})); !errors.Is(
		err,
		ErrConfiguration,
	) {
		t.Fatalf("a list of blanks: %v; want ErrConfiguration", err)
	}
}

func TestNonceMatches(t *testing.T) {
	const raw = "client-nonce-0123456789"
	sum := sha256.Sum256([]byte(raw))
	hashed := hex.EncodeToString(sum[:])
	for _, tc := range []struct {
		name, claim, supplied string
		want                  bool
	}{
		{"raw form (Google, web)", raw, raw, true},
		{"lowercase hex SHA-256 form (Apple native)", hashed, raw, true},
		{"uppercase hex is not accepted", strings.ToUpper(hashed), raw, false},
		{"base64 digest is not accepted", base64.StdEncoding.EncodeToString(sum[:]), raw, false},
		{"different nonce", raw, "other-nonce-0123456789", false},
		{"hash of a different nonce", hashed, "other-nonce-0123456789", false},
		{"claim is a prefix", raw[:10], raw, false},
		{"token lacks the claim", "", raw, false},
		{"empty supplied nonce never matches", "", "", false},
		{"supplied empty, claim set", raw, "", false},
	} {
		if got := NonceMatches(tc.claim, tc.supplied); got != tc.want {
			t.Errorf("%s: NonceMatches = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestNonceAcceptedByMode(t *testing.T) {
	optional, required := &Settings{}, &Settings{RequireNonce: true}
	tagged := Identity{Nonce: "n-0123456789"}
	for _, tc := range []struct {
		name     string
		settings *Settings
		ident    Identity
		supplied string
		want     bool
	}{
		{"optional, none sent", optional, Identity{}, "", true},
		{"optional, none sent, token has one", optional, tagged, "", true},
		{"optional, matching", optional, tagged, "n-0123456789", true},
		{"optional, mismatched", optional, tagged, "other", false},
		{"optional, sent but token lacks claim", optional, Identity{}, "n-0123456789", false},
		{"required, none sent", required, tagged, "", false},
		{"required, matching", required, tagged, "n-0123456789", true},
		{"required, mismatched", required, tagged, "other", false},
	} {
		if got := tc.settings.NonceAccepted(tc.ident, tc.supplied); got != tc.want {
			t.Errorf("%s: NonceAccepted = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestFromEnvironmentRequireNonce(t *testing.T) {
	base := map[string]string{"DONOR_OIDC_MODE": "live", "DONOR_OIDC_GOOGLE_CLIENT_ID": "g.apps"}
	for value, want := range map[string]bool{"": false, "false": false, "true": true, " TRUE ": true} {
		m := maps.Clone(base)
		m["DONOR_OIDC_REQUIRE_NONCE"] = value
		s, err := FromEnvironment(env(m))
		if err != nil || s.RequireNonce != want {
			t.Errorf("DONOR_OIDC_REQUIRE_NONCE=%q: %+v %v, want RequireNonce=%v", value, s, err, want)
		}
	}
	m := maps.Clone(base)
	m["DONOR_OIDC_REQUIRE_NONCE"] = "yes"
	if _, err := FromEnvironment(env(m)); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("invalid value: %v; want ErrConfiguration", err)
	}
}

func TestStubVerifierCarriesNonce(t *testing.T) {
	token := SignStubTokenWithNonce("secret", Google, "s", "g.apps", "", "n-1", time.Now().Add(time.Hour))
	id, err := NewVerifier(stubSettings()).Verify(context.Background(), Google, token)
	if err != nil || id.Nonce != "n-1" {
		t.Fatalf("identity = %+v, %v; want nonce n-1", id, err)
	}
}
