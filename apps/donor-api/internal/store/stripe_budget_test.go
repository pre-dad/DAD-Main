package store

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/dbroles/dbrolestest"
	"dad/donor-api/internal/payments"
)

// freshStripeBudgets clears every bucket's reservations, before and after the test.
func freshStripeBudgets(t *testing.T) {
	t.Helper()
	owner := database(t)
	clear := func() {
		if _, err := owner.Exec(context.Background(), "DELETE FROM donor_stripe_budgets"); err != nil {
			t.Fatal(err)
		}
	}
	clear()
	t.Cleanup(clear)
}

// Two processes holding their own pools, as replicas do, share one bucket:
// together they spend its burst, then one request per interval, never more.
func TestStripeBudgetIsSharedAcrossProcesses(t *testing.T) {
	freshStripeBudgets(t)
	const ceiling = 100.0
	rate, burst, _ := payments.BucketRate(payments.BucketStaff, ceiling)
	replicas := []StripeBudget{
		{DB: dbrolestest.Open(t, dbroles.API), PerSecond: ceiling},
		{DB: dbrolestest.Open(t, dbroles.StripeWorker), PerSecond: ceiling},
	}
	const requests = 60
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	granted := make([]time.Duration, 0, requests)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := range requests {
		wg.Go(func() {
			if err := replicas[i%len(replicas)].Acquire(ctx, payments.BucketStaff); err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			granted = append(granted, time.Since(start))
			mu.Unlock()
		})
	}
	wg.Wait()
	slices.Sort(granted)
	if len(granted) != requests {
		t.Fatalf("%d of %d requests granted", len(granted), requests)
	}
	for i, at := range granted {
		earliest := time.Duration((float64(i+1) - burst) / rate * float64(time.Second))
		if at < earliest-20*time.Millisecond {
			t.Fatalf("request %d ran at %v, before its slot at %v", i, at, earliest)
		}
	}
	if want := time.Duration((requests - burst) / rate * float64(time.Second)); granted[requests-1] > want+time.Second {
		t.Fatalf("last request ran at %v, long after its slot at %v", granted[requests-1], want)
	}
}

// A request whose slot lies beyond its deadline fails at once, without
// reserving the slot, rather than waiting out the deadline.
func TestStripeBudgetRefusesSlotsBeyondTheDeadline(t *testing.T) {
	freshStripeBudgets(t)
	budget := StripeBudget{DB: dbrolestest.Open(t, dbroles.API), PerSecond: 1}
	if err := budget.Acquire(context.Background(), payments.BucketWorker); err != nil {
		t.Fatal(err)
	}
	const deadline = 200 * time.Millisecond
	for range 3 {
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		start := time.Now()
		err := budget.Acquire(ctx, payments.BucketWorker)
		cancel()
		if !errors.Is(err, ErrStripeBudget) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("exhausted bucket: %v", err)
		}
		if waited := time.Since(start); waited >= deadline {
			t.Fatalf("refusal took %v, as long as the %v deadline it should not wait out", waited, deadline)
		}
	}
	other := StripeBudget{DB: dbrolestest.Open(t, dbroles.StripeWorker), PerSecond: 1}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := other.Acquire(ctx, payments.BucketCheckout); err != nil {
		t.Fatalf("an exhausted bucket held back another: %v", err)
	}
	if err := other.Acquire(ctx, "unknown"); !errors.Is(err, payments.ErrUnknownBucket) {
		t.Fatalf("unknown bucket: %v", err)
	}
}
