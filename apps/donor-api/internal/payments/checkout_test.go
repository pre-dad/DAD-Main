package payments

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestCheckoutUsesPinnedSDKAndSavedParameters(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "POST" || r.URL.Path != "/v1/checkout/sessions" || r.Header.Get("Stripe-Version") != APIVersion ||
			r.Header.Get("Idempotency-Key") != "dad-checkout-test" {
			t.Error("Stripe boundary changed")
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("allowed_payment_method_types[0]") != "card" || r.Form.Get("payment_method_types[0]") != "" ||
			r.Form.Get("line_items[0][price_data][unit_amount]") != "3000" ||
			r.Form.Get("customer") != "" {
			t.Errorf("wrong Stripe request form: %v", r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cs_test_fixture","url":"https://checkout.stripe.com/c/test","livemode":false}`))
	}))
	defer server.Close()
	gateway := gatewayAt(server)
	raw := []byte(
		`{"mode":"payment","customer_email":"donor@example.org","allowed_payment_method_types":["card"],"line_items":[{"price_data":{"currency":"usd","unit_amount":3000,"product_data":{"name":"DAD one-time gift"}},"quantity":1}]}`,
	)
	result, err := gateway.CreateCheckout(context.Background(), raw, "dad-checkout-test")
	if err != nil || ValidateRemoteCheckout(result) != nil || calls != 1 {
		t.Fatalf("SDK request failed: %v", err)
	}
	if _, err = gateway.CreateCheckout(context.Background(), []byte(`{"payment_method_types":["card"]}`), "old-key"); err == nil ||
		calls != 1 {
		t.Fatal("unknown persisted params silently changed")
	}
}

func TestValidateRemoteCheckoutBoundaries(t *testing.T) {
	live, test := true, false
	remote := func(mutate func(*RemoteCheckout)) RemoteCheckout {
		r := RemoteCheckout{ID: "cs_test_fixture", URL: "https://checkout.stripe.com/c/test", Livemode: &test}
		mutate(&r)
		return r
	}
	for _, tc := range []struct {
		name   string
		mutate func(*RemoteCheckout)
		ok     bool
	}{
		{"valid", func(*RemoteCheckout) {}, true},
		{"explicit https port", func(r *RemoteCheckout) { r.URL = "https://checkout.stripe.com:443/c/test" }, true},
		{"longest id", func(r *RemoteCheckout) { r.ID = "cs_test_" + strings.Repeat("a", 255-len("cs_test_")) }, true},
		{"id too long", func(r *RemoteCheckout) { r.ID = "cs_test_" + strings.Repeat("a", 256-len("cs_test_")) }, false},
		{"live id", func(r *RemoteCheckout) { r.ID = "cs_live_fixture" }, false},
		{"livemode true", func(r *RemoteCheckout) { r.Livemode = &live }, false},
		{"livemode missing", func(r *RemoteCheckout) { r.Livemode = nil }, false},
		{"http", func(r *RemoteCheckout) { r.URL = "http://checkout.stripe.com/c/test" }, false},
		{"other host", func(r *RemoteCheckout) { r.URL = "https://checkout.stripe.com.example.org/c/test" }, false},
		{"other port", func(r *RemoteCheckout) { r.URL = "https://checkout.stripe.com:8443/c/test" }, false},
		{"userinfo", func(r *RemoteCheckout) { r.URL = "https://user@checkout.stripe.com/c/test" }, false},
	} {
		if err := ValidateRemoteCheckout(remote(tc.mutate)); (err == nil) != tc.ok {
			t.Errorf("%s: got %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

func TestPaymentBoundariesRejectLiveAndUnsafeOrigins(t *testing.T) {
	for _, key := range []string{"sk_live_test", "rk_live_test", ""} {
		if _, err := NewStripeGateway(key); err == nil {
			t.Fatal("non-test credentials accepted")
		}
	}
	for _, origin := range []string{"http://example.org", "https://user:pass@example.org", "https://example.org/", "https://example.org?x=y", "https://example.org#part", "https://", "http://staff.localhost.example.org", "http://localhostx",
		"https://Donor.example.org", "HTTPS://donor.example.org", "https://donor.example.org:443", "http://localhost:80", "https://donor.example.org:",
		"https://donor.example.org:0443", "https://donor.example.org:99999", "https://donor.example.org:0", "http://localhost:03000"} {
		if ValidateOrigin(origin) == nil {
			t.Fatalf("unsafe origin accepted: %s", origin)
		}
	}
	for _, origin := range []string{"http://localhost:3000", "http://127.0.0.1:3000", "http://staff.localhost:3000", "https://donor.example.org"} {
		if err := ValidateOrigin(origin); err != nil {
			t.Fatal(err)
		}
	}
	live := false
	for _, result := range []RemoteCheckout{{ID: "cs_test_fixture", URL: "https://checkout.stripe.com/c/test"}, {ID: "cs_live_fixture", URL: "https://checkout.stripe.com/c/test", Livemode: &live}, {ID: "cs_test_fixture", URL: "https://checkout.stripe.com.evil.example/c/test", Livemode: &live}, {ID: "cs_test_fixture", URL: "https://user@checkout.stripe.com/c/test", Livemode: &live}} {
		if ValidateRemoteCheckout(result) == nil {
			t.Fatal("unsafe checkout response accepted")
		}
	}
}

func TestFinancialGatewayUsesPinnedSDKAndIdempotentCardUpdate(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Stripe-Version") != APIVersion {
			t.Error("API version drift")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" {
			if r.URL.Path != "/v1/subscriptions/sub_fixture" || r.Header.Get("Idempotency-Key") != "dad-payment-apply-fixture" {
				t.Error("mutation identity changed")
			}
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Form.Get("default_payment_method") != "pm_fixture" || r.Form.Get("proration_behavior") != "none" || len(r.Form) != 2 {
				t.Errorf("unexpected card update parameters: %v", r.Form)
			}
		} else if r.Method != "GET" {
			t.Error("unexpected method")
		} else if r.URL.Path == "/v1/payment_intents/fixture" && r.URL.Query().Get("expand[0]") != "latest_charge" {
			t.Errorf("payment intent read lacks its charge: %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"id":"fixture","livemode":false}`))
	}))
	defer server.Close()
	g := gatewayAt(server)
	for _, read := range []func(context.Context, string) (Object, error){g.RetrievePaymentIntent, g.RetrieveRefund, g.RetrieveSubscription, g.RetrieveCheckout, g.RetrieveSetupIntent} {
		value, err := read(context.Background(), "fixture")
		if err != nil || !value.BoolIs("livemode", false) {
			t.Fatal("failed object read", err)
		}
	}
	if err := g.UpdateSubscriptionPaymentMethod(context.Background(), "sub_fixture", "pm_fixture", "dad-payment-apply-fixture"); err != nil {
		t.Fatal(err)
	}
	if calls != 6 {
		t.Fatalf("requests=%d", calls)
	}
}

func TestSubscriptionMutationsNeverProrate(t *testing.T) {
	var forms []map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/subscriptions/sub_fixture" || r.Header.Get("Idempotency-Key") != "key" {
			t.Error("Stripe boundary changed", r.Method, r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		form := map[string]string{}
		for name := range r.PostForm {
			form[name] = r.PostForm.Get(name)
		}
		forms = append(forms, form)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"sub_fixture","object":"subscription"}`))
	}))
	defer server.Close()
	gateway := gatewayAt(server)
	ctx := context.Background()
	for _, err := range []error{
		gateway.ScheduleCancellation(ctx, "sub_fixture", 1800000000, "key"),
		gateway.ChangeMonthlyAmount(ctx, "sub_fixture", "si_fixture", "prod_fixture", 9000, "key"),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	want := []map[string]string{
		{"cancel_at": "1800000000", "proration_behavior": "none"},
		{
			"items[0][id]":                              "si_fixture",
			"items[0][quantity]":                        "1",
			"items[0][price_data][currency]":            "usd",
			"items[0][price_data][product]":             "prod_fixture",
			"items[0][price_data][recurring][interval]": "month",
			"items[0][price_data][unit_amount]":         "9000",
			"proration_behavior":                        "none",
		},
	}
	if !reflect.DeepEqual(forms, want) {
		t.Fatalf("got %v\nwant %v", forms, want)
	}
}

func TestExpireCheckoutReportsWhoWonTheRace(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/checkout/sessions/cs_test_open/expire":
			_, _ = w.Write([]byte(`{"id":"cs_test_open","object":"checkout.session","status":"expired","livemode":false}`))
		case "/v1/checkout/sessions/cs_test_paid/expire":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"not open"}}`))
		case "/v1/checkout/sessions/cs_test_paid":
			_, _ = w.Write([]byte(`{"id":"cs_test_paid","object":"checkout.session","status":"complete","livemode":false}`))
		case "/v1/checkout/sessions/cs_test_gone/expire":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	gateway := gatewayAt(server)
	ctx := context.Background()
	if status, err := gateway.ExpireCheckout(ctx, "cs_test_open", "key"); err != nil || status != "expired" {
		t.Fatalf("open session: %q %v", status, err)
	}
	if status, err := gateway.ExpireCheckout(ctx, "cs_test_paid", "key"); err != nil || status != "complete" {
		t.Fatalf("paid session: %q %v", status, err)
	}
	if _, err := gateway.ExpireCheckout(ctx, "cs_test_gone", "key"); !errors.Is(err, ErrProcessorUnavailable) {
		t.Fatalf("unreadable session: %v", err)
	}
	if len(paths) != 5 || paths[0] != "POST /v1/checkout/sessions/cs_test_open/expire" ||
		paths[2] != "GET /v1/checkout/sessions/cs_test_paid" {
		t.Fatalf("unexpected requests %v", paths)
	}
}
