package payments

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stripe/stripe-go/v87"
)

func testGateway(server *httptest.Server, networkRetries int64) *StripeGateway {
	backends := stripe.NewBackendsWithConfig(&stripe.BackendConfig{
		URL:               stripe.String(server.URL),
		HTTPClient:        server.Client(),
		MaxNetworkRetries: stripe.Int64(networkRetries),
		LeveledLogger:     &stripe.LeveledLogger{Level: stripe.LevelNull},
	})
	return &StripeGateway{client: stripe.NewClient("sk_test_public_fixture", stripe.WithBackends(backends))}
}

func TestProcessorErrorClassifiesByStatus(t *testing.T) {
	const checkout = `{"mode":"payment","customer_email":"donor@example.org","line_items":[{"price_data":{"currency":"usd","unit_amount":3000,"product_data":{"name":"gift"}},"quantity":1}]}`
	for _, tc := range []struct {
		status   int
		body     string
		rejected bool
	}{
		{http.StatusBadRequest, `{"error":{"type":"invalid_request_error","code":"parameter_invalid_integer","message":"donor@example.org is bad"}}`, true},
		{http.StatusUnauthorized, `{"error":{"type":"invalid_request_error","code":"api_key_expired","message":"sk_test_secret"}}`, true},
		{http.StatusForbidden, `{"error":{"type":"invalid_request_error","message":"no"}}`, true},
		{http.StatusNotFound, `{"error":{"type":"invalid_request_error","code":"resource_missing","message":"no such session"}}`, true},
		{http.StatusRequestTimeout, `{"error":{"type":"api_error","message":"slow"}}`, false},
		{http.StatusConflict, `{"error":{"type":"invalid_request_error","code":"lock_timeout","message":"in flight"}}`, false},
		{http.StatusTooManyRequests, `{"error":{"type":"invalid_request_error","code":"rate_limit","message":"slow down"}}`, false},
		{http.StatusInternalServerError, `{"error":{"type":"api_error","message":"boom"}}`, false},
		{http.StatusBadGateway, ``, false},
		{http.StatusServiceUnavailable, `{"error":{"type":"api_error","message":"down"}}`, false},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Request-Id", "req_fixture")
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		gateway := testGateway(server, 0)
		ctx := context.Background()
		for name, call := range map[string]func() error{
			"create": func() error { _, err := gateway.CreateCheckout(ctx, []byte(checkout), "key"); return err },
			"read":   func() error { _, err := gateway.RetrieveSubscription(ctx, "sub_fixture"); return err },
			"update": func() error { return gateway.UpdateSubscriptionPaymentMethod(ctx, "sub_fixture", "pm_fixture", "key") },
			"refund": func() error { _, err := gateway.CreateRefund(ctx, "pi_fixture", 100, "command"); return err },
			"list":   func() error { _, err := gateway.ListPaymentRefunds(ctx, "pi_fixture"); return err },
		} {
			err := call()
			if got := errors.Is(err, ErrProcessorRejected); got != tc.rejected || errors.Is(err, ErrProcessorUnavailable) == tc.rejected {
				t.Errorf("%s status %d: got %v, rejected=%v", name, tc.status, err, tc.rejected)
			}
			if tc.rejected {
				var verdict *ProcessorError
				if !errors.As(err, &verdict) || verdict.Status != tc.status || verdict.RequestID != "req_fixture" {
					t.Errorf("%s status %d: verdict %+v", name, tc.status, verdict)
				}
			}
			if msg := err.Error(); strings.Contains(msg, "donor@example.org") || strings.Contains(msg, "sk_test_secret") {
				t.Errorf("%s status %d leaks processor text: %q", name, tc.status, msg)
			}
		}
		server.Close()
	}
}

func TestProcessorErrorTreatsTransportFailuresAsOutage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	gateway := testGateway(server, 0)
	server.Close()
	if _, err := gateway.RetrieveRefund(context.Background(), "re_fixture"); !errors.Is(err, ErrProcessorUnavailable) {
		t.Fatalf("refused connection: %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := processorError(cancelled.Err()); !errors.Is(err, ErrProcessorUnavailable) {
		t.Fatalf("cancelled context: %v", err)
	}
	if err := processorError(&stripe.Error{HTTPStatusCode: http.StatusOK}); !errors.Is(err, ErrProcessorUnavailable) {
		t.Fatalf("error without a client status: %v", err)
	}
}

func TestRetriedCheckoutReusesItsIdempotencyKey(t *testing.T) {
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		w.Header().Set("Content-Type", "application/json")
		if len(keys) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"type":"api_error","message":"boom"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"cs_test_fixture","url":"https://checkout.stripe.com/c/test","livemode":false}`))
	}))
	defer server.Close()
	gateway := testGateway(server, 0)
	raw := []byte(
		`{"mode":"payment","line_items":[{"price_data":{"currency":"usd","unit_amount":3000,"product_data":{"name":"gift"}},"quantity":1}]}`,
	)
	if _, err := gateway.CreateCheckout(context.Background(), raw, "dad-checkout-attempt"); !errors.Is(err, ErrProcessorUnavailable) {
		t.Fatalf("first attempt: %v", err)
	}
	if _, err := gateway.CreateCheckout(context.Background(), raw, "dad-checkout-attempt"); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] != "dad-checkout-attempt" || keys[1] != keys[0] {
		t.Fatalf("idempotency keys %v", keys)
	}
}

func TestSDKNetworkRetriesShareOneIdempotencyKey(t *testing.T) {
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		w.Header().Set("Content-Type", "application/json")
		if len(keys) < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"lock_timeout","message":"in flight"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"sub_fixture","object":"subscription"}`))
	}))
	defer server.Close()
	gateway := testGateway(server, 2)
	if err := gateway.ScheduleCancellation(context.Background(), "sub_fixture", 1800000000, "dad-cancel-attempt"); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 3 || keys[0] != "dad-cancel-attempt" || keys[1] != keys[0] || keys[2] != keys[0] {
		t.Fatalf("idempotency keys %v", keys)
	}
}

func TestCreateCheckoutRefusesUnusableSavedParameters(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer server.Close()
	_, err := testGateway(server, 0).CreateCheckout(context.Background(), []byte(`{"payment_method_types":["card"]}`), "key")
	if !errors.Is(err, ErrCheckoutRejected) || errors.Is(err, ErrProcessorUnavailable) || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
}

func TestValidateRemoteCheckoutRejectsWithReason(t *testing.T) {
	test, live := false, true
	ok := RemoteCheckout{ID: "cs_test_fixture", URL: "https://checkout.stripe.com/c/test", Livemode: &test}
	if err := ValidateRemoteCheckout(ok); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*RemoteCheckout){
		"missing livemode": func(r *RemoteCheckout) { r.Livemode = nil },
		"livemode":         func(r *RemoteCheckout) { r.Livemode = &live },
		"live id":          func(r *RemoteCheckout) { r.ID = "cs_live_fixture" },
		"long id":          func(r *RemoteCheckout) { r.ID = "cs_test_" + strings.Repeat("a", 250) },
		"foreign host":     func(r *RemoteCheckout) { r.URL = "https://checkout.stripe.com.evil.example/c/test" },
		"plain http":       func(r *RemoteCheckout) { r.URL = "http://checkout.stripe.com/c/test" },
		"userinfo":         func(r *RemoteCheckout) { r.URL = "https://user@checkout.stripe.com/c/test" },
		"odd port":         func(r *RemoteCheckout) { r.URL = "https://checkout.stripe.com:8443/c/test" },
		"unparsable url":   func(r *RemoteCheckout) { r.URL = "https://checkout.stripe.com/%zz" },
	} {
		remote := ok
		mutate(&remote)
		err := ValidateRemoteCheckout(remote)
		if !errors.Is(err, ErrCheckoutRejected) || errors.Is(err, ErrProcessorUnavailable) {
			t.Errorf("%s: %v", name, err)
		}
		if strings.Contains(err.Error(), "evil") || strings.Contains(err.Error(), "user@") {
			t.Errorf("%s: error repeats processor data: %v", name, err)
		}
	}
}
