package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dad/donor-api/internal/store"
)

type emailChangeStub struct {
	calls int
	err   error
}

func (s *emailChangeStub) Request(_ context.Context, session, email string) error {
	s.calls++
	if session != "session" || email != "new@example.org" {
		return errors.New("unexpected payload")
	}
	return s.err
}

func (s *emailChangeStub) Confirm(context.Context, string) (string, error) {
	s.calls++
	return "NEW", s.err
}

func TestEmailChangeHTTPOriginValidationAndSafeErrors(t *testing.T) {
	for _, tc := range []struct {
		path, body, origin string
		failure            error
		status             int
	}{
		{"/donor/me/email-change", `{"email":"new@example.org"}`, "https://dad.example.org", nil, 200},
		{"/donor/me/email-change", `{"email":"new@example.org"}`, "https://other.example.org", nil, 403},
		{"/donor/email-change/confirm", `{"token":"short"}`, "https://dad.example.org", nil, 422},
		{"/donor/email-change/confirm", `{"token":"` + strings.Repeat("a", 43) + `"}`, "https://dad.example.org", store.ErrDenied, 401},
		{"/donor/email-change/confirm", `{"token":"` + strings.Repeat("a", 43) + `"}`, "https://dad.example.org", store.ErrEmailChangeConflict, 409},
		{"/donor/me/email-change", `{"email":"new@example.org"}`, "https://dad.example.org", errors.New("private database detail"), 503},
	} {
		stub := &emailChangeStub{err: tc.failure}
		server := NewServer("", Services{EmailChange: stub, EmailConfigured: true, Origin: "https://dad.example.org"})
		req := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
		req.Header.Set("Origin", tc.origin)
		req.AddCookie(&http.Cookie{Name: "dad_donor_session", Value: "session"})
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, req)
		if response.Code != tc.status || strings.Contains(response.Body.String(), "private") {
			t.Errorf(
				"%s %s from %s: got %d %s, want %d without private detail",
				tc.path,
				tc.body,
				tc.origin,
				response.Code,
				response.Body,
				tc.status,
			)
		}
		if (tc.status == 403 || tc.status == 422) && stub.calls != 0 {
			t.Errorf("%s %s: rejected request reached the service", tc.path, tc.body)
		}
	}
}

func TestEmailChangeWithoutEmailAnswersUnconfiguredBeforeTheService(t *testing.T) {
	for _, tc := range []struct{ path, body string }{
		{"/donor/me/email-change", `{"email":"new@example.org"}`},
		{"/donor/email-change/confirm", `{"token":"` + strings.Repeat("a", 43) + `"}`},
	} {
		stub := &emailChangeStub{}
		server := NewServer("", Services{EmailChange: stub, Origin: "https://dad.example.org"})
		req := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
		req.Header.Set("Origin", "https://dad.example.org")
		req.AddCookie(&http.Cookie{Name: "dad_donor_session", Value: "session"})
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, req)
		if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"EMAIL_UNCONFIGURED"`) ||
			stub.calls != 0 {
			t.Errorf("%s: got %d %s with %d service calls", tc.path, response.Code, response.Body, stub.calls)
		}
	}
}
