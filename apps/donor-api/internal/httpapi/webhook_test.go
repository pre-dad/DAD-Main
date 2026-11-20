package httpapi

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"dad/donor-api/internal/payments"
)

type webhookStub struct {
	err             error
	calls           int
	unbounded       bool
	body, signature string
}

func (s *webhookStub) Receive(ctx context.Context, body []byte, signature string) error {
	if _, ok := ctx.Deadline(); !ok {
		s.unbounded = true
	}
	s.calls++
	s.body = string(body)
	s.signature = signature
	return s.err
}

func TestWebhookAcknowledgementAndLimits(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{{nil, 200}, {payments.ErrInvalidEvent, 400}, {payments.ErrEventShape, 400}, {payments.ErrUnavailable, 503}, {errors.New("private database details"), 503}} {
		stub := &webhookStub{err: tc.err}
		server := NewServer("", Services{Webhook: stub})
		req := httptest.NewRequest("POST", "/donor/stripe/webhook", strings.NewReader("{ \"raw\" : true }"))
		req.Header.Set("Stripe-Signature", "signed bytes")
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, req)
		if response.Code != tc.status || strings.Contains(response.Body.String(), "private database") {
			t.Fatalf("response %d %s", response.Code, response.Body.String())
		}
		if stub.unbounded {
			t.Fatal("inbox write had no deadline")
		}
		if stub.body != "{ \"raw\" : true }" || stub.signature != "signed bytes" || stub.calls != 1 {
			t.Fatal("signature inputs changed")
		}
		if tc.err != nil && strings.Contains(response.Body.String(), "received") {
			t.Fatal("uncommitted event acknowledged")
		}
	}
	stub := &webhookStub{}
	server := NewServer("", Services{Webhook: stub})
	req := httptest.NewRequest("POST", "/donor/stripe/webhook", strings.NewReader(strings.Repeat("x", payments.MaxWebhookBytes+1)))
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, req)
	if response.Code != 413 || stub.calls != 0 {
		t.Fatal("oversized body reached verifier")
	}
}

func TestWebhookUnavailableWithoutVerifierAndUnreadableBody(t *testing.T) {
	server := NewServer("", Services{})
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, httptest.NewRequest("POST", "/donor/stripe/webhook", strings.NewReader("{}")))
	if response.Code != 503 {
		t.Fatalf("no verifier answered %d, want 503", response.Code)
	}

	stub := &webhookStub{}
	server = NewServer("", Services{Webhook: stub})
	req := httptest.NewRequest("POST", "/donor/stripe/webhook", iotest.ErrReader(errors.New("connection reset")))
	response = httptest.NewRecorder()
	server.Handler.ServeHTTP(response, req)
	if response.Code != 400 || stub.calls != 0 {
		t.Fatalf("unreadable body answered %d with %d verifier calls, want 400 and none", response.Code, stub.calls)
	}
}
