package push

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"sync"
	"time"
)

var (
	appleIDPattern = regexp.MustCompile(`^[A-Z0-9]{10}$`)
	topicPattern   = regexp.MustCompile(`^[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+$`)
)

// apnsTokenLifetime reuses a provider token for most of the hour APNs
// accepts it: Apple refuses tokens renewed more often than every 20 minutes.
const apnsTokenLifetime = 40 * time.Minute

// APNs sends through Apple's HTTP/2 provider API with token-based
// authentication: an ES256 JWT signed with the team's .p8 key.
type APNs struct {
	key                  *ecdsa.PrivateKey
	keyID, teamID, topic string

	Client                    *http.Client
	ProductionURL, SandboxURL string

	mu     sync.Mutex
	token  string
	issued time.Time
	now    func() time.Time
}

// NewAPNs loads an APNs authentication key (the PKCS#8 .p8 file), its
// 10-character key ID, the team ID, and the app's bundle ID as the topic.
func NewAPNs(keyPEM []byte, keyID, teamID, topic string) (*APNs, error) {
	if !appleIDPattern.MatchString(keyID) || !appleIDPattern.MatchString(teamID) || !topicPattern.MatchString(topic) {
		return nil, fmt.Errorf(
			"%w: DONOR_PUSH_APNS_KEY_ID and DONOR_PUSH_APNS_TEAM_ID must be 10-character Apple IDs and DONOR_PUSH_APNS_TOPIC the app's bundle ID",
			ErrConfiguration,
		)
	}
	block, rest := pem.Decode(keyPEM)
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("%w: DONOR_PUSH_APNS_KEY_FILE must hold one PEM private key", ErrConfiguration)
	}
	parsed, _ := x509.ParsePKCS8PrivateKey(block.Bytes)
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("%w: DONOR_PUSH_APNS_KEY_FILE must be an APNs P-256 authentication key", ErrConfiguration)
	}
	return &APNs{
		key: key, keyID: keyID, teamID: teamID, topic: topic, Client: newClient(),
		ProductionURL: "https://api.push.apple.com", SandboxURL: "https://api.sandbox.push.apple.com",
		now: time.Now,
	}, nil
}

// bearer returns the current provider token, signing a new one when it is due.
func (a *APNs) bearer() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if a.token != "" && now.Sub(a.issued) < apnsTokenLifetime {
		return a.token, nil
	}
	token, err := jwt(
		map[string]string{"alg": "ES256", "kid": a.keyID},
		map[string]any{"iss": a.teamID, "iat": now.Unix()},
		func(digest []byte) ([]byte, error) {
			r, s, err := ecdsa.Sign(rand.Reader, a.key, digest)
			if err != nil {
				return nil, err
			}
			// JWS encodes an ES256 signature as fixed-width r and s, not ASN.1.
			signature := make([]byte, 64)
			r.FillBytes(signature[:32])
			s.FillBytes(signature[32:])
			return signature, nil
		},
	)
	if err != nil {
		return "", err
	}
	a.token, a.issued = token, now
	return token, nil
}

// forget discards a token APNs refused, so the next send signs another.
func (a *APNs) forget(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token == token {
		a.token = ""
	}
}

func (a *APNs) Send(ctx context.Context, device Device, n Notification) error {
	base := a.ProductionURL
	if device.Environment == "SANDBOX" {
		base = a.SandboxURL
	}
	body, err := json.Marshal(map[string]any{
		"aps":      map[string]any{"alert": map[string]string{"title": n.Title, "body": n.Body}, "sound": "default"},
		"category": n.Category,
		"path":     n.Path,
	})
	if err != nil {
		return err
	}
	token, err := a.bearer()
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/3/device/"+url.PathEscape(device.Token), bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("apns-topic", a.topic)
	request.Header.Set("apns-push-type", "alert")
	request.Header.Set("apns-priority", "10")
	request.Header.Set("apns-expiration", strconv.FormatInt(n.Expires.Unix(), 10))
	request.Header.Set("apns-collapse-id", n.ID)
	response, err := a.Client.Do(request)
	if err != nil {
		// A *url.Error prints the request URL, which carries the device token.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return fmt.Errorf("apns transport: %w", err)
	}
	defer response.Body.Close()
	var reply struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(readResponse(response), &reply)
	switch {
	case response.StatusCode == http.StatusOK:
		return nil
	// Only a verdict on the token itself drops it. DeviceTokenNotForTopic is
	// left to configuration, since a wrong topic would otherwise drop every
	// token.
	case response.StatusCode == http.StatusGone, reply.Reason == "BadDeviceToken":
		return ErrUnregistered
	case response.StatusCode == http.StatusForbidden:
		a.forget(token)
		if reply.Reason == "ExpiredProviderToken" {
			return Unavailable{}
		}
		return ErrRejected
	}
	if err := throttled(response); err != nil {
		return err
	}
	return ErrRejected
}
