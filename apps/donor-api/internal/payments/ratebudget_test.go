package payments

import (
	"context"
	"errors"
	"math"
	"net/http"
	"testing"
	"time"
)

func TestGatewayWaitsForBudgetBeforeCallingStripe(t *testing.T) {
	for _, tc := range []struct {
		name, budget string
		call         func(context.Context, *StripeGateway) error
	}{
		{"checkout", BucketCheckout, func(ctx context.Context, g *StripeGateway) error {
			_, err := g.CreateCheckout(ctx, []byte(`{"mode":"payment"}`), "fixture")
			return err
		}},
		{"checkout read", BucketWorker, func(ctx context.Context, g *StripeGateway) error {
			_, err := g.RetrieveCheckout(ctx, "cs_test_fixture")
			return err
		}},
		{"setup read", BucketWorker, func(ctx context.Context, g *StripeGateway) error {
			_, err := g.RetrieveSetupIntent(ctx, "seti_fixture")
			return err
		}},
		{"refund list", BucketStaff, func(ctx context.Context, g *StripeGateway) error {
			_, err := g.ListPaymentRefunds(ctx, "pi_fixture")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, closeServer := stripeTestGateway(t, func(http.ResponseWriter, *http.Request) {
				t.Error("exhausted budget reached Stripe")
			})
			defer closeServer()
			budget := NewRateBudget(DefaultRequestsPerSecond)
			budget.buckets[tc.budget] = newBucket(1, 0)
			g.budget = budget
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
			defer cancel()
			if err := tc.call(ctx, g); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("exhausted budget: %v", err)
			}
		})
	}
}

func TestRefundPaginationSpendsBudgetForEveryPage(t *testing.T) {
	calls := 0
	g, closeServer := stripeTestGateway(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","has_more":true,"data":[{"id":"re_fixture"}]}`))
	})
	defer closeServer()
	g.budget = &RateBudget{buckets: map[string]*bucket{BucketStaff: newBucket(0.001, 1)}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := g.ListPaymentRefunds(ctx, "pi_fixture"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exhausted second-page budget: %v", err)
	}
	if calls != 1 {
		t.Fatalf("provider calls = %d, want only the budgeted first page", calls)
	}
}

// A fresh bucket grants up to its burst immediately, then blocks until a refill.
func TestRateBudgetBurstThenRefill(t *testing.T) {
	// 10 tokens/second, burst 2: two immediate grants, the third waits ~100ms.
	rb := &RateBudget{buckets: map[string]*bucket{BucketWorker: newBucket(10, 2)}}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		start := time.Now()
		if err := rb.Acquire(ctx, BucketWorker); err != nil {
			t.Fatalf("burst grant %d failed: %v", i, err)
		}
		if waited := time.Since(start); waited > 45*time.Millisecond {
			t.Fatalf("burst grant %d blocked %v, expected immediate", i, waited)
		}
	}
	start := time.Now()
	if err := rb.Acquire(ctx, BucketWorker); err != nil {
		t.Fatalf("post-drain grant failed: %v", err)
	}
	waited := time.Since(start)
	if waited < 50*time.Millisecond {
		t.Fatalf("drained bucket granted after %v, expected a refill wait", waited)
	}
}

// Acquire returns ctx.Err() when the context is cancelled while it waits.
func TestRateBudgetHonorsContextCancellation(t *testing.T) {
	// 1 token/second, burst 1: drain it, then a second Acquire must wait ~1s.
	rb := &RateBudget{buckets: map[string]*bucket{BucketWorker: newBucket(1, 1)}}
	if err := rb.Acquire(context.Background(), BucketWorker); err != nil {
		t.Fatalf("initial grant failed: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := rb.Acquire(ctx, BucketWorker)
	if err != context.DeadlineExceeded {
		t.Fatalf("expected context deadline, got %v", err)
	}
	if time.Since(start) > 300*time.Millisecond {
		t.Fatal("Acquire did not return promptly on cancellation")
	}
}

// Draining every other bucket must not delay a bucket's first request: each
// owns its share of the ceiling.
func TestRateBudgetBucketsAreIndependent(t *testing.T) {
	for name := range shares {
		t.Run(name, func(t *testing.T) {
			rb := NewRateBudget(DefaultRequestsPerSecond)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			for other := range shares {
				if other == name {
					continue
				}
				_, burst, _ := BucketRate(other, DefaultRequestsPerSecond)
				for i := 0; i < int(burst)+1; i++ {
					if err := rb.Acquire(ctx, other); err != nil {
						t.Fatalf("%s drain %d failed: %v", other, i, err)
					}
				}
			}
			start := time.Now()
			if err := rb.Acquire(ctx, name); err != nil {
				t.Fatal(err)
			}
			if waited := time.Since(start); waited > 100*time.Millisecond {
				t.Fatalf("%s waited %v despite an undrained bucket", name, waited)
			}
		})
	}
}

func TestSharesDivideTheWholeCeiling(t *testing.T) {
	total := 0.0
	for name := range shares {
		rate, burst, ok := BucketRate(name, 100)
		if !ok || rate <= 0 || burst < 1 {
			t.Fatalf("%s: rate %v, burst %v", name, rate, burst)
		}
		total += rate
	}
	if math.Abs(total-100) > 1e-9 {
		t.Fatalf("bucket rates sum to %v of a 100 request ceiling", total)
	}
	if _, _, ok := BucketRate("unknown", 100); ok {
		t.Fatal("unknown bucket has a rate")
	}
	if err := NewRateBudget(100).Acquire(context.Background(), "unknown"); !errors.Is(err, ErrUnknownBucket) {
		t.Fatalf("unknown bucket: %v", err)
	}
}

func TestRequestsPerSecondConfiguration(t *testing.T) {
	for value, want := range map[string]float64{"": DefaultRequestsPerSecond, "100": 100, "12.5": 12.5} {
		got, err := RequestsPerSecond(func(string) string { return value })
		if err != nil || got != want {
			t.Errorf("%q: %v, %v", value, got, err)
		}
	}
	for _, value := range []string{"0", "-1", "ten", "10001", "NaN"} {
		if _, err := RequestsPerSecond(func(string) string { return value }); err == nil {
			t.Errorf("%q accepted", value)
		}
	}
}
