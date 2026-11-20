package mail

import (
	"fmt"
	"strconv"
	"strings"

	"dad/donor-api/internal/credentials"
)

// FromEnvironment returns nil when mail is deliberately unconfigured. An
// invalid configuration's error names the variables to fix, never their values.
func FromEnvironment(getenv func(string) string) (*Settings, error) {
	s := Settings{
		Host:     getenv("DONOR_SMTP_HOST"),
		Port:     587,
		Username: getenv("DONOR_SMTP_USERNAME"),
		Password: getenv("DONOR_SMTP_PASSWORD"),
		Sender:   getenv("DONOR_EMAIL_FROM"),
		CAPath:   getenv("DONOR_SMTP_CA"),
	}
	keys := getenv("DONOR_EMAIL_ENCRYPTION_KEY")
	port := getenv("DONOR_SMTP_PORT")
	if s.Host == "" && s.Username == "" && s.Password == "" && s.Sender == "" && s.CAPath == "" && port == "" && keys == "" {
		return nil, nil
	}
	var invalid []string
	if port != "" {
		if value, err := strconv.Atoi(port); err != nil {
			invalid = append(invalid, "DONOR_SMTP_PORT")
		} else {
			s.Port = value
		}
	}
	for _, check := range []struct {
		name string
		ok   bool
	}{
		{"DONOR_SMTP_HOST", s.Host != "" && !strings.ContainsAny(s.Host, "\r\n")},
		// Port 465 needs implicit TLS, which is unsupported; use STARTTLS on 587.
		{"DONOR_SMTP_PORT", s.Port >= 1 && s.Port <= 65535 && s.Port != 465},
		{"DONOR_EMAIL_FROM", validAddress(s.Sender)},
		{"DONOR_SMTP_USERNAME and DONOR_SMTP_PASSWORD", (s.Username == "") == (s.Password == "")},
	} {
		if !check.ok {
			invalid = append(invalid, check.name)
		}
	}
	var err error
	if s.EncryptionKeys, err = credentials.ParseKeyring(keys); err != nil {
		invalid = append(invalid, "DONOR_EMAIL_ENCRYPTION_KEY ("+err.Error()+")")
	}
	// Fail fast on an unreadable or empty CA file rather than at the first send.
	if _, err := s.RootCAs(); err != nil {
		invalid = append(invalid, "DONOR_SMTP_CA")
	}
	if len(invalid) > 0 {
		return nil, fmt.Errorf("%w: check %s", ErrConfiguration, strings.Join(invalid, ", "))
	}
	return &s, nil
}
