package oidc

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"time"
)

// stubVerifier validates an HMAC-SHA256-signed, JWT-shaped token so the sign-in
// flow is exercisable without a live provider. The token is
// base64url(header).base64url(claims).base64url(HMAC(key, header.claims)); the
// verifier checks the signature, issuer, audience and expiry exactly as the
// live verifier will, so only the signature mechanism differs.
type stubVerifier struct {
	clientIDs map[Provider][]string
	key       string
}

// stubIssuers names the issuer the stub expects per provider, matching the real
// providers' iss values so claims are realistic.
var stubIssuers = map[Provider]string{
	Apple:  "https://appleid.apple.com",
	Google: "https://accounts.google.com",
}

type stubClaims struct {
	Iss   string `json:"iss"`
	Sub   string `json:"sub"`
	Aud   string `json:"aud"`
	Exp   int64  `json:"exp"`
	Email string `json:"email"`
	Nonce string `json:"nonce,omitempty"`
}

func (v *stubVerifier) Verify(_ context.Context, provider Provider, idToken string) (Identity, error) {
	if !provider.Valid() {
		return Identity{}, ErrToken
	}
	audiences, ok := v.clientIDs[provider]
	if !ok {
		return Identity{}, ErrToken
	}
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return Identity{}, ErrToken
	}
	mac := hmac.New(sha256.New, []byte(v.key))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	// Constant-time compare so a bad token cannot be probed by timing.
	if subtle.ConstantTimeCompare([]byte(want), []byte(parts[2])) != 1 {
		return Identity{}, ErrToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Identity{}, ErrToken
	}
	var claims stubClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return Identity{}, ErrToken
	}
	if claims.Iss != stubIssuers[provider] || !slices.Contains(audiences, claims.Aud) || claims.Sub == "" {
		return Identity{}, ErrToken
	}
	if claims.Exp <= 0 || time.Now().After(time.Unix(claims.Exp, 0)) {
		return Identity{}, ErrToken
	}
	return Identity{Provider: provider, Subject: claims.Sub, Email: claims.Email, Nonce: claims.Nonce}, nil
}

// SignStubToken builds a token the stub verifier accepts. It is exported for
// tests and local tooling only; a real client never calls it.
func SignStubToken(key string, provider Provider, subject, clientID, email string, exp time.Time) string {
	return SignStubTokenWithNonce(key, provider, subject, clientID, email, "", exp)
}

// SignStubTokenWithNonce is SignStubToken with a nonce claim.
func SignStubTokenWithNonce(key string, provider Provider, subject, clientID, email, nonce string, exp time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	claims, _ := json.Marshal(stubClaims{
		Iss: stubIssuers[provider], Sub: subject, Aud: clientID, Exp: exp.Unix(), Email: email, Nonce: nonce,
	})
	body := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
