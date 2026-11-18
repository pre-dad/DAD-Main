// Package httpapi owns the donor HTTP transport.
package httpapi

import (
	"cmp"
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"dad/donor-api/internal/buildinfo"
	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/wallet"
)

type RateLimitSource interface {
	Admit(ctx context.Context, class, client string, perMinute int) (time.Duration, error)
}

type WebhookReceiver interface {
	Receive(context.Context, []byte, string) error
}

// Services are the optional dependencies behind each route group; a nil
// service makes its routes answer 503 rather than fail at startup.
type Services struct {
	Origin         string // the exact public browser Origin admitted on donor mutations
	StaffOrigin    string // the exact back-office Origin admitted on staff mutations
	TrustedProxies []netip.Prefix
	Ready          func(context.Context) error
	// RateLimit shares each client's limits across replicas; nil leaves only
	// this process's buckets.
	RateLimit RateLimitSource

	Checkout CheckoutSource
	Webhook  WebhookReceiver
	// Partner portal: the public application, the authenticated
	// partner session surface, and the staff-side partner administration.
	PartnerApply        PartnerApplySource
	ApplicationReceipts ApplicationReceiptSource
	PartnerPortal       PartnerPortalSource
	StaffPartner        StaffPartnerSource
	EmployeeDonor       EmployeeDonorSource
	EmployeePartner     EmployeePartnerSource
	EmployeeStaff       EmployeeStaffSource

	Passwords       PasswordSource
	Account         AccountSource
	EmailConfigured bool
	EmailChange     EmailChangeSource
	Donor           DonorSource
	DonorPasskeys   DonorPasskeySource
	PhoneSignIn     PhoneSignInSource
	OIDCSignIn      OIDCSignInSource
	Analytics       AnalyticsSource
	Dashboard       DashboardSource
	Recurring       RecurringSource
	Push            PushSource
	PartnerPush     PushSource
	Wallet          *wallet.Issuer

	Staff           StaffSource
	Admin           AdminSource
	StaffFinance    StaffFinanceSource
	Exports         ExportSource
	ContentAdmin    ContentAdminSource
	Wording         WordingSource
	Initiatives     InitiativeSource
	InitiativeRead  InitiativeReadSource
	TodayInitiative TodayInitiativeSource
	Badges          BadgeSource
	FoundingClaims  FoundingClaimSource
	Network         NetworkSource
	Events          EventSource
	EventRead       EventReadSource
	Reservations    ReservationSource
	Reporting       ReportSource
	// EventCheckInSecret is the HMAC key for QR check-in tokens (E-8); empty
	// disables check-in.
	EventCheckInSecret string
}

type api struct {
	Services
	limiter     *rateLimiter
	secure      bool
	staffSecure bool
	staffCookie string
}

// WriteTimeout exceeds providerTimeout so a slow processor call can still answer.
const writeTimeout = 30 * time.Second

func NewServer(address string, services Services) *http.Server {
	services.Origin = cmp.Or(services.Origin, "http://localhost:3000")
	services.StaffOrigin = cmp.Or(services.StaffOrigin, "http://staff.localhost:3000")
	a := &api{
		Services:    services,
		limiter:     newRateLimiter(services.TrustedProxies),
		secure:      strings.HasPrefix(services.Origin, "https:"),
		staffSecure: strings.HasPrefix(services.StaffOrigin, "https:"),
		staffCookie: staffCookie,
	}
	if a.staffSecure {
		a.staffCookie = "__Host-" + staffCookie
	}
	mux := http.NewServeMux()
	for _, route := range a.routes() {
		mux.HandleFunc(route.pattern, route.handler)
	}
	return &http.Server{
		Addr:              address,
		Handler:           secureHeaders(jsonFallback(mux)),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
}

// secureHeaders applies the API's response defaults; individual handlers may
// relax caching for public data.
func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// jsonFallback keeps ServeMux's answers for unmatched requests, including
// Allow on 405 and Location on path-cleaning redirects, in the JSON error shape.
func jsonFallback(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		capture := &statusCapture{header: http.Header{}}
		mux.ServeHTTP(capture, r)
		for _, name := range []string{"Allow", "Location"} {
			if value := capture.header.Get(name); value != "" {
				w.Header().Set(name, value)
			}
		}
		writeJSON(w, capture.status, problem{Detail: http.StatusText(capture.status)})
	})
}

type statusCapture struct {
	header http.Header
	status int
}

func (c *statusCapture) Header() http.Header         { return c.header }
func (c *statusCapture) Write(b []byte) (int, error) { return len(b), nil }
func (c *statusCapture) WriteHeader(status int)      { c.status = status }

func (a *api) live(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "alive", "commit": buildinfo.Revision()})
}

func (a *api) ready(w http.ResponseWriter, r *http.Request) {
	const unavailable = "Donor database unavailable or migrations pending"
	if a.Ready == nil {
		writeError(w, r, http.StatusServiceUnavailable, unavailable, nil)
		return
	}
	ctx, cancel := withTimeout(r, 3*time.Second)
	defer cancel()
	if err := a.Ready(ctx); err != nil {
		writeError(w, r, http.StatusServiceUnavailable, unavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready", "payments": "test_mode_only", "commit": buildinfo.Revision()})
}

func (a *api) stripeWebhook(w http.ResponseWriter, r *http.Request) {
	const unavailable = "Webhook verification unavailable"
	if a.Webhook == nil {
		writeError(w, r, http.StatusServiceUnavailable, unavailable, nil)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, payments.MaxWebhookBytes))
	if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
		writeError(w, r, http.StatusRequestEntityTooLarge, "Webhook payload is too large", nil)
		return
	}
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "Invalid Stripe event", nil)
		return
	}
	ctx, cancel := withTimeout(r, readTimeout)
	defer cancel()
	switch err := a.Webhook.Receive(ctx, body, r.Header.Get("Stripe-Signature")); {
	case errors.Is(err, payments.ErrInvalidEvent):
		writeError(w, r, http.StatusBadRequest, "Invalid Stripe event", nil)
	case errors.Is(err, payments.ErrEventShape):
		writeError(w, r, http.StatusBadRequest, "Invalid Stripe event shape", nil)
	case err != nil:
		writeError(w, r, http.StatusServiceUnavailable, unavailable, err)
	default:
		writeJSON(w, http.StatusOK, map[string]bool{"received": true})
	}
}
