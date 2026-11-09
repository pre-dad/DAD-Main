package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"

	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/store"
)

type CheckoutSource interface {
	BeginCheckout(context.Context, payments.CheckoutRequest) (payments.CheckoutResult, error)
	BeginNativeCheckout(context.Context, string, pgtype.UUID, int64, *string) (payments.NativeCheckoutResult, error)
}

// restartRetryAfter is the wait a throttled restart is told; the budget
// refills within the hour and an unpaid checkout lapses in one.
const restartRetryAfter = 10 * time.Minute

type checkoutBody struct {
	Name      string          `json:"name,omitempty"`
	Zip       string          `json:"zip,omitempty"`
	City      string          `json:"city,omitempty"`
	Email     *string         `json:"email"`
	Amount    *int64          `json:"amount_minor"`
	Frequency json.RawMessage `json:"frequency"`
}

func (a *api) beginCheckout(w http.ResponseWriter, r *http.Request, session string) {
	const unavailable = "Checkout is temporarily unavailable; retry the same request"
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var body checkoutBody
	if !decodeJSON(w, r, &body) {
		return
	}
	frequency, ok := optional(body.Frequency, "MONTHLY")
	if !ok || frequency != "MONTHLY" || body.Email == nil || body.Amount == nil ||
		utf8.RuneCountInString(*body.Email) > maxEmailRunes || !store.ValidGiftAmount(*body.Amount) {
		invalidFields(w, r)
		return
	}
	if a.Checkout == nil {
		writeError(w, r, http.StatusServiceUnavailable, unavailable, nil)
		return
	}
	request := payments.CheckoutRequest{
		Session:   session,
		Journey:   cookieValue(r, journeyCookie),
		Name:      body.Name,
		Zip:       body.Zip,
		City:      body.City,
		RequestID: key,
		Email:     *body.Email,
		Amount:    *body.Amount,
		Frequency: frequency,
	}
	if visitor := cookieValue(r, referralCookie); visitor != "" {
		request.Visitor = &visitor
	}
	ctx, cancel := withTimeout(r, providerTimeout)
	defer cancel()
	result, err := a.Checkout.BeginCheckout(ctx, request)
	switch {
	case errors.Is(err, payments.ErrUnconfigured):
		paymentsOff.write(w)
	case errors.Is(err, payments.ErrCheckoutConflict):
		writeError(
			w,
			r,
			http.StatusConflict,
			"Your monthly gift is already active or in progress. Wait a few seconds and try again.",
			nil,
		)
	case errors.Is(err, payments.ErrCheckoutThrottled):
		w.Header().Set("Retry-After", strconv.Itoa(int(restartRetryAfter.Seconds())))
		writeError(w, r, http.StatusTooManyRequests, "Too many checkout attempts. Try again later.", nil)
	case errors.Is(err, store.ErrDenied):
		writeError(w, r, http.StatusUnauthorized, "Sign in required", nil)
	case errors.Is(err, payments.ErrCheckoutInput):
		invalidFields(w, r)
	case err != nil:
		writeError(w, r, http.StatusServiceUnavailable, unavailable, err)
	default:
		writeJSON(w, http.StatusCreated, result)
	}
}

type nativeCheckoutBody struct {
	Amount   int64           `json:"amount_minor"`
	Referral json.RawMessage `json:"referral_token"`
}

func (b nativeCheckoutBody) valid() bool {
	referral, ok := optional(b.Referral, "")
	return store.ValidGiftAmount(b.Amount) && ok && (b.Referral == nil || validToken(referral))
}

// visitor is the referral token the app kept, if it sent one.
func (b nativeCheckoutBody) visitor() *string {
	if b.Referral == nil {
		return nil
	}
	referral, _ := optional(b.Referral, "")
	return &referral
}

// startRecurringCheckout opens hosted checkout for the signed-in donor's first
// monthly gift, which the website's guest checkout cannot do for an app: it
// is keyed by an email the account already holds and returns to a web page.
func (a *api) startRecurringCheckout(w http.ResponseWriter, r *http.Request, session string) {
	const unavailable = "Checkout is temporarily unavailable; retry the same request"
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	body, ok := decodeValid[nativeCheckoutBody](w, r)
	if !ok {
		return
	}
	if a.Checkout == nil {
		writeError(w, r, http.StatusServiceUnavailable, unavailable, nil)
		return
	}
	ctx, cancel := withTimeout(r, providerTimeout)
	defer cancel()
	result, err := a.Checkout.BeginNativeCheckout(ctx, session, key, body.Amount, body.visitor())
	switch {
	case errors.Is(err, payments.ErrUnconfigured):
		paymentsOff.write(w)
	case errors.Is(err, payments.ErrCheckoutConflict):
		writeError(w, r, http.StatusConflict, "A monthly gift is already active or in progress", nil)
	case errors.Is(err, store.ErrDenied):
		writeError(w, r, http.StatusUnauthorized, "Sign in required", nil)
	case errors.Is(err, payments.ErrCheckoutInput):
		invalidFields(w, r)
	case err != nil:
		writeError(w, r, http.StatusServiceUnavailable, unavailable, err)
	default:
		writeJSON(w, http.StatusCreated, result)
	}
}
