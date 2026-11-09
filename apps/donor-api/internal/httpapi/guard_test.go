package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

var testClass = rateClass{"test", 20}

func guardedRequest(remote string) *http.Request {
	r := httptest.NewRequest("POST", "/donor/checkouts", nil)
	r.RemoteAddr = remote
	return r
}

func exhaust(t *testing.T, l *rateLimiter, r *http.Request, class rateClass, now time.Time) {
	t.Helper()
	for i := range class.limit {
		if wait := l.wait(r, class, now); wait != 0 {
			t.Fatalf("request %d told to wait %v before the limit", i+1, wait)
		}
	}
	if wait := l.wait(r, class, now); wait == 0 {
		t.Fatal("request over the limit was admitted")
	}
}

func TestTrustedProxySeparatesClientsWithoutTrustingOtherPeers(t *testing.T) {
	l := newRateLimiter([]netip.Prefix{netip.MustParsePrefix("10.20.0.2/32")})
	now := time.Now()
	r := guardedRequest("10.20.0.2:1234")
	for _, ip := range []string{"203.0.113.1", "203.0.113.2", "2001:db8::1"} {
		r.Header.Set("X-Real-IP", ip)
		exhaust(t, l, r, testClass, now)
	}

	untrusted := guardedRequest("10.20.0.3:1234")
	for i := range testClass.limit {
		untrusted.Header.Set("X-Real-IP", fmt.Sprintf("192.0.2.%d", i))
		if wait := l.wait(untrusted, testClass, now); wait != 0 {
			t.Fatalf("untrusted peer limited early: wait %v", wait)
		}
	}
	untrusted.Header.Set("X-Real-IP", "192.0.2.200")
	if wait := l.wait(untrusted, testClass, now); wait == 0 {
		t.Fatal("untrusted peer escaped its quota by spoofing X-Real-IP")
	}

	for _, invalid := range []string{"203.0.113.1, 203.0.113.2", "unknown", "203.0.113.1:42"} {
		r.Header.Set("X-Real-IP", invalid)
		if wait := l.wait(r, testClass, now); wait != 0 {
			t.Fatalf("invalid header %q was not attributed to the proxy peer", invalid)
		}
	}
	if len(l.buckets) != 5 {
		t.Fatalf("got %d buckets, want 5: invalid addresses must share the proxy's bucket", len(l.buckets))
	}
}

func TestForwardedHeadersAreIgnoredWithoutTrustedProxies(t *testing.T) {
	l := newRateLimiter(nil)
	now := time.Now()
	r := guardedRequest("192.0.2.1:1234")
	exhaust(t, l, r, testClass, now)
	r.Header.Set("X-Forwarded-For", "203.0.113.12")
	r.Header.Set("X-Real-IP", "203.0.113.12")
	if wait := l.wait(r, testClass, now); wait == 0 {
		t.Fatal("forwarding headers changed the rate-limit key")
	}
}

func TestRateClassesHaveIndependentBudgets(t *testing.T) {
	l := newRateLimiter(nil)
	now := time.Now()
	r := guardedRequest("192.0.2.1:1234")
	exhaust(t, l, r, authLimit, now)
	if wait := l.wait(r, visitorLimit, now); wait != 0 {
		t.Fatal("exhausting sign-in attempts blocked giving from the same address")
	}
}

func TestRateLimitRefillsAndReportsRetryDelay(t *testing.T) {
	l := newRateLimiter(nil)
	now := time.Now()
	r := guardedRequest("192.0.2.1:1234")
	exhaust(t, l, r, testClass, now)
	delay := l.wait(r, testClass, now)
	if interval := time.Minute / time.Duration(testClass.limit); delay <= 0 || delay > interval {
		t.Fatalf("retry delay %v, want within one refill interval %v", delay, interval)
	}
	if wait := l.wait(r, testClass, now.Add(delay)); wait != 0 {
		t.Fatal("request after the reported delay was rejected")
	}
	if wait := l.wait(r, testClass, now.Add(delay)); wait == 0 {
		t.Fatal("one refilled token admitted two requests")
	}
}

func TestIPv6ClientsShareTheirSlash64(t *testing.T) {
	l := newRateLimiter(nil)
	now := time.Now()
	exhaust(t, l, guardedRequest("[2001:db8:1:2::1]:1234"), testClass, now)
	if wait := l.wait(guardedRequest("[2001:db8:1:2:ffff::9]:1234"), testClass, now); wait == 0 {
		t.Fatal("rotating addresses within one /64 escaped the limit")
	}
	if wait := l.wait(guardedRequest("[2001:db8:1:3::1]:1234"), testClass, now); wait != 0 {
		t.Fatal("a different /64 shared the limit")
	}
}

func TestRateLimiterBoundsMemoryWithoutLockingOutNewClients(t *testing.T) {
	l := newRateLimiter(nil)
	now := time.Now()
	for i := range maxRatePeers {
		if wait := l.wait(guardedRequest(fmt.Sprintf("198.18.%d.%d:1", i/256, i%256)), testClass, now); wait != 0 {
			t.Fatal("capacity reached early")
		}
	}
	if wait := l.wait(guardedRequest("203.0.113.8:1"), testClass, now); wait != 0 {
		t.Fatal("a full table locked out a new client")
	}
	if len(l.buckets) > maxRatePeers {
		t.Fatalf("table grew to %d entries", len(l.buckets))
	}
	if wait := l.wait(guardedRequest("203.0.113.9:1"), testClass, now.Add(time.Minute+time.Second)); wait != 0 || len(l.buckets) != 1 {
		t.Fatalf("idle buckets were not reclaimed: %d remain", len(l.buckets))
	}
}

func TestRejectedRequestsDoNotReachHandlers(t *testing.T) {
	stub := &checkoutStub{}
	server := NewServer("", Services{Checkout: stub})
	for range accountLimit.limit + 1 {
		server.Handler.ServeHTTP(httptest.NewRecorder(), checkoutRequest(`{"email":"donor@example.org","amount_minor":3000}`))
	}
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, checkoutRequest(`{"email":"donor@example.org","amount_minor":3000}`))
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") == "" {
		t.Fatalf("got %d with Retry-After %q", response.Code, response.Header().Get("Retry-After"))
	}
	if stub.calls != accountLimit.limit {
		t.Fatalf("checkout ran %d times, want %d", stub.calls, accountLimit.limit)
	}
}
