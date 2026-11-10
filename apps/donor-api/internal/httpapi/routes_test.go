package httpapi

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

const testID = "7d6f6b1c-6f4e-4c3e-9a51-6f1d9c1e2a3b"

var wildcard = regexp.MustCompile(`\{[a-z_]+\}`)

// routeRequest builds a request for a route pattern with valid path values.
func routeRequest(pattern, body string) *http.Request {
	method, path, _ := strings.Cut(pattern, " ")
	path = wildcard.ReplaceAllStringFunc(path, func(name string) string {
		switch name {
		case "{year}":
			return "2026"
		case "{file}":
			return "gifts.csv"
		case "{code}":
			return "code"
		}
		return testID
	})
	return httptest.NewRequest(method, path, strings.NewReader(body))
}

func allRoutes() []route { return (&api{}).routes() }

// NewServer's local defaults for each surface.
const (
	testPublicOrigin = "http://localhost:3000"
	testStaffOrigin  = "http://staff.localhost:3000"
)

func staffPath(path string) bool { return strings.Contains(path, "/donor/staff/") }

// originFor returns the browser Origin admitted on a path's surface.
func originFor(path string) string {
	if staffPath(path) {
		return testStaffOrigin
	}
	return testPublicOrigin
}

// Every mutation must reject a foreign or missing browser Origin before any
// service runs; a nil service would answer 503 instead of 403. Credentials do
// not help: cookies make a request a browser request. Neither surface admits
// the other's origin, so a page on one host cannot act on the other.
func TestEveryMutationRequiresItsSurfaceOrigin(t *testing.T) {
	const public, staff = "https://dad.example.org", "https://staff.dad.example.org"
	server := NewServer("", Services{Origin: public, StaffOrigin: staff})
	for _, route := range allRoutes() {
		if strings.HasPrefix(route.pattern, "GET ") || route.auth == stripeAuth {
			continue
		}
		other := staff
		if staffPath(route.pattern) {
			other = public
		}
		for _, origin := range []string{"", "https://evil.example.org", other} {
			r := routeRequest(route.pattern, "{}")
			if origin != "" {
				r.Header.Set("Origin", origin)
			}
			r.Header.Set("X-DAD-Client", "native")
			r.AddCookie(&http.Cookie{Name: donorCookie, Value: "session"})
			r.AddCookie(&http.Cookie{Name: staffCookie, Value: "session"})
			response := httptest.NewRecorder()
			server.Handler.ServeHTTP(response, r)
			if response.Code != http.StatusForbidden {
				t.Errorf("%s with origin %q: got %d, want 403", route.pattern, origin, response.Code)
			}
		}
	}
}

func TestSignedInRoutesRejectAnonymousRequestsBeforeStorage(t *testing.T) {
	server := NewServer("", Services{})
	for _, route := range allRoutes() {
		if route.auth != donorAuth && route.auth != staffAuth {
			continue
		}
		r := routeRequest(route.pattern, "{}")
		r.Header.Set("Origin", originFor(route.pattern))
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, r)
		want := http.StatusUnauthorized
		if strings.HasSuffix(route.pattern, "/logout") || strings.HasSuffix(route.pattern, "/sign-out") {
			// Signing out never requires a valid session; with no service it is unavailable.
			want = http.StatusServiceUnavailable
		}
		if response.Code != want {
			t.Errorf("%s anonymously: got %d, want %d", route.pattern, response.Code, want)
		}
	}
}

func TestFailuresAreLoggedWithoutPrivateDetail(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	failure := errors.New("donor@example.org could not be read")
	server := NewServer("", Services{Dashboard: &dashboardStub{err: failure}})
	r := httptest.NewRequest("GET", "/donor/me/dashboard", nil)
	r.AddCookie(&http.Cookie{Name: donorCookie, Value: "session"})
	server.Handler.ServeHTTP(httptest.NewRecorder(), r)

	out := logs.String()
	if !strings.Contains(out, `"route":"GET /donor/me/dashboard"`) || !strings.Contains(out, `"status":503`) ||
		!strings.Contains(out, `"cause":"*errors.errorString"`) {
		t.Fatalf("failure not logged with route, status, and class: %s", out)
	}
	if strings.Contains(out, "donor@example.org") {
		t.Fatal("log exposed private error text")
	}
}

func TestClientCancellationIsNotLoggedAsAFailure(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	server := NewServer("", Services{Dashboard: &dashboardStub{err: context.Canceled}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequestWithContext(ctx, "GET", "/donor/me/dashboard", nil)
	r.AddCookie(&http.Cookie{Name: donorCookie, Value: "session"})
	server.Handler.ServeHTTP(httptest.NewRecorder(), r)

	out := logs.String()
	if strings.Contains(out, `"level":"ERROR"`) || !strings.Contains(out, `"msg":"request canceled by client"`) {
		t.Fatalf("client cancellation logged as a failure: %s", out)
	}

	logs.Reset()
	server = NewServer("", Services{Dashboard: &dashboardStub{err: context.Canceled}})
	r = httptest.NewRequest("GET", "/donor/me/dashboard", nil)
	r.AddCookie(&http.Cookie{Name: donorCookie, Value: "session"})
	server.Handler.ServeHTTP(httptest.NewRecorder(), r)
	if !strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Fatalf("a cancellation the client did not cause must stay an error: %s", logs.String())
	}
}
