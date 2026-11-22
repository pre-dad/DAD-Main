// Package sms sends one-time sign-in codes over SMS. It mirrors internal/mail:
// FromEnvironment returns nil when SMS is deliberately unconfigured, so a
// deployment without an SMS provider simply offers no phone sign-in rather than
// failing. The Sender interface is what the store depends on; the concrete
// provider, Twilio or a recording stub for development, is injected.
package sms

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// ErrConfiguration names the variables to fix; it never echoes their values.
var ErrConfiguration = errors.New("invalid SMS configuration")

// ErrUnavailable is returned by a Sender that cannot deliver right now.
var ErrUnavailable = errors.New("SMS delivery is unavailable")

// Sender delivers a sign-in code to a phone in E.164 form. The message text is
// composed by the caller so the wording lives with the rest of the app's copy.
type Sender interface {
	Send(ctx context.Context, phone, message string) error
}

// Settings is a resolved SMS configuration. Provider selects the gateway:
// "twilio", or "stub", which records messages instead of sending them and must
// never be configured in production.
type Settings struct {
	Provider  string
	AccountID string
	AuthToken string
	Sender    string // the From number or alphanumeric sender id
}

// FromEnvironment reads the SMS configuration, or returns nil when none is set
// (phone sign-in then stays off). An invalid configuration errors naming the
// offending variables.
func FromEnvironment(getenv func(string) string) (*Settings, error) {
	s := Settings{
		Provider:  strings.ToLower(strings.TrimSpace(getenv("DONOR_SMS_PROVIDER"))),
		AccountID: getenv("DONOR_SMS_ACCOUNT_ID"),
		AuthToken: getenv("DONOR_SMS_AUTH_TOKEN"),
		Sender:    getenv("DONOR_SMS_FROM"),
	}
	if s.Provider == "" && s.AccountID == "" && s.AuthToken == "" && s.Sender == "" {
		return nil, nil
	}
	var invalid []string
	switch s.Provider {
	case "stub":
		// The stub needs no credentials; a sender id is still required so the
		// shape matches a real provider and the From copy is exercised.
		if s.Sender == "" {
			invalid = append(invalid, "DONOR_SMS_FROM")
		}
	case "twilio":
		if !twilioAccount.MatchString(s.AccountID) {
			invalid = append(invalid, "DONOR_SMS_ACCOUNT_ID")
		}
		if s.AuthToken == "" {
			invalid = append(invalid, "DONOR_SMS_AUTH_TOKEN")
		}
		if !twilioSender.MatchString(s.Sender) {
			invalid = append(invalid, "DONOR_SMS_FROM")
		}
	default:
		invalid = append(invalid, "DONOR_SMS_PROVIDER (expected: twilio or stub)")
	}
	if len(invalid) > 0 {
		return nil, fmt.Errorf("%w: check %s", ErrConfiguration, strings.Join(invalid, ", "))
	}
	return &s, nil
}

// NewSender builds the Sender for a resolved configuration.
func NewSender(s *Settings) Sender {
	if s == nil {
		return nil
	}
	switch s.Provider {
	case "stub":
		return &StubSender{}
	case "twilio":
		return newTwilioSender(s)
	default:
		return nil
	}
}

// StubSender records the most recent message per phone instead of sending it,
// so development and tests can read back the code. It is safe for concurrent
// use. It is not a gateway: production must configure Twilio.
type StubSender struct {
	mu   sync.Mutex
	last map[string]string
}

// Send records message as the latest for phone.
func (s *StubSender) Send(_ context.Context, phone, message string) error {
	if phone == "" || message == "" {
		return ErrUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		s.last = map[string]string{}
	}
	s.last[phone] = message
	return nil
}

// Last returns the most recent message sent to phone, for tests and local dev.
func (s *StubSender) Last(phone string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	msg, ok := s.last[phone]
	return msg, ok
}
