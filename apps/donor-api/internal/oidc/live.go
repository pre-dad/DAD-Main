package oidc

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// providerKeys are the published JWKS endpoints. Apple and Google both sign
// id_tokens with RS256.
var providerKeys = map[Provider]string{
	Apple:  "https://appleid.apple.com/auth/keys",
	Google: "https://www.googleapis.com/oauth2/v3/certs",
}

// providerIssuers lists the iss values each provider uses; Google documents
// both forms.
var providerIssuers = map[Provider][]string{
	Apple:  {"https://appleid.apple.com"},
	Google: {"https://accounts.google.com", "accounts.google.com"},
}

const (
	// clockSkew tolerates small differences between our clock and the
	// provider's when checking exp and iat.
	clockSkew = time.Minute
	// keysLifetime bounds how long a fetched key set is trusted before the
	// next token refetches it; providers rotate keys over days.
	keysLifetime = time.Hour
	// keysRefetchGap stops a stream of tokens naming unknown keys from making
	// a fetch each.
	keysRefetchGap = time.Minute
	maxKeysBody    = 1 << 20
)

// liveVerifier verifies provider id_tokens against the provider's published
// signing keys.
type liveVerifier struct {
	clientIDs map[Provider][]string
	keyURLs   map[Provider]string
	client    *http.Client
	now       func() time.Time

	mu     sync.Mutex
	sets   map[Provider]*keySet
	failed map[Provider]time.Time
}

type keySet struct {
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
}

func newLiveVerifier(clientIDs map[Provider][]string) *liveVerifier {
	return &liveVerifier{
		clientIDs: clientIDs,
		keyURLs:   providerKeys,
		client:    &http.Client{Timeout: 5 * time.Second},
		now:       time.Now,
		sets:      map[Provider]*keySet{},
		failed:    map[Provider]time.Time{},
	}
}

type liveClaims struct {
	Iss string `json:"iss"`
	Sub string `json:"sub"`
	// Aud is a string, or an array holding our client id.
	Aud   json.RawMessage `json:"aud"`
	Azp   string          `json:"azp"`
	Exp   int64           `json:"exp"`
	Iat   int64           `json:"iat"`
	Email string          `json:"email"`
	HD    string          `json:"hd"`
	// EmailVerified is a boolean from Google and the string "true" or
	// "false" from Apple.
	EmailVerified json.RawMessage `json:"email_verified"`
}

func (v *liveVerifier) Verify(ctx context.Context, provider Provider, idToken string) (Identity, error) {
	audiences, ok := v.clientIDs[provider]
	if !provider.Valid() || !ok {
		return Identity{}, ErrToken
	}
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return Identity{}, ErrToken
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if !decodeSegment(parts[0], &header) || header.Alg != "RS256" || header.Kid == "" {
		return Identity{}, ErrToken
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Identity{}, ErrToken
	}
	key, err := v.key(ctx, provider, header.Kid)
	if err != nil {
		return Identity{}, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature) != nil {
		return Identity{}, ErrToken
	}
	var claims liveClaims
	if !decodeSegment(parts[1], &claims) {
		return Identity{}, ErrToken
	}
	now := v.now()
	if !issuedBy(provider, claims.Iss) || !audienceHolds(claims.Aud, audiences) || !authorizedParty(claims, audiences) ||
		claims.Sub == "" || len(claims.Sub) > 255 || strings.ContainsRune(claims.Sub, 0) ||
		claims.Exp <= 0 || now.After(time.Unix(claims.Exp, 0).Add(clockSkew)) ||
		claims.Iat <= 0 || claims.Iat >= claims.Exp ||
		time.Unix(claims.Iat, 0).After(now.Add(clockSkew)) {
		return Identity{}, ErrToken
	}
	identity := Identity{Provider: provider, Subject: claims.Sub}
	// An unverified address must never link a provider account to the donor
	// who owns that address.
	if emailVerified(claims.EmailVerified) && (provider != Google || claims.HD != "" ||
		strings.HasSuffix(strings.ToLower(claims.Email), "@gmail.com")) {
		identity.Email = claims.Email
	}
	return identity, nil
}

func authorizedParty(claims liveClaims, audiences []string) bool {
	if claims.Azp != "" {
		return slices.Contains(audiences, claims.Azp)
	}
	var many []string
	return json.Unmarshal(claims.Aud, &many) != nil || len(many) <= 1
}

// key returns the provider's signing key kid, refetching the key set when it
// is stale or does not hold kid (the provider rotated keys).
func (v *liveVerifier) key(ctx context.Context, provider Provider, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	set := v.sets[provider]
	now := v.now()
	if set != nil && now.Sub(set.fetchedAt) < keysLifetime {
		if key, ok := set.keys[kid]; ok {
			return key, nil
		}
		if now.Sub(set.fetchedAt) < keysRefetchGap {
			return nil, ErrToken
		}
	}
	// A provider outage should not end sign-in while the key is still known,
	// nor make every sign-in wait out a failing fetch while holding the lock.
	stale := func(err error) (*rsa.PublicKey, error) {
		if set != nil {
			if key, ok := set.keys[kid]; ok {
				return key, nil
			}
		}
		return nil, err
	}
	if failedAt, ok := v.failed[provider]; ok && now.Sub(failedAt) < keysRefetchGap {
		return stale(ErrKeys)
	}
	keys, err := v.fetch(ctx, provider)
	if err == nil && len(keys) == 0 {
		err = fmt.Errorf("%w: no usable signing keys", ErrKeys)
	}
	if err != nil {
		// A caller that gave up says nothing about the provider, and must not
		// hold every other sign-in off it for a backoff window.
		if ctx.Err() == nil {
			v.failed[provider] = now
		}
		return stale(err)
	}
	delete(v.failed, provider)
	v.sets[provider] = &keySet{keys: keys, fetchedAt: now}
	if key, ok := keys[kid]; ok {
		return key, nil
	}
	return nil, ErrToken
}

// ErrKeys is returned when a provider's signing keys cannot be fetched, so the
// caller can answer "try again" rather than "invalid token".
var ErrKeys = errors.New("the sign-in provider's keys could not be fetched")

func (v *liveVerifier) fetch(ctx context.Context, provider Provider) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.keyURLs[provider], nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrKeys, err)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrKeys, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", ErrKeys, resp.StatusCode)
	}
	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			Alg string `json:"alg"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxKeysBody)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrKeys, err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || k.Kid == "" || (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != "RS256") {
			continue
		}
		n, errN := base64.RawURLEncoding.DecodeString(k.N)
		e, errE := base64.RawURLEncoding.DecodeString(k.E)
		if errN != nil || errE != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		key := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		if key.N.BitLen() < 2048 {
			continue
		}
		keys[k.Kid] = key
	}
	return keys, nil
}

func decodeSegment(segment string, into any) bool {
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	return err == nil && json.Unmarshal(raw, into) == nil
}

func issuedBy(provider Provider, iss string) bool {
	for _, want := range providerIssuers[provider] {
		if iss == want {
			return true
		}
	}
	return false
}

func audienceHolds(raw json.RawMessage, audiences []string) bool {
	var single string
	if json.Unmarshal(raw, &single) == nil {
		return slices.Contains(audiences, single)
	}
	var many []string
	if json.Unmarshal(raw, &many) != nil {
		return false
	}
	return slices.ContainsFunc(many, func(aud string) bool { return slices.Contains(audiences, aud) })
}

func emailVerified(raw json.RawMessage) bool {
	var flag bool
	if json.Unmarshal(raw, &flag) == nil {
		return flag
	}
	var text string
	return json.Unmarshal(raw, &text) == nil && text == "true"
}
