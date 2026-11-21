package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
)

type countedQueries struct {
	queries.DBTX
	reads int
}

func (db *countedQueries) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	db.reads++
	return db.DBTX.Query(ctx, sql, args...)
}

func (db *countedQueries) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	db.reads++
	return db.DBTX.QueryRow(ctx, sql, args...)
}

func TestAnnualSummaryReadsGiftsAndRefundsInTwoQueries(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	owner, checkout := testID(t), testID(t)
	mustExec(t, tx, "INSERT INTO donors(id,email) VALUES($1,$2)", owner, owner.String()+"@example.org")
	mustExec(
		t,
		tx,
		"INSERT INTO donor_checkouts(id,donor_id,amount_minor,currency,frequency) VALUES($1,$2,3000,'USD','MONTHLY')",
		checkout,
		owner,
	)
	paid := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for i := range 100 {
		gift, refund := testID(t), testID(t)
		mustExec(
			t,
			tx,
			"INSERT INTO donor_gifts(id,donor_id,checkout_id,stripe_object_id,amount_minor,currency,frequency,paid_at) VALUES($1,$2,$3,$4,3000,'USD','MONTHLY',$5)",
			gift,
			owner,
			checkout,
			"pi_"+gift.String(),
			paid.Add(time.Duration(i)*time.Minute),
		)
		mustExec(
			t,
			tx,
			"INSERT INTO donor_refunds(id,gift_id,stripe_refund_id,amount_minor,currency,status,observed_at) VALUES($1,$2,$3,500,'USD','SUCCEEDED',$4)",
			refund,
			gift,
			"re_"+refund.String(),
			paid.Add(time.Hour),
		)
	}
	actAs(t, tx, dbroles.MessageWorker)
	db := &countedQueries{DBTX: tx}
	summary, err := readAnnualSummary(ctx, queries.New(db), owner, 2026, paid.AddDate(1, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if db.reads != 2 || len(summary.Gifts) != 100 || summary.Amount != 300000 || summary.Refunded != 50000 || summary.Net != 250000 {
		t.Fatal("incorrect or unbatched annual summary", db.reads, summary)
	}
	for i, gift := range summary.Gifts {
		if len(gift.Refunds) != 1 || gift.Net != 2500 || gift.Refunds[0].ID == "" ||
			gift.PaidAt != iso(paid.Add(time.Duration(i)*time.Minute)) {
			t.Fatal("gift/refund ordering or identity lost", gift)
		}
	}
}

func TestReceiptsScopeNewYorkYearAndLaterRefunds(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	q := queries.New(tx)
	owner, other := testID(t), testID(t)
	for _, id := range []string{owner.String(), other.String()} {
		mustExec(t, tx, "INSERT INTO donors(id,email) VALUES($1,$2)", id, id+"@example.org")
	}
	checkout := testID(t)
	mustExec(
		t,
		tx,
		"INSERT INTO donor_checkouts(id,donor_id,amount_minor,currency,frequency) VALUES($1,$2,3000,'USD','MONTHLY')",
		checkout,
		owner,
	)
	early, late, future := testID(t), testID(t), testID(t)
	boundary := time.Date(2027, 1, 1, 5, 0, 0, 0, time.UTC)
	for i, id := range []string{early.String(), late.String(), future.String()} {
		at := boundary.Add(time.Duration(i-1) * time.Second)
		if i == 2 {
			at = boundary.AddDate(2, 0, 0)
		}
		mustExec(
			t,
			tx,
			"INSERT INTO donor_gifts(id,donor_id,checkout_id,stripe_object_id,amount_minor,currency,frequency,paid_at) VALUES($1,$2,$3,$4,3000,'USD','MONTHLY',$5)",
			id,
			owner,
			checkout,
			"pi_"+id,
			at,
		)
	}
	now := boundary.AddDate(1, 0, 0)
	refund := testID(t)
	mustExec(
		t,
		tx,
		"INSERT INTO donor_refunds(id,gift_id,stripe_refund_id,amount_minor,currency,status,observed_at) VALUES($1,$2,$3,500,'USD','SUCCEEDED',$4)",
		refund,
		early,
		"re_"+refund.String(),
		now,
	)
	summary, err := readAnnualSummary(ctx, q, owner, 2026, now)
	if err != nil || len(summary.Gifts) != 1 || summary.Gifts[0].ID != early.String() || summary.Amount != 3000 ||
		summary.Refunded != 500 ||
		summary.Net != 2500 {
		t.Fatal("annual boundary/refund mismatch", summary, err)
	}
	if summary.TaxReceipt || !strings.Contains(renderSummary(summary), "NOT A TAX RECEIPT") {
		t.Fatal("sandbox represented as legal receipt")
	}
	document, err := readReceipt(ctx, q, owner, early, now)
	if err != nil || document.Gift.Net != 2500 || !strings.Contains(renderReceipt(document), "$25.00 USD") {
		t.Fatal(document, err)
	}
	if _, err = readReceipt(ctx, q, other, early, now); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("other donor's receipt exposed", err)
	}
	if _, err = readReceipt(ctx, q, owner, future, now); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("future gift exposed", err)
	}
	if _, err = readAnnualSummary(ctx, q, owner, 2025, now); !errors.Is(err, ErrReceipt) {
		t.Fatal("invalid year accepted")
	}
	mustExec(t, tx, "UPDATE donor_refunds SET amount_minor=3001 WHERE id=$1", refund)
	if _, err = readReceipt(ctx, q, owner, early, now); !errors.Is(err, ErrReceipt) {
		t.Fatal("over-refunded gift accepted", err)
	}
}

func TestMoneyFormatsMinorUnits(t *testing.T) {
	for want, minor := range map[string]int64{
		"$0.00":         0,
		"$0.05":         5,
		"$0.99":         99,
		"$1.00":         100,
		"$999.99":       99999,
		"$1,000.00":     100000,
		"$12,345.67":    1234567,
		"$1,234,567.89": 123456789,
	} {
		if got := money(minor); got != want {
			t.Errorf("money(%d) = %q, want %q", minor, got, want)
		}
	}
}
