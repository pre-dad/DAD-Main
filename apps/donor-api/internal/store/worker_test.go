package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stripe/stripe-go/v87"
)

type financialGateway struct {
	intents             map[string]payments.Object
	refunds             map[string]payments.Object
	subscriptions       map[string]payments.Object
	paymentMethods      map[string]payments.Object
	sessions            map[string]payments.Object
	setups              map[string]payments.Object
	updateKeys          []string
	loseAcknowledgement bool
}

func (g *financialGateway) RetrievePaymentIntent(_ context.Context, id string) (payments.Object, error) {
	value, ok := g.intents[id]
	if !ok {
		return nil, payments.ErrProcessorUnavailable
	}
	return value, nil
}

func (g *financialGateway) RetrievePaymentMethod(_ context.Context, id string) (payments.Object, error) {
	value, ok := g.paymentMethods[id]
	if !ok {
		return nil, payments.ErrProcessorUnavailable
	}
	return value, nil
}

func (g *financialGateway) RetrieveRefund(_ context.Context, id string) (payments.Object, error) {
	value, ok := g.refunds[id]
	if !ok {
		return nil, payments.ErrProcessorUnavailable
	}
	return value, nil
}

func (g *financialGateway) RetrieveSubscription(_ context.Context, id string) (payments.Object, error) {
	value, ok := g.subscriptions[id]
	if !ok {
		return nil, payments.ErrProcessorUnavailable
	}
	return value, nil
}

func (g *financialGateway) RetrieveCheckout(_ context.Context, id string) (payments.Object, error) {
	value, ok := g.sessions[id]
	if !ok {
		return nil, payments.ErrProcessorUnavailable
	}
	return value, nil
}

func (g *financialGateway) RetrieveSetupIntent(_ context.Context, id string) (payments.Object, error) {
	value, ok := g.setups[id]
	if !ok {
		return nil, payments.ErrProcessorUnavailable
	}
	return value, nil
}

func (g *financialGateway) UpdateSubscriptionPaymentMethod(_ context.Context, id, method, key string) error {
	g.updateKeys = append(g.updateKeys, key)
	value, ok := g.subscriptions[id]
	if !ok {
		return payments.ErrProcessorUnavailable
	}
	value["default_payment_method"], _ = json.Marshal(method)
	if g.loseAcknowledgement {
		g.loseAcknowledgement = false
		return payments.ErrProcessorUnavailable
	}
	return nil
}

type workerFixture struct {
	// db is the owner's pool, for fixtures and assertions.
	db      *pgxpool.Pool
	service CheckoutService
	request payments.CheckoutRequest
	donorID pgtype.UUID
	worker  EventWorker
	events  []string
	gateway *financialGateway
}

func newWorkerFixture(t *testing.T, frequency string) *workerFixture {
	t.Helper()
	service, request, id, _ := checkoutFixture(t)
	request.Frequency = frequency
	gateway := &financialGateway{
		intents:       map[string]payments.Object{},
		refunds:       map[string]payments.Object{},
		subscriptions: map[string]payments.Object{},
		sessions:      map[string]payments.Object{},
		setups:        map[string]payments.Object{},
	}
	f := &workerFixture{
		db:      database(t),
		service: service,
		request: request,
		donorID: id,
		worker:  EventWorker{DB: as(t, dbroles.StripeWorker), Gateway: gateway},
		gateway: gateway,
	}
	t.Cleanup(func() {
		ctx := context.Background()
		for _, sql := range []string{
			"DELETE FROM donor_messages WHERE donor_id=$1 OR (kind='REFERRAL' AND context->>'referred_id'=$2)",
			"DELETE FROM donor_referral_corrections WHERE donor_id=$1",
			"DELETE FROM donor_billing_observations WHERE period_id IN (SELECT id FROM donor_billing_periods WHERE donor_id=$1)",
			"DELETE FROM donor_refund_observations WHERE refund_id IN (SELECT r.id FROM donor_refunds r JOIN donor_gifts g ON g.id=r.gift_id WHERE g.donor_id=$1)",
			"DELETE FROM donor_refunds WHERE gift_id IN (SELECT id FROM donor_gifts WHERE donor_id=$1)",
			"DELETE FROM donor_gifts WHERE donor_id=$1",
			"DELETE FROM donor_billing_periods WHERE donor_id=$1",
			"DELETE FROM donor_metric_projections WHERE donor_id=$1",
			"DELETE FROM donor_login_emails WHERE donor_id=$1",
			"DELETE FROM donor_subscription_leases WHERE donor_id=$1",
			"DELETE FROM donor_subscriptions WHERE donor_id=$1",
			"DELETE FROM donor_amount_changes WHERE donor_id=$1",
			"DELETE FROM donor_payment_updates WHERE donor_id=$1",
		} {
			args := []any{id}
			if sql == "DELETE FROM donor_messages WHERE donor_id=$1 OR (kind='REFERRAL' AND context->>'referred_id'=$2)" {
				args = append(args, id.String())
			}
			if _, err := f.db.Exec(ctx, sql, args...); err != nil {
				t.Error(err)
			}
		}
		if _, err := f.db.Exec(ctx, "DELETE FROM donor_stripe_events WHERE stripe_event_id=ANY($1::text[])", f.events); err != nil {
			t.Error(err)
		}
	})
	return f
}

func (f *workerFixture) checkout(t *testing.T) payments.CheckoutResult {
	t.Helper()
	result, err := f.service.BeginCheckout(context.Background(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func (f *workerFixture) receive(t *testing.T, kind string, obj map[string]any) string {
	t.Helper()
	return f.receiveAt(t, kind, obj, time.Now())
}

func (f *workerFixture) receiveAt(t *testing.T, kind string, obj map[string]any, occurred time.Time) string {
	t.Helper()
	eventID := "evt_" + f.request.RequestID.String() + "_" + strconv.Itoa(len(f.events))
	f.events = append(f.events, eventID)
	f.deliver(t, eventID, kind, obj, occurred)
	return eventID
}

// deliver posts a signed event under eventID, as Stripe does for every
// attempt at the same event.
func (f *workerFixture) deliver(t *testing.T, eventID, kind string, obj map[string]any, occurred time.Time) {
	t.Helper()
	now := time.Now()
	body, err := json.Marshal(
		map[string]any{
			"id":          eventID,
			"object":      "event",
			"type":        kind,
			"api_version": payments.APIVersion,
			"created":     occurred.Unix(),
			"livemode":    false,
			"data":        map[string]any{"object": obj},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	const secret = "whsec_public_worker_test"
	signature := fmt.Sprintf("t=%d,v1=%x", now.Unix(), stripe.ComputeSignature(now, body, secret))
	receiver := payments.Receiver{Secret: secret, Inbox: StripeInbox{DB: f.service.DB}}
	if err = receiver.Receive(context.Background(), body, signature); err != nil {
		t.Fatal(err)
	}
}

func (f *workerFixture) process(t *testing.T) {
	t.Helper()
	worked, err := f.worker.ProcessNext(context.Background())
	if err != nil || !worked {
		t.Fatalf("worker did not handle event: %v", err)
	}
}

// retryNow lifts the backoff on the fixture's failed events.
func (f *workerFixture) retryNow(t *testing.T) {
	t.Helper()
	if _, err := f.db.Exec(context.Background(), "UPDATE donor_stripe_events SET available_at=now() WHERE stripe_event_id=ANY($1::text[]) AND processed_at IS NULL", f.events); err != nil {
		t.Fatal(err)
	}
}

func invoice(checkout, id string, start time.Time, paid bool) map[string]any {
	status, amountPaid := "open", 0
	var paidAt any
	if paid {
		status = "paid"
		amountPaid = 3000
		paidAt = start.Add(time.Hour).Unix()
	}
	return map[string]any{
		"id":               id,
		"status":           status,
		"currency":         "usd",
		"amount_paid":      amountPaid,
		"amount_due":       3000,
		"total":            3000,
		"starting_balance": 0,
		"ending_balance":   0,
		"billing_reason":   "subscription_cycle",
		"parent": map[string]any{
			"subscription_details": map[string]any{
				"subscription": "sub_" + checkout,
				"metadata":     map[string]any{"dad_checkout_id": checkout},
			},
		},
		"status_transitions": map[string]any{"paid_at": paidAt},
		"lines": map[string]any{
			"has_more": false,
			"data": []any{
				map[string]any{
					"amount":   3000,
					"currency": "usd",
					"period":   map[string]any{"start": start.Unix(), "end": start.Add(30 * 24 * time.Hour).Unix()},
					"parent": map[string]any{
						"type":                      "subscription_item_details",
						"subscription_item_details": map[string]any{"subscription": "sub_" + checkout, "proration": false},
					},
				},
			},
		},
		"payments": map[string]any{
			"has_more": false,
			"data": []any{
				map[string]any{
					"status":      "paid",
					"amount_paid": 3000,
					"currency":    "usd",
					"invoice":     id,
					"livemode":    false,
					"payment":     map[string]any{"type": "payment_intent", "payment_intent": "pi_" + id},
				},
			},
		},
	}
}

func TestLegacyGuestInvoiceClaimDashboardAndRefund(t *testing.T) {
	pool := database(t)
	referrer, _ := donor(t, pool)
	accountCleanup(t, pool, referrer)
	f := newWorkerFixture(t, "MONTHLY")
	ctx := context.Background()
	visitor := "visitor-" + f.donorID.String()
	f.request.Visitor = &visitor
	if _, err := f.db.Exec(ctx, `INSERT INTO donor_referral_touches (id,token_hash,referrer_id,kind,touched_at,expires_at) VALUES ($1,$2,$3,'IRL',$4,$5)`, testID(t), HashToken(visitor), referrer, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, "DELETE FROM donor_referral_touches WHERE referrer_id=$1", referrer); err != nil {
			t.Error(err)
		}
	})
	checkout := f.checkout(t)
	// Guest checkouts issued before mandatory verification must still settle and send account proof.
	mustExec(t, pool, "UPDATE donors SET claimed_at=NULL WHERE id=$1", f.donorID)
	mustExec(t, pool, "UPDATE donor_sessions SET revoked_at=now() WHERE donor_id=$1", f.donorID)
	invoiceID := "in_" + f.donorID.String()
	start := time.Now().UTC().Truncate(time.Second).Add(-2 * 24 * time.Hour)
	obj := invoice(checkout.CheckoutID, invoiceID, start, true)
	eventID := f.receive(t, "invoice.payment_succeeded", obj)
	f.process(t)
	var processed *time.Time
	if err := pool.QueryRow(ctx, "SELECT processed_at FROM donor_stripe_events WHERE stripe_event_id=$1", eventID).Scan(&processed); err != nil ||
		processed == nil {
		t.Fatal("valid gift not committed")
	}
	f.receive(t, "invoice.payment_succeeded", obj)
	f.process(t)
	// A delayed failure must not undo a confirmed period.
	f.receive(t, "invoice.payment_failed", invoice(checkout.CheckoutID, invoiceID, start, false))
	f.process(t)
	var gifts, receipts, emails, recovery int
	for _, tc := range []struct {
		sql    string
		target *int
	}{
		{"SELECT count(*) FROM donor_gifts WHERE donor_id=$1", &gifts}, {"SELECT count(*) FROM donor_messages WHERE donor_id=$1 AND kind='RECEIPT'", &receipts},
		{"SELECT count(*) FROM donor_login_emails WHERE donor_id=$1", &emails}, {"SELECT count(*) FROM donor_messages WHERE donor_id=$1 AND kind='RECOVERY'", &recovery},
	} {
		if err := pool.QueryRow(ctx, tc.sql, f.donorID).Scan(tc.target); err != nil {
			t.Fatal(err)
		}
	}
	if gifts != 1 || receipts != 1 || emails != 1 || recovery != 0 {
		t.Fatalf("duplicate or out-of-order side effects: %d %d %d %d", gifts, receipts, emails, recovery)
	}
	api := as(t, dbroles.API)
	token, err := (AccountService{DB: api}).ClaimAccount(ctx, link(t, pool, f.donorID, time.Now()), "", Client{})
	if err != nil {
		t.Fatal(err)
	}
	dashboard, err := (DashboardReader{DB: api}).DonorDashboard(ctx, token.Access)
	if err != nil || !dashboard.FoundingDonor || dashboard.Streak == nil || dashboard.Streak.Current != 1 {
		t.Fatalf("claimed dashboard: %v %v", dashboard, err)
	}
	refToken, err := (AccountService{DB: api}).ClaimAccount(ctx, link(t, pool, referrer, time.Now()), "", Client{})
	if err != nil {
		t.Fatal(err)
	}
	refDashboard, err := (DashboardReader{DB: api}).DonorDashboard(ctx, refToken.Access)
	if err != nil {
		t.Fatal(err)
	}
	attribution := refDashboard.Referrals
	if attribution == nil || attribution.IRL != 1 || attribution.TotalMinor != 3000 {
		t.Fatal("legacy guest referral not credited")
	}
	refundID := "re_" + f.donorID.String()
	raw, err := json.Marshal(
		map[string]any{"id": refundID, "payment_intent": "pi_" + invoiceID, "amount": 3000, "currency": "usd", "status": "succeeded"},
	)
	if err != nil {
		t.Fatal(err)
	}
	f.gateway.refunds[refundID], err = payments.DecodeObject(raw)
	if err != nil {
		t.Fatal(err)
	}
	f.receive(t, "refund.updated", map[string]any{"id": refundID, "payment_intent": "pi_" + invoiceID, "status": "pending"})
	f.process(t)
	projection, err := queries.New(pool).Projection(ctx, f.donorID)
	if err != nil || projection.NetMinor.Int64 != 0 || projection.CurrentStreak.Int32 != 0 || projection.BestStreak != 1 ||
		!projection.FoundingAwardedAt.Valid {
		t.Fatalf("refund recognition: %+v %v", projection, err)
	}
	refDashboard, err = (DashboardReader{DB: api}).DonorDashboard(ctx, refToken.Access)
	if err != nil {
		t.Fatal(err)
	}
	attribution = refDashboard.Referrals
	if attribution == nil || attribution.IRL != 0 || attribution.TotalMinor != 0 {
		t.Fatal("refund retained collected dollars")
	}
}

func TestInvoiceFailureRecoveryPreservesGrace(t *testing.T) {
	f := newWorkerFixture(t, "MONTHLY")
	checkout := f.checkout(t)
	ctx := context.Background()
	start := time.Now().UTC().Truncate(time.Second).Add(-32 * 24 * time.Hour)
	f.receive(t, "invoice.payment_succeeded", invoice(checkout.CheckoutID, "in_first_"+f.donorID.String(), start, true))
	f.process(t)
	second := "in_second_" + f.donorID.String()
	due := start.Add(30 * 24 * time.Hour)
	f.receive(t, "invoice.payment_failed", invoice(checkout.CheckoutID, second, due, false))
	f.process(t)
	p, err := queries.New(f.db).Projection(ctx, f.donorID)
	if err != nil || !p.CurrentStreak.Valid || p.CurrentStreak.Int32 != 1 || !p.AtRisk ||
		!p.GraceEndsAt.Time.Equal(due.Add(7*24*time.Hour)) {
		t.Fatalf("grace lost: %+v %v", p, err)
	}
	f.receive(t, "invoice.payment_succeeded", invoice(checkout.CheckoutID, second, due, true))
	f.process(t)
	p, err = queries.New(f.db).Projection(ctx, f.donorID)
	if err != nil || p.CurrentStreak.Int32 != 2 || p.AtRisk || p.GraceEndsAt.Valid {
		t.Fatal("recovery did not restore streak")
	}
}

func TestInvalidInvoiceRollsBackAllFinancialEffects(t *testing.T) {
	f := newWorkerFixture(t, "MONTHLY")
	checkout := f.checkout(t)
	ctx := context.Background()
	obj := invoice(checkout.CheckoutID, "in_bad_"+f.donorID.String(), time.Now().Add(-24*time.Hour), true)
	obj["amount_paid"], obj["amount_due"], obj["total"] = 9000, 9000, 9000 // Fails after recording the valid period inside the savepoint.
	obj["payments"].(map[string]any)["data"].([]any)[0].(map[string]any)["amount_paid"] = 9000
	eventID := f.receive(t, "invoice.payment_succeeded", obj)
	for i := 0; i < 10; i++ {
		f.process(t)
		f.retryNow(t)
	}
	if worked, err := f.worker.ProcessNext(ctx); err != nil || worked {
		t.Fatal("exhausted event did not park")
	}
	for _, sql := range []string{
		"SELECT count(*) FROM donor_gifts WHERE donor_id=$1",
		"SELECT count(*) FROM donor_billing_periods WHERE donor_id=$1",
		// Checkout seeds an uncomputed projection row; a computed one would be a partial write.
		"SELECT count(*) FROM donor_metric_projections WHERE donor_id=$1 AND net_minor IS NOT NULL",
		"SELECT count(*) FROM donor_login_emails WHERE donor_id=$1",
		"SELECT count(*) FROM donor_messages WHERE donor_id=$1",
	} {
		var count int
		if err := f.db.QueryRow(ctx, sql, f.donorID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial financial write survived: %s %d %v", sql, count, err)
		}
	}
	var observations int
	if err := f.db.QueryRow(ctx, "SELECT count(*) FROM donor_billing_observations WHERE source_event_id IN (SELECT id FROM donor_stripe_events WHERE stripe_event_id=$1)", eventID).Scan(&observations); err != nil ||
		observations != 0 {
		t.Fatalf("partial billing observation survived: %d %v", observations, err)
	}
	var attempts int
	var processed *time.Time
	var failure string
	if err := f.db.QueryRow(ctx, "SELECT attempts,processed_at,last_error FROM donor_stripe_events WHERE stripe_event_id=$1", eventID).Scan(&attempts, &processed, &failure); err != nil ||
		attempts != 10 ||
		processed != nil ||
		failure != "InvalidFacts" {
		t.Fatal("failure budget not durable")
	}
}

func eventRetryState(t *testing.T, f *workerFixture, id string) (attempts int, failure string, delay time.Duration) {
	t.Helper()
	var seconds float64
	if err := f.db.QueryRow(context.Background(), "SELECT attempts,coalesce(last_error,''),extract(epoch FROM available_at-now()) FROM donor_stripe_events WHERE stripe_event_id=$1", id).Scan(&attempts, &failure, &seconds); err != nil {
		t.Fatal(err)
	}
	return attempts, failure, time.Duration(seconds * float64(time.Second))
}

func TestTransientGatewayFailureBacksOffThenSucceeds(t *testing.T) {
	f := newWorkerFixture(t, "MONTHLY")
	checkout := f.checkout(t)
	ctx := context.Background()
	invoiceID := "in_" + f.donorID.String()
	payment := "pi_" + invoiceID
	start := time.Now().UTC().Truncate(time.Second).Add(-2 * 24 * time.Hour)
	f.receive(t, "invoice.payment_succeeded", invoice(checkout.CheckoutID, invoiceID, start, true))
	f.process(t)
	refundID := "re_" + f.donorID.String()
	event := f.receive(t, "refund.updated", map[string]any{"id": refundID, "payment_intent": payment, "status": "succeeded"})
	for attempt, want := range []time.Duration{time.Minute, 2 * time.Minute} {
		if attempt > 0 {
			f.retryNow(t)
		}
		f.process(t)
		attempts, failure, delay := eventRetryState(t, f, event)
		if attempts != attempt+1 || failure != "PaymentUnavailable" || delay < want-10*time.Second || delay > want {
			t.Fatalf("attempt %d: attempts=%d failure=%s delay=%s", attempt+1, attempts, failure, delay)
		}
		assertEventProcessed(t, f, event, false)
	}
	f.gateway.refunds[refundID] = paymentObject(
		t,
		map[string]any{"id": refundID, "payment_intent": payment, "amount": 1000, "currency": "usd", "status": "succeeded"},
	)
	f.retryNow(t)
	f.process(t)
	assertEventProcessed(t, f, event, true)
	p, err := queries.New(f.db).Projection(ctx, f.donorID)
	if err != nil || p.NetMinor.Int64 != 2000 {
		t.Fatal("retried refund not applied", p.NetMinor, err)
	}
}

func TestRefundBeforeGiftIsRetriedAfterGiftExists(t *testing.T) {
	f := newWorkerFixture(t, "MONTHLY")
	checkout := f.checkout(t)
	ctx := context.Background()
	invoiceID := "in_" + f.donorID.String()
	payment := "pi_" + invoiceID
	refundID := "re_" + f.donorID.String()
	f.gateway.refunds[refundID] = paymentObject(
		t,
		map[string]any{"id": refundID, "payment_intent": payment, "amount": 3000, "currency": "usd", "status": "succeeded"},
	)
	refund := f.receive(t, "refund.created", map[string]any{"id": refundID, "payment_intent": payment, "status": "succeeded"})
	f.process(t)
	if attempts, failure, delay := eventRetryState(t, f, refund); attempts != 1 || failure != "MissingRecord" || delay <= 0 {
		t.Fatalf("early refund: attempts=%d failure=%s delay=%s", attempts, failure, delay)
	}
	start := time.Now().UTC().Truncate(time.Second).Add(-2 * 24 * time.Hour)
	gift := f.receive(t, "invoice.payment_succeeded", invoice(checkout.CheckoutID, invoiceID, start, true))
	f.process(t)
	assertEventProcessed(t, f, gift, true)
	assertEventProcessed(t, f, refund, false)
	f.retryNow(t)
	f.process(t)
	assertEventProcessed(t, f, refund, true)
	p, err := queries.New(f.db).Projection(ctx, f.donorID)
	if err != nil || p.NetMinor.Int64 != 0 || !p.FoundingAwardedAt.Valid {
		t.Fatal("late refund not reconciled", p.NetMinor, err)
	}
}

// Stripe redelivers an event until it is acknowledged, and may do so after
// it was processed: the replay must keep the original facts and state and
// must not recognize the gift a second time.
func TestReplayedEventKeepsOriginalFactsAndIsNotReprocessed(t *testing.T) {
	f := newWorkerFixture(t, "MONTHLY")
	checkout := f.checkout(t)
	ctx := context.Background()
	start := time.Now().UTC().Truncate(time.Second).Add(-2 * 24 * time.Hour)
	original := invoice(checkout.CheckoutID, "in_replay_"+f.donorID.String(), start, true)
	event := f.receive(t, "invoice.payment_succeeded", original)
	f.process(t)
	var data string
	var processed time.Time
	read := func() (string, time.Time, int) {
		var gifts int
		if err := f.db.QueryRow(ctx, "SELECT data::text,processed_at FROM donor_stripe_events WHERE stripe_event_id=$1", event).Scan(&data, &processed); err != nil {
			t.Fatal(err)
		}
		if err := f.db.QueryRow(ctx, "SELECT count(*) FROM donor_gifts WHERE donor_id=$1", f.donorID).Scan(&gifts); err != nil {
			t.Fatal(err)
		}
		return data, processed, gifts
	}
	firstData, firstProcessed, gifts := read()
	if gifts != 1 {
		t.Fatalf("%d gifts after the first delivery", gifts)
	}

	forged := invoice(checkout.CheckoutID, "in_replay_"+f.donorID.String(), start, true)
	forged["amount_paid"] = 9000
	f.deliver(t, event, "invoice.payment_succeeded", forged, time.Now())
	var rows int
	if err := f.db.QueryRow(ctx, "SELECT count(*) FROM donor_stripe_events WHERE stripe_event_id=$1", event).Scan(&rows); err != nil ||
		rows != 1 {
		t.Fatalf("%d rows for one event id: %v", rows, err)
	}
	if worked, err := f.worker.ProcessNext(ctx); err != nil || worked {
		t.Fatalf("a replayed event was queued again: worked=%v err=%v", worked, err)
	}
	data, processed, gifts = read()
	if data != firstData || !processed.Equal(firstProcessed) || gifts != 1 {
		t.Fatalf("replay changed the event or its effects: data %s, processed %v, gifts %d", data, processed, gifts)
	}
}
