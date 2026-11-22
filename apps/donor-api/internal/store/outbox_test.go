package store

import (
	"context"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestFinancialOutboxDeduplicationYearBoundaryAndRollback(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	q := queries.New(tx)
	id, referrer := testID(t), testID(t)
	for _, donor := range []pgtype.UUID{id, referrer} {
		if err = q.InsertGuest(ctx, queries.InsertGuestParams{ID: donor, Email: donor.String() + "@example.org"}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2028, 1, 1, 0, 30, 0, 0, time.UTC) // Still 2027 in the receipt tax-year zone.
	for _, paid := range []time.Time{time.Date(2027, 1, 1, 0, 30, 0, 0, time.UTC), time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)} {
		draft, gift := testID(t), testID(t)
		mustExec(
			t,
			tx,
			`INSERT INTO donor_checkouts (id,donor_id,amount_minor,currency,frequency) VALUES ($1,$2,3000,'USD','MONTHLY')`,
			draft,
			id,
		)
		mustExec(
			t,
			tx,
			`INSERT INTO donor_gifts (id,donor_id,checkout_id,stripe_object_id,amount_minor,currency,frequency,paid_at) VALUES ($1,$2,$3,$4,3000,'USD','MONTHLY',$5)`,
			gift,
			id,
			draft,
			"pi_"+gift.String(),
			paid,
		)
	}
	draft, period := testID(t), testID(t)
	due := now.Add(-8 * 24 * time.Hour)
	mustExec(
		t,
		tx,
		`INSERT INTO donor_checkouts (id,donor_id,amount_minor,currency,frequency) VALUES ($1,$2,3000,'USD','MONTHLY')`,
		draft,
		id,
	)
	mustExec(
		t,
		tx,
		`INSERT INTO donor_billing_periods (id,donor_id,checkout_id,stripe_invoice_id,stripe_subscription_id,due_at,ends_at,status) VALUES ($1,$2,$3,$4,'sub_test',$5,$6,'FAILED')`,
		period,
		id,
		draft,
		"in_"+period.String(),
		due,
		due.Add(30*24*time.Hour),
	)
	if err = q.SaveProjection(ctx, queries.SaveProjectionParams{ID: testID(t), DonorID: id, ReferrerID: referrer, ReferralKind: pgtype.Text{String: "IRL", Valid: true}, NetMinor: pgtype.Int8{Int64: 6000, Valid: true}, ComputedAt: timestamp(now), NextRefreshAt: timestamp(now.Add(time.Hour))}); err != nil {
		t.Fatal(err)
	}
	actAs(t, tx, dbroles.ProjectionWorker)
	for i := 0; i < 2; i++ {
		if err = q.QueueFinancialIntents(ctx, queries.QueueFinancialIntentsParams{DonorID: id, AsOf: timestamp(now)}); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	// Two RECEIPTs (one per gift) + ANNUAL(2026) + REFERRAL + RECOVERY(day 0)
	// + RECOVERY(day 5) + LAPSED, once each however often the queue runs.
	if err = tx.QueryRow(ctx, "SELECT count(*) FROM donor_messages WHERE donor_id=$1 OR donor_id=$2", id, referrer).Scan(&count); err != nil ||
		count != 7 {
		t.Fatalf("wrong outbox cardinality: %d %v", count, err)
	}
	var year int
	if err = tx.QueryRow(ctx, "SELECT (context->>'year')::integer FROM donor_messages WHERE donor_id=$1 AND kind='ANNUAL'", id).Scan(&year); err != nil ||
		year != 2026 {
		t.Fatal("annual summary crossed New York year boundary")
	}
	var channel string
	if err = tx.QueryRow(ctx, "SELECT context->>'kind' FROM donor_messages WHERE donor_id=$1 AND kind='REFERRAL'", referrer).Scan(&channel); err != nil ||
		channel != "IRL" {
		t.Fatal("referral channel lost")
	}
	rows, err := tx.Query(
		ctx,
		"SELECT (context->>'day')::integer,available_at FROM donor_messages WHERE donor_id=$1 AND kind='RECOVERY'",
		id,
	)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var day int
		var at time.Time
		if err = rows.Scan(&day, &at); err != nil {
			t.Fatal(err)
		}
		if !at.Equal(due.Add(time.Duration(day) * 24 * time.Hour)) {
			t.Fatal("recovery timing changed")
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM donor_messages WHERE donor_id=$1 OR donor_id=$2", id, referrer).Scan(&count); err != nil ||
		count != 0 {
		t.Fatal("rolled back financial messages persisted")
	}
}

func TestFinancialOutboxBatchesLongHistoryAndExcludesFutureGifts(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	id, checkout := testID(t), testID(t)
	mustExec(t, tx, "INSERT INTO donors(id,email) VALUES($1,$2)", id, id.String()+"@example.org")
	mustExec(
		t,
		tx,
		"INSERT INTO donor_checkouts(id,donor_id,amount_minor,currency,frequency) VALUES($1,$2,3000,'USD','MONTHLY')",
		checkout,
		id,
	)
	now := time.Date(2027, 6, 1, 12, 0, 0, 0, time.UTC)
	mustExec(t, tx, `INSERT INTO donor_gifts(id,donor_id,checkout_id,stripe_object_id,amount_minor,currency,frequency,paid_at)
 SELECT gen_random_uuid(),$1,$2,'pi_'||($1::uuid)::text||'_'||n,3000,'USD','MONTHLY',$3::timestamptz+n*interval '1 minute' FROM generate_series(-999,1) n`, id, checkout, now)
	actAs(t, tx, dbroles.ProjectionWorker)
	db := &countedOutbox{DBTX: tx}
	for i := range 2 {
		if err = queries.New(db).QueueFinancialIntents(ctx, queries.QueueFinancialIntentsParams{DonorID: id, AsOf: timestamp(now)}); err != nil {
			t.Fatal(err)
		}
		var count int
		if err = tx.QueryRow(ctx, "SELECT count(*) FROM donor_messages WHERE donor_id=$1", id).Scan(&count); err != nil || count != 1000 {
			t.Fatal(count, err)
		}
		if db.calls != i+1 {
			t.Fatal("queue cost grows with gift history", db.calls)
		}
	}
}

type countedOutbox struct {
	queries.DBTX
	calls int
}

func (db *countedOutbox) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	db.calls++
	return db.DBTX.Exec(ctx, sql, args...)
}

func (db *countedOutbox) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	db.calls++
	return db.DBTX.Query(ctx, sql, args...)
}

func (db *countedOutbox) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	db.calls++
	return db.DBTX.QueryRow(ctx, sql, args...)
}
