package sms

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Built at runtime so the fixtures do not read as real Twilio identifiers.
var (
	testAccount = "AC" + strings.Repeat("0a", 16)
	testService = "MG" + strings.Repeat("0b", 16)
)

func twilio(t *testing.T, from string, status int) (*TwilioSender, *http.Request, *url.Values) {
	t.Helper()
	got := &http.Request{}
	form := &url.Values{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = *r
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		*form = r.PostForm
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"message":"refused body never surfaces"}`))
	}))
	t.Cleanup(server.Close)
	settings, err := FromEnvironment(env(map[string]string{
		"DONOR_SMS_PROVIDER": "twilio", "DONOR_SMS_ACCOUNT_ID": testAccount,
		"DONOR_SMS_AUTH_TOKEN": "secret-token", "DONOR_SMS_FROM": from,
	}))
	if err != nil {
		t.Fatal(err)
	}
	sender, ok := NewSender(settings).(*TwilioSender)
	if !ok {
		t.Fatal("NewSender(twilio) is not the Twilio sender")
	}
	sender.base, sender.client = server.URL, server.Client()
	return sender, got, form
}

func TestTwilioSendsFromANumber(t *testing.T) {
	sender, got, form := twilio(t, "+14155550100", http.StatusCreated)
	if err := sender.Send(context.Background(), "+14155550123", "Your code is 123456."); err != nil {
		t.Fatal(err)
	}
	user, pass, ok := got.BasicAuth()
	if got.Method != http.MethodPost || got.URL.Path != "/2010-04-01/Accounts/"+testAccount+"/Messages.json" ||
		!ok || user != testAccount || pass != "secret-token" {
		t.Fatalf("request %s %s auth %q/%q", got.Method, got.URL.Path, user, pass)
	}
	if form.Get("To") != "+14155550123" || form.Get("From") != "+14155550100" || form.Get("Body") != "Your code is 123456." ||
		form.Has("MessagingServiceSid") {
		t.Fatalf("form %v", form)
	}
}

func TestTwilioSendsThroughAMessagingService(t *testing.T) {
	sender, _, form := twilio(t, testService, http.StatusCreated)
	if err := sender.Send(context.Background(), "+14155550123", "code"); err != nil {
		t.Fatal(err)
	}
	if form.Get("MessagingServiceSid") != testService || form.Has("From") {
		t.Fatalf("form %v", form)
	}
}

func TestTwilioRefusalIsUnavailableWithoutSecrets(t *testing.T) {
	sender, _, _ := twilio(t, "+14155550100", http.StatusBadRequest)
	err := sender.Send(context.Background(), "+14155550123", "Your code is 123456.")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v; want ErrUnavailable", err)
	}
	for _, secret := range []string{"123456", "secret-token", "refused body"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error %q leaks %q", err, secret)
		}
	}
}

func TestTwilioTransportFailureKeepsCause(t *testing.T) {
	sender, _, _ := twilio(t, "+14155550100", http.StatusOK)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := sender.Send(ctx, "+14155550123", "Your code is 123456.")
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; want ErrUnavailable wrapping context.Canceled", err)
	}
}
