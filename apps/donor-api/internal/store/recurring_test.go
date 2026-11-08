package store

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5/pgtype"
)

// subscriptionGateway applies recurring mutations to the fixture's processor
// state the way Stripe would, and records each idempotency key.
type subscriptionGateway struct {
	*financialGateway
	t              *testing.T
	mu             sync.Mutex
	keys           []string
	fail           error
	retrievals     int
	beforeRetrieve func()
	refundRows     []payments.Object
	refundLists    int
	beforeList     func()
	beforeMutate   func()
	// loseAcknowledgement applies the next mutation but reports a failure.
	loseAcknowledgement bool
}

func (g *subscriptionGateway) RetrieveSubscription(ctx context.Context, id string) (payments.Object, error) {
	g.mu.Lock()
	g.retrievals++
	hook := g.beforeRetrieve
	g.beforeRetrieve = nil
	g.mu.Unlock()
	if hook != nil {
		hook()
	}
	return g.financialGateway.RetrieveSubscription(ctx, id)
}

func (g *subscriptionGateway) CreateCheckout(_ context.Context, _ []byte, key string) (payments.RemoteCheckout, error) {
	g.intercept(key)
	live := false
	return payments.RemoteCheckout{ID: "cs_test_" + key, URL: "https://checkout.stripe.com/c/test", Livemode: &live}, nil
}

func (g *subscriptionGateway) ExpireCheckout(_ context.Context, _, key string) (string, error) {
	g.intercept(key)
	return "expired", nil
}

// stall pauses the next mutation until the returned gate opens.
func (g *subscriptionGateway) stall(t *testing.T) *gate {
	stalled := newGate(t)
	g.mu.Lock()
	g.beforeMutate = stalled.wait
	g.mu.Unlock()
	return stalled
}

// intercept runs any pending stall outside the lock, then records key.
func (g *subscriptionGateway) intercept(key string) {
	g.mu.Lock()
	hook := g.beforeMutate
	g.beforeMutate = nil
	g.mu.Unlock()
	if hook != nil {
		hook()
	}
	g.mu.Lock()
	g.keys = append(g.keys, key)
	g.mu.Unlock()
}

func (g *subscriptionGateway) mutate(id, key string, edit func(map[string]any)) error {
	g.intercept(key)
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.fail; err != nil {
		g.fail = nil
		return err
	}
	raw, err := json.Marshal(g.subscriptions[id])
	if err != nil {
		return err
	}
	var remote map[string]any
	if err = json.Unmarshal(raw, &remote); err != nil {
		return err
	}
	edit(remote)
	g.subscriptions[id] = paymentObject(g.t, remote)
	if g.loseAcknowledgement {
		g.loseAcknowledgement = false
		return payments.ErrProcessorUnavailable
	}
	return nil
}

func (g *subscriptionGateway) ScheduleCancellation(_ context.Context, id string, end int64, key string) error {
	return g.mutate(id, key, func(r map[string]any) { r["cancel_at"] = end })
}

func (g *subscriptionGateway) ChangeMonthlyAmount(_ context.Context, id, item, product string, amount int64, key string) error {
	return g.mutate(id, key, func(r map[string]any) {
		line := subscriptionItem(r)
		line["id"] = item
		line["price"].(map[string]any)["product"] = product
		line["price"].(map[string]any)["unit_amount"] = amount
	})
}

func (g *subscriptionGateway) ListPaymentRefunds(context.Context, string) ([]payments.Object, error) {
	g.refundLists++
	if hook := g.beforeList; hook != nil {
		g.beforeList = nil
		hook()
	}
	return g.refundRows, nil
}

func (g *subscriptionGateway) CreateRefund(context.Context, string, int64, string) (payments.Object, error) {
	return nil, payments.ErrProcessorUnavailable
}

type recurringFixture struct {
	*workerFixture
	checkout string
	sub      *subscriptionGateway
	service  RecurringService
	token    string
	attempt  pgtype.UUID
	subID    string
	start    time.Time
	end      time.Time
}

// newRecurringFixture binds an active monthly subscription that renews on its
// billing anchor one calendar month after the current period started, stored
// as its webhook syncs it.
func newRecurringFixture(t *testing.T) *recurringFixture {
	t.Helper()
	f := newWorkerFixture(t, "MONTHLY")
	result := f.checkout(t)
	ctx := context.Background()
	pool := f.db
	api := as(t, dbroles.API)
	start := time.Now().UTC().Truncate(time.Second).Add(-24 * time.Hour)
	// Keep the anchor off month-end so renewal dates are unambiguous.
	if day := start.Day(); day > 28 {
		start = start.AddDate(0, 0, 28-day)
	}
	end := start.AddDate(0, 1, 0)
	subID := "sub_" + result.CheckoutID
	var attempt pgtype.UUID
	if err := pool.QueryRow(ctx, "UPDATE donor_stripe_checkouts SET stripe_subscription_id=$2,status='COMPLETE' WHERE checkout_id=$1 RETURNING id", result.CheckoutID, subID).Scan(&attempt); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, sql := range []string{
			"DELETE FROM donor_refund_observations WHERE source_action_id IN (SELECT id FROM donor_staff_actions WHERE donor_id=$1)",
			"DELETE FROM donor_staff_actions WHERE donor_id=$1",
			"DELETE FROM donor_cancellations WHERE donor_id=$1",
		} {
			if _, err := pool.Exec(context.Background(), sql, f.donorID); err != nil {
				t.Error(err)
			}
		}
	})
	remote := subscription(result.CheckoutID, start)
	subscriptionItem(remote)["current_period_end"] = end.Unix()
	f.gateway.subscriptions[subID] = paymentObject(t, remote)
	f.syncSubscription(t, result.CheckoutID)
	tokens, err := (AccountService{DB: api}).ClaimAccount(ctx, link(t, pool, f.donorID, time.Now()), "", Client{})
	if err != nil {
		t.Fatal(err)
	}
	gateway := &subscriptionGateway{financialGateway: f.gateway, t: t}
	return &recurringFixture{
		workerFixture: f,
		checkout:      result.CheckoutID,
		sub:           gateway,
		service:       RecurringService{DB: api, Gateway: gateway, Origin: "https://donor.example.org"},
		token:         tokens.Access,
		attempt:       attempt,
		subID:         subID,
		start:         start,
		end:           end,
	}
}

// syncSubscription delivers a webhook about checkout's subscription, which
// stores whatever the processor holds now.
func (f *workerFixture) syncSubscription(t *testing.T, checkout string) {
	t.Helper()
	event := f.receive(t, "customer.subscription.updated", subscription(checkout, time.Now()))
	f.process(t)
	assertEventProcessed(t, f, event, true)
}

// changeRemotely edits the subscription the processor holds, as a change made
// in Stripe rather than through DAD would.
func (r *recurringFixture) changeRemotely(t *testing.T, edit func(map[string]any)) {
	t.Helper()
	raw, err := json.Marshal(r.gateway.subscriptions[r.subID])
	if err != nil {
		t.Fatal(err)
	}
	var remote map[string]any
	if err = json.Unmarshal(raw, &remote); err != nil {
		t.Fatal(err)
	}
	edit(remote)
	r.gateway.subscriptions[r.subID] = paymentObject(t, remote)
}

// offlineGateway is a processor that cannot be reached. It records every
// call, so a test can show which work needs the processor at all.
type offlineGateway struct {
	mu    sync.Mutex
	calls []string
}

func (g *offlineGateway) call(name string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, name)
	return payments.ErrProcessorUnavailable
}

func (g *offlineGateway) RetrievePaymentIntent(context.Context, string) (payments.Object, error) {
	return nil, g.call("RetrievePaymentIntent")
}

func (g *offlineGateway) RetrievePaymentMethod(context.Context, string) (payments.Object, error) {
	return nil, g.call("RetrievePaymentMethod")
}

func (g *offlineGateway) RetrieveRefund(context.Context, string) (payments.Object, error) {
	return nil, g.call("RetrieveRefund")
}

func (g *offlineGateway) RetrieveSubscription(context.Context, string) (payments.Object, error) {
	return nil, g.call("RetrieveSubscription")
}

func (g *offlineGateway) RetrieveCheckout(context.Context, string) (payments.Object, error) {
	return nil, g.call("RetrieveCheckout")
}

func (g *offlineGateway) RetrieveSetupIntent(context.Context, string) (payments.Object, error) {
	return nil, g.call("RetrieveSetupIntent")
}

func (g *offlineGateway) UpdateSubscriptionPaymentMethod(context.Context, string, string, string) error {
	return g.call("UpdateSubscriptionPaymentMethod")
}

func (g *offlineGateway) CreateCheckout(context.Context, []byte, string) (payments.RemoteCheckout, error) {
	return payments.RemoteCheckout{}, g.call("CreateCheckout")
}

func (g *offlineGateway) ExpireCheckout(context.Context, string, string) (string, error) {
	return "", g.call("ExpireCheckout")
}

func (g *offlineGateway) ScheduleCancellation(context.Context, string, int64, string) error {
	return g.call("ScheduleCancellation")
}

func (g *offlineGateway) ChangeMonthlyAmount(context.Context, string, string, string, int64, string) error {
	return g.call("ChangeMonthlyAmount")
}

func (g *offlineGateway) ListPaymentRefunds(context.Context, string) ([]payments.Object, error) {
	return nil, g.call("ListPaymentRefunds")
}

func (g *offlineGateway) CreateRefund(context.Context, string, int64, string) (payments.Object, error) {
	return nil, g.call("CreateRefund")
}

// jsonView decodes a service result the way the web client receives it.
func jsonView(t *testing.T, value any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err = json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func jsonTime(t *testing.T, value any) time.Time {
	t.Helper()
	text, _ := value.(string)
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		t.Fatalf("not a timestamp: %v", value)
	}
	return parsed
}

func assertKeys(t *testing.T, view map[string]any, keys ...string) {
	t.Helper()
	if len(view) != len(keys) {
		t.Fatalf("fields %v, want %v", view, keys)
	}
	for _, key := range keys {
		if _, ok := view[key]; !ok {
			t.Fatalf("missing field %s in %v", key, view)
		}
	}
}

func TestRecurringCommandReadsProcessorOutsideDonorLock(t *testing.T) {
	for _, reconciledMeanwhile := range []bool{false, true} {
		name := "snapshot used"
		if reconciledMeanwhile {
			name = "stale snapshot refetched"
		}
		t.Run(name, func(t *testing.T) {
			r := newRecurringFixture(t)
			ctx := context.Background()
			pool := r.db
			request := CancellationRequest{RequestID: testID(t), AttemptID: r.attempt, PeriodEnd: r.end}
			reading, release := make(chan struct{}), make(chan struct{})
			r.sub.beforeRetrieve = func() { close(reading); <-release }
			type outcome struct {
				result RecurringDetails
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := r.service.Cancel(ctx, r.token, request)
				done <- outcome{result, err}
			}()
			<-reading
			lock, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			mustExec(t, lock, "SET LOCAL lock_timeout='2s'")
			if _, err = queries.New(lock).LockDonor(ctx, r.donorID); err != nil {
				t.Fatal("processor read held the donor lock", err)
			}
			if reconciledMeanwhile {
				mustExec(
					t,
					lock,
					"UPDATE donor_subscriptions SET observed_at=observed_at+interval '1 millisecond' WHERE attempt_id=$1",
					r.attempt,
				)
			}
			if err = lock.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			close(release)
			got := <-done
			if got.err != nil {
				t.Fatal(got.err)
			}
			// One read before the change and one after it, plus a second read
			// when the stored state moved while the first was outstanding.
			want := 2
			if reconciledMeanwhile {
				want = 3
			}
			if r.sub.retrievals != want {
				t.Fatalf("processor reads=%d want %d", r.sub.retrievals, want)
			}
			if sub := jsonView(t, got.result)["subscription"].(map[string]any); !jsonTime(t, sub["stops_at"]).Equal(r.end) {
				t.Fatalf("cancellation: %v", sub)
			}
		})
	}
}

func TestViewsServeSyncedStateWithoutCallingProcessor(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	_, finance := staffFixture(t, pool, "SUPER_ADMIN")
	_, reader := staffFixture(t, pool, "SUPER_ADMIN")
	r := newRecurringFixture(t)
	ctx := context.Background()
	r.receive(t, "invoice.payment_succeeded", invoice(r.checkout, "in_stored_"+r.donorID.String(), r.start, true))
	r.process(t)
	var gift pgtype.UUID
	if err := pool.QueryRow(ctx, "SELECT id FROM donor_gifts WHERE donor_id=$1", r.donorID).Scan(&gift); err != nil {
		t.Fatal(err)
	}
	refundObserved := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx, "INSERT INTO donor_refunds(id,gift_id,stripe_refund_id,amount_minor,currency,status,observed_at) VALUES($1,$2,$3,500,'USD','PENDING',$4)", testID(t), gift, "re_stored_"+gift.String(), refundObserved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "DELETE FROM donor_refunds WHERE gift_id=$1", gift); err != nil {
			t.Error(err)
		}
	})
	var synced time.Time
	if err := pool.QueryRow(ctx, "SELECT observed_at FROM donor_subscriptions WHERE attempt_id=$1", r.attempt).Scan(&synced); err != nil {
		t.Fatal(err)
	}
	offline := &offlineGateway{}
	service := RecurringService{DB: api, Gateway: offline, Origin: r.service.Origin}
	staff := StaffFinanceService{Staff: &StaffService{DB: api}, Gateway: offline, Origin: r.service.Origin}

	details, err := service.Details(ctx, r.token)
	if err != nil {
		t.Fatal(err)
	}
	view := jsonView(t, details)
	assertKeys(t, view, "mode", "observed_at", "state", "subscription")
	sub := view["subscription"].(map[string]any)
	assertKeys(t, sub, "id", "amount_minor", "currency", "status", "period_end", "stops_at")
	if view["mode"] != "test" || view["state"] != "ready" || !jsonTime(t, view["observed_at"]).Equal(synced) ||
		sub["id"] != r.attempt.String() ||
		sub["amount_minor"] != float64(3000) ||
		sub["currency"] != "USD" ||
		sub["status"] != "active" ||
		sub["stops_at"] != nil ||
		!jsonTime(t, sub["period_end"]).Equal(r.end) {
		t.Fatalf("details: %v", view)
	}
	amount, err := service.AmountOptions(ctx, r.token)
	if view := jsonView(t, amount); err != nil || view["state"] != "ready" || view["amount_minor"] != float64(3000) ||
		!jsonTime(t, view["effective_at"]).Equal(r.end) {
		t.Fatal("amount options", view, err)
	}
	card, err := service.PaymentMethodStatus(ctx, r.token)
	if err != nil || card.Status != "NONE" {
		t.Fatal("payment method status", card, err)
	}
	quote, err := staff.CancellationQuote(ctx, finance, r.donorID)
	if view := jsonView(t, quote); err != nil || view["subscription"] == nil || !jsonTime(t, view["observed_at"]).Equal(synced) {
		t.Fatal("cancellation quote", view, err)
	}
	refund, err := staff.RefundQuote(ctx, finance, r.donorID, gift)
	if view := jsonView(t, refund); err != nil || view["available_minor"] != float64(2500) ||
		!jsonTime(t, view["observed_at"]).Equal(refundObserved) {
		t.Fatal("refund quote: want 2500 available", view, err)
	}
	detail, err := (&StaffService{DB: api}).DonorDetail(ctx, reader, r.donorID, 0, 0)
	if err != nil || detail.Subscription == nil || !detail.Subscription.ObservedAt.Equal(synced) {
		t.Fatal("staff detail", detail.Subscription, err)
	}
	if len(offline.calls) != 0 {
		t.Fatalf("views called the processor: %v", offline.calls)
	}

	if _, err = service.Cancel(ctx, r.token, CancellationRequest{RequestID: testID(t), AttemptID: r.attempt, PeriodEnd: r.end}); !errors.Is(
		err,
		payments.ErrProcessorUnavailable,
	) {
		t.Fatalf("cancellation without the processor: got %v, want ErrProcessorUnavailable", err)
	}
	if len(offline.calls) != 1 || offline.calls[0] != "RetrieveSubscription" {
		t.Fatalf("cancellation did not read the processor first: %v", offline.calls)
	}
}

func TestViewsFollowWebhookSyncs(t *testing.T) {
	r := newRecurringFixture(t)
	ctx := context.Background()
	observed := func() time.Time {
		details, err := r.service.Details(ctx, r.token)
		if err != nil {
			t.Fatal(err)
		}
		return jsonTime(t, jsonView(t, details)["observed_at"])
	}
	states := func() string {
		amount, err := r.service.AmountOptions(ctx, r.token)
		if err != nil {
			t.Fatal(err)
		}
		return amount.State
	}
	first := observed()

	// A schedule made in Stripe holds every change until it is released.
	r.changeRemotely(t, func(remote map[string]any) { remote["schedule"] = "sub_sched_fixture" })
	r.syncSubscription(t, r.checkout)
	if amount := states(); amount != "unavailable" {
		t.Fatalf("changes offered under a processor schedule: amount %s", amount)
	}
	if _, err := r.service.ChangeAmount(ctx, r.token, AmountRequest{RequestID: testID(t), AttemptID: r.attempt, Amount: 9000, Previous: 3000, EffectiveAt: r.end}); !errors.Is(
		err,
		ErrSubscriptionConflict,
	) {
		t.Fatal("amount change under a processor schedule", err)
	}
	if r.sub.retrievals != 1 || len(r.sub.keys) != 0 {
		t.Fatalf("refused amount change: %d processor reads, mutations %v; want one read and none", r.sub.retrievals, r.sub.keys)
	}
	r.changeRemotely(t, func(remote map[string]any) { delete(remote, "schedule") })
	r.syncSubscription(t, r.checkout)
	if amount := states(); amount != "ready" {
		t.Fatalf("changes withheld once the schedule was released: amount %s", amount)
	}

	// A cancellation made in Stripe shows once its webhook is processed.
	r.changeRemotely(t, func(remote map[string]any) { remote["cancel_at"] = r.end.Unix() })
	r.syncSubscription(t, r.checkout)
	details, err := r.service.Details(ctx, r.token)
	if err != nil {
		t.Fatal(err)
	}
	view := jsonView(t, details)
	if sub := view["subscription"].(map[string]any); !jsonTime(t, sub["stops_at"]).Equal(r.end) ||
		!jsonTime(t, view["observed_at"]).After(first) {
		t.Fatalf("webhook cancellation not shown: %v", view)
	}
	if amount := states(); amount != "unavailable" {
		t.Fatalf("changes offered for a stopping gift: amount %s", amount)
	}
	if r.sub.retrievals != 1 {
		t.Fatalf("processor reads=%d, want only the amount change's", r.sub.retrievals)
	}
}

func TestCommandAfterStripeEndedTheGiftReleasesItsReservation(t *testing.T) {
	r := newRecurringFixture(t)
	ctx := context.Background()
	r.changeRemotely(t, func(remote map[string]any) { remote["status"] = "canceled" })
	if _, err := r.service.ChangeAmount(ctx, r.token, AmountRequest{RequestID: testID(t), AttemptID: r.attempt, Amount: 9000, Previous: 3000, EffectiveAt: r.end}); !errors.Is(
		err,
		ErrSubscriptionConflict,
	) {
		t.Fatalf("amount change for an ended gift: got %v, want ErrSubscriptionConflict", err)
	}
	var attempt string
	var reservations int
	if err := r.db.QueryRow(ctx, "SELECT a.status,(SELECT count(*) FROM donor_recurring_reservations WHERE donor_id=$2) FROM donor_stripe_checkouts a WHERE a.id=$1", r.attempt, r.donorID).Scan(&attempt, &reservations); err != nil ||
		attempt != "CANCELLED" ||
		reservations != 0 {
		t.Fatalf("attempt %s with %d reservations after the command synced an ended gift: %v", attempt, reservations, err)
	}
}

func TestRefusedCommandLeavesViewsCurrent(t *testing.T) {
	r := newRecurringFixture(t)
	ctx := context.Background()
	// Stripe renews without its webhook reaching DAD yet.
	next := r.end.AddDate(0, 1, 0)
	r.changeRemotely(t, func(remote map[string]any) {
		subscriptionItem(remote)["current_period_start"] = r.end.Unix()
		subscriptionItem(remote)["current_period_end"] = next.Unix()
	})
	periodEnd := func() time.Time {
		details, err := r.service.Details(ctx, r.token)
		if err != nil {
			t.Fatal(err)
		}
		return jsonTime(t, jsonView(t, details)["subscription"].(map[string]any)["period_end"])
	}
	if !periodEnd().Equal(r.end) {
		t.Fatal("view changed without a sync")
	}
	if _, err := r.service.Cancel(ctx, r.token, CancellationRequest{RequestID: testID(t), AttemptID: r.attempt, PeriodEnd: r.end}); !errors.Is(
		err,
		ErrSubscriptionConflict,
	) {
		t.Fatal("cancellation at a boundary Stripe has passed", err)
	}
	if len(r.sub.keys) != 0 {
		t.Fatal("refused cancellation reached the processor", r.sub.keys)
	}
	if !periodEnd().Equal(next) {
		t.Fatal("refused command did not leave the current period shown")
	}
	result, err := r.service.Cancel(ctx, r.token, CancellationRequest{RequestID: testID(t), AttemptID: r.attempt, PeriodEnd: next})
	if err != nil || !jsonTime(t, jsonView(t, result)["subscription"].(map[string]any)["stops_at"]).Equal(next) {
		t.Fatal("cancellation at the current boundary", result, err)
	}
}

func TestRecurringDetailsWithoutSubscription(t *testing.T) {
	r := newRecurringFixture(t)
	ctx := context.Background()
	if _, err := r.db.Exec(ctx, "UPDATE donor_stripe_checkouts SET stripe_subscription_id=NULL WHERE id=$1", r.attempt); err != nil {
		t.Fatal(err)
	}
	result, err := r.service.Details(ctx, r.token)
	if err != nil {
		t.Fatal(err)
	}
	if view := jsonView(t, result); view["state"] != "preparing" || view["subscription"] != nil || view["mode"] != "test" {
		t.Fatalf("unbound subscription: %v", view)
	}
	if _, err = r.service.Details(ctx, "not-a-session"); !errors.Is(err, ErrDenied) {
		t.Fatal("anonymous details", err)
	}
}

func TestDonorCancellationStopsAtPeriodEndOnce(t *testing.T) {
	r := newRecurringFixture(t)
	ctx := context.Background()
	request := CancellationRequest{RequestID: testID(t), AttemptID: r.attempt, PeriodEnd: r.end}
	if _, err := r.service.Cancel(ctx, r.token, CancellationRequest{RequestID: testID(t), AttemptID: r.attempt, PeriodEnd: r.end.Add(time.Hour)}); !errors.Is(
		err,
		ErrSubscriptionConflict,
	) {
		t.Fatal("cancellation at a different boundary", err)
	}
	if len(r.sub.keys) != 0 {
		t.Fatal("rejected cancellation reached processor")
	}
	result, err := r.service.Cancel(ctx, r.token, request)
	if err != nil {
		t.Fatal(err)
	}
	sub := jsonView(t, result)["subscription"].(map[string]any)
	if !jsonTime(t, sub["stops_at"]).Equal(r.end) || sub["status"] != "active" {
		t.Fatalf("cancellation view: %v", sub)
	}
	if _, err = r.service.Cancel(ctx, r.token, request); err != nil {
		t.Fatal("retry", err)
	}
	if len(r.sub.keys) != 1 || r.sub.keys[0] != "dad-cancel-"+request.RequestID.String() {
		t.Fatalf("processor mutations: %v", r.sub.keys)
	}
	var completed bool
	if err = r.db.QueryRow(ctx, "SELECT completed_at IS NOT NULL FROM donor_cancellations WHERE id=$1", request.RequestID).Scan(&completed); err != nil ||
		!completed {
		t.Fatal("cancellation not completed", err)
	}
	changed := request
	changed.PeriodEnd = r.end.Add(-time.Hour)
	if _, err = r.service.Cancel(ctx, r.token, changed); !errors.Is(err, ErrSubscriptionConflict) {
		t.Fatal("idempotency key reused for another boundary", err)
	}
	invalid := request
	invalid.RequestID.Bytes[6] = 0x10
	if _, err = r.service.Cancel(ctx, r.token, invalid); !errors.Is(err, payments.ErrFacts) {
		t.Fatal("non-v4 request id accepted", err)
	}
}

func TestAmountChangeAppliesAtNextRenewal(t *testing.T) {
	r := newRecurringFixture(t)
	ctx := context.Background()
	options, err := r.service.AmountOptions(ctx, r.token)
	if err != nil {
		t.Fatal(err)
	}
	view := jsonView(t, options)
	assertKeys(t, view, "state", "subscription_id", "amount_minor", "effective_at", "change")
	if view["state"] != "ready" || view["amount_minor"] != float64(3000) || view["change"] != nil ||
		!jsonTime(t, view["effective_at"]).Equal(r.end) {
		t.Fatalf("amount options: %v", view)
	}
	request := AmountRequest{RequestID: testID(t), AttemptID: r.attempt, Amount: 9000, Previous: 3000, EffectiveAt: r.end}
	if _, err = r.service.ChangeAmount(ctx, r.token, AmountRequest{RequestID: testID(t), AttemptID: r.attempt, Amount: 9000, Previous: 3000, EffectiveAt: r.start}); !errors.Is(
		err,
		ErrSubscriptionConflict,
	) {
		t.Fatal("mid-period amount change accepted", err)
	}
	changed, err := r.service.ChangeAmount(ctx, r.token, request)
	if err != nil {
		t.Fatal(err)
	}
	change := jsonView(t, changed)
	assertKeys(t, change, "request_id", "amount_minor", "previous_minor", "effective_at", "confirmed")
	if change["confirmed"] != true || change["amount_minor"] != float64(9000) || change["previous_minor"] != float64(3000) ||
		!jsonTime(t, change["effective_at"]).Equal(r.end) {
		t.Fatalf("amount change: %v", change)
	}
	if _, err = r.service.ChangeAmount(ctx, r.token, request); err != nil {
		t.Fatal("retry", err)
	}
	if len(r.sub.keys) != 1 || r.sub.keys[0] != "dad-amount-"+request.RequestID.String() {
		t.Fatalf("processor mutations: %v", r.sub.keys)
	}
	// The current period keeps the old amount: neither prorated nor repriced.
	repriced := invoice(r.checkout, "in_repriced_"+r.donorID.String(), r.start, true)
	for _, key := range []string{"amount_paid", "amount_due", "total"} {
		repriced[key] = 9000
	}
	repriced["lines"].(map[string]any)["data"].([]any)[0].(map[string]any)["amount"] = 9000
	repriced["payments"].(map[string]any)["data"].([]any)[0].(map[string]any)["amount_paid"] = 9000
	rejected := r.receive(t, "invoice.payment_succeeded", repriced)
	r.process(t)
	assertEventProcessed(t, r.workerFixture, rejected, false)
	accepted := r.receive(t, "invoice.payment_succeeded", invoice(r.checkout, "in_current_"+r.donorID.String(), r.start, true))
	r.process(t)
	assertEventProcessed(t, r.workerFixture, accepted, true)
	details, err := r.service.Details(ctx, r.token)
	if err != nil || jsonView(t, details)["subscription"].(map[string]any)["amount_minor"] != float64(9000) {
		t.Fatal("next renewal amount not shown", details, err)
	}
}

func TestAmountChangeRespectsTheGiftCeiling(t *testing.T) {
	r := newRecurringFixture(t)
	ctx := context.Background()
	// A cent over the $900/mo ceiling is refused before any processor call.
	over := AmountRequest{RequestID: testID(t), AttemptID: r.attempt, Amount: 90001, Previous: 3000, EffectiveAt: r.end}
	if _, err := r.service.ChangeAmount(ctx, r.token, over); !errors.Is(err, payments.ErrFacts) {
		t.Fatal("amount above the ceiling accepted", err)
	}
	// Exactly the ceiling ($30/day) is allowed.
	at := AmountRequest{RequestID: testID(t), AttemptID: r.attempt, Amount: 90000, Previous: 3000, EffectiveAt: r.end}
	if _, err := r.service.ChangeAmount(ctx, r.token, at); err != nil {
		t.Fatal("ceiling amount rejected", err)
	}
}

func TestStaffCancellationIsAuditedWithReasonAndOutcome(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	// Staff outlive the fixture's staff actions, so they are created first.
	_, finance := staffFixture(t, pool, "SUPER_ADMIN")
	_, support := staffFixture(t, pool, "OP_PARTNERSHIPS")
	r := newRecurringFixture(t)
	ctx := context.Background()
	s := StaffFinanceService{Staff: &StaffService{DB: api}, Gateway: r.sub, Origin: "https://donor.example.org"}
	request := StaffFinancialRequest{
		RequestID: testID(t),
		DonorID:   r.donorID,
		TargetID:  r.attempt,
		Reason:    "Donor asked by phone to stop",
		PeriodEnd: r.end,
	}
	if _, err := s.Cancel(ctx, support, request); !errors.Is(err, ErrStaffForbidden) {
		t.Fatal("support cancelled a gift", err)
	}
	short := request
	short.Reason = "no"
	if _, err := s.Cancel(ctx, finance, short); !errors.Is(err, ErrStaffInput) {
		t.Fatal("cancellation without a reason", err)
	}
	quote, err := s.CancellationQuote(ctx, finance, r.donorID)
	if err != nil || !jsonTime(t, jsonView(t, quote)["subscription"].(map[string]any)["period_end"]).Equal(r.end) {
		t.Fatal("cancellation quote", quote, err)
	}
	r.sub.fail = payments.ErrProcessorUnavailable
	if _, err = s.Cancel(ctx, finance, request); !errors.Is(err, payments.ErrProcessorUnavailable) {
		t.Fatal("processor failure hidden", err)
	}
	result, err := s.Cancel(ctx, finance, request)
	if err != nil {
		t.Fatal(err)
	}
	view := jsonView(t, result)
	if view["request_id"] != request.RequestID.String() || !jsonTime(t, view["subscription"].(map[string]any)["stops_at"]).Equal(r.end) {
		t.Fatalf("staff cancellation: %v", view)
	}
	if len(r.sub.keys) != 2 || r.sub.keys[0] != r.sub.keys[1] {
		t.Fatal("retry changed the processor idempotency key", r.sub.keys)
	}
	var status string
	if err = pool.QueryRow(ctx, "SELECT status FROM donor_staff_actions WHERE id=$1", request.RequestID).Scan(&status); err != nil ||
		status != "OBSERVED" {
		t.Fatal("staff action not completed", status, err)
	}
	rows, err := pool.Query(
		ctx,
		"SELECT action,details::text FROM donor_staff_audit WHERE target=$1 ORDER BY created_at,action",
		request.RequestID.String(),
	)
	if err != nil {
		t.Fatal(err)
	}
	audit := map[string]map[string]string{}
	for rows.Next() {
		var action, raw string
		if err = rows.Scan(&action, &raw); err != nil {
			t.Fatal(err)
		}
		details := map[string]string{}
		if err = json.Unmarshal([]byte(raw), &details); err != nil {
			t.Fatal(err)
		}
		audit[action] = details
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"gift.action_authorized", "gift.cancellation_failed", "gift.cancellation_confirmed"} {
		if audit[action]["reason"] != request.Reason {
			t.Fatalf("%s audit lacks reason: %v", action, audit)
		}
	}
	if audit["gift.cancellation_failed"]["outcome"] != "processor_unavailable" {
		t.Fatalf("failure outcome not audited: %v", audit)
	}
}

func TestStaffRefundRetrySettlesLostOutcome(t *testing.T) {
	for _, recordedMeanwhile := range []bool{false, true} {
		name := "snapshot used"
		if recordedMeanwhile {
			name = "stale snapshot refetched"
		}
		t.Run(name, func(t *testing.T) {
			pool := database(t)
			api := as(t, dbroles.API)
			actor, finance := staffFixture(t, pool, "SUPER_ADMIN")
			r := newRecurringFixture(t)
			ctx := context.Background()
			r.receive(t, "invoice.payment_succeeded", invoice(r.checkout, "in_quote_"+r.donorID.String(), r.start, true))
			r.process(t)
			var gift queries.DonorGift
			if err := pool.QueryRow(ctx, "SELECT id,stripe_payment_intent_id FROM donor_gifts WHERE donor_id=$1", r.donorID).Scan(&gift.ID, &gift.StripePaymentIntentID); err != nil {
				t.Fatal(err)
			}
			// Stripe created this refund, but the command lost its outcome.
			action := testID(t)
			if _, err := pool.Exec(ctx, "INSERT INTO donor_staff_actions(id,actor_id,donor_id,kind,target_id,amount_minor,reason) VALUES($1,$2,$3,'REFUND',$4,1000,'Duplicate gift reported')", action, actor, r.donorID, gift.ID); err != nil {
				t.Fatal(err)
			}
			refund := paymentObject(
				t,
				map[string]any{
					"id":             "re_" + action.String(),
					"payment_intent": gift.StripePaymentIntentID.String,
					"amount":         1000,
					"currency":       "usd",
					"status":         "succeeded",
					"metadata":       map[string]string{"dad_staff_action": action.String()},
				},
			)
			r.sub.refundRows = []payments.Object{refund}
			s := StaffFinanceService{Staff: &StaffService{DB: api}, Gateway: r.sub, Origin: "https://donor.example.org"}
			quote, err := s.RefundQuote(ctx, finance, r.donorID, gift.ID)
			if err != nil {
				t.Fatal(err)
			}
			view := jsonView(t, quote)
			assertKeys(t, view, "gift_id", "donor_id", "amount_minor", "available_minor", "currency", "mode")
			if view["available_minor"] != float64(3000) || view["amount_minor"] != float64(3000) || view["gift_id"] != gift.ID.String() ||
				r.sub.refundLists != 0 {
				t.Fatalf("quote: %v after %d processor lists", view, r.sub.refundLists)
			}
			pending, err := (&StaffService{DB: api}).PendingAction(ctx, finance, r.donorID, gift.ID, "REFUND")
			if err != nil || pending.Action == nil || pending.Action.ID != action {
				t.Fatal("unsettled refund not offered for retry", pending, err)
			}
			if recordedMeanwhile {
				// A webhook records the refund between the retry's processor read and its lock.
				r.sub.beforeList = func() {
					event := r.receive(
						t,
						"refund.updated",
						map[string]any{
							"id":             "re_" + action.String(),
							"payment_intent": gift.StripePaymentIntentID.String,
							"status":         "pending",
						},
					)
					r.gateway.refunds["re_"+action.String()] = paymentObject(
						t,
						map[string]any{
							"id":             "re_" + action.String(),
							"payment_intent": gift.StripePaymentIntentID.String,
							"amount":         1000,
							"currency":       "usd",
							"status":         "pending",
						},
					)
					r.process(t)
					assertEventProcessed(t, r.workerFixture, event, true)
				}
			}
			result, err := s.Refund(
				ctx,
				finance,
				StaffFinancialRequest{
					RequestID: action,
					DonorID:   r.donorID,
					TargetID:  gift.ID,
					Amount:    1000,
					Reason:    "Duplicate gift reported",
				},
			)
			if err != nil || result.Status != "SUCCEEDED" {
				t.Fatal("retry did not settle the refund", result, err)
			}
			lists := 1
			if recordedMeanwhile {
				lists = 2
			}
			if r.sub.refundLists != lists {
				t.Fatalf("processor lists=%d want %d", r.sub.refundLists, lists)
			}
			var status, refundStatus string
			if err = pool.QueryRow(ctx, "SELECT a.status,r.status FROM donor_staff_actions a JOIN donor_refunds r ON r.stripe_refund_id=a.stripe_refund_id WHERE a.id=$1", action).Scan(&status, &refundStatus); err != nil ||
				status != "OBSERVED" ||
				refundStatus != "SUCCEEDED" {
				t.Fatal("staff refund not settled", status, refundStatus, err)
			}
			var observedAudits, previews int
			if err = pool.QueryRow(ctx, "SELECT count(*) FILTER (WHERE action='gift.refund_observed' AND target=$2),count(*) FILTER (WHERE action='gift.refund_preview' AND target=$3) FROM donor_staff_audit WHERE actor_id=$1", actor, action.String(), gift.ID.String()).Scan(&observedAudits, &previews); err != nil ||
				observedAudits != 1 ||
				previews != 1 {
				t.Fatal("refund not audited", observedAudits, previews, err)
			}
			p, err := queries.New(pool).Projection(ctx, r.donorID)
			if err != nil || p.NetMinor.Int64 != 2000 {
				t.Fatal("settled refund not projected", err)
			}
			quote, err = s.RefundQuote(ctx, finance, r.donorID, gift.ID)
			if view := jsonView(t, quote); err != nil || view["available_minor"] != float64(2000) || view["observed_at"] == nil ||
				r.sub.refundLists != lists {
				t.Fatal("quote after the settled refund", view, err)
			}
		})
	}
}

func TestGiftListsAndStaffDetailShowRefundsAndSubscription(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	_, staff := staffFixture(t, pool, "SUPER_ADMIN")
	r := newRecurringFixture(t)
	ctx := context.Background()
	r.receive(t, "invoice.payment_succeeded", invoice(r.checkout, "in_detail_"+r.donorID.String(), r.start, true))
	r.process(t)
	var gift pgtype.UUID
	if err := pool.QueryRow(ctx, "SELECT id FROM donor_gifts WHERE donor_id=$1", r.donorID).Scan(&gift); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"SUCCEEDED", "FAILED"} {
		if _, err := pool.Exec(ctx, "INSERT INTO donor_refunds(id,gift_id,stripe_refund_id,amount_minor,currency,status,observed_at) VALUES($1,$2,$3,1000,'USD',$4,now())", testID(t), gift, "re_"+status+gift.String(), status); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "DELETE FROM donor_refunds WHERE gift_id=$1", gift); err != nil {
			t.Error(err)
		}
	})
	if _, err := r.service.Details(ctx, r.token); err != nil {
		t.Fatal(err)
	}
	donorPage, err := DonorService{DB: api, Origin: "https://donor.example.org"}.Gifts(ctx, r.token, 0)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := (&StaffService{DB: api}).DonorDetail(ctx, staff, r.donorID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for name, gifts := range map[string][]queries.DonorGiftsPageRow{"donor": donorPage.Gifts, "staff": detail.Gifts} {
		if len(gifts) != 1 || gifts[0].RefundedMinor != 1000 || gifts[0].NetMinor != 2000 {
			t.Fatalf("%s gifts: got %+v, want 1000 refunded and 2000 net", name, gifts)
		}
	}
	view := jsonView(t, detail)
	sub, _ := view["subscription"].(map[string]any)
	if view["claimed"] != true || view["founding_donor"] == nil || sub == nil || sub["status"] != "active" ||
		sub["amount_minor"] != float64(3000) ||
		!jsonTime(t, sub["next_renewal"]).Equal(r.end) {
		t.Fatalf("staff detail: %v", view)
	}
}

func TestValidGiftAmountIsWholeDollarsADayFromOneToThirty(t *testing.T) {
	for _, c := range []struct {
		minor int64
		want  bool
	}{
		{-3000, false}, {0, false}, {100, false}, {2999, false}, {3000, true}, {3001, false}, {4500, false}, {6000, true},
		{89999, false}, {90000, true}, {90001, false}, {93000, false}, {9_000_000, false},
	} {
		if got := ValidGiftAmount(c.minor); got != c.want {
			t.Errorf("ValidGiftAmount(%d) = %v, want %v", c.minor, got, c.want)
		}
	}
}

// A refused amount change must reach neither the processor nor the database.
func TestRefusedAmountChangesLeaveNothingBehind(t *testing.T) {
	r := newRecurringFixture(t)
	ctx := context.Background()
	for _, c := range []struct {
		name string
		edit func(*AmountRequest)
		want error
	}{
		{"below the one dollar a day floor", func(a *AmountRequest) { a.Amount = 1500 }, payments.ErrFacts},
		{"not a whole number of dollars a day", func(a *AmountRequest) { a.Amount = 4500 }, payments.ErrFacts},
		{"a day over the ceiling", func(a *AmountRequest) { a.Amount = 93000 }, payments.ErrFacts},
		{"unchanged amount", func(a *AmountRequest) { a.Amount = 3000 }, ErrSubscriptionConflict},
		{"stale previous amount", func(a *AmountRequest) { a.Previous = 6000 }, ErrSubscriptionConflict},
		{"unknown subscription", func(a *AmountRequest) { a.AttemptID = testID(t) }, ErrSubscriptionConflict},
	} {
		request := AmountRequest{RequestID: testID(t), AttemptID: r.attempt, Amount: 9000, Previous: 3000, EffectiveAt: r.end}
		c.edit(&request)
		if _, err := r.service.ChangeAmount(ctx, r.token, request); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
		var stored int
		if err := r.db.QueryRow(ctx, "SELECT count(*) FROM donor_amount_changes WHERE id=$1", request.RequestID).Scan(&stored); err != nil ||
			stored != 0 {
			t.Errorf("%s persisted %d amount changes: %v", c.name, stored, err)
		}
	}
	if len(r.sub.keys) != 0 {
		t.Fatalf("refused changes reached the processor: %v", r.sub.keys)
	}
}

// Once a gift is set to stop at period end, an amount change is refused and a
// repeat cancellation does not reach the processor again.
func TestStoppingGiftRefusesFurtherCommands(t *testing.T) {
	r := newRecurringFixture(t)
	ctx := context.Background()
	if _, err := r.service.Cancel(ctx, r.token, CancellationRequest{RequestID: testID(t), AttemptID: r.attempt, PeriodEnd: r.end}); err != nil {
		t.Fatal(err)
	}
	mutations := len(r.sub.keys)
	if _, err := r.service.ChangeAmount(ctx, r.token, AmountRequest{RequestID: testID(t), AttemptID: r.attempt, Amount: 9000, Previous: 3000, EffectiveAt: r.end}); !errors.Is(
		err,
		ErrSubscriptionConflict,
	) {
		t.Fatalf("amount change for a stopping gift: got %v, want ErrSubscriptionConflict", err)
	}
	// A repeat cancellation is already satisfied: it reports the stop date without a second processor call.
	again, err := r.service.Cancel(ctx, r.token, CancellationRequest{RequestID: testID(t), AttemptID: r.attempt, PeriodEnd: r.end})
	if err != nil || !jsonTime(t, jsonView(t, again)["subscription"].(map[string]any)["stops_at"]).Equal(r.end) {
		t.Fatalf("second cancellation: %v, %v; want the existing stop date", again, err)
	}
	if len(r.sub.keys) != mutations {
		t.Fatalf("refused commands reached the processor: %v", r.sub.keys[mutations:])
	}
}
