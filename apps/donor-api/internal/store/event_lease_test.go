package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/workers"
)

// queuedRefund queues a refund event whose processing needs one processor read.
func queuedRefund(t *testing.T) (*workerFixture, string) {
	t.Helper()
	f := newWorkerFixture(t, "MONTHLY")
	checkout := f.checkout(t)
	invoiceID := "in_" + f.donorID.String()
	start := time.Now().UTC().Truncate(time.Second).Add(-2 * 24 * time.Hour)
	f.receive(t, "invoice.payment_succeeded", invoice(checkout.CheckoutID, invoiceID, start, true))
	f.process(t)
	refund := "re_" + f.donorID.String()
	f.gateway.refunds[refund] = paymentObject(
		t,
		map[string]any{"id": refund, "payment_intent": "pi_" + invoiceID, "amount": 3000, "currency": "usd", "status": "succeeded"},
	)
	event := f.receive(t, "refund.created", map[string]any{"id": refund, "payment_intent": "pi_" + invoiceID, "status": "succeeded"})
	return f, event
}

type eventRow struct {
	attempts  int
	processed bool
	failure   string
}

func readEvent(t *testing.T, f *workerFixture, id string) eventRow {
	t.Helper()
	var row eventRow
	if err := f.db.QueryRow(
		context.Background(),
		"SELECT attempts,processed_at IS NOT NULL,coalesce(last_error,'') FROM donor_stripe_events WHERE stripe_event_id=$1",
		id,
	).Scan(&row.attempts, &row.processed, &row.failure); err != nil {
		t.Fatal(err)
	}
	return row
}

func refundObservations(t *testing.T, f *workerFixture) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(
		context.Background(),
		"SELECT count(*) FROM donor_refund_observations o JOIN donor_refunds r ON r.id=o.refund_id JOIN donor_gifts g ON g.id=r.gift_id WHERE g.donor_id=$1",
		f.donorID,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// assertNoTransactionHolds fails if the event row is locked or any backend sits
// in an open transaction, as a claim held across a processor call would.
func assertNoTransactionHolds(t *testing.T, f *workerFixture, id string) {
	t.Helper()
	ctx := context.Background()
	tx, err := f.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var locked int
	err = tx.QueryRow(ctx, "SELECT count(*) FROM (SELECT 1 FROM donor_stripe_events WHERE stripe_event_id=$1 FOR UPDATE NOWAIT) x", id).
		Scan(&locked)
	if rollback := tx.Rollback(ctx); err != nil || rollback != nil {
		t.Fatal("event row is locked during the processor call", err, rollback)
	}
	var open int
	if err = f.db.QueryRow(
		ctx,
		"SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND state LIKE 'idle in transaction%'",
	).Scan(&open); err != nil || open != 0 {
		t.Fatalf("transactions open during the processor call: %d %v", open, err)
	}
}

func TestEventClaimHoldsNoTransactionDuringProcessorCall(t *testing.T) {
	f, event := queuedRefund(t)
	gateway := &stalledReads{financialGateway: f.gateway, gate: newGate(t)}
	worker := EventWorker{DB: as(t, dbroles.StripeWorker), Gateway: gateway}
	err := whileStalled(t, gateway.gate, func() error {
		_, err := worker.ProcessNext(context.Background())
		return err
	}, func() {
		assertNoTransactionHolds(t, f, event)
		assertDonorUnlocked(t, f.db, f.donorID)
		if row := readEvent(t, f, event); row.attempts != 1 || row.processed {
			t.Fatalf("event not leased to the attempt: %+v", row)
		}
		var leaseSeconds float64
		if err := f.db.QueryRow(context.Background(), "SELECT extract(epoch FROM available_at-now()) FROM donor_stripe_events WHERE stripe_event_id=$1", event).
			Scan(&leaseSeconds); err != nil ||
			leaseSeconds < (workers.ProcessorStepBudget).Seconds() {
			t.Fatalf("lease %.0fs does not cover the processor step budget: %v", leaseSeconds, err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	assertEventProcessed(t, f, event, true)
	if row := readEvent(t, f, event); row.attempts != 1 || row.failure != "" {
		t.Fatalf("unexpected final state %+v", row)
	}
}

func TestLeasedEventIsSkippedByOtherWorkers(t *testing.T) {
	f, event := queuedRefund(t)
	gateway := &stalledReads{financialGateway: f.gateway, gate: newGate(t)}
	slow := EventWorker{DB: as(t, dbroles.StripeWorker), Gateway: gateway}
	err := whileStalled(t, gateway.gate, func() error {
		_, err := slow.ProcessNext(context.Background())
		return err
	}, func() {
		for range 3 {
			if worked, err := f.worker.ProcessNext(context.Background()); worked || err != nil {
				t.Fatal("second worker took a leased event", worked, err)
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if row := readEvent(t, f, event); !row.processed || row.attempts != 1 || refundObservations(t, f) != 1 {
		t.Fatalf("event not applied exactly once: %+v", row)
	}
}

func TestConcurrentWorkersApplyAnEventOnce(t *testing.T) {
	f, event := queuedRefund(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	worked := 0
	for range 4 {
		worker := EventWorker{DB: as(t, dbroles.StripeWorker), Gateway: f.gateway}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := worker.ProcessNext(context.Background())
			if err != nil {
				t.Error(err)
			}
			if ok {
				mu.Lock()
				worked++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if row := readEvent(t, f, event); worked != 1 || !row.processed || row.attempts != 1 || refundObservations(t, f) != 1 {
		t.Fatalf("workers=%d state=%+v observations=%d", worked, row, refundObservations(t, f))
	}
}

func TestCrashBetweenClaimAndApplyRetriesAfterLeaseExpiry(t *testing.T) {
	f, event := queuedRefund(t)
	ctx := context.Background()
	if _, found, err := f.worker.claim(ctx, time.Now().UTC()); err != nil || !found {
		t.Fatal("claim failed", found, err)
	}
	// The claiming worker dies here: nothing else was written.
	if worked, err := f.worker.ProcessNext(ctx); worked || err != nil {
		t.Fatal("event was processed while leased", worked, err)
	}
	if row := readEvent(t, f, event); row.processed || row.attempts != 1 || refundObservations(t, f) != 0 {
		t.Fatalf("crashed claim left effects: %+v", row)
	}
	f.retryNow(t) // the lease expires
	f.process(t)
	if row := readEvent(t, f, event); !row.processed || row.attempts != 2 || row.failure != "" || refundObservations(t, f) != 1 {
		t.Fatalf("retry after lease expiry: %+v", row)
	}
}

func TestExpiredLeaseHolderCannotApplyAfterSuccessor(t *testing.T) {
	f, event := queuedRefund(t)
	gateway := &stalledReads{financialGateway: f.gateway, gate: newGate(t)}
	stale := EventWorker{DB: as(t, dbroles.StripeWorker), Gateway: gateway}
	err := whileStalled(t, gateway.gate, func() error {
		_, err := stale.ProcessNext(context.Background())
		return err
	}, func() {
		f.retryNow(t)
		f.process(t)
		assertEventProcessed(t, f, event, true)
	})
	if err != nil {
		t.Fatal(err)
	}
	if row := readEvent(t, f, event); !row.processed || row.attempts != 2 || row.failure != "" || refundObservations(t, f) != 1 {
		t.Fatalf("stale holder changed the event: %+v observations=%d", row, refundObservations(t, f))
	}
}

func TestExpiredLeaseHolderCannotFailSuccessorsEvent(t *testing.T) {
	f, event := queuedRefund(t)
	gateway := &failingRefunds{financialGateway: f.gateway, gate: newGate(t)}
	stale := EventWorker{DB: as(t, dbroles.StripeWorker), Gateway: gateway}
	err := whileStalled(t, gateway.gate, func() error {
		_, err := stale.ProcessNext(context.Background())
		return err
	}, func() {
		f.retryNow(t)
		f.process(t)
	})
	if err != nil {
		t.Fatal(err)
	}
	if row := readEvent(t, f, event); !row.processed || row.failure != "" {
		t.Fatalf("stale failure overwrote the applied event: %+v", row)
	}
}

// failingRefunds stalls the first refund read and then reports the processor down.
type failingRefunds struct {
	*financialGateway
	gate *gate
	once sync.Once
}

func (g *failingRefunds) RetrieveRefund(context.Context, string) (payments.Object, error) {
	g.once.Do(g.gate.wait)
	return nil, payments.ErrProcessorUnavailable
}

func TestProcessorFailureReleasesLeaseWithBackoff(t *testing.T) {
	f, event := queuedRefund(t)
	delete(f.gateway.refunds, "re_"+f.donorID.String())
	f.process(t)
	attempts, failure, delay := eventRetryState(t, f, event)
	if attempts != 1 || failure != "PaymentUnavailable" || delay > 2*time.Minute || delay < 30*time.Second {
		t.Fatalf("attempts=%d failure=%s delay=%s", attempts, failure, delay)
	}
	assertNoTransactionHolds(t, f, event)
}

// blockedRefunds holds the refund read until the caller's context ends.
type blockedRefunds struct {
	*financialGateway
	started chan struct{}
}

func (g *blockedRefunds) RetrieveRefund(ctx context.Context, _ string) (payments.Object, error) {
	close(g.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestCancelledStepReleasesLeaseWithoutCountingAttempt(t *testing.T) {
	f, event := queuedRefund(t)
	gateway := &blockedRefunds{financialGateway: f.gateway, started: make(chan struct{})}
	worker := EventWorker{DB: as(t, dbroles.StripeWorker), Gateway: gateway}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := worker.ProcessNext(ctx)
		done <- err
	}()
	<-gateway.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled step did not report cancellation", err)
	}
	if row := readEvent(t, f, event); row.processed || row.attempts != 0 || row.failure != "" {
		t.Fatalf("cancelled step kept its claim: %+v", row)
	}
	f.process(t)
	assertEventProcessed(t, f, event, true)
}

func TestDuplicateDeliveryAppliesOnce(t *testing.T) {
	f, event := queuedRefund(t)
	f.deliver(
		t,
		event,
		"refund.created",
		map[string]any{"id": "re_" + f.donorID.String(), "payment_intent": "pi_in_" + f.donorID.String(), "status": "succeeded"},
		time.Now(),
	)
	f.process(t)
	if worked, err := f.worker.ProcessNext(context.Background()); worked || err != nil {
		t.Fatal("duplicate delivery produced a second event", worked, err)
	}
	if row := readEvent(t, f, event); !row.processed || row.attempts != 1 || refundObservations(t, f) != 1 {
		t.Fatalf("duplicate delivery: %+v", row)
	}
}

func TestEventLeaseOutlastsProcessorStepBudget(t *testing.T) {
	if eventLease <= workers.ProcessorStepBudget {
		t.Fatalf("lease %s must outlast the step budget %s", eventLease, workers.ProcessorStepBudget)
	}
}
