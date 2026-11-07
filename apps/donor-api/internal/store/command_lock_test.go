package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// gate holds one processor call until it is opened.
type gate struct {
	started, release chan struct{}
	once             sync.Once
}

func newGate(t *testing.T) *gate {
	g := &gate{started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(g.open)
	return g
}

func (g *gate) wait() {
	close(g.started)
	<-g.release
}

func (g *gate) open() { g.once.Do(func() { close(g.release) }) }

// assertDonorUnlocked fails unless another transaction can take the donor
// lock promptly.
func assertDonorUnlocked(t *testing.T, pool *pgxpool.Pool, donor pgtype.UUID) {
	t.Helper()
	ctx := context.Background()
	lock, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(ctx)
	mustExec(t, lock, "SET LOCAL lock_timeout='2s'")
	if _, err = queries.New(lock).LockDonor(ctx, donor); err != nil {
		t.Fatal("processor call held the donor lock", err)
	}
}

// whileStalled runs command until it reaches the stalled processor call, runs
// during, then lets the call finish and returns the command's error.
func whileStalled(t *testing.T, stalled *gate, command func() error, during func()) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- command() }()
	select {
	case <-stalled.started:
	case err := <-done:
		t.Fatal("command finished without the processor call", err)
	}
	during()
	stalled.open()
	return <-done
}

func TestRecurringCommandsCallProcessorOutsideDonorLock(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	_, finance := staffFixture(t, pool, "SUPER_ADMIN")
	ctx := context.Background()
	for _, c := range []struct {
		name string
		run  func(*recurringFixture) error
	}{
		{"cancel", func(r *recurringFixture) error {
			_, err := r.service.Cancel(ctx, r.token, CancellationRequest{RequestID: testID(t), AttemptID: r.attempt, PeriodEnd: r.end})
			return err
		}},
		{"amount", func(r *recurringFixture) error {
			_, err := r.service.ChangeAmount(ctx, r.token, AmountRequest{RequestID: testID(t), AttemptID: r.attempt, Amount: 9000, Previous: 3000, EffectiveAt: r.end})
			return err
		}},
		{"payment method", func(r *recurringFixture) error {
			update, err := r.service.BeginPaymentUpdate(ctx, r.token, testID(t))
			if err == nil && update.URL == nil {
				err = errors.New("no hosted setup URL")
			}
			return err
		}},
		{"staff cancel", func(r *recurringFixture) error {
			s := StaffFinanceService{Staff: &StaffService{DB: api}, Gateway: r.sub, Origin: "https://donor.example.org"}
			_, err := s.Cancel(ctx, finance, StaffFinancialRequest{RequestID: testID(t), DonorID: r.donorID, TargetID: r.attempt, Reason: "Donor asked by phone to stop", PeriodEnd: r.end})
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRecurringFixture(t)
			before := len(r.sub.keys)
			err := whileStalled(
				t,
				r.sub.stall(t),
				func() error { return c.run(r) },
				func() { assertDonorUnlocked(t, pool, r.donorID) },
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(r.sub.keys) != before+1 {
				t.Fatalf("processor mutations: %v", r.sub.keys[before:])
			}
		})
	}
}

func TestOutstandingProcessorRequestBlocksOtherSubscriptionCommands(t *testing.T) {
	r := newRecurringFixture(t)
	ctx := context.Background()
	pool := r.db
	amount := AmountRequest{RequestID: testID(t), AttemptID: r.attempt, Amount: 9000, Previous: 3000, EffectiveAt: r.end}
	cancel := CancellationRequest{RequestID: testID(t), AttemptID: r.attempt, PeriodEnd: r.end}
	err := whileStalled(t, r.sub.stall(t), func() error {
		_, err := r.service.ChangeAmount(ctx, r.token, amount)
		return err
	}, func() {
		if _, err := r.service.ChangeAmount(ctx, r.token, amount); !errors.Is(err, ErrSubscriptionConflict) {
			t.Error("duplicate request raced its outstanding processor call", err)
		}
		var holder pgtype.UUID
		if err := pool.QueryRow(ctx, "SELECT command_id FROM donor_subscription_leases WHERE attempt_id=$1", r.attempt).Scan(&holder); err != nil ||
			holder != amount.RequestID {
			t.Error("duplicate request released the active lease", holder, err)
		}
		if _, err := r.service.Cancel(ctx, r.token, cancel); !errors.Is(err, ErrSubscriptionConflict) {
			t.Error("cancellation raced an outstanding amount change", err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.sub.keys) != 1 || r.sub.keys[0] != "dad-amount-"+amount.RequestID.String() {
		t.Fatalf("processor mutations: %v", r.sub.keys)
	}
	var rejected int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM donor_cancellations WHERE id=$1", cancel.RequestID).Scan(&rejected); err != nil ||
		rejected != 0 {
		t.Fatal("rejected commands were persisted", rejected, err)
	}
	cancel.RequestID = testID(t)
	if _, err = r.service.Cancel(ctx, r.token, cancel); err != nil {
		t.Fatal("completed amount change kept its lease", err)
	}
	if len(r.sub.keys) != 2 {
		t.Fatalf("processor mutations: %v", r.sub.keys)
	}
}

func TestRetryAfterCrashAppliesProcessorResultOnce(t *testing.T) {
	r := newRecurringFixture(t)
	ctx := context.Background()
	pool := r.db
	request := AmountRequest{RequestID: testID(t), AttemptID: r.attempt, Amount: 9000, Previous: 3000, EffectiveAt: r.end}
	r.sub.loseAcknowledgement = true
	if _, err := r.service.ChangeAmount(ctx, r.token, request); !errors.Is(err, payments.ErrProcessorUnavailable) {
		t.Fatal("expected an unacknowledged mutation", err)
	}
	// A crash after the processor call leaves the command's lease behind too.
	if _, err := pool.Exec(ctx, "INSERT INTO donor_subscription_leases(attempt_id,donor_id,command_id,expires_at) VALUES($1,$2,$3,now()+interval '2 minutes')", r.attempt, r.donorID, request.RequestID); err != nil {
		t.Fatal(err)
	}
	confirmedAt := func() *time.Time {
		t.Helper()
		var at *time.Time
		if err := pool.QueryRow(ctx, "SELECT confirmed_at FROM donor_amount_changes WHERE id=$1", request.RequestID).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	if confirmedAt() != nil {
		t.Fatal("unacknowledged amount change recorded as confirmed")
	}
	result, err := r.service.ChangeAmount(ctx, r.token, request)
	if err != nil || !result.Confirmed {
		t.Fatal("retry did not complete the amount change", result, err)
	}
	first := confirmedAt()
	if _, err = r.service.ChangeAmount(ctx, r.token, request); err != nil {
		t.Fatal(err)
	}
	if again := confirmedAt(); first == nil || !again.Equal(*first) {
		t.Fatal("amount result applied more than once", first, again)
	}
	if len(r.sub.keys) != 1 || r.sub.keys[0] != "dad-amount-"+request.RequestID.String() {
		t.Fatalf("processor mutations: %v", r.sub.keys)
	}
	var leases int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM donor_subscription_leases WHERE attempt_id=$1", r.attempt).Scan(&leases); err != nil ||
		leases != 0 {
		t.Fatal("completed command kept its lease", leases, err)
	}
}

// stalledReads holds the first processor read until its gate opens and counts
// every read.
type stalledReads struct {
	*financialGateway
	gate  *gate
	once  sync.Once
	reads int
}

func (g *stalledReads) read() {
	g.reads++
	g.once.Do(g.gate.wait)
}

func (g *stalledReads) RetrievePaymentIntent(ctx context.Context, id string) (payments.Object, error) {
	g.read()
	return g.financialGateway.RetrievePaymentIntent(ctx, id)
}

func (g *stalledReads) RetrieveRefund(ctx context.Context, id string) (payments.Object, error) {
	g.read()
	return g.financialGateway.RetrieveRefund(ctx, id)
}

func (g *stalledReads) RetrieveSubscription(ctx context.Context, id string) (payments.Object, error) {
	g.read()
	return g.financialGateway.RetrieveSubscription(ctx, id)
}

func TestEventWorkerReadsProcessorOutsideDonorLock(t *testing.T) {
	process := func(t *testing.T, f *workerFixture, during func()) *stalledReads {
		t.Helper()
		gateway := &stalledReads{financialGateway: f.gateway, gate: newGate(t)}
		worker := EventWorker{DB: as(t, dbroles.StripeWorker), Gateway: gateway}
		err := whileStalled(t, gateway.gate, func() error {
			worked, err := worker.ProcessNext(context.Background())
			if err == nil && !worked {
				err = errors.New("no event processed")
			}
			return err
		}, func() {
			assertDonorUnlocked(t, f.db, f.donorID)
			if during != nil {
				during()
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		return gateway
	}
	t.Run("subscription", func(t *testing.T) {
		r := newRecurringFixture(t)
		event := r.receive(t, "customer.subscription.updated", subscription(r.checkout, r.start))
		if reads := process(t, r.workerFixture, nil).reads; reads != 1 {
			t.Fatalf("processor reads=%d", reads)
		}
		assertEventProcessed(t, r.workerFixture, event, true)
	})
	for _, recordedMeanwhile := range []bool{false, true} {
		name := "refund"
		if recordedMeanwhile {
			name = "refund recorded meanwhile"
		}
		t.Run(name, func(t *testing.T) {
			f := newWorkerFixture(t, "MONTHLY")
			checkout := f.checkout(t)
			invoiceID := "in_" + f.donorID.String()
			payment := "pi_" + invoiceID
			start := time.Now().UTC().Truncate(time.Second).Add(-2 * 24 * time.Hour)
			f.receive(t, "invoice.payment_succeeded", invoice(checkout.CheckoutID, invoiceID, start, true))
			f.process(t)
			refund := "re_" + f.donorID.String()
			f.gateway.refunds[refund] = paymentObject(
				t,
				map[string]any{"id": refund, "payment_intent": payment, "amount": 3000, "currency": "usd", "status": "succeeded"},
			)
			event := f.receive(t, "refund.created", map[string]any{"id": refund, "payment_intent": payment, "status": "succeeded"})
			var later string
			if recordedMeanwhile {
				later = f.receive(t, "refund.updated", map[string]any{"id": refund, "payment_intent": payment, "status": "succeeded"})
			}
			gateway := process(t, f, func() {
				if recordedMeanwhile {
					f.process(t)
				}
			})
			want := 1
			if recordedMeanwhile {
				want = 2
				assertEventProcessed(t, f, later, true)
			}
			if gateway.reads != want {
				t.Fatalf("processor reads=%d want %d", gateway.reads, want)
			}
			assertEventProcessed(t, f, event, true)
			if p, err := queries.New(f.db).Projection(context.Background(), f.donorID); err != nil || p.NetMinor.Int64 != 0 {
				t.Fatal("refund not applied", err)
			}
		})
	}
}
