package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"dad/donor-api/internal/store"
)

type passkeyStub struct {
	StaffSource
	credential string
	signedOut  string
}

func (s *passkeyStub) SignIn(_ context.Context, ceremony string, credential []byte) (string, error) {
	s.credential = string(credential)
	return "staff-session", nil
}

func (s *passkeyStub) SignOut(_ context.Context, session string) error {
	s.signedOut = session
	return nil
}

func (s *passkeyStub) Invite(context.Context, string, string, string, string) (string, error) {
	return "private-invitation", nil
}

func (s *passkeyStub) ChangeAccess(context.Context, string, pgtype.UUID, string, string) (store.AccessChange, error) {
	return store.AccessChange{Token: "private-recovery"}, nil
}

// The staff session must never reach the public host: it carries no Domain,
// and over HTTPS the __Host- prefix makes browsers enforce that.
func TestStaffSessionCookieStaysOnTheStaffHost(t *testing.T) {
	for _, tc := range []struct {
		origin, name string
		secure       bool
	}{
		{"https://staff.dad.example.org", "__Host-dad_staff_session", true},
		{testStaffOrigin, "dad_staff_session", false},
	} {
		stub := &passkeyStub{}
		server := NewServer("", Services{Origin: "https://dad.example.org", StaffOrigin: tc.origin, Staff: stub})
		r := httptest.NewRequest(
			"POST",
			"/donor/staff/sign-in",
			strings.NewReader(`{"ceremony":"`+strings.Repeat("c", 43)+`","credential":{"id":"passkey"}}`),
		)
		r.Header.Set("Origin", tc.origin)
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, r)
		cookies := response.Result().Cookies()
		if response.Code != http.StatusOK || len(cookies) != 1 || stub.credential != `{"id":"passkey"}` {
			t.Fatalf("%s: got %d %s, credential %q", tc.origin, response.Code, response.Body, stub.credential)
		}
		cookie := cookies[0]
		if cookie.Name != tc.name || cookie.Domain != "" || cookie.Path != "/" || !cookie.HttpOnly || cookie.Secure != tc.secure ||
			cookie.SameSite != http.SameSiteStrictMode {
			t.Fatalf("%s: unsafe staff cookie %+v", tc.origin, cookie)
		}
		r = httptest.NewRequest("POST", "/donor/staff/sign-out", nil)
		r.Header.Set("Origin", tc.origin)
		r.AddCookie(&http.Cookie{Name: tc.name, Value: cookie.Value})
		r.AddCookie(&http.Cookie{Name: "dad_staff_session", Value: "planted"})
		server.Handler.ServeHTTP(httptest.NewRecorder(), r)
		if stub.signedOut != "staff-session" {
			t.Fatalf("%s: signed out %q, want the session the cookie carries", tc.origin, stub.signedOut)
		}
	}
}

func TestStaffInvitationsLinkToTheStaffHost(t *testing.T) {
	const staff = "https://staff.dad.example.org"
	server := NewServer("", Services{Origin: "https://dad.example.org", StaffOrigin: staff, Staff: &passkeyStub{}})
	for path, body := range map[string]string{
		"/donor/staff/accounts":                         `{"email":"new@example.org","name":"New","role":"OP_PARTNERSHIPS"}`,
		"/donor/staff/accounts/" + testID + "/recovery": "",
	} {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("Origin", staff)
		r.AddCookie(&http.Cookie{Name: "__Host-dad_staff_session", Value: "staff-session"})
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, r)
		if response.Code != http.StatusOK ||
			!strings.Contains(response.Body.String(), `"invitation_url":"`+staff+`/staff/activate#token=private-`) {
			t.Fatalf("%s: got %d %s", path, response.Code, response.Body)
		}
	}
}

// The public host is the same site as the staff host, so its links carry the
// SameSite=Strict staff cookie; the browser's Sec-Fetch-Site still tells them
// apart from the back office's own requests.
func TestStaffRoutesRefuseRequestsStartedOnAnotherHost(t *testing.T) {
	server := NewServer("", Services{Origin: testPublicOrigin, StaffOrigin: testStaffOrigin})
	send := func(pattern, site string) int {
		r := routeRequest(pattern, "{}")
		r.Header.Set("Origin", testStaffOrigin)
		r.Header.Set("Sec-Fetch-Site", site)
		r.AddCookie(&http.Cookie{Name: staffCookie, Value: "session"})
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, r)
		return response.Code
	}
	for _, route := range allRoutes() {
		if !staffPath(route.pattern) {
			continue
		}
		for _, site := range []string{"same-site", "cross-site"} {
			if code := send(route.pattern, site); code != http.StatusForbidden {
				t.Errorf("%s from a %s page: got %d, want 403", route.pattern, site, code)
			}
		}
	}
	for _, pattern := range []string{"GET /donor/staff/exports/{file}", "POST /donor/staff/sign-in/options"} {
		for _, site := range []string{"same-origin", "none", ""} {
			if code := send(pattern, site); code == http.StatusForbidden {
				t.Errorf("%s with Sec-Fetch-Site %q refused", pattern, site)
			}
		}
	}
}

// Generating a partner report retains it, so the partner portal's Lax cookie
// must not be usable by a link on another site.
func TestPartnerReportRefusesRequestsStartedOnAnotherSite(t *testing.T) {
	server := NewServer("", Services{Origin: testPublicOrigin, StaffOrigin: testStaffOrigin})
	send := func(site string) int {
		r := routeRequest("GET /donor/partner/report.pdf", "")
		r.Header.Set("Sec-Fetch-Site", site)
		r.AddCookie(&http.Cookie{Name: partnerCookie, Value: "session"})
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, r)
		return response.Code
	}
	for _, site := range []string{"same-site", "cross-site"} {
		if code := send(site); code != http.StatusForbidden {
			t.Errorf("report from a %s page: got %d, want 403", site, code)
		}
	}
	for _, site := range []string{"same-origin", "none", ""} {
		if code := send(site); code == http.StatusForbidden {
			t.Errorf("report with Sec-Fetch-Site %q refused", site)
		}
	}
}
