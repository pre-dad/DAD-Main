package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"dad/donor-api/internal/payments"
	"github.com/jackc/pgx/v5/pgtype"
)

// abandon ages the donor's checkouts past the grace in which a website request
// leaves a just-begun one alone.
func abandon(t *testing.T, f *workerFixture) {
	t.Helper()
	mustExec(
		t,
		f.db,
		"UPDATE donor_stripe_checkouts SET created_at=now()-interval '1 minute' WHERE checkout_id IN (SELECT id FROM donor_checkouts WHERE donor_id=$1)",
		f.donorID,
	)
}

func webRestartFixture(t *testing.T) (*workerFixture, *checkoutGateway, payments.CheckoutResult) {
	t.Helper()
	f := newWorkerFixture(t, "MONTHLY")
	first, err := f.service.BeginCheckout(context.Background(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	abandon(t, f)
	return f, f.service.Gateway.(*checkoutGateway), first
}

func restarted(t *testing.T, f *workerFixture, amount int64) (payments.CheckoutRequest, payments.CheckoutResult, error) {
	t.Helper()
	abandon(t, f)
	request := f.request
	request.RequestID, request.Amount = testID(t), amount
	result, err := f.service.BeginCheckout(context.Background(), request)
	return request, result, err
}

func TestWebCheckoutRestartExpiresTheAbandonedSession(t *testing.T) {
	for name, amount := range map[string]int64{"same amount": 3000, "new amount": 6000} {
		t.Run(name, func(t *testing.T) {
			f, gateway, first := webRestartFixture(t)
			request, second, err := restarted(t, f, amount)
			if err != nil {
				t.Fatal(err)
			}
			if second.URL == first.URL || second.CheckoutID == first.CheckoutID || len(gateway.expired) != 1 ||
				gateway.expired[0] != "cs_test_dad-checkout-"+f.request.RequestID.String() || len(gateway.calls) != 2 {
				t.Fatalf("old session not replaced: %+v then %+v, expired %v", first, second, gateway.expired)
			}
			if holder, n := reservationHolder(t, f); n != 1 || holder != request.RequestID.String() {
				t.Fatalf("reservation %q x%d, want only %s", holder, n, request.RequestID)
			}
			var status string
			if err = f.db.QueryRow(context.Background(), "SELECT status FROM donor_stripe_checkouts WHERE id=$1", f.request.RequestID).
				Scan(&status); err != nil || status != "EXPIRED" {
				t.Fatalf("old attempt is %q, want EXPIRED (%v)", status, err)
			}
			if again, err := f.service.BeginCheckout(context.Background(), request); err != nil || again != second {
				t.Fatalf("retry with the same key: %+v %v, want %+v", again, err, second)
			}
			if _, err = f.service.BeginCheckout(context.Background(), f.request); !errors.Is(err, payments.ErrCheckoutConflict) {
				t.Fatalf("the replaced attempt's key reopened it: %v", err)
			}
			if len(gateway.calls) != 2 || len(gateway.expired) != 1 {
				t.Fatalf("retries reached the processor: %d creations, %d expiries", len(gateway.calls), len(gateway.expired))
			}
		})
	}
}

func TestWebCheckoutRestartReplacesANativeSession(t *testing.T) {
	f := newWorkerFixture(t, "MONTHLY")
	gateway := f.service.Gateway.(*checkoutGateway)
	token := claimSession(t, f.db, f.donorID, app).Access
	native, err := f.service.BeginNativeCheckout(context.Background(), token, testID(t), 3000, nil)
	if err != nil {
		t.Fatal(err)
	}
	abandon(t, f)
	web, err := f.service.BeginCheckout(context.Background(), f.request)
	if err != nil || web.URL == native.URL || len(gateway.expired) != 1 || gateway.expired[0] != native.SessionID {
		t.Fatalf("native session not replaced: %+v %v, expired %v", web, err, gateway.expired)
	}
}

func TestWebCheckoutPaidWhileRestartingIsAnActiveGift(t *testing.T) {
	for name, arrange := range map[string]func(*workerFixture, *checkoutGateway){
		"processor reports the session complete": func(_ *workerFixture, g *checkoutGateway) { g.expireStatus = "complete" },
		"webhook completes it as the expiry runs": func(f *workerFixture, g *checkoutGateway) {
			g.beforeExpire = func() {
				mustExec(t, f.db, "UPDATE donor_stripe_checkouts SET status='COMPLETE' WHERE id=$1", f.request.RequestID)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, gateway, _ := webRestartFixture(t)
			arrange(f, gateway)
			if _, _, err := restarted(t, f, 6000); !errors.Is(err, payments.ErrCheckoutConflict) {
				t.Fatalf("got %v, want a conflict", err)
			}
			if holder, n := reservationHolder(t, f); n != 1 || holder != f.request.RequestID.String() || len(gateway.calls) != 1 {
				t.Fatalf("reservation %q x%d, %d creations", holder, n, len(gateway.calls))
			}
		})
	}
}

func TestWebCheckoutKeepsTheReservationWhenExpiryFails(t *testing.T) {
	f, gateway, first := webRestartFixture(t)
	ctx := context.Background()
	gateway.expireErr = payments.ErrProcessorUnavailable
	request, _, err := restarted(t, f, 6000)
	if !errors.Is(err, payments.ErrProcessorUnavailable) {
		t.Fatalf("got %v, want the processor error", err)
	}
	var attempts int
	if err = f.db.QueryRow(ctx, "SELECT count(*) FROM donor_stripe_checkouts a JOIN donor_checkouts c ON c.id=a.checkout_id WHERE c.donor_id=$1", f.donorID).
		Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if holder, n := reservationHolder(t, f); n != 1 || holder != f.request.RequestID.String() || attempts != 1 || len(gateway.calls) != 1 {
		t.Fatalf("reservation %q x%d, %d attempts, %d creations", holder, n, attempts, len(gateway.calls))
	}
	gateway.expireErr = nil
	if second, err := f.service.BeginCheckout(ctx, request); err != nil || second.URL == first.URL {
		t.Fatalf("retry after the processor recovered: %+v %v", second, err)
	}
	if holder, n := reservationHolder(t, f); n != 1 || holder != request.RequestID.String() {
		t.Fatalf("reservation %q x%d after the retry", holder, n)
	}
}

func TestWebCheckoutLateExpiryReleasesOnlyItsOwnAttempt(t *testing.T) {
	f, _, first := webRestartFixture(t)
	request, _, err := restarted(t, f, 6000)
	if err != nil {
		t.Fatal(err)
	}
	expired := f.receive(t, "checkout.session.expired", map[string]any{
		"id": "cs_test_dad-checkout-" + f.request.RequestID.String(), "status": "expired",
		"metadata": map[string]any{"dad_checkout_id": first.CheckoutID},
	})
	f.process(t)
	assertEventProcessed(t, f, expired, true)
	if holder, n := reservationHolder(t, f); n != 1 || holder != request.RequestID.String() {
		t.Fatalf("the late expiry changed the reservation to %q x%d", holder, n)
	}
}

func TestWebCheckoutRestartsRaceToOneOpenSession(t *testing.T) {
	f, gateway, _ := webRestartFixture(t)
	start := make(chan struct{})
	results := make(chan payments.CheckoutResult, 4)
	errs := make(chan error, 4)
	var wg sync.WaitGroup
	for range 4 {
		request := f.request
		request.RequestID, request.Amount = testID(t), 6000
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := f.service.BeginCheckout(context.Background(), request)
			results <- result
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil && !errors.Is(err, payments.ErrCheckoutConflict) {
			t.Fatal(err)
		}
	}
	// Whoever meets the replacement before its session exists is told the gift
	// is in progress; the reservation stays with exactly one attempt.
	if _, n := reservationHolder(t, f); n != 1 {
		t.Fatalf("%d reservations", n)
	}
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	if len(gateway.calls) != 2 {
		t.Fatalf("%d sessions created, want the original and one replacement", len(gateway.calls))
	}
}

func TestWebCheckoutRestartsAreThrottledPerEmail(t *testing.T) {
	f, gateway, _ := webRestartFixture(t)
	for i := 1; i < restartBudget; i++ {
		if _, _, err := restarted(t, f, 3000); err != nil {
			t.Fatalf("restart %d: %v", i, err)
		}
	}
	holder, _ := reservationHolder(t, f)
	if _, _, err := restarted(t, f, 3000); !errors.Is(err, payments.ErrCheckoutThrottled) {
		t.Fatalf("got %v, want a throttle after %d restarts", err, restartBudget-1)
	}
	if now, n := reservationHolder(t, f); n != 1 || now != holder || len(gateway.expired) != restartBudget-1 {
		t.Fatalf("the throttled restart changed state: %q x%d, %d expiries", now, n, len(gateway.expired))
	}
}

func TestWebCheckoutLeavesAJustBegunSessionAlone(t *testing.T) {
	f := newWorkerFixture(t, "MONTHLY")
	gateway := f.service.Gateway.(*checkoutGateway)
	if _, err := f.service.BeginCheckout(context.Background(), f.request); err != nil {
		t.Fatal(err)
	}
	request := f.request
	request.RequestID = testID(t)
	if _, err := f.service.BeginCheckout(context.Background(), request); !errors.Is(err, payments.ErrCheckoutConflict) {
		t.Fatalf("got %v, want a conflict", err)
	}
	if holder, n := reservationHolder(t, f); n != 1 || holder != f.request.RequestID.String() || len(gateway.expired) != 0 {
		t.Fatalf("reservation %q x%d, %d expiries", holder, n, len(gateway.expired))
	}
}

func TestWebCheckoutRefusesInvalidInputBeforeTheProcessor(t *testing.T) {
	service, request, id, gateway := checkoutFixture(t)
	pool := database(t)
	ctx := context.Background()
	cases := map[string]func(*payments.CheckoutRequest){
		"amount zero":          func(r *payments.CheckoutRequest) { r.Amount = 0 },
		"below the minimum":    func(r *payments.CheckoutRequest) { r.Amount = 2999 },
		"not a daily multiple": func(r *payments.CheckoutRequest) { r.Amount = 4500 },
		"above the maximum":    func(r *payments.CheckoutRequest) { r.Amount = 93000 },
		"one-time frequency":   func(r *payments.CheckoutRequest) { r.Frequency = "ONCE" },
		"malformed email":      func(r *payments.CheckoutRequest) { r.Email = "not-an-email" },
		"oversized email":      func(r *payments.CheckoutRequest) { r.Email = strings.Repeat("a", 320) + "@example.org" },
		"zero request id":      func(r *payments.CheckoutRequest) { r.RequestID = pgtype.UUID{} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			bad := request
			mutate(&bad)
			if _, err := service.BeginCheckout(ctx, bad); !errors.Is(err, payments.ErrCheckoutInput) {
				t.Fatalf("got %v, want an input error", err)
			}
		})
	}
	var checkouts int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_checkouts WHERE donor_id=$1", id).Scan(&checkouts); err != nil ||
		checkouts != 0 {
		t.Fatalf("refused requests left %d checkouts (%v)", checkouts, err)
	}
	if len(gateway.calls) != 0 {
		t.Fatalf("refused requests reached the processor %d times", len(gateway.calls))
	}
}
