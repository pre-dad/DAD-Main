package store

import (
	"context"
	"testing"
	"time"

	"dad/donor-api/internal/recognition"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5/pgtype"
)

func testID(t *testing.T) pgtype.UUID {
	t.Helper()
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustExec(t *testing.T, tx queries.DBTX, sql string, args ...any) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

// cleanup runs sql as the database owner when the test ends.
func cleanup(t *testing.T, sql string, args ...any) {
	t.Helper()
	t.Cleanup(func() { cleanExec(t, sql, args...) })
}

// cleanExec runs a cleanup statement as the database owner and reports a
// failure, so a test never silently leaves rows behind for later tests.
func cleanExec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := database(t).Exec(context.Background(), sql, args...); err != nil {
		t.Errorf("cleanup %q: %v", sql, err)
	}
}

func TestFinancialHistoryProjectionAndDashboard(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	q := queries.New(tx)
	donorID, referrerID := testID(t), testID(t)
	for _, id := range []pgtype.UUID{donorID, referrerID} {
		if err = q.InsertGuest(ctx, queries.InsertGuestParams{ID: id, Email: id.String() + "@example.org"}); err != nil {
			t.Fatal(err)
		}
	}
	jan := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	feb := jan.AddDate(0, 1, 0)
	mar := feb.AddDate(0, 1, 0)
	now := feb.Add(4 * 24 * time.Hour)
	checkout, firstPeriod, secondPeriod, firstGift, secondGift := testID(t), testID(t), testID(t), testID(t), testID(t)
	mustExec(
		t,
		tx,
		`INSERT INTO donor_checkouts (id,donor_id,amount_minor,currency,frequency,referrer_id,referral_kind,referral_expires_at) VALUES ($1,$2,3000,'USD','MONTHLY',$3,'IRL',$4)`,
		checkout,
		donorID,
		referrerID,
		jan.Add(24*time.Hour),
	)
	for _, p := range []struct {
		id         pgtype.UUID
		start, end time.Time
	}{{firstPeriod, jan, feb}, {secondPeriod, feb, mar}} {
		mustExec(
			t,
			tx,
			`INSERT INTO donor_billing_periods (id,donor_id,checkout_id,stripe_invoice_id,stripe_subscription_id,due_at,ends_at,status) VALUES ($1,$2,$3,$4,'sub_test',$5,$6,'PAID')`,
			p.id,
			donorID,
			checkout,
			"in_"+p.id.String(),
			p.start,
			p.end,
		)
	}
	for _, g := range []struct {
		id, period, referrer pgtype.UUID
		paid                 time.Time
	}{{firstGift, firstPeriod, referrerID, jan}, {secondGift, secondPeriod, pgtype.UUID{}, feb}} {
		mustExec(
			t,
			tx,
			`INSERT INTO donor_gifts (id,donor_id,checkout_id,stripe_object_id,amount_minor,currency,frequency,paid_at,period_id,referrer_id,referral_kind) VALUES ($1,$2,$3,$4,3000,'USD','MONTHLY',$5,$6,$7,CASE WHEN $7::uuid IS NULL THEN NULL ELSE 'IRL' END)`,
			g.id,
			donorID,
			checkout,
			"in_"+g.period.String(),
			g.paid,
			g.period,
			g.referrer,
		)
	}
	preparing, err := dashboard(ctx, q, queries.Donor{ID: referrerID}, now)
	if err != nil || preparing.State != "preparing" {
		t.Fatalf("missing projection: %v %v", preparing, err)
	}
	if _, err = projectFacts(ctx, q, referrerID, now); err != nil {
		t.Fatal(err)
	}
	missing, err := dashboard(ctx, q, queries.Donor{ID: referrerID}, now)
	if err != nil || missing.Referrals != nil {
		t.Fatal("missing referred history shown as zero")
	}
	initial, err := projectFacts(ctx, q, donorID, now)
	if err != nil || !initial.CurrentStreak.Valid || initial.CurrentStreak.Int32 != 2 || initial.BestStreak != 2 ||
		initial.NetMinor.Int64 != 6000 ||
		initial.ReferrerID != referrerID ||
		initial.ReferralKind.String != "IRL" ||
		!initial.FoundingAwardedAt.Valid {
		t.Fatalf("wrong initial projection: %+v %v", initial, err)
	}
	view, err := dashboard(ctx, q, queries.Donor{ID: referrerID}, now)
	if err != nil {
		t.Fatal(err)
	}
	referrals := view.Referrals
	if referrals == nil || referrals.IRL != 1 || referrals.Link != 0 || referrals.TotalMinor != 6000 {
		t.Fatalf("wrong attribution: %v", referrals)
	}
	own, err := dashboard(ctx, q, queries.Donor{ID: donorID}, now)
	if err != nil || own.Impact.GivenMinor == nil || *own.Impact.GivenMinor != 6000 || own.Impact.Currency != "USD" ||
		own.Impact.InitiativesFunded != 0 || own.Impact.EventsVolunteered != 0 {
		t.Fatalf("impact totals: %+v %v", own.Impact, err)
	}
	// A full refund breaks the current streak; the attained best and badge survive.
	refund, event := testID(t), testID(t)
	observed := feb.Add(24 * time.Hour)
	mustExec(
		t,
		tx,
		`INSERT INTO donor_refunds (id,gift_id,stripe_refund_id,amount_minor,currency,status,observed_at) VALUES ($1,$2,$3,3000,'USD','SUCCEEDED',$4)`,
		refund,
		secondGift,
		"re_"+refund.String(),
		observed,
	)
	mustExec(
		t,
		tx,
		`INSERT INTO donor_stripe_events (id,stripe_event_id,event_type,occurred_at,data) VALUES ($1,$2,'refund.updated',$3,'{}')`,
		event,
		"evt_"+event.String(),
		observed,
	)
	mustExec(
		t,
		tx,
		`INSERT INTO donor_refund_observations (id,refund_id,source_event_id,status,observed_at) VALUES ($1,$2,$3,'SUCCEEDED',$4)`,
		testID(t),
		refund,
		event,
		observed,
	)
	// Rebuild from facts, not the existing best, proving historical refund replay.
	mustExec(t, tx, `UPDATE donor_metric_projections SET best_streak=0 WHERE donor_id=$1`, donorID)
	refunded, err := projectFacts(ctx, q, donorID, now)
	if err != nil || refunded.NetMinor.Int64 != 3000 || refunded.CurrentStreak.Int32 != 0 || refunded.BestStreak != 2 ||
		!refunded.FoundingAwardedAt.Time.Equal(initial.FoundingAwardedAt.Time) {
		t.Fatalf("wrong refunded projection: %+v %v", refunded, err)
	}
	stale, err := dashboard(ctx, q, queries.Donor{ID: referrerID}, now.Add(maxRefreshInterval))
	if err != nil || stale.Referrals != nil || stale.Streak != nil {
		t.Fatal("stale values exposed")
	}
	// An unobserved renewal keeps the current count through its grace window.
	due, err := projectFacts(ctx, q, donorID, mar)
	if err != nil || !due.CurrentStreak.Valid || due.CurrentStreak.Int32 != 0 || due.AtRisk ||
		!due.NextRefreshAt.Time.Equal(mar.Add(recognition.Grace)) {
		t.Fatalf("unobserved renewal: %+v %v", due, err)
	}
	// Missing renewal history past grace cannot manufacture a zero streak.
	lapsed := mar.Add(14 * 24 * time.Hour)
	late, err := projectFacts(ctx, q, donorID, lapsed)
	if err != nil || late.CurrentStreak.Valid || !late.NextRefreshAt.Time.Equal(lapsed.Add(5*time.Minute)) {
		t.Fatalf("missing history: %+v %v", late, err)
	}
}

func TestProjectionKeepsContinuityAndHistoricalBest(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	q := queries.New(tx)
	donor, checkout, attempt := testID(t), testID(t), testID(t)
	jan := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	feb, mar, apr := jan.AddDate(0, 1, 0), jan.AddDate(0, 2, 0), jan.AddDate(0, 3, 0)
	mustExec(t, tx, "INSERT INTO donors(id,email) VALUES($1,$2)", donor, donor.String()+"@example.org")
	mustExec(t, tx, "INSERT INTO donor_checkouts(id,donor_id,amount_minor,frequency) VALUES($1,$2,3000,'MONTHLY')", checkout, donor)
	mustExec(
		t,
		tx,
		"INSERT INTO donor_stripe_checkouts(id,checkout_id,request_hash,request_params,status) VALUES($1,$2,$3,'{}','COMPLETE')",
		attempt,
		checkout,
		attempt.String(),
	)
	mustExec(
		t,
		tx,
		"INSERT INTO donor_subscriptions(id,attempt_id,donor_id,status,period_end,observed_at) VALUES($1,$2,$3,'active',$4,$5)",
		testID(t),
		attempt,
		donor,
		apr,
		mar,
	)
	var refundedGift pgtype.UUID
	for i, due := range []time.Time{jan, feb, mar} {
		period, gift := testID(t), testID(t)
		mustExec(
			t,
			tx,
			"INSERT INTO donor_billing_periods(id,donor_id,checkout_id,stripe_invoice_id,stripe_subscription_id,due_at,ends_at,status) VALUES($1,$2,$3,$4,'sub_fixture',$5,$6,'PAID')",
			period,
			donor,
			checkout,
			"in_"+period.String(),
			due,
			due.AddDate(0, 1, 0),
		)
		mustExec(
			t,
			tx,
			"INSERT INTO donor_gifts(id,donor_id,checkout_id,stripe_object_id,amount_minor,currency,frequency,paid_at,period_id) VALUES($1,$2,$3,$4,3000,'USD','MONTHLY',$5,$6)",
			gift,
			donor,
			checkout,
			"in_"+period.String(),
			due,
			period,
		)
		if i == 1 {
			refundedGift = gift
		}
	}
	observed := apr.Add(-time.Hour)
	refund := testID(t)
	mustExec(
		t,
		tx,
		"INSERT INTO donor_refunds(id,gift_id,stripe_refund_id,amount_minor,currency,status,observed_at) VALUES($1,$2,$3,3000,'USD','SUCCEEDED',$4)",
		refund,
		refundedGift,
		"re_"+refund.String(),
		observed,
	)
	got, err := projectFacts(ctx, q, donor, observed)
	if err != nil || !got.CurrentStreak.Valid || got.CurrentStreak.Int32 != 1 || got.BestStreak != 3 {
		t.Fatal("projection lost the attained best after a mid-streak refund", got, err)
	}
	mustExec(t, tx, "UPDATE donor_subscriptions SET stops_at=$2 WHERE donor_id=$1", donor, apr)
	got, err = projectFacts(ctx, q, donor, apr)
	if err != nil || !got.CurrentStreak.Valid || got.CurrentStreak.Int32 != 0 || got.BestStreak != 3 {
		t.Fatal("later cancellation changed the historical best", got, err)
	}
}

func TestRecordGiftEventsBatchesRecentGiftsAndIsIdempotent(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	id, _ := donor(t, pool)
	accountCleanup(t, pool, id)
	t.Cleanup(func() { cleanExec(t, "DELETE FROM analytics_events WHERE donor_id=$1", id) })
	now := time.Now().UTC().Truncate(time.Second)
	gifts := []recognition.Gift{
		{ID: "recent-" + id.String(), PaidAt: now.AddDate(0, -1, 0)},
		{ID: "older-" + id.String(), PaidAt: now.AddDate(0, -12, 0)},
		{ID: "ancient-" + id.String(), PaidAt: now.AddDate(0, -14, 0)},
		{ID: "future-" + id.String(), PaidAt: now.Add(time.Hour)},
	}
	q := queries.New(pool)
	for range 2 {
		if err := recordGiftEvents(ctx, q, id, gifts, now); err != nil {
			t.Fatal(err)
		}
	}
	var stored int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM analytics_events WHERE donor_id=$1 AND name='gift_completed' AND source='SERVER'", id).Scan(&stored); err != nil || stored != 2 {
		t.Fatalf("%d gift events stored after two refreshes, want the 2 inside the window: %v", stored, err)
	}
	if err := recordGiftEvents(ctx, q, id, nil, now); err != nil {
		t.Fatal(err)
	}
}
