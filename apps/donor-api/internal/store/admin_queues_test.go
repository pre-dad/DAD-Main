package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"dad/donor-api/internal/dbroles"
)

// failedPeriod gives a new donor one billing period of the given status that
// came due the given time ago, with an optional recorded failure amount.
func failedPeriod(t *testing.T, pool *pgxpool.Pool, status string, ago time.Duration, amount int64) (donorID, periodID pgtype.UUID) {
	t.Helper()
	ctx := context.Background()
	donorID, _ = donor(t, pool)
	checkout, period := testID(t), testID(t)
	due := time.Now().UTC().Add(-ago).Truncate(time.Microsecond)
	mustExec(
		t,
		pool,
		`INSERT INTO donor_checkouts (id,donor_id,amount_minor,currency,frequency) VALUES ($1,$2,3000,'USD','MONTHLY')`,
		checkout,
		donorID,
	)
	mustExec(
		t,
		pool,
		`INSERT INTO donor_billing_periods (id,donor_id,checkout_id,stripe_invoice_id,stripe_subscription_id,due_at,ends_at,status)
VALUES ($1,$2,$3,$4,'sub_queue',$5,$6,$7)`,
		period, donorID, checkout, "in_"+period.String(), due, due.Add(30*24*time.Hour), status,
	)
	event := testID(t)
	if amount > 0 {
		mustExec(
			t,
			pool,
			`INSERT INTO donor_stripe_events (id,stripe_event_id,event_type,occurred_at,data) VALUES ($1,$2,'invoice.payment_failed',$3,'{}')`,
			event,
			"evt_"+event.String(),
			due,
		)
		mustExec(
			t,
			pool,
			`INSERT INTO donor_billing_observations (id,period_id,source_event_id,status,amount_minor,currency,billing_reason,occurred_at,event_created_at,recorded_at)
VALUES ($1,$2,$3,'FAILED',$4,'USD','subscription_cycle',$5,$5,$5)`,
			testID(t),
			period,
			event,
			amount,
			due,
		)
	}
	t.Cleanup(func() {
		for _, step := range []struct {
			sql string
			arg pgtype.UUID
		}{
			{"DELETE FROM donor_billing_observations WHERE period_id=$1", period},
			{"DELETE FROM donor_billing_periods WHERE id=$1", period},
			{"DELETE FROM donor_checkouts WHERE id=$1", checkout},
			{"DELETE FROM donor_stripe_events WHERE id=$1", event},
		} {
			if _, err := pool.Exec(ctx, step.sql, step.arg); err != nil {
				t.Error(err)
			}
		}
	})
	return donorID, period
}

func TestStaffFailedPaymentsListStatesAndAudit(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	staff, token := staffFixture(t, pool, "SUPER_ADMIN")
	_, growth := staffFixture(t, pool, "OP_GROWTH")
	s := &StaffService{DB: as(t, dbroles.API)}

	graceDonor, gracePeriod := failedPeriod(t, pool, "FAILED", 2*24*time.Hour, 3000)
	lapsedDonor, lapsedPeriod := failedPeriod(t, pool, "FAILED", 9*24*time.Hour, 0)
	_, paidPeriod := failedPeriod(t, pool, "PAID", time.Hour, 0)

	if _, err := s.FailedPayments(ctx, growth, "", ""); !errors.Is(err, ErrStaffForbidden) {
		t.Fatal("operator read the failed-payment queue", err)
	}
	if _, err := s.FailedPayments(ctx, "not-a-session", "", ""); !errors.Is(err, ErrStaffDenied) {
		t.Fatal("anonymous read", err)
	}
	for _, bad := range []struct{ state, cursor string }{{"bogus", ""}, {"", "not base64!"}, {"", "e30"}, {"", "aGVsbG8"}} {
		if _, err := s.FailedPayments(ctx, token, bad.state, bad.cursor); !errors.Is(err, ErrStaffInput) {
			t.Fatalf("invalid query %+v accepted: %v", bad, err)
		}
	}

	reads := 0
	find := func(state string) map[pgtype.UUID]FailedPayment {
		t.Helper()
		out := map[pgtype.UUID]FailedPayment{}
		after := ""
		for range 20 {
			got, err := s.FailedPayments(ctx, token, state, after)
			if err != nil {
				t.Fatal(err)
			}
			reads++
			if len(got.Payments) > pageSize {
				t.Fatal("page exceeds its bound", len(got.Payments))
			}
			for _, p := range got.Payments {
				if _, dup := out[p.PeriodID]; dup {
					t.Fatal("period listed twice across pages", p.PeriodID)
				}
				out[p.PeriodID] = p
			}
			if got.NextCursor == nil {
				return out
			}
			after = *got.NextCursor
		}
		t.Fatal("paging did not finish")
		return nil
	}
	all := find("all")
	grace, lapsed := all[gracePeriod], all[lapsedPeriod]
	if grace.State != FailedInGrace || grace.DonorID != graceDonor || grace.AmountMinor == nil || *grace.AmountMinor != 3000 ||
		!grace.GraceEndsAt.Equal(grace.FailedAt.Add(7*24*time.Hour)) {
		t.Fatalf("in-grace row wrong: %+v", grace)
	}
	if lapsed.State != FailedLapsed || lapsed.DonorID != lapsedDonor || lapsed.AmountMinor != nil || lapsed.Email == "" {
		t.Fatalf("lapsed row wrong: %+v", lapsed)
	}
	if _, listed := all[paidPeriod]; listed {
		t.Fatal("a paid period is listed as a failed payment")
	}
	if _, ok := find(FailedInGrace)[lapsedPeriod]; ok {
		t.Fatal("lapsed account in the grace list")
	}
	if _, ok := find(FailedInGrace)[gracePeriod]; !ok {
		t.Fatal("grace account missing from the grace list")
	}
	if _, ok := find(FailedLapsed)[gracePeriod]; ok {
		t.Fatal("grace account in the lapsed list")
	}
	var audited int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_staff_audit WHERE actor_id=$1 AND action='payments.failed_list'", staff).Scan(&audited); err != nil ||
		audited != reads {
		t.Fatal("failed-payment reads not audited", audited, err)
	}
}

func TestStaffFailedPaymentsKeysetPagesAreStable(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	_, token := staffFixture(t, pool, "SUPER_ADMIN")
	s := &StaffService{DB: as(t, dbroles.API)}
	want := map[pgtype.UUID]bool{}
	for i := range pageSize + 2 {
		_, period := failedPeriod(t, pool, "FAILED", time.Duration(i+1)*time.Second, 0)
		want[period] = true
	}
	first, err := s.FailedPayments(ctx, token, FailedInGrace, "")
	if err != nil || len(first.Payments) != pageSize || first.NextCursor == nil {
		t.Fatalf("first page: %d %v %v", len(first.Payments), first.NextCursor, err)
	}
	// A failure recorded after the first page must not shift the later ones.
	_, newest := failedPeriod(t, pool, "FAILED", 0, 0)
	seen := map[pgtype.UUID]bool{}
	for _, p := range first.Payments {
		seen[p.PeriodID] = true
	}
	for next := first.NextCursor; next != nil; {
		page, err := s.FailedPayments(ctx, token, FailedInGrace, *next)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range page.Payments {
			if seen[p.PeriodID] {
				t.Fatal("row repeated across pages")
			}
			seen[p.PeriodID] = true
		}
		next = page.NextCursor
	}
	if seen[newest] {
		t.Fatal("a row newer than the cursor appeared on a later page")
	}
	for period := range want {
		if !seen[period] {
			t.Fatal("fixture missing from the pages", period)
		}
	}
}

func TestStaffAccountDeletionsListIsReadOnlyAndAudited(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	staff, token := staffFixture(t, pool, "SUPER_ADMIN")
	_, support := staffFixture(t, pool, "OP_PARTNERSHIPS")
	s := &StaffService{DB: as(t, dbroles.API)}

	id, _ := donor(t, pool)
	checkout, gift := testID(t), testID(t)
	deletedAt := time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond)
	mustExec(
		t,
		pool,
		`INSERT INTO donor_checkouts (id,donor_id,amount_minor,currency,frequency) VALUES ($1,$2,2500,'USD','MONTHLY')`,
		checkout,
		id,
	)
	mustExec(
		t,
		pool,
		`INSERT INTO donor_gifts (id,donor_id,checkout_id,stripe_object_id,amount_minor,currency,frequency,paid_at) VALUES ($1,$2,$3,$4,2500,'USD','MONTHLY',$5)`,
		gift,
		id,
		checkout,
		"pi_"+gift.String(),
		deletedAt,
	)
	mustExec(t, pool, "INSERT INTO donor_deletions (donor_id, deleted_at) VALUES ($1,$2)", id, deletedAt)
	t.Cleanup(func() {
		for _, sql := range []string{"DELETE FROM donor_deletions WHERE donor_id=$1", "DELETE FROM donor_gifts WHERE donor_id=$1", "DELETE FROM donor_checkouts WHERE donor_id=$1"} {
			if _, err := pool.Exec(ctx, sql, id); err != nil {
				t.Error(err)
			}
		}
	})

	if _, err := s.AccountDeletions(ctx, support, ""); !errors.Is(err, ErrStaffForbidden) {
		t.Fatal("operator read the deletion log", err)
	}
	if _, err := s.AccountDeletions(ctx, token, "###"); !errors.Is(err, ErrStaffInput) {
		t.Fatal("malformed cursor accepted", err)
	}
	got, err := s.AccountDeletions(ctx, token, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Deletions) == 0 || got.Deletions[0].DonorID != id || got.Deletions[0].RetainedGifts != 1 ||
		!got.Deletions[0].DeletedAt.Equal(deletedAt) {
		t.Fatalf("newest deletion not first with its retained gifts: %+v", got.Deletions)
	}
	if got.NextCursor != nil {
		next, err := s.AccountDeletions(ctx, token, *got.NextCursor)
		if err != nil || len(next.Deletions) > pageSize {
			t.Fatal("second page", err)
		}
		for _, d := range next.Deletions {
			if d.DonorID == id {
				t.Fatal("row repeated on the next page")
			}
		}
	}
	var audited int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM donor_staff_audit WHERE actor_id=$1 AND action='deletions.list'", staff).Scan(&audited); err != nil ||
		audited != 1 {
		t.Fatal("deletion log read not audited", audited, err)
	}
}
