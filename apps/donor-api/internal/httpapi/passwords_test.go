package httpapi

import (
	"context"
	"dad/donor-api/internal/store"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type passwordStub struct {
	err    error
	client store.Client
}

func (s *passwordStub) Signup(context.Context, store.SignupInput) error { return s.err }
func (s *passwordStub) Set(context.Context, string, string, string) error {
	return s.err
}
func (s *passwordStub) SignIn(_ context.Context, _, _ string, client store.Client) (store.Tokens, error) {
	s.client = client
	return appTokens, s.err
}

func TestSignupAnswersSentTrue(t *testing.T) {
	r := httptest.NewRequest("POST", "/donor/signup", strings.NewReader(`{"name":"A","email":"a@example.org","zip":"10001","city":"X"}`))
	r.Header.Set("X-DAD-Client", "native")
	response := httptest.NewRecorder()
	NewServer("", Services{Passwords: &passwordStub{}, EmailConfigured: true}).Handler.ServeHTTP(response, r)
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"sent":true}` {
		t.Fatalf("signup: got %d %s, want 200 {\"sent\":true} as documented", response.Code, response.Body)
	}
}

func TestSetPasswordSeparatesWrongCurrentPasswordFromBadSession(t *testing.T) {
	body := `{"current_password":"typo","password":"a long and unique test password"}`
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"wrong current password", store.ErrCurrentPassword, http.StatusUnprocessableEntity, codeCurrentPassword},
		{"revoked session", store.ErrDenied, http.StatusUnauthorized, ""},
		{"weak new password", store.ErrPassword, http.StatusUnprocessableEntity, ""},
		{"outage", errors.New("private failure"), http.StatusServiceUnavailable, ""},
		{"saved", nil, http.StatusOK, ""},
	} {
		response := httptest.NewRecorder()
		NewServer(
			"",
			Services{Passwords: &passwordStub{err: tc.err}},
		).Handler.ServeHTTP(
			response,
			appRequest("POST", "/donor/me/password", body),
		)
		var got struct{ Detail, Code string }
		_ = json.Unmarshal(response.Body.Bytes(), &got)
		if response.Code != tc.status || got.Code != tc.code {
			t.Errorf("%s: got %d %s, want %d with code %q", tc.name, response.Code, response.Body, tc.status, tc.code)
		}
		if tc.err == store.ErrCurrentPassword && got.Detail != "The email or password is incorrect" {
			t.Errorf("%s: detail %q changed", tc.name, got.Detail)
		}
	}
}

func TestPasswordSignInLabelsOnlyTheAppsSession(t *testing.T) {
	name := strings.Repeat("é", store.MaxDeviceName)
	for _, tc := range []struct {
		name, fields string
		app          bool
		status       int
		client       store.Client
	}{
		{"app device", `,"device_name":"` + name + `","platform":"ANDROID"`, true, http.StatusOK, store.Client{App: true, Device: name, Platform: "ANDROID"}},
		{"app without description", ``, true, http.StatusOK, store.Client{App: true}},
		{"null fields", `,"device_name":null,"platform":null`, true, http.StatusOK, store.Client{App: true}},
		{"browser description ignored", `,"device_name":"Laptop","platform":"IOS"`, false, http.StatusOK, store.Client{}},
		{"unknown platform", `,"platform":"WINDOWS"`, true, http.StatusUnprocessableEntity, store.Client{}},
		{"long device name", `,"device_name":"` + name + `x"`, true, http.StatusUnprocessableEntity, store.Client{}},
	} {
		r := httptest.NewRequest(
			"POST",
			"/donor/password/sign-in",
			strings.NewReader(`{"email":"a@example.org","password":"pw"`+tc.fields+`}`),
		)
		if tc.app {
			r.Header.Set("X-DAD-Client", "native")
		} else {
			r.Header.Set("Origin", "http://localhost:3000")
		}
		stub := &passwordStub{}
		response := httptest.NewRecorder()
		NewServer("", Services{Passwords: stub}).Handler.ServeHTTP(response, r)
		if response.Code != tc.status || stub.client != tc.client {
			t.Errorf("%s: got %d issuing to %+v, want %d issuing to %+v", tc.name, response.Code, stub.client, tc.status, tc.client)
		}
	}
}

func TestSignupDoesNotRevealAnUnavailableIdentity(t *testing.T) {
	for _, err := range []error{nil, store.EmailRoleError{Role: "STAFF"}, store.EmailRoleError{Role: "PARTNER"}, store.ErrDenied} {
		r := httptest.NewRequest(
			"POST",
			"/donor/signup",
			strings.NewReader(`{"name":"Ada","email":"ada@example.org","zip":"10001","city":"X"}`),
		)
		r.Header.Set("Origin", "http://localhost:3000")
		response := httptest.NewRecorder()
		NewServer("", Services{Passwords: &passwordStub{err: err}, EmailConfigured: true}).Handler.ServeHTTP(response, r)
		if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"sent":true}` {
			t.Fatalf("signup revealed an identity state: %d %s", response.Code, response.Body)
		}
	}
}
