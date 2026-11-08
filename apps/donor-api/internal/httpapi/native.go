package httpapi

import (
	"net/http"
	"strings"
)

// native reports whether a request comes from the donor app rather than a
// browser. Browsers attach cookies automatically and send Origin on every
// mutation, and they cannot add X-DAD-Client to a cross-origin request without
// a CORS preflight that this API never grants. A request with the header but
// with neither Origin nor cookies therefore cannot be a cross-site forgery.
func native(r *http.Request) bool {
	_, cookies := r.Header["Cookie"]
	_, origin := r.Header["Origin"]
	return r.Header.Get("X-DAD-Client") == "native" && !cookies && !origin
}

// donorSession returns the caller's donor session token: the bearer token of a
// native request, otherwise the browser cookie. A request never mixes the two,
// so a bearer token cannot lend a browser request its Origin exemption.
func donorSession(r *http.Request) string {
	if !native(r) {
		return sessionCookieValue(r, donorCookie)
	}
	scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if !strings.EqualFold(scheme, "Bearer") || !validToken(token) {
		return ""
	}
	return token
}
