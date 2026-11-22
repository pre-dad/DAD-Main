package store

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestReferrerRefreshCannotOverwriteConcurrentContribution(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing_projection=%t", existing), func(t *testing.T) {
			pool := database(t)
			worker := as(t, dbroles.ProjectionWorker)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			parent, _ := donor(t, pool)
			child, _ := donor(t, pool)
			now := time.Now().UTC()
			checkout := testID(t)
			t.Cleanup(func() {
				for _, sql := range []string{
					"DELETE FROM donor_messages WHERE donor_id=ANY($1)",
					"DELETE FROM donor_metric_projections WHERE donor_id=ANY($1)",
					"DELETE FROM donor_gifts WHERE donor_id=ANY($1)",
					"DELETE FROM donor_checkouts WHERE donor_id=ANY($1)",
				} {
					if _, err := pool.Exec(context.Background(), sql, []pgtype.UUID{parent, child}); err != nil {
						t.Error(err)
					}
				}
			})
			mustExec(
				t,
				pool,
				"INSERT INTO donor_checkouts(id,donor_id,amount_minor,currency,frequency,referrer_id,referral_kind,referral_expires_at) VALUES($1,$2,3000,'USD','MONTHLY',$3,'LINK',$4)",
				checkout,
				child,
				parent,
				now.Add(time.Hour),
			)
			mustExec(
				t,
				pool,
				"INSERT INTO donor_gifts(id,donor_id,checkout_id,stripe_object_id,amount_minor,currency,frequency,paid_at,referrer_id,referral_kind) VALUES($1,$2,$3,$4,3000,'USD','MONTHLY',$5,$6,'LINK')",
				testID(t),
				child,
				checkout,
				"pi_"+checkout.String(),
				now.Add(-time.Hour),
				parent,
			)
			if existing {
				if err := seedProjection(ctx, queries.New(worker), parent, now); err != nil {
					t.Fatal(err)
				}
			}
			referrer, err := worker.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer referrer.Rollback(context.Background())
			read := make(chan struct{})
			resume := make(chan struct{})
			var release sync.Once
			defer release.Do(func() { close(resume) })
			referrerDone := make(chan error, 1)
			go func() {
				q := queries.New(&pausedReferralAggregate{DBTX: referrer, read: read, resume: resume})
				_, err := projectFacts(ctx, q, parent, now)
				referrerDone <- err
			}()
			select {
			case <-read:
			case err := <-referrerDone:
				t.Fatalf("referrer failed before reading totals: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			referred, err := worker.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer referred.Rollback(context.Background())
			childDone := make(chan error, 1)
			go func() {
				_, err := projectFacts(ctx, queries.New(referred), child, now)
				childDone <- err
			}()
			awaitBlocked(ctx, t, pool, referred, childDone)
			release.Do(func() { close(resume) })
			if err := <-referrerDone; err != nil {
				t.Fatal(err)
			}
			if err := referrer.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-childDone; err != nil {
				t.Fatal(err)
			}
			if err := referred.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			p, err := queries.New(pool).Projection(ctx, parent)
			if err != nil || p.ReferredLinkCount != 1 || p.ReferredLinkMinor != 3000 {
				t.Fatalf("concurrent contribution was lost: %+v %v", p, err)
			}
		})
	}
}

type pausedReferralAggregate struct {
	queries.DBTX
	read   chan struct{}
	resume chan struct{}
}

func (db *pausedReferralAggregate) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	row := db.DBTX.QueryRow(ctx, sql, args...)
	if strings.HasPrefix(sql, "-- name: ReferralTotalsFromReferred") {
		return pausedReferralRow{Row: row, read: db.read, resume: db.resume, ctx: ctx}
	}
	return row
}

type pausedReferralRow struct {
	pgx.Row
	read   chan struct{}
	resume chan struct{}
	ctx    context.Context
}

func (row pausedReferralRow) Scan(dest ...any) error {
	err := row.Row.Scan(dest...)
	close(row.read)
	select {
	case <-row.resume:
		return err
	case <-row.ctx.Done():
		return row.ctx.Err()
	}
}

func TestConcurrentFinancialRefreshPreservesLongHistories(t *testing.T) {
	pool := database(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	const donors = 32
	ids := make([]pgtype.UUID, donors)
	for i := range ids {
		ids[i] = testID(t)
	}
	t.Cleanup(func() {
		for _, id := range ids {
			for _, table := range []string{"donor_messages", "donor_metric_projections", "donor_gifts", "donor_billing_periods", "donor_checkouts"} {
				if _, err := pool.Exec(context.Background(), "DELETE FROM "+table+" WHERE donor_id=$1", id); err != nil {
					t.Error(err)
				}
			}
			if _, err := pool.Exec(context.Background(), "DELETE FROM donors WHERE id=$1", id); err != nil {
				t.Error(err)
			}
		}
	})
	now := time.Date(2028, 1, 15, 12, 0, 0, 0, time.UTC)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for _, id := range ids {
		mustExec(t, tx, "INSERT INTO donors(id,email) VALUES($1,$2)", id, id.String()+"@example.org")
		checkout := testID(t)
		mustExec(
			t,
			tx,
			"INSERT INTO donor_checkouts(id,donor_id,amount_minor,currency,frequency) VALUES($1,$2,3000,'USD','MONTHLY')",
			checkout,
			id,
		)
		mustExec(
			t,
			tx,
			`INSERT INTO donor_billing_periods(id,donor_id,checkout_id,stripe_invoice_id,stripe_subscription_id,due_at,ends_at,status)
  SELECT gen_random_uuid(),$1,$2,'in_'||($1::uuid)::text||'_'||n,'sub_'||($1::uuid)::text,
  '2026-02-01 12:00Z'::timestamptz+n*interval '1 month','2026-03-01 12:00Z'::timestamptz+n*interval '1 month','PAID' FROM generate_series(0,23) n`,
			id,
			checkout,
		)
		mustExec(t, tx, `INSERT INTO donor_gifts(id,donor_id,checkout_id,stripe_object_id,amount_minor,currency,frequency,paid_at,period_id)
  SELECT gen_random_uuid(),donor_id,checkout_id,stripe_invoice_id,3000,'USD','MONTHLY',due_at,id FROM donor_billing_periods WHERE donor_id=$1`, id)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	worker := as(t, dbroles.ProjectionWorker)
	start := time.Now()
	var wg sync.WaitGroup
	failures := make(chan error, donors*2)
	// Two independent writers contend for every donor; locking must prevent duplicate intents.
	for round := range 2 {
		for _, id := range ids {
			wg.Add(1)
			go func(id pgtype.UUID, round int) {
				defer wg.Done()
				tx, err := worker.Begin(ctx)
				if err != nil {
					failures <- err
					return
				}
				defer tx.Rollback(ctx)
				facts, err := refreshProjection(ctx, tx, id, now)
				if err == nil &&
					(!facts.CurrentStreak.Valid || facts.CurrentStreak.Int32 != 24 || facts.BestStreak != 24 || facts.NetMinor.Int64 != 72000) {
					err = fmt.Errorf("incorrect financial projection in writer %d", round)
				}
				if err == nil {
					err = tx.Commit(ctx)
				}
				if err != nil {
					failures <- err
				}
			}(id, round)
		}
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	for _, id := range ids {
		var receipts, annual int
		err = pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE kind='RECEIPT'),count(*) FILTER(WHERE kind='ANNUAL') FROM donor_messages WHERE donor_id=$1`, id).
			Scan(&receipts, &annual)
		if err != nil || receipts != 24 || annual != 2 {
			t.Fatal("duplicate or lost notification intents", receipts, annual, err)
		}
		p, err := queries.New(pool).Projection(ctx, id)
		if err != nil || p.BestStreak != 24 {
			t.Fatal(p, err)
		}
	}
	t.Logf("64 contending financial refreshes, 32 donors, 768 gifts: %s", time.Since(start))
}

// TestConcurrentReferralDeltaConverges proves that N referred donors sharing one
// referrer, each refreshed with two contending writers, still leave the
// referrer's four denormalized columns equal to the OLD FILTER aggregate:
// ApplyReferralDelta (col = col + delta) is race-safe under separate donor
// locks, and the referrer's own recompute backstop reconciles any lost deltas.
func TestConcurrentReferralDeltaConverges(t *testing.T) {
	pool := database(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	const siblings = 8
	now := time.Date(2028, 2, 1, 12, 0, 0, 0, time.UTC)
	referrerID := testID(t)
	siblingIDs := make([]pgtype.UUID, siblings)
	for i := range siblingIDs {
		siblingIDs[i] = testID(t)
	}
	allIDs := append([]pgtype.UUID{referrerID}, siblingIDs...)
	t.Cleanup(func() {
		for _, sql := range []string{
			"DELETE FROM donor_messages WHERE donor_id=ANY($1)",
			"DELETE FROM donor_refunds WHERE gift_id IN (SELECT id FROM donor_gifts WHERE donor_id=ANY($1))",
			"DELETE FROM donor_gifts WHERE donor_id=ANY($1)",
			"DELETE FROM donor_checkouts WHERE donor_id=ANY($1)",
			"DELETE FROM donor_metric_projections WHERE donor_id=ANY($1)",
			"DELETE FROM donors WHERE id=ANY($1)",
		} {
			if _, err := pool.Exec(context.Background(), sql, allIDs); err != nil {
				t.Error(err)
			}
		}
	})
	// Seed referrer + referred donors. Each sibling has a gift
	// referred via LINK to referrerID.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	expires := now.Add(24 * time.Hour)
	mustExec(t, tx, "INSERT INTO donors(id,email) VALUES($1,$2)", referrerID, referrerID.String()+"@example.org")
	for i, id := range siblingIDs {
		mustExec(t, tx, "INSERT INTO donors(id,email) VALUES($1,$2)", id, id.String()+"@example.org")
		checkout := testID(t)
		mustExec(
			t,
			tx,
			"INSERT INTO donor_checkouts(id,donor_id,amount_minor,currency,frequency,referrer_id,referral_kind,referral_expires_at) VALUES($1,$2,3000,'USD','MONTHLY',$3,'LINK',$4)",
			checkout,
			id,
			referrerID,
			expires,
		)
		paidAt := now.Add(-time.Duration(siblings-i) * time.Hour)
		mustExec(
			t,
			tx,
			"INSERT INTO donor_gifts(id,donor_id,checkout_id,stripe_object_id,amount_minor,currency,frequency,paid_at,referrer_id,referral_kind) VALUES($1,$2,$3,$4,3000,'USD','MONTHLY',$5,$6,'LINK')",
			testID(t),
			id,
			checkout,
			"pi_"+id.String(),
			paidAt,
			referrerID,
		)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	worker := as(t, dbroles.ProjectionWorker)
	// Project the referrer first so its row exists for the deltas.
	{
		tx, err := worker.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = refreshProjection(ctx, tx, referrerID, now); err != nil {
			tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// Concurrently project all siblings, two contending writers each.
	var wg sync.WaitGroup
	failures := make(chan error, siblings*2)
	for range 2 {
		for _, id := range siblingIDs {
			wg.Add(1)
			go func(id pgtype.UUID) {
				defer wg.Done()
				tx, err := worker.Begin(ctx)
				if err != nil {
					failures <- err
					return
				}
				defer tx.Rollback(ctx)
				_, err = refreshProjection(ctx, tx, id, now)
				if err == nil {
					err = tx.Commit(ctx)
				}
				if err != nil {
					failures <- err
				}
			}(id)
		}
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	// Each sibling is LINK 3000, so aggregate: link_count = siblings, irl_count = 0,
	// link_minor = 3000 * siblings, irl_minor = 0.
	want := [4]int64{siblings, 0, 3000 * siblings, 0}
	stored := func() [4]int64 {
		p, err := queries.New(pool).Projection(ctx, referrerID)
		if err != nil {
			t.Fatal(err)
		}
		return [4]int64{p.ReferredLinkCount, p.ReferredIrlCount, p.ReferredLinkMinor, p.ReferredIrlMinor}
	}
	// The delta path alone must converge; the recompute below would mask a lost delta.
	if got := stored(); got != want {
		t.Fatalf("referral deltas lost before the backstop recompute: got %v want %v", got, want)
	}
	refresh, err := worker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = refreshProjection(ctx, refresh, referrerID, now); err != nil {
		refresh.Rollback(ctx)
		t.Fatal(err)
	}
	if err = refresh.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := stored(); got != want {
		t.Fatalf("referral columns diverged after the backstop recompute: got %v want %v", got, want)
	}
}
