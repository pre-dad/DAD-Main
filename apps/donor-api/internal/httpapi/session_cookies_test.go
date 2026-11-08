package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dad/donor-api/internal/oidc"
	"dad/donor-api/internal/store"
)

const secureOrigin = "https://dad.example.org"

type deleteStub struct {
	DonorSource
	deleted []string
}

func (s *deleteStub) DeleteAccount(_ context.Context, token string) error {
	s.deleted = append(s.deleted, token)
	return nil
}

type oidcStub struct {
	provider oidc.Provider
	idToken  string
	nonce    string
	calls    int
}

func (s *oidcStub) SignIn(_ context.Context, p oidc.Provider, idToken, nonce string, _ store.Client) (store.Tokens, error) {
	s.provider, s.idToken, s.nonce = p, idToken, nonce
	s.calls++
	return browserTokens, nil
}

func cookieByName(cookies []*http.Cookie, name string) *http.Cookie {
	for _, c := range cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// expectIssued fails unless cookies hold exactly the live session cookie and
// the expiry of the other name.
func expectIssued(t *testing.T, cookies []*http.Cookie, live, expired string, maxAge int) {
	t.Helper()
	if len(cookies) != 2 {
		t.Fatalf("got cookies %+v, want the live %s and the expired %s", cookies, live, expired)
	}
	got, gone := cookieByName(cookies, live), cookieByName(cookies, expired)
	if got == nil || got.Value == "" || got.MaxAge != maxAge || got.Path != "/" || got.Domain != "" || !got.HttpOnly ||
		got.SameSite != http.SameSiteLaxMode {
		t.Fatalf("live cookie %+v, want a host-only, HttpOnly, Lax %s for %ds", got, live, maxAge)
	}
	if gone == nil || gone.Value != "" || gone.MaxAge >= 0 || gone.Path != "/" || gone.Domain != "" {
		t.Fatalf("expired cookie %+v, want %s cleared", gone, expired)
	}
}

func TestDonorSessionCookieNameFollowsTheOriginScheme(t *testing.T) {
	for _, tc := range []struct {
		origin string
		secure bool
	}{{secureOrigin, true}, {testPublicOrigin, false}} {
		oidcSource := &oidcStub{}
		server := NewServer("", Services{OIDCSignIn: oidcSource, Origin: tc.origin})
		r := httptest.NewRequest("POST", "/donor/oidc/google/sign-in", strings.NewReader(`{"id_token":"t"}`))
		r.Header.Set("Origin", tc.origin)
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, r)
		cookies := response.Result().Cookies()
		if response.Code != http.StatusOK {
			t.Fatalf("%s: sign-in answered %d %s", tc.origin, response.Code, response.Body)
		}
		if tc.secure {
			expectIssued(t, cookies, "__Host-dad_donor_session", "dad_donor_session", 30*86400)
			if !cookieByName(cookies, "__Host-dad_donor_session").Secure {
				t.Fatal("a __Host- cookie must be Secure")
			}
			continue
		}
		if len(cookies) != 1 || cookies[0].Name != "dad_donor_session" || cookies[0].Secure || cookies[0].MaxAge != 30*86400 {
			t.Fatalf("%s: got cookies %+v, want the one unprefixed, non-Secure cookie", tc.origin, cookies)
		}
	}
}

func TestDonorSessionReadsNewNameThenFallsBackToOld(t *testing.T) {
	for name, tc := range map[string]struct {
		cookies []*http.Cookie
		want    string
	}{
		"new name only": {[]*http.Cookie{{Name: "__Host-dad_donor_session", Value: "new"}}, "new"},
		"old name only": {[]*http.Cookie{{Name: "dad_donor_session", Value: "old"}}, "old"},
		"both present, new wins": {
			[]*http.Cookie{{Name: "dad_donor_session", Value: "old"}, {Name: "__Host-dad_donor_session", Value: "new"}}, "new",
		},
		"both present, new listed first": {
			[]*http.Cookie{{Name: "__Host-dad_donor_session", Value: "new"}, {Name: "dad_donor_session", Value: "old"}}, "new",
		},
		"neither": {nil, ""},
	} {
		r := httptest.NewRequest("GET", "/donor/me", nil)
		for _, c := range tc.cookies {
			r.AddCookie(c)
		}
		if got := donorSession(r); got != tc.want {
			t.Errorf("%s: donorSession = %q, want %q", name, got, tc.want)
		}
	}
}

func TestPartnerSessionReadsNewNameThenOldThenNeverTheDonorCookie(t *testing.T) {
	for name, tc := range map[string]struct {
		cookies []*http.Cookie
		want    string
	}{
		"new name only": {[]*http.Cookie{{Name: "__Host-dad_partner_session", Value: "new"}}, "new"},
		"old name only": {[]*http.Cookie{{Name: "dad_partner_session", Value: "old"}}, "old"},
		"both present, new wins": {
			[]*http.Cookie{{Name: "dad_partner_session", Value: "old"}, {Name: "__Host-dad_partner_session", Value: "new"}}, "new",
		},
		"donor cookies are not partner sessions": {
			[]*http.Cookie{{Name: "dad_donor_session", Value: "donor"}, {Name: "__Host-dad_donor_session", Value: "donor"}}, "",
		},
	} {
		r := httptest.NewRequest("GET", "/donor/partner/overview", nil)
		for _, c := range tc.cookies {
			r.AddCookie(c)
		}
		if got := partnerSession(r); got != tc.want {
			t.Errorf("%s: partnerSession = %q, want %q", name, got, tc.want)
		}
	}
}

func TestPartnerBearerStillRequiresTheNativeMarker(t *testing.T) {
	// A prefixed cookie plus a bearer is a browser request: the cookie decides
	// and the bearer is ignored.
	r := appRequest("GET", "/donor/partner/overview", "")
	r.AddCookie(&http.Cookie{Name: "__Host-dad_partner_session", Value: "cookie-session"})
	if got := partnerSession(r); got != "cookie-session" {
		t.Fatalf("partnerSession = %q, want the cookie to win over the bearer", got)
	}
	native := appRequest("GET", "/donor/partner/overview", "")
	if got := partnerSession(native); got != appSession {
		t.Fatalf("native partnerSession = %q, want the bearer", got)
	}
}

func TestPartnerCookiesOverHTTPS(t *testing.T) {
	portal := &partnerPortalDouble{}
	server := NewServer("", Services{PartnerPortal: portal, Origin: secureOrigin})

	r := httptest.NewRequest("POST", "/donor/partner/accept-invite", strings.NewReader(partnerInviteBody))
	r.Header.Set("Origin", secureOrigin)
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, r)
	expectIssued(t, response.Result().Cookies(), "__Host-dad_partner_session", "dad_partner_session", 12*3600)

	read := httptest.NewRequest("GET", "/donor/partner/overview", nil)
	read.AddCookie(&http.Cookie{Name: "dad_partner_session", Value: "old-session"})
	response = httptest.NewRecorder()
	server.Handler.ServeHTTP(response, read)
	if response.Code != http.StatusOK || portal.overview[len(portal.overview)-1] != "old-session" {
		t.Fatalf("old-name fallback: %d %v", response.Code, portal.overview)
	}
	read = httptest.NewRequest("GET", "/donor/partner/overview", nil)
	read.AddCookie(&http.Cookie{Name: "dad_partner_session", Value: "old-session"})
	read.AddCookie(&http.Cookie{Name: "__Host-dad_partner_session", Value: "new-session"})
	response = httptest.NewRecorder()
	server.Handler.ServeHTTP(response, read)
	if response.Code != http.StatusOK || portal.overview[len(portal.overview)-1] != "new-session" {
		t.Fatalf("both present: %d %v", response.Code, portal.overview)
	}

	out := httptest.NewRequest("POST", "/donor/partner/sign-out", nil)
	out.Header.Set("Origin", secureOrigin)
	out.AddCookie(&http.Cookie{Name: "__Host-dad_partner_session", Value: "new-session"})
	response = httptest.NewRecorder()
	server.Handler.ServeHTTP(response, out)
	cookies := response.Result().Cookies()
	if response.Code != http.StatusOK || len(cookies) != 2 {
		t.Fatalf("sign-out: %d %+v", response.Code, cookies)
	}
	for _, name := range []string{"__Host-dad_partner_session", "dad_partner_session"} {
		if c := cookieByName(cookies, name); c == nil || c.MaxAge >= 0 || c.Value != "" {
			t.Errorf("sign-out left %s as %+v, want it cleared", name, c)
		}
	}
	if portal.signedOut[len(portal.signedOut)-1] != "new-session" {
		t.Fatalf("sign-out revoked %v, want new-session", portal.signedOut)
	}
}

func TestPartnerCookiesOverHTTPKeepTheUnprefixedName(t *testing.T) {
	server := NewServer("", Services{PartnerPortal: &partnerPortalDouble{}, Origin: testPublicOrigin})
	r := httptest.NewRequest("POST", "/donor/partner/sign-out", nil)
	r.Header.Set("Origin", testPublicOrigin)
	r.AddCookie(&http.Cookie{Name: "dad_partner_session", Value: "cookie-session"})
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, r)
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "dad_partner_session" || cookies[0].MaxAge >= 0 {
		t.Fatalf("got %+v, want only the unprefixed partner cookie cleared", cookies)
	}
}

func TestDonorSignOutAndDeletionClearBothCookies(t *testing.T) {
	deletion := &deleteStub{}
	server := NewServer("", Services{Account: &accountStub{}, Donor: deletion, Origin: secureOrigin})
	for name, request := range map[string]func() *http.Request{
		"logout": func() *http.Request { return httptest.NewRequest("POST", "/donor/logout", nil) },
		"delete": func() *http.Request { return httptest.NewRequest("POST", "/donor/me/delete", strings.NewReader("{}")) },
	} {
		r := request()
		r.Header.Set("Origin", secureOrigin)
		r.AddCookie(&http.Cookie{Name: "__Host-dad_donor_session", Value: strings.Repeat("s", 43)})
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, r)
		cookies := response.Result().Cookies()
		if response.Code != http.StatusOK || len(cookies) != 2 {
			t.Fatalf("%s: %d %+v", name, response.Code, cookies)
		}
		for _, cookieName := range []string{"__Host-dad_donor_session", "dad_donor_session"} {
			if c := cookieByName(cookies, cookieName); c == nil || c.MaxAge >= 0 || c.Value != "" || !c.Secure {
				t.Errorf("%s left %s as %+v, want it cleared", name, cookieName, c)
			}
		}
	}
	if len(deletion.deleted) != 1 || deletion.deleted[0] != strings.Repeat("s", 43) {
		t.Fatalf("deletion revoked %v, want the prefixed cookie's session", deletion.deleted)
	}
}

func TestOIDCSignInPassesTheNonceThrough(t *testing.T) {
	source := &oidcStub{}
	server := NewServer("", Services{OIDCSignIn: source})
	post := func(body string) int {
		r := appRequest("POST", "/donor/oidc/apple/sign-in", body)
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, r)
		return response.Code
	}
	if code := post(`{"id_token":"tok","nonce":"raw-nonce"}`); code != http.StatusOK ||
		source.provider != oidc.Apple || source.idToken != "tok" || source.nonce != "raw-nonce" {
		t.Fatalf("with nonce: %d %+v", code, source)
	}
	if code := post(`{"id_token":"tok"}`); code != http.StatusOK || source.nonce != "" {
		t.Fatalf("without nonce: %d %+v", code, source)
	}
	calls := source.calls
	if code := post(`{"id_token":"tok","nonce":"` + strings.Repeat("n", maxOIDCNonce+1) + `"}`); code != http.StatusUnprocessableEntity ||
		source.calls != calls {
		t.Fatalf("oversized nonce: %d after %d calls, want 422 before verification", code, source.calls)
	}
}
