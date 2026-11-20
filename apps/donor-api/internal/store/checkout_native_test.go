package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/payments"
)

func nativeCheckoutParams(t *testing.T, gateway *checkoutGateway, call int) map[string]any {
	t.Helper()
	var params map[string]any
	if err := json.Unmarshal(gateway.calls[call].params, &params); err != nil {
		t.Fatal(err)
	}
	return params
}

func TestNativeCheckoutOpensTheSignedInDonorsMonthlyGift(t *testing.T) {
	service, request, id, gateway := checkoutFixture(t)
	ctx := context.Background()
	token := claimSession(t, database(t), id, app).Access

	result, err := service.BeginNativeCheckout(ctx, token, request.RequestID, 6000, nil)
	if err != nil {
		t.Fatal(err)
	}
	params := nativeCheckoutParams(t, gateway, 0)
	line := params["line_items"].([]any)[0].(map[string]any)["price_data"].(map[string]any)
	if params["mode"] != "subscription" || params["customer_email"] != request.Email || line["unit_amount"] != float64(6000) ||
		params["success_url"] != "http://localhost:3000/app/return?result=success&session_id={CHECKOUT_SESSION_ID}" ||
		params["cancel_url"] != "http://localhost:3000/app/return?result=cancel" {
		t.Fatalf("wrong native parameters: %v", params)
	}
	if want := time.Unix(int64(params["expires_at"].(float64)), 0).UTC(); !result.ExpiresAt.Equal(want) {
		t.Fatalf("expires %v, want %v", result.ExpiresAt, want)
	}
	if result.SessionID != "cs_test_dad-checkout-"+request.RequestID.String() ||
		result.URL != "https://checkout.stripe.com/c/dad-checkout-"+request.RequestID.String() {
		t.Fatalf("unexpected session %+v", result)
	}

	repeated, err := service.BeginNativeCheckout(ctx, token, request.RequestID, 6000, nil)
	if err != nil || repeated != result || len(gateway.calls) != 1 {
		t.Fatalf("a retry did not reuse the session: %+v %v with %d processor calls", repeated, err, len(gateway.calls))
	}
	if _, err = service.BeginNativeCheckout(ctx, token, request.RequestID, 9000, nil); !errors.Is(err, payments.ErrCheckoutConflict) {
		t.Fatalf("a changed amount reused the key: %v", err)
	}
	other, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	again, err := service.BeginNativeCheckout(ctx, token, other, 6000, nil)
	if err != nil || again != result || len(gateway.calls) != 1 || len(gateway.expired) != 0 {
		t.Fatalf("a new key did not return the open session: %+v %v", again, err)
	}
}

func reservationHolder(t *testing.T, f *workerFixture) (holder string, count int) {
	t.Helper()
	rows, err := f.db.Query(context.Background(), "SELECT attempt_id::text FROM donor_recurring_reservations WHERE donor_id=$1", f.donorID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		count++
		if err = rows.Scan(&holder); err != nil {
			t.Fatal(err)
		}
	}
	return holder, count
}

func nativeRestartFixture(t *testing.T) (*workerFixture, *checkoutGateway, string, payments.NativeCheckoutResult) {
	t.Helper()
	f := newWorkerFixture(t, "MONTHLY")
	token := claimSession(t, f.db, f.donorID, app).Access
	first, err := f.service.BeginNativeCheckout(context.Background(), token, f.request.RequestID, 3000, nil)
	if err != nil {
		t.Fatal(err)
	}
	return f, f.service.Gateway.(*checkoutGateway), token, first
}

func TestNativeCheckoutAfterAbandonedPageReturnsTheOpenSession(t *testing.T) {
	f, gateway, token, first := nativeRestartFixture(t)
	for range 2 {
		id, err := NewID()
		if err != nil {
			t.Fatal(err)
		}
		again, err := f.service.BeginNativeCheckout(context.Background(), token, id, 3000, nil)
		if err != nil || again != first {
			t.Fatalf("got %+v %v, want %+v", again, err, first)
		}
	}
	if holder, n := reservationHolder(t, f); n != 1 || holder != f.request.RequestID.String() || len(gateway.calls) != 1 ||
		len(gateway.expired) != 0 {
		t.Fatalf("reservation %q x%d, %d creations, %d expiries", holder, n, len(gateway.calls), len(gateway.expired))
	}
}

func TestNativeCheckoutWithANewAmountExpiresTheAbandonedSession(t *testing.T) {
	f, gateway, token, first := nativeRestartFixture(t)
	ctx := context.Background()
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.service.BeginNativeCheckout(ctx, token, id, 6000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.SessionID == first.SessionID || len(gateway.expired) != 1 || gateway.expired[0] != first.SessionID ||
		len(gateway.calls) != 2 {
		t.Fatalf("old session not replaced: %+v then %+v, expired %v", first, second, gateway.expired)
	}
	line := nativeCheckoutParams(t, gateway, 1)["line_items"].([]any)[0].(map[string]any)["price_data"].(map[string]any)
	if line["unit_amount"] != float64(6000) {
		t.Fatalf("new session charges %v", line["unit_amount"])
	}
	if holder, n := reservationHolder(t, f); n != 1 || holder != id.String() {
		t.Fatalf("reservation %q x%d, want only %s", holder, n, id)
	}
	var status string
	if err = f.db.QueryRow(ctx, "SELECT status FROM donor_stripe_checkouts WHERE id=$1", f.request.RequestID).Scan(&status); err != nil ||
		status != "EXPIRED" {
		t.Fatalf("old attempt is %q, want EXPIRED (%v)", status, err)
	}
	if repeated, err := f.service.BeginNativeCheckout(ctx, token, id, 6000, nil); err != nil || repeated != second {
		t.Fatalf("retry of the replacement: %+v %v", repeated, err)
	}
	if _, err = f.service.BeginNativeCheckout(ctx, token, f.request.RequestID, 3000, nil); !errors.Is(err, payments.ErrCheckoutConflict) {
		t.Fatalf("the expired attempt's key reopened it: %v", err)
	}

	// The processor's expiry notice for the replaced session must not release the new reservation.
	expired := f.receive(t, "checkout.session.expired", map[string]any{
		"id": first.SessionID, "status": "expired",
		"metadata": map[string]any{"dad_checkout_id": mustCheckoutOf(t, f, f.request.RequestID.String())},
	})
	f.process(t)
	assertEventProcessed(t, f, expired, true)
	if holder, n := reservationHolder(t, f); n != 1 || holder != id.String() {
		t.Fatalf("the late expiry changed the reservation to %q x%d", holder, n)
	}
}

func mustCheckoutOf(t *testing.T, f *workerFixture, attempt string) string {
	t.Helper()
	var checkout string
	if err := f.db.QueryRow(context.Background(), "SELECT checkout_id::text FROM donor_stripe_checkouts WHERE id=$1", attempt).Scan(&checkout); err != nil {
		t.Fatal(err)
	}
	return checkout
}

func TestNativeCheckoutPaidWhileRestartingIsAnActiveGift(t *testing.T) {
	for name, arrange := range map[string]func(*testing.T, *workerFixture, *checkoutGateway){
		"processor reports the session complete": func(_ *testing.T, _ *workerFixture, g *checkoutGateway) { g.expireStatus = "complete" },
		"webhook completes it as the expiry runs": func(t *testing.T, f *workerFixture, g *checkoutGateway) {
			g.beforeExpire = func() {
				mustExec(t, f.db, "UPDATE donor_stripe_checkouts SET status='COMPLETE' WHERE id=$1", f.request.RequestID)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, gateway, token, _ := nativeRestartFixture(t)
			arrange(t, f, gateway)
			id, err := NewID()
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.service.BeginNativeCheckout(context.Background(), token, id, 6000, nil); !errors.Is(
				err,
				payments.ErrCheckoutConflict,
			) {
				t.Fatalf("got %v, want a conflict", err)
			}
			if holder, n := reservationHolder(t, f); n != 1 || holder != f.request.RequestID.String() || len(gateway.calls) != 1 {
				t.Fatalf("reservation %q x%d, %d creations", holder, n, len(gateway.calls))
			}
		})
	}
}

func TestNativeCheckoutKeepsTheReservationWhenExpiryFails(t *testing.T) {
	f, gateway, token, first := nativeRestartFixture(t)
	ctx := context.Background()
	gateway.expireErr = payments.ErrProcessorUnavailable
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.BeginNativeCheckout(ctx, token, id, 6000, nil); !errors.Is(err, payments.ErrProcessorUnavailable) {
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
	if second, err := f.service.BeginNativeCheckout(ctx, token, id, 6000, nil); err != nil || second.SessionID == first.SessionID {
		t.Fatalf("retry after the processor recovered: %+v %v", second, err)
	}
}

func TestNativeCheckoutRestartsRaceToOneOpenSession(t *testing.T) {
	f, gateway, token, _ := nativeRestartFixture(t)
	ctx := context.Background()
	start := make(chan struct{})
	type outcome struct {
		result payments.NativeCheckoutResult
		err    error
	}
	results := make(chan outcome, 4)
	for range 4 {
		id, err := NewID()
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			<-start
			result, err := f.service.BeginNativeCheckout(ctx, token, id, 6000, nil)
			results <- outcome{result, err}
		}()
	}
	close(start)
	// A request that meets the replacement before its session exists is told
	// the gift is in progress; every other one gets the single new session.
	var winner payments.NativeCheckoutResult
	for range 4 {
		got := <-results
		switch {
		case errors.Is(got.err, payments.ErrCheckoutConflict):
		case got.err != nil:
			t.Fatal(got.err)
		case winner.SessionID == "":
			winner = got.result
		case got.result != winner:
			t.Fatalf("two sessions handed out: %+v and %+v", winner, got.result)
		}
	}
	if winner.SessionID == "" {
		t.Fatal("no request got the replacement session")
	}
	if _, n := reservationHolder(t, f); n != 1 {
		t.Fatalf("%d reservations", n)
	}
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	if len(gateway.calls) != 2 {
		t.Fatalf("%d sessions created, want the original and one replacement", len(gateway.calls))
	}
}

func TestNativeCheckoutRefusesAnInvalidAmountOrSession(t *testing.T) {
	service, request, id, gateway := checkoutFixture(t)
	ctx := context.Background()
	token := claimSession(t, database(t), id, app).Access
	for _, amount := range []int64{0, 2999, 4500, 93000} {
		if _, err := service.BeginNativeCheckout(ctx, token, request.RequestID, amount, nil); !errors.Is(err, payments.ErrCheckoutInput) {
			t.Errorf("amount %d: %v", amount, err)
		}
	}
	if _, err := service.BeginNativeCheckout(ctx, strings.Repeat("x", 43), request.RequestID, 3000, nil); !errors.Is(err, ErrDenied) {
		t.Errorf("an unknown session was admitted: %v", err)
	}
	unconfigured := service
	unconfigured.Gateway = nil
	if _, err := unconfigured.BeginNativeCheckout(ctx, token, request.RequestID, 3000, nil); !errors.Is(err, payments.ErrUnconfigured) {
		t.Errorf("no processor: %v", err)
	}
	if len(gateway.calls) != 0 {
		t.Fatalf("a refused request reached the processor %d times", len(gateway.calls))
	}
}

func TestNativeCheckoutReturnsToItsOwnOrigin(t *testing.T) {
	service, request, id, gateway := checkoutFixture(t)
	token := claimSession(t, database(t), id, app).Access
	service.NativeReturnOrigin = "https://app.example.org"
	if _, err := service.BeginNativeCheckout(context.Background(), token, request.RequestID, 3000, nil); err != nil {
		t.Fatal(err)
	}
	params := nativeCheckoutParams(t, gateway, 0)
	if params["success_url"] != "https://app.example.org/app/return?result=success&session_id={CHECKOUT_SESSION_ID}" ||
		params["cancel_url"] != "https://app.example.org/app/return?result=cancel" {
		t.Fatalf("returned to %v and %v", params["success_url"], params["cancel_url"])
	}
}

// A checkout the app starts completes through the same verified webhooks as one
// the website starts: the donor ends with an active monthly gift.
func TestNativeCheckoutCompletesThroughTheWebhook(t *testing.T) {
	f := newWorkerFixture(t, "MONTHLY")
	ctx := context.Background()
	token := claimSession(t, f.db, f.donorID, app).Access
	started, err := f.service.BeginNativeCheckout(ctx, token, f.request.RequestID, 3000, nil)
	if err != nil {
		t.Fatal(err)
	}
	recurring := RecurringService{DB: as(t, dbroles.API), Origin: f.service.Origin}
	before, err := recurring.Details(ctx, token)
	if err != nil || before.Subscription != nil {
		t.Fatalf("a gift was active before payment: %+v %v", before, err)
	}
	var checkoutID string
	if err = f.db.QueryRow(ctx, "SELECT checkout_id::text FROM donor_stripe_checkouts WHERE id=$1", f.request.RequestID).Scan(&checkoutID); err != nil {
		t.Fatal(err)
	}
	subID := "sub_" + checkoutID
	start := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	f.gateway.subscriptions[subID] = paymentObject(t, subscription(checkoutID, start))
	event := f.receive(t, "checkout.session.completed", map[string]any{
		"id":           started.SessionID,
		"status":       "complete",
		"subscription": subID,
		"metadata":     map[string]any{"dad_checkout_id": checkoutID},
	})
	f.process(t)
	assertEventProcessed(t, f, event, true)

	after, err := recurring.Details(ctx, token)
	if err != nil || after.Subscription == nil || after.Subscription.Status != "active" || after.Subscription.AmountMinor != 3000 {
		t.Fatalf("the webhook did not activate the gift: %+v %v", after, err)
	}
	if _, err = f.service.BeginNativeCheckout(ctx, token, f.request.RequestID, 3000, nil); !errors.Is(err, payments.ErrCheckoutConflict) {
		t.Fatalf("a retry of the completed checkout reopened it: %v", err)
	}
	other, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.BeginNativeCheckout(ctx, token, other, 3000, nil); !errors.Is(err, payments.ErrCheckoutConflict) {
		t.Fatalf("a donor with an active gift started another: %v", err)
	}
}

func TestNativeCheckoutCreditsTheAppsReferral(t *testing.T) {
	service, request, id, gateway := checkoutFixture(t)
	pool := database(t)
	ctx := context.Background()
	referrer, _ := donor(t, pool)
	cleanReferralCheckout(t, pool, id, referrer)
	visitor := "native-visitor-" + testID(t).String()
	if _, err := pool.Exec(
		ctx,
		`INSERT INTO donor_referral_touches (id,token_hash,referrer_id,kind,touched_at,expires_at) VALUES ($1,$2,$3,'LINK',$4,$5)`,
		testID(t), HashToken(visitor), referrer, time.Now(), time.Now().Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	token := claimSession(t, pool, id, app).Access
	referrerOf := func(call int) any {
		params := nativeCheckoutParams(t, gateway, call)
		return params["subscription_data"].(map[string]any)["metadata"].(map[string]any)["dad_referrer_id"]
	}

	plain, err := service.BeginNativeCheckout(ctx, token, request.RequestID, 3000, nil)
	if err != nil || referrerOf(0) != nil {
		t.Fatalf("checkout without a referral: %v credited %v", err, referrerOf(0))
	}
	referred, err := service.BeginNativeCheckout(ctx, token, testID(t), 3000, &visitor)
	if err != nil || referred.SessionID == plain.SessionID || len(gateway.expired) != 1 || referrerOf(1) != referrer.String() {
		t.Fatalf(
			"a referred start reused the uncredited session or lost the referrer: %+v %v, expired %d",
			referred,
			err,
			len(gateway.expired),
		)
	}
	again, err := service.BeginNativeCheckout(ctx, token, testID(t), 3000, &visitor)
	if err != nil || again != referred || len(gateway.calls) != 2 {
		t.Fatalf("the same referral did not return the open session: %+v %v", again, err)
	}
}
