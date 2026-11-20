package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dad/donor-api/internal/store"
)

// partnerPortalDouble records the partner session each call carries.
type partnerPortalDouble struct {
	PartnerPortalSource
	acceptedApp  []bool
	overview     []string
	signedOut    []string
	signOutError error
}

func (d *partnerPortalDouble) AcceptInvite(_ context.Context, _ string, app bool) (string, error) {
	d.acceptedApp = append(d.acceptedApp, app)
	return strings.Repeat("p", 43), nil
}

func (d *partnerPortalDouble) Overview(_ context.Context, token string) (store.PartnerOverview, error) {
	d.overview = append(d.overview, token)
	return store.PartnerOverview{}, nil
}

func (d *partnerPortalDouble) SignOut(_ context.Context, token string) error {
	d.signedOut = append(d.signedOut, token)
	return d.signOutError
}

var partnerInviteBody = `{"invite":"` + strings.Repeat("i", 43) + `"}`

func TestPartnerAcceptInviteIssuesBearerToTheAppAndCookieToTheBrowser(t *testing.T) {
	portal := &partnerPortalDouble{}
	server := NewServer("", Services{PartnerPortal: portal})

	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, appRequest("POST", "/donor/partner/accept-invite", partnerInviteBody))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"session":"`+strings.Repeat("p", 43)+`"`) {
		t.Fatalf("native accept: %d %s", response.Code, response.Body)
	}
	if len(response.Result().Cookies()) != 0 || !portal.acceptedApp[0] {
		t.Fatalf("native accept set cookies %v or was not flagged as an app session", response.Result().Cookies())
	}

	r := httptest.NewRequest("POST", "/donor/partner/accept-invite", strings.NewReader(partnerInviteBody))
	r.Header.Set("Origin", testPublicOrigin)
	response = httptest.NewRecorder()
	server.Handler.ServeHTTP(response, r)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "session") || portal.acceptedApp[1] {
		t.Fatalf("browser accept leaked a token or was flagged as an app: %d %s", response.Code, response.Body)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != partnerCookie || !cookies[0].HttpOnly {
		t.Fatalf("browser accept cookies: %v", cookies)
	}
}

func TestPartnerRoutesReadTheBearerOnlyFromTheApp(t *testing.T) {
	portal := &partnerPortalDouble{}
	server := NewServer("", Services{PartnerPortal: portal})
	serve := func(r *http.Request) int {
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, r)
		return response.Code
	}

	if code := serve(appRequest("GET", "/donor/partner/overview", "")); code != http.StatusOK || portal.overview[0] != appSession {
		t.Fatalf("native partner read: %d %v", code, portal.overview)
	}

	anonymous := appRequest("GET", "/donor/partner/overview", "")
	anonymous.Header.Del("Authorization")
	if code := serve(anonymous); code != http.StatusUnauthorized {
		t.Fatalf("native read without a session: %d, want 401", code)
	}

	// The web proxy drops Authorization and sends no client marker, so a
	// browser request is judged by its cookie alone, never by a bearer token.
	browser := httptest.NewRequest("GET", "/donor/partner/overview", nil)
	browser.Header.Set("Authorization", "Bearer "+appSession)
	if code := serve(browser); code != http.StatusUnauthorized {
		t.Fatalf("browser read with only a bearer: %d, want 401", code)
	}
	browser.AddCookie(&http.Cookie{Name: partnerCookie, Value: "cookie-session"})
	if code := serve(browser); code != http.StatusOK || portal.overview[len(portal.overview)-1] != "cookie-session" {
		t.Fatalf("browser read with the cookie: %d %v", code, portal.overview)
	}

	// A donor cookie is never a partner session.
	donor := httptest.NewRequest("GET", "/donor/partner/overview", nil)
	donor.AddCookie(&http.Cookie{Name: donorCookie, Value: "donor-session"})
	reads := len(portal.overview)
	if code := serve(donor); code != http.StatusUnauthorized || len(portal.overview) != reads {
		t.Fatalf("donor cookie on a partner route: %d", code)
	}

	// A native request that also carries a cookie is not native: the bearer
	// cannot lend a browser request its origin exemption.
	mixed := appRequest("GET", "/donor/partner/overview", "")
	mixed.AddCookie(&http.Cookie{Name: donorCookie, Value: "donor-session"})
	if code := serve(mixed); code != http.StatusUnauthorized || len(portal.overview) != reads {
		t.Fatalf("mixed bearer and cookie: %d", code)
	}
}

func TestPartnerSignOutRevokesTheNativeSession(t *testing.T) {
	portal := &partnerPortalDouble{}
	server := NewServer("", Services{PartnerPortal: portal})
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, appRequest("POST", "/donor/partner/sign-out", ""))
	if response.Code != http.StatusOK || len(portal.signedOut) != 1 || portal.signedOut[0] != appSession {
		t.Fatalf("native sign-out: %d %v", response.Code, portal.signedOut)
	}

	if len(response.Result().Cookies()) != 0 {
		t.Fatal("native sign-out set browser cookies")
	}
	r := httptest.NewRequest("POST", "/donor/partner/sign-out", nil)
	r.AddCookie(&http.Cookie{Name: partnerCookie, Value: "cookie-session"})
	response = httptest.NewRecorder()
	server.Handler.ServeHTTP(response, r)
	if response.Code != http.StatusForbidden || len(portal.signedOut) != 1 {
		t.Fatalf("cookie sign-out without Origin: %d, want 403 before storage", response.Code)
	}
}
