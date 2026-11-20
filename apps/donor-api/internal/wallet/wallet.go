// Package wallet signs referral passes without sending donor data to a provider.
package wallet

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

var ErrConfiguration = errors.New("wallet configuration is invalid")
var ErrUnavailable = errors.New("wallet is not configured")

var (
	serviceAccountPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+@[a-zA-Z0-9.-]+\.iam\.gserviceaccount\.com$`)
	classIDPattern        = regexp.MustCompile(`^[0-9]+\.[a-zA-Z0-9._-]+$`)
	referralPathPattern   = regexp.MustCompile(`^/r/[a-zA-Z0-9_-]+$`)
)

const (
	monthlyAmount = "$30/month"
	disclosure    = "About a dollar a day · " + monthlyAmount + ". DAD launches in April 2027. Sandbox preview: no real donations."
	passHeadline  = "DAD sandbox referral code"
	passLogoText  = "DAD · Sandbox"
)

type Issuer struct {
	Apple  *Apple
	Google *Google
	Origin string
}

type Google struct {
	Email, ClassID string
	Key            *rsa.PrivateKey
}

func readFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrConfiguration
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	if err != nil || len(data) > 1024*1024 {
		return nil, ErrConfiguration
	}
	return data, nil
}

func privateKey(data []byte) (*rsa.PrivateKey, error) {
	block, rest := pem.Decode(data)
	if block == nil || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, ErrConfiguration
	}
	var key *rsa.PrivateKey
	if block.Type == "RSA PRIVATE KEY" {
		key, _ = x509.ParsePKCS1PrivateKey(block.Bytes)
	} else if block.Type == "PRIVATE KEY" {
		parsed, _ := x509.ParsePKCS8PrivateKey(block.Bytes)
		key, _ = parsed.(*rsa.PrivateKey)
	}
	if key == nil || key.N.BitLen() < 2048 || key.Validate() != nil {
		return nil, ErrConfiguration
	}
	return key, nil
}

// FromEnvironment loads the Apple and Google issuers whose settings are
// present. It returns nil when neither is configured, and an error when one
// is configured incompletely or the origin is not HTTPS.
func FromEnvironment(getenv func(string) string, origin string) (*Issuer, error) {
	result := &Issuer{Origin: origin}
	applePaths := []string{
		getenv("DONOR_WALLET_APPLE_CERT_FILE"),
		getenv("DONOR_WALLET_APPLE_KEY_FILE"),
		getenv("DONOR_WALLET_APPLE_WWDR_FILE"),
	}
	enabled := false
	for _, p := range applePaths {
		enabled = enabled || p != ""
	}
	if enabled {
		data := make([][]byte, 3)
		for i, p := range applePaths {
			var err error
			data[i], err = readFile(p)
			if err != nil {
				return nil, err
			}
		}
		apple, err := newApple(data[0], data[1], data[2], time.Now())
		if err != nil {
			return nil, err
		}
		result.Apple = apple
	}
	googlePath, classID := getenv("DONOR_WALLET_GOOGLE_KEY_FILE"), getenv("DONOR_WALLET_GOOGLE_CLASS_ID")
	if googlePath != "" || classID != "" {
		data, err := readFile(googlePath)
		if err != nil {
			return nil, err
		}
		var credentials struct {
			Type  string `json:"type"`
			Email string `json:"client_email"`
			Key   string `json:"private_key"`
		}
		if json.Unmarshal(data, &credentials) != nil || credentials.Type != "service_account" ||
			!serviceAccountPattern.MatchString(credentials.Email) ||
			!classIDPattern.MatchString(classID) {
			return nil, ErrConfiguration
		}
		key, err := privateKey([]byte(credentials.Key))
		if err != nil {
			return nil, err
		}
		result.Google = &Google{Email: credentials.Email, ClassID: classID, Key: key}
	}
	if result.Apple == nil && result.Google == nil {
		return nil, nil
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return nil, ErrConfiguration
	}
	return result, nil
}

func (i *Issuer) validate(link string) error {
	u, err := url.Parse(link)
	if err != nil || u.Scheme+"://"+u.Host != i.Origin || u.User != nil || u.Fragment != "" || !referralPathPattern.MatchString(u.Path) ||
		u.RawQuery != "via=irl" {
		return ErrConfiguration
	}
	return nil
}

func passID(link string) string {
	sum := sha256.Sum256([]byte(link))
	return hex.EncodeToString(sum[:])
}

func localized(value string) map[string]any {
	return map[string]any{"defaultValue": map[string]string{"language": "en-US", "value": value}}
}

// GoogleURL returns a signed Save to Google Wallet link for a pass whose QR
// code is link, the donor's IRL referral URL.
func (i *Issuer) GoogleURL(link string, now time.Time) (string, error) {
	if i == nil || i.Google == nil {
		return "", ErrUnavailable
	}
	if err := i.validate(link); err != nil {
		return "", err
	}
	g := i.Google
	object := map[string]any{
		"id": strings.SplitN(g.ClassID, ".", 2)[0] + ".dad_" + passID(link), "classId": g.ClassID, "state": "ACTIVE",
		"cardTitle": localized("Dynamic Aid Delivery"), "header": localized("Invite someone to DAD"),
		"subheader": localized("Sandbox referral code"), "hexBackgroundColor": "#FFFFFF",
		"barcode": map[string]string{"type": "QR_CODE", "value": link},
		"textModulesData": []map[string]string{
			{"id": "giving", "header": "Join the circle", "body": disclosure},
			{
				"id":     "replacement",
				"header": "Keep your code current",
				"body":   "If you replace your referral link, remove this pass and add the new one from your DAD account.",
			},
		},
	}
	payload, err := json.Marshal(
		map[string]any{
			"iss":     g.Email,
			"aud":     "google",
			"typ":     "savetowallet",
			"iat":     now.Unix(),
			"origins": []string{i.Origin},
			"payload": map[string]any{"genericObjects": []any{object}},
		},
	)
	if err != nil {
		return "", err
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	unsigned := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, g.Key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return "https://pay.google.com/gp/v/save/" + unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}
