package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dad/donor-api/internal/store"
)

type accountStub struct {
	err                              error
	proof, old, email, next, refresh string
	client                           store.Client
}

var (
	browserTokens = store.Tokens{Access: "new-secret-session", AccessTTL: 30 * 24 * time.Hour}
	appTokens     = store.Tokens{
		Access:     "new-access-token",
		AccessTTL:  15 * time.Minute,
		Refresh:    "new-refresh-token",
		RefreshTTL: 60 * 24 * time.Hour,
	}
)

func (s *accountStub) ClaimAccount(_ context.Context, proof, old string, client store.Client) (store.Tokens, error) {
	s.proof, s.old, s.client = proof, old, client
	if client.App {
		return appTokens, s.err
	}
	return browserTokens, s.err
}

func (s *accountStub) Refresh(_ context.Context, token string) (store.Tokens, error) {
	s.refresh = token
	return appTokens, s.err
}

func (s *accountStub) SignOut(_ context.Context, token string) error { s.old = token; return s.err }
func (s *accountStub) QueueLogin(_ context.Context, email, next string) error {
	s.email, s.next = email, next
	return s.err
}

func TestClaimSetsCookieOnlyAfterSuccess(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{{nil, 200}, {store.ErrDenied, 401}, {errors.New("private failure"), 503}} {
		stub := &accountStub{err: tc.err}
		server := NewServer("", Services{Account: stub, Origin: "https://dad.example.org"})
		proof := strings.Repeat("a", 43)
		req := httptest.NewRequest("POST", "/donor/claim", strings.NewReader(`{"token":"`+proof+`"}`))
		req.Header.Set("Origin", "https://dad.example.org")
		req.AddCookie(&http.Cookie{Name: "dad_donor_session", Value: "old-session"})
		req.AddCookie(&http.Cookie{Name: "dad_company", Value: "captured-company-touch"})
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, req)
		if response.Code != tc.status || stub.proof != proof || stub.old != "old-session" ||
			stub.client.CompanyToken != "captured-company-touch" {
			t.Fatalf(
				"%v: got %d with proof %q revoking %q, want %d with the proof revoking old-session",
				tc.err,
				response.Code,
				stub.proof,
				stub.old,
				tc.status,
			)
		}
		if body := response.Body.String(); strings.Contains(body, proof) || strings.Contains(body, "private") ||
			strings.Contains(body, "new-secret-session") {
			t.Fatalf("%v: claim body leaked a credential or cause: %s", tc.err, body)
		}
		cookies := response.Result().Cookies()
		if tc.err != nil {
			if len(cookies) > 0 {
				t.Fatalf("%v: failed claim set %v", tc.err, cookies)
			}
			continue
		}
		if len(cookies) != 2 || cookies[0].Name != "__Host-dad_donor_session" || cookies[0].Value != "new-secret-session" ||
			!cookies[0].HttpOnly ||
			!cookies[0].Secure ||
			cookies[0].SameSite != http.SameSiteLaxMode ||
			cookies[0].MaxAge != 30*86400 ||
			cookies[0].Path != "/" ||
			cookies[0].Domain != "" {
			t.Fatalf("got cookies %+v, want a host-prefixed HttpOnly, Secure, Lax, 30-day session cookie first", cookies)
		}
		if cookies[1].Name != "dad_donor_session" || cookies[1].Value != "" || cookies[1].MaxAge >= 0 {
			t.Fatalf("got %+v, want the unprefixed cookie expired", cookies[1])
		}
	}
}

func TestLoginIsNonEnumeratingAndConfigurationGated(t *testing.T) {
	for _, configured := range []bool{false, true} {
		stub := &accountStub{}
		server := NewServer("", Services{Account: stub, EmailConfigured: configured})
		req := httptest.NewRequest("POST", "/donor/login", strings.NewReader(`{"email":"donor@example.org"}`))
		req.Header.Set("Origin", "http://localhost:3000")
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, req)
		want := 503
		if configured {
			want = 202
		}
		if response.Code != want || strings.Contains(response.Body.String(), "donor@example.org") {
			t.Errorf("configured=%v: got %d %s, want %d without echoing the address", configured, response.Code, response.Body, want)
		}
		if !configured && stub.email != "" {
			t.Error("unconfigured email still queued a sign-in link")
		}
	}
}

func TestLogoutClearsCookieAndToleratesMissingSession(t *testing.T) {
	stub := &accountStub{}
	req := httptest.NewRequest("POST", "/donor/logout", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	response := httptest.NewRecorder()
	NewServer("", Services{Account: stub}).Handler.ServeHTTP(response, req)
	cookies := response.Result().Cookies()
	if response.Code != 200 || len(cookies) != 1 || cookies[0].Name != donorCookie || cookies[0].MaxAge >= 0 {
		t.Fatalf("got %d with cookies %+v, want 200 expiring the session cookie", response.Code, cookies)
	}
}

func TestLoginReturnPathIsAllowlisted(t *testing.T) {
	for _, tc := range []struct {
		next   string
		status int
		want   string
	}{
		{``, 202, ""},
		{`,"next":"/account/payment-method"`, 202, "/account/payment-method"},
		{`,"next":"/account/dashboard"`, 202, "/account/dashboard"},
		{`,"next":"/donate"`, 202, "/donate"},
		{`,"next":"https://evil.example.org"`, 422, ""},
		{`,"next":"//evil.example.org"`, 422, ""},
		{`,"next":"/account/payment-method?x=1"`, 422, ""},
		{`,"next":null`, 422, ""},
	} {
		stub := &accountStub{}
		req := httptest.NewRequest("POST", "/donor/login", strings.NewReader(`{"email":"donor@example.org"`+tc.next+`}`))
		req.Header.Set("Origin", "http://localhost:3000")
		response := httptest.NewRecorder()
		NewServer("", Services{Account: stub, EmailConfigured: true}).Handler.ServeHTTP(response, req)
		if response.Code != tc.status || stub.next != tc.want {
			t.Errorf("next %s: got %d passing %q, want %d passing %q", tc.next, response.Code, stub.next, tc.status, tc.want)
		}
	}
}
