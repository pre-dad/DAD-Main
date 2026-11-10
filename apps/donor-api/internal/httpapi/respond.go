package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/store"
)

const (
	readTimeout     = 5 * time.Second
	staffTimeout    = 15 * time.Second
	providerTimeout = 25 * time.Second
	maxJSONBody     = 256 << 10
	maxAmountMinor  = 99_999_999
)

const (
	donorCookie    = "dad_donor_session"
	staffCookie    = "dad_staff_session"
	partnerCookie  = "dad_partner_session"
	referralCookie = "dad_referral"
	companyCookie  = "dad_company"
	journeyCookie  = "dad_analytics_journey"
)

// classifier maps a service error to a public status and message.
type classifier func(error) (int, string)

// featureOff is the answer for a feature this deployment deliberately leaves
// unconfigured. Startup logs it once, so requests do not log it again, and
// clients get a stable code instead of advice to retry.
type featureOff struct{ code, detail string }

var (
	paymentsOff       = featureOff{"PAYMENTS_UNCONFIGURED", "Payments are not set up in this environment"}
	emailOff          = featureOff{"EMAIL_UNCONFIGURED", "Account email is not set up in this environment"}
	sourceStorageOff  = featureOff{"SOURCE_STORAGE_UNCONFIGURED", "Source document storage is not set up in this environment"}
	pushOff           = featureOff{"PUSH_UNCONFIGURED", "App notifications are not set up for this platform in this environment"}
	foundingClaimsOff = featureOff{"FOUNDING_CLAIMS_UNCONFIGURED", "Waitlist imports are not set up in this environment"}
	passkeysOff       = featureOff{"PASSKEYS_UNCONFIGURED", "Passkey sign-in is not set up in this environment"}
)

func (f featureOff) write(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, problem{Detail: f.detail, Code: f.code})
}

// unconfigured recognizes a service error that reports a feature left off.
func unconfigured(err error) (featureOff, bool) {
	switch {
	case errors.Is(err, payments.ErrUnconfigured):
		return paymentsOff, true
	case errors.Is(err, store.ErrSourceStorageUnconfigured):
		return sourceStorageOff, true
	case errors.Is(err, store.ErrPushUnconfigured):
		return pushOff, true
	case errors.Is(err, store.ErrFoundingClaimsUnconfigured):
		return foundingClaimsOff, true
	}
	return featureOff{}, false
}

// service describes the routes backed by one optional dependency: what they
// answer while it is unconfigured, their time budget, and their error mapping.
type service struct {
	configured  bool
	unavailable string
	timeout     time.Duration
	classify    classifier
}

// missing names what a lookup that found no row could not find.
func (s service) missing(detail string) service {
	classify := s.classify
	s.classify = func(err error) (int, string) {
		if errors.Is(err, pgx.ErrNoRows) {
			return http.StatusNotFound, detail
		}
		return classify(err)
	}
	return s
}

func (s service) available(w http.ResponseWriter, r *http.Request) bool {
	if !s.configured {
		writeError(w, r, http.StatusServiceUnavailable, s.unavailable, nil)
	}
	return s.configured
}

// serve answers with one bounded call to the service.
func (s service) serve(w http.ResponseWriter, r *http.Request, call func(context.Context) (any, error)) {
	if !s.available(w, r) {
		return
	}
	ctx, cancel := withTimeout(r, s.timeout)
	defer cancel()
	body, err := call(ctx)
	respond(w, r, body, err, s.classify)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, detail string, cause error) {
	if status >= http.StatusInternalServerError && cause != nil {
		// A client that disconnects cancels its own request; that is not a server failure.
		level, message := slog.LevelError, "request failed"
		if errors.Is(cause, context.Canceled) && r.Context().Err() != nil {
			level, message = slog.LevelInfo, "request canceled by client"
		}
		slog.LogAttrs(r.Context(), level, message,
			slog.String("route", r.Pattern), slog.Int("status", status), slog.String("cause", errorClass(cause)))
	}
	writeJSON(w, status, problem{Detail: detail})
}

func invalidFields(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusUnprocessableEntity, "Invalid request fields", nil)
}

func respond(w http.ResponseWriter, r *http.Request, body any, err error, classify classifier) {
	if off, ok := unconfigured(err); ok {
		off.write(w)
		return
	}
	if err != nil {
		status, detail := classify(err)
		writeError(w, r, status, detail, err)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// errorClass names a failure without its message, which may carry donor data.
func errorClass(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	if pg, ok := errors.AsType[*pgconn.PgError](err); ok {
		return "postgres " + pg.Code
	}
	for next := errors.Unwrap(err); next != nil; next = errors.Unwrap(err) {
		err = next
	}
	return fmt.Sprintf("%T", err)
}

// validToken bounds an opaque secret (an emailed proof or a session token)
// before it is hashed for lookup.
func validToken(token string) bool {
	n := utf8.RuneCountInString(token)
	return n >= 32 && n <= 128
}

// pathID reads a record ID from the path. A malformed ID names no record, so
// it answers 404 like any other unknown ID.
func pathID(w http.ResponseWriter, r *http.Request, name string) (pgtype.UUID, bool) {
	var id pgtype.UUID
	if id.Scan(r.PathValue(name)) != nil || !id.Valid {
		writeError(w, r, http.StatusNotFound, "Record not found", nil)
		return id, false
	}
	return id, true
}

// idempotencyKey reads the client's version 4 UUID for a retryable command,
// answering the request itself when it is missing or malformed.
func idempotencyKey(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	var key pgtype.UUID
	if key.Scan(r.Header.Get("Idempotency-Key")) != nil || !store.ValidRequestID(key) {
		writeError(w, r, http.StatusUnprocessableEntity, "Idempotency-Key must be a version 4 UUID", nil)
		return key, false
	}
	return key, true
}

func offsetParam(r *http.Request, name string) (int64, bool) {
	value := r.URL.Query().Get(name)
	if value == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(value, 10, 64)
	return n, err == nil && n >= 0 && n <= math.MaxInt64-50
}

func cookieValue(r *http.Request, name string) string {
	if cookie, err := r.Cookie(name); err == nil {
		return cookie.Value
	}
	return ""
}

// hostPrefix makes browsers refuse a cookie that has a Domain, a Path other
// than "/", or no Secure flag, so a sibling subdomain cannot plant a session.
const hostPrefix = "__Host-"

// sessionCookieValue reads a donor or partner session cookie. The __Host- name
// wins; the unprefixed name is the fallback for sessions issued before the
// prefix, which lapse within one session lifetime (30 days for donors, 12
// hours for partners), after which this fallback can be deleted.
func sessionCookieValue(r *http.Request, base string) string {
	if value := cookieValue(r, hostPrefix+base); value != "" {
		return value
	}
	return cookieValue(r, base)
}

// setSessionCookie issues a donor or partner session cookie, prefixed over
// HTTPS. It also expires the unprefixed name so a browser never holds two live
// sessions; an empty value with maxAge -1 clears both.
func (a *api) setSessionCookie(w http.ResponseWriter, base, value string, maxAge int) {
	if !a.secure {
		a.setCookie(w, base, value, maxAge, http.SameSiteLaxMode)
		return
	}
	a.setCookie(w, hostPrefix+base, value, maxAge, http.SameSiteLaxMode)
	a.setCookie(w, base, "", -1, http.SameSiteLaxMode)
}

func (a *api) setCookie(w http.ResponseWriter, name, value string, maxAge int, sameSite http.SameSite) {
	http.SetCookie(
		w,
		&http.Cookie{Name: name, Value: value, MaxAge: maxAge, Path: "/", HttpOnly: true, Secure: a.secure, SameSite: sameSite},
	)
}

func withTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}
