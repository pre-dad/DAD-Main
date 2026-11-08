package store

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/recognition"
	"dad/donor-api/internal/store/queries"

	"github.com/jackc/pgx/v5/pgtype"
)

// epochFixture seeds donors whose projections are current, then restores the
// shared rules and epoch when the test ends. The historical clock keeps other
// tests' projections out of the worker's outdated queue.
type epochFixture struct {
	ids   []pgtype.UUID
	admin string
	rules WordingService
	now   time.Time
}

func newEpochFixture(t *testing.T, donors int) epochFixture {
	t.Helper()
	pool := database(t)
	ctx := context.Background()
	_, admin := staffFixture(t, pool, "SUPER_ADMIN")
	f := epochFixture{
		admin: admin,
		rules: WordingService{Staff: &StaffService{DB: as(t, dbroles.API)}},
		now:   time.Date(2000, 3, 1, 0, 0, 0, 0, time.UTC),
	}
	var epoch int32
	if err := pool.QueryRow(ctx, "SELECT epoch FROM donor_recognition_epoch").Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	for range donors {
		f.ids = append(f.ids, testID(t))
	}
	t.Cleanup(func() {
		for _, sql := range []string{
			"DELETE FROM donor_messages WHERE donor_id=ANY($1)",
			"DELETE FROM donor_metric_projections WHERE donor_id=ANY($1)",
			"DELETE FROM donors WHERE id=ANY($1)",
		} {
			if _, err := pool.Exec(context.Background(), sql, f.ids); err != nil {
				t.Error(err)
			}
		}
		for _, sql := range []string{"DELETE FROM donor_badge_rules", "DELETE FROM donor_milestone_rules"} {
			mustExec(t, pool, sql)
		}
		mustExec(t, pool, "UPDATE donor_recognition_epoch SET epoch=$1", epoch)
	})
	version := projectionRules + epoch
	for _, id := range f.ids {
		mustExec(t, pool, "INSERT INTO donors(id,email,created_at) VALUES($1,$2,$3)", id, id.String()+"@example.org", f.now)
		mustExec(t, pool, `INSERT INTO donor_metric_projections(id,donor_id,computed_at,next_refresh_at,rules_version)
			VALUES($1,$2,$3,$4,$5)`, testID(t), id, f.now.Add(-time.Hour), f.now.AddDate(0, 6, 0), version)
	}
	return f
}

// currentVersion is the rules version a projection computed now carries: other
// tests save rules, so the epoch is not zero.
func currentVersion(t *testing.T) int32 {
	t.Helper()
	version, err := currentRulesVersion(context.Background(), queries.New(database(t)))
	if err != nil {
		t.Fatal(err)
	}
	return version
}

// save stores a milestone ladder that differs from the previous call's.
func (f epochFixture) save(t *testing.T, first int64) {
	t.Helper()
	rules, err := f.rules.Rules(context.Background(), f.admin)
	if err != nil {
		t.Fatal(err)
	}
	rules.Milestones = []int64{first, 30, 100, 365}
	if err := f.rules.SetRules(context.Background(), f.admin, rules); err != nil {
		t.Fatal(err)
	}
}

func (f epochFixture) current(t *testing.T, pool queries.DBTX) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM donor_metric_projections p, donor_recognition_epoch e
		WHERE p.donor_id=ANY($1) AND p.rules_version>=$2+e.epoch`, f.ids, projectionRules).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSavingRulesBumpsTheEpochWithoutRewritingProjections(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	f := newEpochFixture(t, 40)
	xmins := func() []string {
		rows, err := pool.Query(ctx, "SELECT xmin::text FROM donor_metric_projections WHERE donor_id=ANY($1) ORDER BY donor_id", f.ids)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var x string
			if err := rows.Scan(&x); err != nil {
				t.Fatal(err)
			}
			out = append(out, x)
		}
		return out
	}
	before := xmins()
	var epochBefore int32
	if err := pool.QueryRow(ctx, "SELECT epoch FROM donor_recognition_epoch").Scan(&epochBefore); err != nil {
		t.Fatal(err)
	}
	f.save(t, 7)
	if after := xmins(); !slices.Equal(before, after) {
		t.Fatal("saving rules rewrote donor projections inside the staff request")
	}
	var epochAfter int32
	if err := pool.QueryRow(ctx, "SELECT epoch FROM donor_recognition_epoch").Scan(&epochAfter); err != nil || epochAfter != epochBefore+1 {
		t.Fatalf("epoch %d -> %d: %v", epochBefore, epochAfter, err)
	}
	if n := f.current(t, pool); n != 0 {
		t.Fatalf("%d projections still claim to be current after the rules changed", n)
	}
	var audits int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_staff_audit WHERE action='content.recognition_rules_changed' AND details->'milestones'->>0='7'").Scan(&audits); err != nil ||
		audits < 1 {
		t.Fatalf("rules change not audited: %d %v", audits, err)
	}
}

func TestOutdatedProjectionsConvergeInBoundedResumableSteps(t *testing.T) {
	pool := database(t)
	worker := as(t, dbroles.ProjectionWorker)
	ctx := context.Background()
	f := newEpochFixture(t, 25)
	f.save(t, 7)
	const batch = 8

	// An interrupted step changes nothing, and the next one resumes.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := RefreshDueProjections(cancelled, worker, f.now, batch); err == nil {
		t.Fatal("cancelled step reported success")
	}
	if n := f.current(t, pool); n != 0 {
		t.Fatalf("interrupted step refreshed %d donors", n)
	}
	steps := 0
	for {
		n, err := RefreshDueProjections(ctx, worker, f.now, batch)
		if err != nil {
			t.Fatal(err)
		}
		if n > batch {
			t.Fatalf("step claimed %d donors, bound is %d", n, batch)
		}
		if n == 0 {
			break
		}
		steps++
		if got := f.current(t, pool); got != min(steps*batch, len(f.ids)) {
			t.Fatalf("after %d steps %d donors are current", steps, got)
		}
	}
	if steps != 4 || f.current(t, pool) != len(f.ids) {
		t.Fatalf("converged in %d steps with %d current", steps, f.current(t, pool))
	}
	// Re-running with nothing outdated claims nothing.
	if n, err := RefreshDueProjections(ctx, worker, f.now.Add(time.Hour), batch); err != nil || n != 0 {
		t.Fatalf("idempotent re-run claimed %d: %v", n, err)
	}
}

func TestDonorReadBetweenRulesChangeAndRefreshUsesCurrentRules(t *testing.T) {
	pool := database(t)
	worker := as(t, dbroles.ProjectionWorker)
	ctx := context.Background()
	f := newEpochFixture(t, 1)
	id := f.ids[0]
	q := queries.New(pool)
	old, err := q.Projection(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	f.save(t, 7)

	// Not yet refreshed: the stored projection is the previous one, but the
	// ladder and badge thresholds a donor is shown follow the saved rules.
	between, err := q.Projection(ctx, id)
	if err != nil || between.ComputedAt != old.ComputedAt || between.RulesVersion != old.RulesVersion {
		t.Fatalf("projection changed before its refresh: %+v %v", between, err)
	}
	since := f.now.AddDate(0, 0, -3)
	ladder, err := configuredLadder(ctx, q, recognition.Streak{Since: &since}, f.now, time.UTC)
	if err != nil || ladder.NextMilestone == nil || *ladder.NextMilestone != 7 {
		t.Fatalf("ladder ignores the saved rules before refresh: %+v %v", ladder, err)
	}

	if n, err := RefreshDueProjections(ctx, worker, f.now, 4); err != nil || n != 1 {
		t.Fatalf("refresh: %d %v", n, err)
	}
	after, err := q.Projection(ctx, id)
	if err != nil || after.RulesVersion <= old.RulesVersion || !after.ComputedAt.Time.Equal(f.now) {
		t.Fatalf("donor not refreshed under the new rules: %+v %v", after, err)
	}
}

func TestRulesSavedDuringARefreshConvergeToTheLatestEpoch(t *testing.T) {
	pool := database(t)
	worker := as(t, dbroles.ProjectionWorker)
	ctx := context.Background()
	f := newEpochFixture(t, 30)
	f.save(t, 7)
	if _, err := RefreshDueProjections(ctx, worker, f.now, 10); err != nil {
		t.Fatal(err)
	}
	// A second save lands mid-refresh: the donors already refreshed fall back
	// to outdated and the rest are refreshed once, against the newest rules.
	f.save(t, 9)
	if n := f.current(t, pool); n != 0 {
		t.Fatalf("%d projections current after a second save", n)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		f.save(t, 11)
		f.save(t, 13)
	}()
	for {
		n, err := RefreshDueProjections(ctx, worker, f.now, 6)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	wg.Wait()
	for {
		n, err := RefreshDueProjections(ctx, worker, f.now, 6)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	if n := f.current(t, pool); n != len(f.ids) {
		t.Fatalf("%d of %d donors current at the latest epoch", n, len(f.ids))
	}
}

func fixedClock(at time.Time) func() time.Time { return func() time.Time { return at } }

func TestCatchUpProjectionsConvergesInBoundedTransactions(t *testing.T) {
	pool := database(t)
	worker := as(t, dbroles.ProjectionWorker)
	ctx := context.Background()
	f := newEpochFixture(t, 2*OutdatedBatch+88)
	f.save(t, 7)

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if n, err := CatchUpProjections(cancelled, worker, fixedClock(f.now), time.Hour); err == nil || n != 0 {
		t.Fatalf("cancelled catch-up claimed %d: %v", n, err)
	}
	if n := f.current(t, pool); n != 0 {
		t.Fatalf("cancelled catch-up refreshed %d donors", n)
	}

	// An exhausted slice still runs one transaction, never more than one batch.
	n, err := CatchUpProjections(ctx, worker, fixedClock(f.now), 0)
	if err != nil || n != OutdatedBatch || f.current(t, pool) != OutdatedBatch {
		t.Fatalf("first step claimed %d, %d current: %v", n, f.current(t, pool), err)
	}
	n, err = CatchUpProjections(ctx, worker, fixedClock(f.now), 0)
	if err != nil || n != OutdatedBatch || f.current(t, pool) != 2*OutdatedBatch {
		t.Fatalf("second step claimed %d, %d current: %v", n, f.current(t, pool), err)
	}
	// A generous slice finishes the remainder and stops when nothing is left.
	n, err = CatchUpProjections(ctx, worker, fixedClock(f.now), time.Hour)
	if err != nil || n != 88 || f.current(t, pool) != len(f.ids) {
		t.Fatalf("final step claimed %d, %d current: %v", n, f.current(t, pool), err)
	}
	if n, err = CatchUpProjections(ctx, worker, fixedClock(f.now), time.Hour); err != nil || n != 0 {
		t.Fatalf("converged catch-up claimed %d: %v", n, err)
	}
}

func TestCatchUpProjectionsServesDueDonorsBeforeTheBacklog(t *testing.T) {
	pool := database(t)
	worker := as(t, dbroles.ProjectionWorker)
	ctx := context.Background()
	f := newEpochFixture(t, OutdatedBatch+10)
	f.save(t, 7)
	due := f.ids[len(f.ids)-1]
	mustExec(t, pool, "UPDATE donor_metric_projections SET next_refresh_at=$2 WHERE donor_id=$1", due, f.now.Add(-time.Minute))

	n, err := CatchUpProjections(ctx, worker, fixedClock(f.now), 0)
	if err != nil || n != 1 {
		t.Fatalf("first transaction claimed %d, want only the due donor: %v", n, err)
	}
	if got := f.current(t, pool); got != 1 {
		t.Fatalf("%d donors current after the due refresh, want 1", got)
	}
	// Another donor falls due mid-backlog: the very next transaction takes it.
	mustExec(t, pool, "UPDATE donor_metric_projections SET next_refresh_at=$2 WHERE donor_id=$1", f.ids[0], f.now.Add(-time.Minute))
	if n, err = CatchUpProjections(ctx, worker, fixedClock(f.now), 0); err != nil || n != 1 {
		t.Fatalf("second transaction claimed %d, want only the newly due donor: %v", n, err)
	}
	if n, err = CatchUpProjections(ctx, worker, fixedClock(f.now), time.Hour); err != nil || n != OutdatedBatch+8 {
		t.Fatalf("backlog claimed %d, want %d: %v", n, OutdatedBatch+8, err)
	}
	if f.current(t, pool) != len(f.ids) {
		t.Fatalf("%d of %d donors current", f.current(t, pool), len(f.ids))
	}
}

func TestConcurrentCatchUpRefreshesEachDonorOnce(t *testing.T) {
	pool := database(t)
	worker := as(t, dbroles.ProjectionWorker)
	ctx := context.Background()
	f := newEpochFixture(t, 3*OutdatedBatch)
	f.save(t, 7)

	const replicas = 3
	claimed := make([]int, replicas)
	var wg sync.WaitGroup
	for i := range replicas {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := CatchUpProjections(ctx, worker, fixedClock(f.now), time.Hour)
			if err != nil {
				t.Error(err)
			}
			claimed[i] = n
		}()
	}
	wg.Wait()
	total := 0
	for _, n := range claimed {
		total += n
	}
	if total != len(f.ids) || f.current(t, pool) != len(f.ids) {
		t.Fatalf("replicas claimed %v (total %d) for %d donors, %d current", claimed, total, len(f.ids), f.current(t, pool))
	}
}

func TestCatchUpProjectionsStopsWhenTheSliceIsSpent(t *testing.T) {
	worker := as(t, dbroles.ProjectionWorker)
	f := newEpochFixture(t, 3*OutdatedBatch)
	f.save(t, 7)
	// The clock is read once per transaction; the slice is real time, so a
	// tiny slice ends the loop after the first transaction that outlasts it.
	n, err := CatchUpProjections(context.Background(), worker, fixedClock(f.now), time.Nanosecond)
	if err != nil || n != OutdatedBatch {
		t.Fatalf("spent slice claimed %d, want one batch of %d: %v", n, OutdatedBatch, err)
	}
}
