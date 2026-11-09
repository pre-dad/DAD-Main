package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/store"
)

type checkoutStub struct {
	calls   int
	request payments.CheckoutRequest
	session string
	err     error
}

func (s *checkoutStub) BeginCheckout(ctx context.Context, r payments.CheckoutRequest) (payments.CheckoutResult, error) {
	if _, ok := ctx.Deadline(); !ok {
		panic("unbounded checkout")
	}
	s.calls++
	s.request = r
	return payments.CheckoutResult{CheckoutID: r.RequestID.String(), URL: "https://checkout.stripe.com/c/test"}, s.err
}

func (s *checkoutStub) BeginNativeCheckout(
	ctx context.Context,
	session string,
	key pgtype.UUID,
	amount int64,
	visitor *string,
) (payments.NativeCheckoutResult, error) {
	if _, ok := ctx.Deadline(); !ok {
		panic("unbounded checkout")
	}
	s.calls++
	s.session, s.request.RequestID, s.request.Amount, s.request.Visitor = session, key, amount, visitor
	return payments.NativeCheckoutResult{
		ExpiresAt: time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC),
		SessionID: "cs_test_native",
		URL:       "https://checkout.stripe.com/c/test",
	}, s.err
}

func checkoutRequest(body string) *http.Request {
	r := httptest.NewRequest("POST", "/donor/checkouts", strings.NewReader(body))
	r.AddCookie(&http.Cookie{Name: donorCookie, Value: "verified-donor-session"})
	r.Header.Set("Origin", "http://localhost:3000")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", "8f4f52bb-99f6-490e-8313-5156cc4f8b45")
	return r
}

func TestCheckoutRejectsInvalidRequestsBeforeTheProcessor(t *testing.T) {
	valid := `{"email":"donor@example.org","amount_minor":3000}`
	for _, tc := range []struct {
		name, body string
		header     map[string]string
		status     int
	}{
		{"boolean amount", `{"email":"donor@example.org","amount_minor":true}`, nil, 422},
		{"fractional amount", `{"email":"donor@example.org","amount_minor":3000.0}`, nil, 422},
		{"string amount", `{"email":"donor@example.org","amount_minor":"3000"}`, nil, 422},
		{"null amount", `{"email":"donor@example.org","amount_minor":null}`, nil, 422},
		{"null email", `{"email":null,"amount_minor":3000}`, nil, 422},
		{"null frequency", `{"email":"donor@example.org","amount_minor":3000,"frequency":null}`, nil, 422},
		{"unknown frequency", `{"email":"donor@example.org","amount_minor":3000,"frequency":"WEEKLY"}`, nil, 422},
		{"unknown field", `{"email":"donor@example.org","amount_minor":3000,"unexpected":"secret"}`, nil, 422},
		{"trailing value", valid + ` {}`, nil, 422},
		{"amount over ceiling by a cent", `{"email":"donor@example.org","amount_minor":90001}`, nil, 422},
		{"amount at $31 a day", `{"email":"donor@example.org","amount_minor":93000}`, nil, 422},
		{"amount under minimum", `{"email":"donor@example.org","amount_minor":99}`, nil, 422},
		{"amount under $1 a day", `{"email":"donor@example.org","amount_minor":2999}`, nil, 422},
		{"amount not whole dollars a day", `{"email":"donor@example.org","amount_minor":4500}`, nil, 422},
		{"missing key", valid, map[string]string{"Idempotency-Key": ""}, 422},
		{"version 1 key", valid, map[string]string{"Idempotency-Key": "8f4f52bb-99f6-190e-8313-5156cc4f8b45"}, 422},
		{"text body", valid, map[string]string{"Content-Type": "text/plain"}, 415},
		{"oversized body", `{"email":"` + strings.Repeat("a", maxJSONBody) + `"}`, nil, 413},
		{"foreign origin", valid, map[string]string{"Origin": "https://evil.example.org"}, 403},
	} {
		stub := &checkoutStub{}
		r := checkoutRequest(tc.body)
		for name, value := range tc.header {
			r.Header.Set(name, value)
		}
		response := httptest.NewRecorder()
		NewServer("", Services{Checkout: stub}).Handler.ServeHTTP(response, r)
		if response.Code != tc.status || stub.calls != 0 {
			t.Errorf("%s: got %d after %d processor calls, want %d before any", tc.name, response.Code, stub.calls, tc.status)
		}
		if strings.Contains(response.Body.String(), "secret") {
			t.Errorf("%s: response echoed request content", tc.name)
		}
	}
}

func TestSignedInCheckoutCarriesReferralAndDefaultsToMonthly(t *testing.T) {
	stub := &checkoutStub{}
	r := checkoutRequest(`{"email":"donor@example.org","amount_minor":3000}`)
	r.AddCookie(&http.Cookie{Name: "dad_referral", Value: "visitor"})
	response := httptest.NewRecorder()
	NewServer("", Services{Checkout: stub}).Handler.ServeHTTP(response, r)
	if stub.request.Session != "verified-donor-session" {
		t.Fatal("checkout did not carry the authenticated session")
	}
	if response.Code != 201 || stub.request.Frequency != "MONTHLY" || stub.request.Visitor == nil || *stub.request.Visitor != "visitor" {
		t.Fatalf("got %d %+v, want 201 with a monthly request carrying the referral visitor", response.Code, stub.request)
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("checkout response headers: %v", response.Header())
	}
}

func TestCheckoutAcceptsTheGiftCeiling(t *testing.T) {
	stub := &checkoutStub{}
	r := checkoutRequest(`{"email":"donor@example.org","amount_minor":90000}`)
	response := httptest.NewRecorder()
	NewServer("", Services{Checkout: stub}).Handler.ServeHTTP(response, r)
	if response.Code != 201 || stub.request.Amount != 90000 {
		t.Fatalf("got %d amount %d, want 201 at the $900/mo ceiling", response.Code, stub.request.Amount)
	}
}

func TestCheckoutErrorsArePrivate(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{{payments.ErrCheckoutConflict, 409}, {payments.ErrCheckoutThrottled, 429}, {payments.ErrCheckoutInput, 422}, {store.ErrDenied, 401}, {errors.New("private processor details"), 503}} {
		server := NewServer("", Services{Checkout: &checkoutStub{err: tc.err}})
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, checkoutRequest(`{"email":"donor@example.org","amount_minor":3000}`))
		if response.Code != tc.status || strings.Contains(response.Body.String(), "private processor") {
			t.Errorf("%v: got %d %s, want %d without processor detail", tc.err, response.Code, response.Body, tc.status)
		}
	}
}

func TestThrottledCheckoutRestartTellsTheClientWhenToRetry(t *testing.T) {
	server := NewServer("", Services{Checkout: &checkoutStub{err: payments.ErrCheckoutThrottled}})
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, checkoutRequest(`{"email":"donor@example.org","amount_minor":3000}`))
	if response.Code != 429 || response.Header().Get("Retry-After") != "600" {
		t.Fatalf("got %d with Retry-After %q, want 429 and 600", response.Code, response.Header().Get("Retry-After"))
	}
}

func nativeCheckoutRequest(body string) *http.Request {
	r := appRequest("POST", "/donor/me/recurring/checkout", body)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", "8f4f52bb-99f6-490e-8313-5156cc4f8b45")
	return r
}

func TestNativeRecurringCheckoutStartsForTheSignedInDonor(t *testing.T) {
	stub := &checkoutStub{}
	response := httptest.NewRecorder()
	NewServer("", Services{Checkout: stub}).Handler.ServeHTTP(response, nativeCheckoutRequest(`{"amount_minor":6000}`))
	if response.Code != http.StatusCreated || stub.session != appSession || stub.request.Amount != 6000 ||
		stub.request.RequestID.String() != "8f4f52bb-99f6-490e-8313-5156cc4f8b45" {
		t.Fatalf("got %d %s for session %q, request %+v", response.Code, response.Body, stub.session, stub.request)
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || len(body) != 3 || body["session_id"] != "cs_test_native" ||
		body["url"] != "https://checkout.stripe.com/c/test" || body["expires_at"] != "2027-01-02T03:04:05Z" {
		t.Fatalf("unexpected body %s", response.Body)
	}
}

func TestNativeRecurringCheckoutCarriesTheAppsReferral(t *testing.T) {
	stub := &checkoutStub{}
	visitor := strings.Repeat("v", 43)
	response := httptest.NewRecorder()
	NewServer("", Services{Checkout: stub}).Handler.ServeHTTP(
		response,
		nativeCheckoutRequest(`{"amount_minor":6000,"referral_token":"`+visitor+`"}`),
	)
	if response.Code != http.StatusCreated || stub.request.Visitor == nil || *stub.request.Visitor != visitor {
		t.Fatalf("got %d with visitor %v, want 201 carrying the referral token", response.Code, stub.request.Visitor)
	}
}

func TestNativeRecurringCheckoutRefusals(t *testing.T) {
	const key = "8f4f52bb-99f6-490e-8313-5156cc4f8b45"
	for _, tc := range []struct {
		name   string
		mutate func(*http.Request)
		body   string
		err    error
		status int
	}{
		{"fractional amount", nil, `{"amount_minor":3000.0}`, nil, 422},
		{"amount off the daily grid", nil, `{"amount_minor":4500}`, nil, 422},
		{"amount under the minimum", nil, `{"amount_minor":2999}`, nil, 422},
		{"amount over the ceiling", nil, `{"amount_minor":93000}`, nil, 422},
		{"missing amount", nil, `{}`, nil, 422},
		{"email in the body", nil, `{"amount_minor":3000,"email":"other@example.org"}`, nil, 422},
		{"short referral token", nil, `{"amount_minor":3000,"referral_token":"short"}`, nil, 422},
		{"null referral token", nil, `{"amount_minor":3000,"referral_token":null}`, nil, 422},
		{"missing key", func(r *http.Request) { r.Header.Del("Idempotency-Key") }, `{"amount_minor":3000}`, nil, 422},
		{"anonymous", func(r *http.Request) { r.Header.Del("Authorization") }, `{"amount_minor":3000}`, nil, 401},
		{"browser request", func(r *http.Request) {
			r.Header.Del("X-DAD-Client")
			r.Header.Del("Authorization")
			r.AddCookie(&http.Cookie{Name: donorCookie, Value: "verified-donor-session"})
			r.Header.Set("Origin", "http://localhost:3000")
			r.AddCookie(&http.Cookie{Name: donorCookie, Value: appSession})
		}, `{"amount_minor":3000}`, nil, 403},
		{"gift already active", nil, `{"amount_minor":3000}`, payments.ErrCheckoutConflict, 409},
		{"session ended", nil, `{"amount_minor":3000}`, store.ErrDenied, 401},
		{"no processor", nil, `{"amount_minor":3000}`, payments.ErrUnconfigured, 503},
		{"processor failure", nil, `{"amount_minor":3000}`, errors.New("private processor details"), 503},
	} {
		stub := &checkoutStub{err: tc.err}
		r := nativeCheckoutRequest(tc.body)
		r.Header.Set("Idempotency-Key", key)
		if tc.mutate != nil {
			tc.mutate(r)
		}
		response := httptest.NewRecorder()
		NewServer("", Services{Checkout: stub}).Handler.ServeHTTP(response, r)
		if response.Code != tc.status || strings.Contains(response.Body.String(), "private") {
			t.Errorf("%s: got %d %s, want %d", tc.name, response.Code, response.Body, tc.status)
		}
		if tc.err == nil && stub.calls != 0 {
			t.Errorf("%s: reached the processor", tc.name)
		}
	}
}

func TestNativeRecurringCheckoutIsRateLimited(t *testing.T) {
	server := NewServer("", Services{Checkout: &checkoutStub{}})
	limited := false
	for range 200 {
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, nativeCheckoutRequest(`{"amount_minor":3000}`))
		if response.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("the account budget did not apply to native checkout")
	}
}

func TestCheckoutRequiresSignInBeforeLookingAtEmail(t *testing.T) {
	for _, email := range []string{"new@example.org", "donor@example.org", "staff@example.org"} {
		stub := &checkoutStub{}
		r := checkoutRequest(`{"email":"` + email + `","amount_minor":3000}`)
		r.Header.Del("Cookie")
		response := httptest.NewRecorder()
		NewServer("", Services{Checkout: stub}).Handler.ServeHTTP(response, r)
		if response.Code != http.StatusUnauthorized || stub.calls != 0 {
			t.Fatalf("anonymous checkout got %d after %d calls", response.Code, stub.calls)
		}
	}
}
