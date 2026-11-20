package mail

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEnvironmentConfiguration(t *testing.T) {
	key := base64.URLEncoding.EncodeToString(make([]byte, 32))
	valid := map[string]string{
		"DONOR_SMTP_HOST":            "smtp.example.org",
		"DONOR_EMAIL_FROM":           "DAD <sender@example.org>",
		"DONOR_EMAIL_ENCRYPTION_KEY": key,
	}
	for _, tc := range []struct {
		name, field, value string
		invalid            bool
	}{
		{"defaults", "", "", false},
		{"custom port", "DONOR_SMTP_PORT", "2525", false},
		{"bad port", "DONOR_SMTP_PORT", "abc", true},
		{"port range", "DONOR_SMTP_PORT", "65536", true},
		{"implicit TLS port", "DONOR_SMTP_PORT", "465", true},
		{"missing host", "DONOR_SMTP_HOST", "", true},
		{"bad sender", "DONOR_EMAIL_FROM", "not-an-address", true},
		{"header injection", "DONOR_EMAIL_FROM", "sender@example.org\r\nBcc: victim@example.org", true},
		{"invalid key", "DONOR_EMAIL_ENCRYPTION_KEY", "configured", true},
		{"key list", "DONOR_EMAIL_ENCRYPTION_KEY", base64.URLEncoding.EncodeToString([]byte(strings.Repeat("n", 32))) + "," + key, false},
		{"invalid retired key", "DONOR_EMAIL_ENCRYPTION_KEY", key + ",configured", true},
		{"partial credentials", "DONOR_SMTP_USERNAME", "sender", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := FromEnvironment(func(key string) string {
				if key == tc.field {
					return tc.value
				}
				return valid[key]
			})
			if tc.invalid {
				if !errors.Is(err, ErrConfiguration) || s != nil {
					t.Fatalf("invalid settings accepted: %v", err)
				}
				if want := tc.field; !strings.Contains(err.Error(), want) || (tc.value != "" && strings.Contains(err.Error(), tc.value)) {
					t.Fatalf("got %q, want it to name %s without its value", err, want)
				}
				if tc.name == "invalid retired key" &&
					(!strings.Contains(err.Error(), "key 2 of 2") || strings.Contains(err.Error(), key)) {
					t.Fatalf("got %q, want it to name the second key's position without key material", err)
				}
				return
			}
			if err != nil || s == nil {
				t.Fatalf("valid settings rejected: %v", err)
			}
			wantPort := 587
			if tc.name == "custom port" {
				wantPort = 2525
			}
			if s.Port != wantPort {
				t.Fatalf("port = %d", s.Port)
			}
		})
	}
	s, err := FromEnvironment(func(string) string { return "" })
	if s != nil || err != nil {
		t.Fatal("unconfigured mail should be disabled")
	}
	for _, only := range []string{"DONOR_SMTP_CA", "DONOR_SMTP_PORT"} {
		s, err = FromEnvironment(func(key string) string {
			if key == only {
				return "2525"
			}
			return ""
		})
		if s != nil || !errors.Is(err, ErrConfiguration) {
			t.Fatalf("%s alone: got %v, %v; want a configuration error", only, s, err)
		}
	}
}

func TestRootCAsFromCAPath(t *testing.T) {
	// A self-signed CA PEM, as a local Mailpit cert would be.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "mailpit.local"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	notPEM := filepath.Join(dir, "garbage.pem")
	if err := os.WriteFile(notPEM, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("unset means system roots", func(t *testing.T) {
		pool, err := Settings{}.RootCAs()
		if err != nil || pool != nil {
			t.Fatalf("got pool=%v err=%v, want nil pool and no error", pool, err)
		}
	})
	t.Run("valid PEM builds a pool", func(t *testing.T) {
		pool, err := Settings{CAPath: certPath}.RootCAs()
		if err != nil || pool == nil {
			t.Fatalf("got pool=%v err=%v, want a pool and no error", pool, err)
		}
	})
	t.Run("missing file is a configuration error", func(t *testing.T) {
		if _, err := (Settings{CAPath: filepath.Join(dir, "absent.pem")}).RootCAs(); !errors.Is(err, ErrConfiguration) {
			t.Fatalf("got %v, want ErrConfiguration", err)
		}
	})
	t.Run("non-certificate content is a configuration error", func(t *testing.T) {
		if _, err := (Settings{CAPath: notPEM}).RootCAs(); !errors.Is(err, ErrConfiguration) {
			t.Fatalf("got %v, want ErrConfiguration", err)
		}
	})

	// FromEnvironment surfaces a bad CA path by name, without its value.
	key32 := base64.URLEncoding.EncodeToString(make([]byte, 32))
	env := map[string]string{
		"DONOR_SMTP_HOST": "smtp.example.org", "DONOR_EMAIL_FROM": "DAD <sender@example.org>",
		"DONOR_EMAIL_ENCRYPTION_KEY": key32, "DONOR_SMTP_CA": notPEM,
	}
	_, err = FromEnvironment(func(k string) string { return env[k] })
	if !errors.Is(err, ErrConfiguration) || !strings.Contains(err.Error(), "DONOR_SMTP_CA") || strings.Contains(err.Error(), notPEM) {
		t.Fatalf("got %v, want it to name DONOR_SMTP_CA without the path", err)
	}
}
