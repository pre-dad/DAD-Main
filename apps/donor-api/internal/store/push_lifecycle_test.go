package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/push"
	"dad/donor-api/internal/store/queries"
)

func TestLegacyPreferenceSavePreservesGiftPushOptOut(t *testing.T) {
	pool := database(t)
	id, token := signedInDonor(t, pool)
	pushCleanup(t, pool, id)
	ctx := context.Background()
	s := DonorService{DB: as(t, dbroles.API)}
	p, err := s.Profile(ctx, token)
	if err != nil || !p.PushGiftProcessed {
		t.Fatalf("gift push default: %+v %v", p, err)
	}
	p.PushGiftProcessed = false
	p, err = s.SavePreferences(ctx, token, p.Preferences.Replace())
	if err != nil || p.PushGiftProcessed {
		t.Fatalf("gift push opt-out: %+v %v", p, err)
	}
	change := p.Preferences.Replace()
	change.PushGiftProcessed = nil
	p, err = s.SavePreferences(ctx, token, change)
	if err != nil || p.PushGiftProcessed {
		t.Fatalf("legacy client cleared opt-out: %+v %v", p, err)
	}
	var choice bool
	if err := pool.QueryRow(ctx, "SELECT push_gift_processed FROM donor_preference_events WHERE donor_id=$1 ORDER BY version DESC LIMIT 1", id).Scan(&choice); err != nil ||
		choice {
		t.Fatalf("consent choice: %t %v", choice, err)
	}
}

func TestGiftProcessedPushIsPrivateIdempotentAndOptional(t *testing.T) {
	f := newWorkerFixture(t, "MONTHLY")
	pushCleanup(t, f.db, f.donorID)
	checkout := f.checkout(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	appDevice(t, f.db, f.donorID, push.Android, androidToken, now.Add(24*time.Hour))
	f.receive(t, "invoice.payment_succeeded", invoice(checkout.CheckoutID, "in_"+checkout.CheckoutID, now.Add(-2*time.Hour), true))
	f.process(t)
	for range 2 {
		tx, err := as(t, dbroles.ProjectionWorker).Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := refreshProjection(ctx, tx, f.donorID, now); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	intents := intentsOf(t, f.db, f.donorID, "GIFT_PROCESSED")
	if len(intents) != 1 {
		t.Fatalf("gift replay queued %d notices", len(intents))
	}
	note, err := pushContent(intents[0])
	if err != nil || note.Path != "/account/gifts" || note.Title != "Your gift was processed" {
		t.Fatalf("gift notice %+v %v", note, err)
	}
	q := queries.New(as(t, dbroles.PushWorker))
	d, err := queries.New(f.db).LockDonor(ctx, f.donorID)
	if err != nil {
		t.Fatal(err)
	}
	if reason, err := pushSuppression(ctx, q, intents[0], d, now); err != nil || reason != "" {
		t.Fatalf("fresh gift suppressed: %q %v", reason, err)
	}
	mustExec(t, f.db, `INSERT INTO donor_preferences(id,donor_id,version,referral_emails,marketing_emails,push_gift_processed)
		VALUES($1,$2,1,false,false,false) ON CONFLICT(donor_id) DO UPDATE SET push_gift_processed=false`, testID(t), f.donorID)
	if reason, err := pushSuppression(ctx, q, intents[0], d, now); err != nil || reason != "opted_out" {
		t.Fatalf("gift opt-out: %q %v", reason, err)
	}
	mustExec(t, f.db, "UPDATE donor_preferences SET push_gift_processed=true WHERE donor_id=$1", f.donorID)
	mustExec(t, f.db, `INSERT INTO donor_refunds(id,gift_id,stripe_refund_id,amount_minor,currency,status,observed_at)
		SELECT $1,id,$2,amount_minor,'USD','SUCCEEDED',$3 FROM donor_gifts WHERE donor_id=$4`, testID(t), "re_"+checkout.CheckoutID, now, f.donorID)
	if reason, err := pushSuppression(ctx, q, intents[0], d, now); err != nil || reason != "gift_refunded" {
		t.Fatalf("refunded gift: %q %v", reason, err)
	}
}

func TestDayFivePushWaitsAndStopsAfterRecovery(t *testing.T) {
	f := newWorkerFixture(t, "MONTHLY")
	pushCleanup(t, f.db, f.donorID)
	checkout := f.checkout(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	appDevice(t, f.db, f.donorID, push.Android, androidToken, now.Add(8*24*time.Hour))
	for i := range 3 {
		f.receive(
			t,
			"invoice.payment_succeeded",
			invoice(checkout.CheckoutID, "in_"+checkout.CheckoutID+string(rune('a'+i)), now.Add(time.Duration(i-3)*30*24*time.Hour), true),
		)
		f.process(t)
	}
	failedID := "in_" + checkout.CheckoutID + "_failed"
	f.receive(t, "invoice.payment_failed", invoice(checkout.CheckoutID, failedID, now, false))
	f.process(t)
	var due, expires time.Time
	if err := f.db.QueryRow(ctx, `SELECT available_at,expires_at FROM donor_push_notifications
		WHERE donor_id=$1 AND category='STREAK_AT_RISK' AND context->>'day'='5'`, f.donorID).Scan(&due, &expires); err != nil {
		t.Fatal(err)
	}
	if !due.Equal(now.Add(5*24*time.Hour)) || !expires.Equal(due.Add(pushLifetime)) {
		t.Fatalf("day-five schedule %s .. %s", due, expires)
	}
	q := queries.New(as(t, dbroles.PushWorker))
	d, err := queries.New(f.db).LockDonor(ctx, f.donorID)
	if err != nil {
		t.Fatal(err)
	}
	var reminder queries.DonorPushNotification
	for _, intent := range intentsOf(t, f.db, f.donorID, "STREAK_AT_RISK") {
		if strings.HasSuffix(intent.DedupKey, ":5") {
			reminder = intent
		}
	}
	if !reminder.ID.Valid {
		t.Fatal("no day-five reminder was queued")
	}
	if reason, err := pushSuppression(ctx, q, reminder, d, due); err != nil || reason != "" {
		t.Fatalf("day-five reminder suppressed: %q %v", reason, err)
	}
	f.receive(t, "invoice.payment_succeeded", invoice(checkout.CheckoutID, failedID, now, true))
	f.process(t)
	if reason, err := pushSuppression(ctx, q, reminder, d, due); err != nil || reason != "payment_recovered" {
		t.Fatalf("recovered reminder: %q %v", reason, err)
	}
}
