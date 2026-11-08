// Package oidc verifies Apple and Google id_tokens for "Sign in with Apple /
// Google" (PRD §5.1). Like internal/sms it degrades: FromEnvironment returns
// nil when no provider is configured, so a deployment without client ids simply
// offers no social sign-in. A Verifier turns a provider's id_token into a
// verified Identity (stable subject); the store then links that identity to a
// donor.
//
// The live verifier (DONOR_OIDC_MODE=live) checks the RS256 signature against
// the provider's published keys, then the issuer, audience, and expiry, and
// passes the email on only when the provider marks it verified. The stub
// verifier (DONOR_OIDC_MODE=stub) accepts an HMAC-signed test token instead, so
// the sign-in and account-linking flow is exercisable without a provider; it
// must never be configured in production.
package oidc

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// ErrConfiguration names the variables to fix without echoing their values.
var ErrConfiguration = errors.New("invalid OIDC configuration")

// ErrToken is returned for any id_token that does not verify.
var ErrToken = errors.New("the sign-in token could not be verified")

// Provider is one social sign-in provider.
type Provider string

const (
	Apple  Provider = "APPLE"
	Google Provider = "GOOGLE"
)

// Valid reports whether p names a supported provider.
func (p Provider) Valid() bool { return p == Apple || p == Google }

// ParseProvider maps a path segment ("apple"/"google") to a Provider.
func ParseProvider(s string) (Provider, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "apple":
		return Apple, true
	case "google":
		return Google, true
	default:
		return "", false
	}
}

// Identity is a verified external account: the provider and its stable, opaque
// subject ('sub'). Email is carried for first-time account creation only and is
// never the identity key, since it can change.
type Identity struct {
	Provider Provider
	Subject  string
	Email    string
	// Nonce is the id_token's nonce claim, empty when the token has none.
	Nonce string
}

// Verifier validates a provider's id_token and returns the verified Identity.
type Verifier interface {
	Verify(ctx context.Context, provider Provider, idToken string) (Identity, error)
}

// Settings is a resolved OIDC configuration. ClientIDs are the audiences
// accepted per provider: an app and a website have different ones. Mode selects the verifier: "live" or "stub".
type Settings struct {
	Mode      string
	ClientIDs map[Provider][]string
	StubKey   string // HMAC key the stub verifier accepts; required in stub mode
	// RequireNonce rejects a sign-in that does not bind a nonce
	// (DONOR_OIDC_REQUIRE_NONCE). Off, a nonce is checked only when supplied.
	RequireNonce bool
}

// FromEnvironment reads the OIDC configuration, or nil when none is set.
func FromEnvironment(getenv func(string) string) (*Settings, error) {
	mode := strings.ToLower(strings.TrimSpace(getenv("DONOR_OIDC_MODE")))
	apple := clientIDs(getenv("DONOR_OIDC_APPLE_CLIENT_ID"))
	google := clientIDs(getenv("DONOR_OIDC_GOOGLE_CLIENT_ID"))
	stubKey := getenv("DONOR_OIDC_STUB_KEY")
	if mode == "" && apple == nil && google == nil && stubKey == "" {
		return nil, nil
	}
	s := Settings{Mode: mode, ClientIDs: map[Provider][]string{}, StubKey: stubKey}
	switch strings.ToLower(strings.TrimSpace(getenv("DONOR_OIDC_REQUIRE_NONCE"))) {
	case "", "false":
	case "true":
		s.RequireNonce = true
	default:
		return nil, fmt.Errorf("%w: check DONOR_OIDC_REQUIRE_NONCE (expected: true or false)", ErrConfiguration)
	}
	if apple != nil {
		s.ClientIDs[Apple] = apple
	}
	if google != nil {
		s.ClientIDs[Google] = google
	}
	var invalid []string
	switch mode {
	case "stub":
		if stubKey == "" {
			invalid = append(invalid, "DONOR_OIDC_STUB_KEY")
		}
	case "live":
	default:
		invalid = append(invalid, "DONOR_OIDC_MODE (expected: live or stub)")
	}
	if len(s.ClientIDs) == 0 {
		invalid = append(invalid, "DONOR_OIDC_APPLE_CLIENT_ID or DONOR_OIDC_GOOGLE_CLIENT_ID")
	}
	if len(invalid) > 0 {
		return nil, fmt.Errorf("%w: check %s", ErrConfiguration, strings.Join(invalid, ", "))
	}
	return &s, nil
}

// clientIDs splits a comma-separated list of client ids, or returns nil.
func clientIDs(value string) []string {
	var ids []string
	for id := range strings.SplitSeq(value, ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// NewVerifier builds the Verifier for a resolved configuration.
func NewVerifier(s *Settings) Verifier {
	if s == nil {
		return nil
	}
	switch s.Mode {
	case "stub":
		return &stubVerifier{clientIDs: s.ClientIDs, key: s.StubKey}
	case "live":
		return newLiveVerifier(s.ClientIDs)
	default:
		return nil
	}
}

// Configured reports whether a provider is accepted by this configuration.
func (s *Settings) Configured(p Provider) bool {
	if s == nil {
		return false
	}
	_, ok := s.ClientIDs[p]
	return ok
}

// NonceMatches reports whether an id_token's nonce claim answers the nonce a
// client supplied. Google Credential Manager and web flows put the raw value in
// the token; Sign in with Apple's native flow puts the lowercase hex SHA-256 of
// it, so both forms are accepted. Both comparisons always run, in constant
// time, so the answer reveals neither form nor how much of a guess matched.
func NonceMatches(claim, supplied string) bool {
	if claim == "" || supplied == "" {
		return false
	}
	sum := sha256.Sum256([]byte(supplied))
	raw := subtle.ConstantTimeCompare([]byte(claim), []byte(supplied))
	hashed := subtle.ConstantTimeCompare([]byte(claim), []byte(hex.EncodeToString(sum[:])))
	return raw|hashed == 1
}

// NonceAccepted applies the nonce policy to a verified identity: a supplied
// nonce must match the token's claim, and an absent one is allowed only while
// RequireNonce is off.
func (s *Settings) NonceAccepted(ident Identity, supplied string) bool {
	if supplied == "" {
		return !s.RequireNonce
	}
	return NonceMatches(ident.Nonce, supplied)
}
