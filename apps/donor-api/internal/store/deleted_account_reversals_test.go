package store

import (
	"context"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/payments"
)

type reversalGateway struct {
	refunds       []string
	refundAmounts []int64
	refundKeys    []string
	cancels       []string
	subscriptions map[string]payments.Object
	cancelErr     error
	refundErr     error
}

func (g *reversalGateway) RetrieveSubscription(_ context.Context, id string) (payments.Object, error) {
	if value, ok := g.subscriptions[id]; ok {
		return value, nil
	}
	return nil, payments.ErrProcessorUnavailable
}

func (g *reversalGateway) RefundAfterDeletion(_ context.Context, payment string, amount int64, reversal string) error {
	if g.refundErr != nil {
		return g.refundErr
	}
	g.refunds = append(g.refunds, payment)
	g.refundAmounts = append(g.refundAmounts, amount)
	g.refundKeys = append(g.refundKeys, reversal)
	return nil
}

func (g *reversalGateway) CancelSubscriptionNow(_ context.Context, id, _ string) error {
	if g.cancelErr != nil {
		return g.cancelErr
	}
	g.cancels = append(g.cancels, id)
	return nil
}

// TestLateCheckoutAfterDeletionIsReversed covers a monthly gift Stripe reports
// after its donor deleted the account: the payment is refunded and the
// subscription ended, while a gift taken before the deletion stands.
func TestLateCheckoutAfterDeletionIsReversed(t *testing.T) {
	f := newWorkerFixture(t, "MONTHLY")
	ctx := context.Background()
	t.Cleanup(func() {
		cleanExec(t, "DELETE FROM donor_deleted_account_reversals WHERE donor_id=$1", f.donorID)
		cleanExec(t, "DELETE FROM donor_deletions WHERE donor_id=$1", f.donorID)
	})
	result := f.checkout(t)
	subID := "sub_" + result.CheckoutID
	start := time.Now().UTC().Truncate(time.Second).Add(-31 * 24 * time.Hour)
	f.gateway.subscriptions[subID] = paymentObject(t, subscription(result.CheckoutID, start))

	before := invoice(result.CheckoutID, "in_before_"+result.CheckoutID, start, true)
	assertEventProcessed(t, f, f.receive(t, "invoice.payment_succeeded", before), false)
	f.process(t)
	assertEventProcessed(t, f, f.events[len(f.events)-1], true)
	reversals := func() map[string]int64 {
		t.Helper()
		rows, err := f.db.Query(
			ctx,
			"SELECT kind||' '||target, coalesce(amount_minor,0) FROM donor_deleted_account_reversals WHERE donor_id=$1",
			f.donorID,
		)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		found := map[string]int64{}
		for rows.Next() {
			var key string
			var amount int64
			if err := rows.Scan(&key, &amount); err != nil {
				t.Fatal(err)
			}
			found[key] = amount
		}
		return found
	}
	if got := reversals(); len(got) != 0 {
		t.Fatalf("a donor with an account had reversals queued: %v", got)
	}

	mustExec(t, f.db, "INSERT INTO donor_deletions (donor_id, deleted_at) VALUES ($1, $2)", f.donorID, start.Add(29*24*time.Hour))
	renewal := invoice(result.CheckoutID, "in_after_"+result.CheckoutID, start.Add(30*24*time.Hour), true)
	for range 2 {
		f.receive(t, "invoice.payment_succeeded", renewal)
		f.process(t)
		assertEventProcessed(t, f, f.events[len(f.events)-1], true)
	}
	got := reversals()
	if len(got) != 2 || got["REFUND pi_in_after_"+result.CheckoutID] != 3000 || got["CANCEL "+subID] != 0 {
		t.Fatalf("reversals = %v; want the later payment refunded and %s cancelled", got, subID)
	}

	gateway := &reversalGateway{cancelErr: payments.ErrProcessorUnavailable}
	worker := DeletedAccountReversals{DB: as(t, dbroles.StripeWorker), Gateway: gateway}
	now := time.Now().UTC()
	for range 2 {
		if worked, err := worker.RunNext(ctx, now); err != nil || !worked {
			t.Fatalf("RunNext = %v, %v", worked, err)
		}
	}
	if worked, err := worker.RunNext(ctx, now); err != nil || worked {
		t.Fatalf("a failed reversal was retried at once: %v %v", worked, err)
	}
	if len(gateway.refunds) != 1 || gateway.refunds[0] != "pi_in_after_"+result.CheckoutID {
		t.Fatalf("refunds sent: %v", gateway.refunds)
	}
	var refundJob string
	if err := f.db.QueryRow(ctx, "SELECT id::text FROM donor_deleted_account_reversals WHERE kind='REFUND' AND donor_id=$1", f.donorID).Scan(&refundJob); err != nil {
		t.Fatal(err)
	}
	if gateway.refundAmounts[0] != 3000 || gateway.refundKeys[0] != refundJob {
		t.Fatalf(
			"refund sent %d under key %q; want 3000 under the reversal id %q",
			gateway.refundAmounts[0],
			gateway.refundKeys[0],
			refundJob,
		)
	}
	var lastError string
	if err := f.db.QueryRow(ctx, "SELECT last_error FROM donor_deleted_account_reversals WHERE kind='CANCEL' AND completed_at IS NULL AND donor_id=$1", f.donorID).Scan(&lastError); err != nil ||
		lastError != "PaymentUnavailable" {
		t.Fatalf("failed cancellation: %q %v; want it kept for a retry", lastError, err)
	}

	ended := subscription(result.CheckoutID, start)
	ended["status"] = "canceled"
	gateway.subscriptions = map[string]payments.Object{subID: paymentObject(t, ended)}
	if worked, err := worker.RunNext(ctx, now.Add(reversalBackoff)); err != nil || !worked {
		t.Fatalf("retry RunNext = %v, %v", worked, err)
	}
	var open int
	if err := f.db.QueryRow(ctx, "SELECT count(*) FROM donor_deleted_account_reversals WHERE completed_at IS NULL AND donor_id=$1", f.donorID).Scan(&open); err != nil ||
		open != 0 {
		t.Fatalf("%d reversals still open, err %v; a subscription Stripe already ended needs no cancellation", open, err)
	}
}
