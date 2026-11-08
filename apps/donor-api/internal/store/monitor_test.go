package store

import (
	"context"
	"strconv"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/store/queries"
)

func TestQueueHealthCountsParkedAndIgnoresFutureAndCompleted(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// An isolated historical cutoff excludes other tests' committed queue rows.
	now := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	id := testID(t)
	mustExec(t, tx, "INSERT INTO donors(id,email,created_at) VALUES($1,$2,$3)", id, id.String()+"@example.org", now)
	mustExec(t, tx, "INSERT INTO donor_metric_projections(id,donor_id,computed_at,next_refresh_at) VALUES($1,$2,$3,$3)", testID(t), id, now)
	for i, delta := range []time.Duration{-time.Hour, -time.Minute, time.Hour, -time.Hour} {
		job := testID(t)
		mustExec(t, tx, `INSERT INTO donor_messages(id,donor_id,kind,dedup_key,context,available_at,attempts,completed_at)
  VALUES($1,$2,'RECEIPT',$3,'{}',$4,$5,CASE WHEN $6 THEN $4::timestamptz ELSE NULL END)`, job, id, job.String(), now.Add(delta), i*5, i == 3)
	}
	for _, push := range []struct {
		due, expires time.Duration
		attempts     int
	}{{-2 * time.Minute, time.Hour, 0}, {-time.Minute, time.Hour, 5}, {time.Hour, 2 * time.Hour, 0}, {-time.Hour, -time.Second, 5}} {
		job := testID(t)
		mustExec(t, tx, `INSERT INTO donor_push_notifications(id,donor_id,category,dedup_key,context,available_at,expires_at,attempts)
  VALUES($1,$2,'STREAK_MILESTONE',$3,'{}',$4,$5,$6)`, job, id, job.String(), now.Add(push.due), now.Add(push.expires), push.attempts)
	}
	for i, reversal := range []struct {
		due      time.Duration
		attempts int
		done     bool
	}{{-30 * time.Minute, 10, false}, {-time.Minute, 2, false}, {time.Hour, 0, false}, {-time.Hour, 1, true}} {
		mustExec(t, tx, `INSERT INTO donor_deleted_account_reversals(id,donor_id,kind,target,available_at,attempts,completed_at,created_at)
  VALUES($1,$2,'CANCEL',$3,$4,$5,CASE WHEN $6 THEN $4::timestamptz ELSE NULL END,$4)`,
			testID(t), id, "sub_monitor_"+strconv.Itoa(i), now.Add(reversal.due), reversal.attempts, reversal.done)
	}
	day := 24 * time.Hour
	for _, sub := range []struct {
		status            string
		periodEnd, synced time.Duration
	}{
		{"active", -2 * day, -3 * day},      // missed the sync due a day after renewal
		{"past_due", -12 * time.Hour, -day}, // renewed less than a day ago
		{"active", -2 * day, -day},          // synced after its period end
		{"canceled", -2 * day, -3 * day},    // ended
	} {
		checkout, attempt := testID(t), testID(t)
		mustExec(t, tx, "INSERT INTO donor_checkouts(id,donor_id,amount_minor,frequency) VALUES($1,$2,3000,'MONTHLY')", checkout, id)
		mustExec(
			t,
			tx,
			"INSERT INTO donor_stripe_checkouts(id,checkout_id,request_hash,request_params,stripe_subscription_id,status) VALUES($1,$2,'hash','{}',$3,'COMPLETE')",
			attempt,
			checkout,
			"sub_"+attempt.String(),
		)
		mustExec(
			t,
			tx,
			"INSERT INTO donor_subscriptions(id,attempt_id,donor_id,status,period_end,observed_at) VALUES($1,$2,$3,$4,$5,$6)",
			testID(t),
			attempt,
			id,
			sub.status,
			now.Add(sub.periodEnd),
			now.Add(sub.synced),
		)
	}
	actAs(t, tx, dbroles.Monitor)
	rows, err := queries.New(tx).QueueHealth(ctx, timestamp(now))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 8 {
		t.Fatal(rows)
	}
	for queue, want := range map[string]struct{ due, parked, oldest int64 }{
		"messages":      {2, 1, 3600},
		"push":          {2, 1, 120},
		"subscriptions": {1, 0, 86400},
		"reversals":     {2, 1, 1800},
	} {
		row := queueHealthRow(t, rows, queue)
		if row.Due != want.due || row.Parked != want.parked || row.OldestSeconds != want.oldest {
			t.Fatalf("%s: %+v, want %+v", queue, row, want)
		}
	}
	if row := queueHealthRow(t, rows, "projections"); row.Due != 1 {
		t.Fatal(row)
	}
}

func queueHealthRow(t *testing.T, rows []queries.QueueHealthRow, queue string) queries.QueueHealthRow {
	t.Helper()
	for _, row := range rows {
		if row.Queue == queue {
			return row
		}
	}
	t.Fatalf("queue health reports no %q queue: %+v", queue, rows)
	return queries.QueueHealthRow{}
}
