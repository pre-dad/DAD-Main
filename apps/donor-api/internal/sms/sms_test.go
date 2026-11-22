package sms

import (
	"context"
	"errors"
	"testing"
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
		"DONOR_SMS_PROVIDER": "stub", "DONOR_SMS_FROM": "DAD",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if s == nil || s.Provider != "stub" || s.Sender != "DAD" {
		t.Fatalf("stub settings = %+v", s)
	}
	if NewSender(s) == nil {
		t.Fatal("NewSender(stub) = nil")
	}
}

func TestFromEnvironmentInvalid(t *testing.T) {
	for _, m := range []map[string]string{
		{"DONOR_SMS_PROVIDER": "stub"},                            // missing DONOR_SMS_FROM
		{"DONOR_SMS_PROVIDER": "twilio", "DONOR_SMS_FROM": "DAD"}, // no credentials, and a sender Twilio cannot use
		{"DONOR_SMS_PROVIDER": "twilio", "DONOR_SMS_ACCOUNT_ID": "AC123", "DONOR_SMS_AUTH_TOKEN": "t", "DONOR_SMS_FROM": "+14155550100"},
		{"DONOR_SMS_PROVIDER": "nope", "DONOR_SMS_FROM": "DAD"}, // unknown provider
	} {
		if _, err := FromEnvironment(env(m)); !errors.Is(err, ErrConfiguration) {
			t.Errorf("FromEnvironment(%v) error = %v; want ErrConfiguration", m, err)
		}
	}
}

func TestStubSenderRecords(t *testing.T) {
	s := &StubSender{}
	if err := s.Send(context.Background(), "+14155550123", "code 123456"); err != nil {
		t.Fatal(err)
	}
	if msg, ok := s.Last("+14155550123"); !ok || msg != "code 123456" {
		t.Fatalf("Last = %q, %v", msg, ok)
	}
	if _, ok := s.Last("+10000000000"); ok {
		t.Fatal("Last returned a message for an unsent phone")
	}
	if err := s.Send(context.Background(), "", "x"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("empty phone error = %v; want ErrUnavailable", err)
	}
}
