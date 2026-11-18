package httpapi

import (
	"context"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"
)

// rateClass separates budgets so a burst on one surface cannot starve another
// for clients sharing an address, such as everyone on an event's Wi-Fi.
type rateClass struct {
	name  string
	limit int // requests per minute, also the burst size
}

var (
	authLimit    = rateClass{"auth", 10}
	visitorLimit = rateClass{"visitor", 60}
	accountLimit = rateClass{"account", 30}
	staffLimit   = rateClass{"staff", 60}
	// Analytics is batched (up to MaxAnalyticsBatch events per call) and emitted
	// frequently by every client, so it gets a higher per-minute allowance.
	analyticsLimit = rateClass{"analytics", 120}
)

const maxRatePeers = 65536

// rateLimitTimeout bounds the shared limit's round trip; past it the request
// is refused rather than admitted without a limit.
const rateLimitTimeout = 2 * time.Second

type rateKey struct {
	class  string
	client netip.Addr
}

type bucket struct {
	tokens float64
	last   time.Time
}

// rateLimiter keeps a token bucket per client address and class. Only
// explicitly trusted proxy peers may supply the client address.
type rateLimiter struct {
	trustedProxies []netip.Prefix

	mu        sync.Mutex
	buckets   map[rateKey]*bucket
	nextSweep time.Time
}

func newRateLimiter(trustedProxies []netip.Prefix) *rateLimiter {
	return &rateLimiter{trustedProxies: trustedProxies, buckets: map[rateKey]*bucket{}}
}

// wait takes a token for the request's client and returns zero, or leaves the
// bucket untouched and returns how long until a request would be admitted.
func (l *rateLimiter) wait(r *http.Request, class rateClass, now time.Time) time.Duration {
	key := rateKey{class.name, l.client(r)}
	rate := float64(class.limit) / time.Minute.Seconds()

	l.mu.Lock()
	defer l.mu.Unlock()
	if now.After(l.nextSweep) {
		l.sweep(now)
	}
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= maxRatePeers {
			l.evictOne()
		}
		b = &bucket{tokens: float64(class.limit), last: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(float64(class.limit), b.tokens+now.Sub(b.last).Seconds()*rate)
	b.last = now
	if b.tokens < 1 {
		return time.Duration((1 - b.tokens) / rate * float64(time.Second))
	}
	b.tokens--
	return 0
}

func (l *rateLimiter) client(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	peer = peer.Unmap()
	for _, prefix := range l.trustedProxies {
		if prefix.Contains(peer) {
			if forwarded, err := netip.ParseAddr(r.Header.Get("X-Real-IP")); err == nil {
				peer = forwarded.Unmap()
			}
			break
		}
	}
	// One IPv6 subscriber typically controls a whole /64.
	if peer.Is6() {
		peer = netip.PrefixFrom(peer, 64).Masked().Addr()
	}
	return peer
}

// sweep drops buckets that have refilled; they are indistinguishable from new ones.
func (l *rateLimiter) sweep(now time.Time) {
	for key, b := range l.buckets {
		if now.Sub(b.last) >= time.Minute {
			delete(l.buckets, key)
		}
	}
	l.nextSweep = now.Add(time.Minute)
}

// evictOne bounds memory without refusing unseen clients, which would let a
// flood of addresses lock everyone else out.
func (l *rateLimiter) evictOne() {
	for key := range l.buckets {
		delete(l.buckets, key)
		return
	}
}

// guarded admits a mutation only from the given browser origin or, where
// appClients is set, from the native donor app, and rate limits either per
// client address before the handler runs: first in this process, which sheds
// a flood without a database round trip, then in the limit every replica
// shares.
func (a *api) guarded(origin string, class rateClass, appClients bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != origin && !(appClients && native(r)) {
			writeError(w, r, http.StatusForbidden, "Request origin is not allowed", nil)
			return
		}
		wait := a.limiter.wait(r, class, time.Now())
		if wait == 0 && a.RateLimit != nil {
			ctx, cancel := context.WithTimeout(r.Context(), rateLimitTimeout)
			var err error
			wait, err = a.RateLimit.Admit(ctx, class.name, a.limiter.client(r).String(), class.limit)
			cancel()
			if err != nil {
				writeError(w, r, http.StatusServiceUnavailable, "Request limits are temporarily unavailable", err)
				return
			}
		}
		if wait > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
			writeError(w, r, http.StatusTooManyRequests, "Too many requests", nil)
			return
		}
		next(w, r)
	}
}
