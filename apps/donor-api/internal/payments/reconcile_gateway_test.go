package payments

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v87"
)

// stripeTestGateway points the pinned SDK at a local test server, exactly as
// checkout_test.go does, so no live Stripe is touched.
func stripeTestGateway(t *testing.T, handler http.HandlerFunc) (*StripeGateway, func()) {
	t.Helper()
	server := httptest.NewServer(handler)
	backends := stripe.NewBackendsWithConfig(
		&stripe.BackendConfig{
			URL:               stripe.String(server.URL),
			HTTPClient:        server.Client(),
			MaxNetworkRetries: stripe.Int64(0),
			LeveledLogger:     &stripe.LeveledLogger{Level: stripe.LevelNull},
		},
	)
	return &StripeGateway{client: stripe.NewClient("sk_test_public_fixture", stripe.WithBackends(backends)), budget: DefaultRateBudget()}, server.Close
}

func TestReconcileListPagesByCreatedWindow(t *testing.T) {
	// A full day in New_York converted to a UTC created range.
	start := time.Date(2027, 3, 1, 5, 0, 0, 0, time.UTC)
	end := time.Date(2027, 3, 2, 4, 59, 59, 0, time.UTC)
	for _, tc := range []struct {
		name, path string
		call       func(g *StripeGateway) ([]Object, error)
	}{
		{"charges", "/v1/charges", func(g *StripeGateway) ([]Object, error) {
			return g.ListChargesByCreated(context.Background(), start, end)
		}},
		{"refunds", "/v1/refunds", func(g *StripeGateway) ([]Object, error) {
			return g.ListRefundsByCreated(context.Background(), start, end)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			g, closeServer := stripeTestGateway(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "GET" || r.URL.Path != tc.path || r.Header.Get("Stripe-Version") != APIVersion {
					t.Errorf("Stripe boundary changed: %s %s", r.Method, r.URL.Path)
				}
				q := r.URL.Query()
				if q.Get("created[gte]") != "1803877200" || q.Get("created[lte]") != "1803963599" || q.Get("limit") != "100" {
					t.Errorf("wrong window query: %s", r.URL.RawQuery)
				}
				w.Header().Set("Content-Type", "application/json")
				// First page has_more=true, second closes the window.
				if q.Get("starting_after") == "" {
					_, _ = w.Write(
						[]byte(
							`{"object":"list","has_more":true,"data":[{"id":"obj_1","amount":3000,"livemode":false,"metadata":{"email":"private@example.org"},"billing_details":{"name":"Private Donor"}},{"id":"obj_2","amount":5000,"livemode":false}]}`,
						),
					)
				} else if q.Get("starting_after") == "obj_2" {
					_, _ = w.Write([]byte(`{"object":"list","has_more":false,"data":[{"id":"obj_3","amount":7000,"livemode":false}]}`))
				} else {
					t.Errorf("unexpected cursor: %s", q.Get("starting_after"))
				}
			})
			defer closeServer()
			rows, err := tc.call(g)
			if err != nil {
				t.Fatalf("list failed: %v", err)
			}
			if calls != 2 || len(rows) != 3 {
				t.Fatalf("calls=%d rows=%d, want 2 pages of 3 objects", calls, len(rows))
			}
			if rows[0].Text("id") != "obj_1" || rows[2].Text("id") != "obj_3" {
				t.Errorf("pages not concatenated in order: %v", rows)
			}
			if _, ok := rows[0]["metadata"]; ok {
				t.Fatal("reconciliation retained provider metadata")
			}
			if _, ok := rows[0]["billing_details"]; ok {
				t.Fatal("reconciliation retained donor billing details")
			}
		})
	}
}

func TestReconcileCanceledReadMakesNoProviderRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g, closeServer := stripeTestGateway(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("canceled reconciliation reached Stripe")
	})
	defer closeServer()
	if _, err := g.ListChargesByCreated(ctx, time.Unix(1000, 0), time.Unix(2000, 0)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read: %v", err)
	}
}

func TestReconcileListRejectsLivemodeObject(t *testing.T) {
	start := time.Unix(1000, 0).UTC()
	end := time.Unix(2000, 0).UTC()
	g, closeServer := stripeTestGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","has_more":false,"data":[{"id":"ch_live","amount":1,"livemode":true}]}`))
	})
	defer closeServer()
	if _, err := g.ListChargesByCreated(context.Background(), start, end); err == nil {
		t.Fatal("a livemode charge was reconciled")
	}
	if _, err := g.ListRefundsByCreated(context.Background(), start, end); err == nil {
		t.Fatal("a livemode refund was reconciled")
	}
}

// Ensure StripeGateway satisfies the read-only ReconcileGateway interface.
var _ ReconcileGateway = (*StripeGateway)(nil)

func TestInvoicePaymentLookupIsReadOnlyBoundedAndPrivate(t *testing.T) {
	for _, invoice := range []bool{true, false} {
		t.Run(map[bool]string{true: "invoice", false: "payment"}[invoice], func(t *testing.T) {
			g, closeServer := stripeTestGateway(t, func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				if r.Method != "GET" || r.URL.Path != "/v1/invoice_payments" || r.Header.Get("Stripe-Version") != APIVersion ||
					q.Get("limit") != "100" ||
					q.Get("status") != "paid" {
					t.Errorf("unsafe invoice lookup: %s %s", r.Method, r.URL)
				}
				if invoice && (q.Get("invoice") != "in_test" || q.Get("payment[type]") != "") {
					t.Error("wrong invoice filter", q)
				}
				if !invoice &&
					(q.Get("invoice") != "" || q.Get("payment[type]") != "payment_intent" || q.Get("payment[payment_intent]") != "pi_test") {
					t.Error("wrong PaymentIntent filter", q)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(
					[]byte(
						`{"object":"list","has_more":false,"data":[{"id":"inpay_test","invoice":"in_test","amount_paid":3000,"currency":"usd","status":"paid","livemode":false,"created":1000,"metadata":{"private":"fixture"},"payment":{"type":"payment_intent","payment_intent":"pi_test","private":"fixture"}}]}`,
					),
				)
			})
			defer closeServer()
			in, pi := "", "pi_test"
			if invoice {
				in, pi = "in_test", ""
			}
			rows, err := g.ListInvoicePayments(context.Background(), in, pi)
			if err != nil || len(rows) != 1 {
				t.Fatal(rows, err)
			}
			if _, ok := rows[0]["metadata"]; ok {
				t.Fatal("invoice metadata retained")
			}
			payment, err := rows[0].Object("payment")
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := payment["private"]; ok {
				t.Fatal("unneeded payment details retained")
			}
		})
	}
}

func TestInvoicePaymentLookupRejectsTruncatedAndLiveLists(t *testing.T) {
	for _, raw := range []string{
		`{"object":"list","has_more":false,"data":null}`,
		`{"object":"list","has_more":true,"data":[]}`,
		`{"object":"list","has_more":false,"data":[{"livemode":true}]}`,
	} {
		g, closeServer := stripeTestGateway(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(raw))
		})
		_, err := g.ListInvoicePayments(context.Background(), "in_test", "")
		closeServer()
		if err == nil {
			t.Fatal("incomplete or live proof accepted")
		}
	}
}

func TestInvoicePaymentLookupRequiresExactlyOneFilter(t *testing.T) {
	g, closeServer := stripeTestGateway(
		t,
		func(w http.ResponseWriter, r *http.Request) { t.Error("unscoped invoice lookup reached Stripe") },
	)
	defer closeServer()
	for _, filters := range [][2]string{{"", ""}, {"in_test", "pi_test"}} {
		if _, err := g.ListInvoicePayments(context.Background(), filters[0], filters[1]); err == nil {
			t.Fatal("unscoped or conflicting lookup accepted")
		}
	}
}
