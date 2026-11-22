package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"dad/donor-api/internal/store/queries"
)

func TestStaffRecognitionRepairUsesFactsAndAuditsAtomically(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	actor, admin := staffFixture(t, pool, "SUPER_ADMIN")
	_, operator := staffFixture(t, pool, "OP_GROWTH")
	service, _, fixture, _ := refundFixture(t)
	q := queries.New(pool)
	baseline, err := q.Projection(ctx, fixture.donorID)
	if err != nil || !baseline.CurrentStreak.Valid || baseline.CurrentStreak.Int32 == 0 {
		t.Fatalf("fixture lacks paid continuity: %+v %v", baseline, err)
	}
	mustExec(t, pool, "UPDATE donor_metric_projections SET current_streak=999 WHERE donor_id=$1", fixture.donorID)
	reason := "Repair projection after documented system error DAD-123"
	if err := service.Staff.RecomputeRecognition(ctx, operator, fixture.donorID, reason); !errors.Is(err, ErrStaffForbidden) {
		t.Fatalf("operator repaired recognition: %v", err)
	}
	if err := service.Staff.RecomputeRecognition(ctx, admin, fixture.donorID, " "); !errors.Is(err, ErrStaffInput) {
		t.Fatalf("missing repair reason accepted: %v", err)
	}
	if err := service.Staff.RecomputeRecognition(ctx, admin, fixture.donorID, reason); err != nil {
		t.Fatal(err)
	}
	after, err := q.Projection(ctx, fixture.donorID)
	if err != nil || after.CurrentStreak != baseline.CurrentStreak || after.NetMinor != baseline.NetMinor ||
		after.BestStreak != baseline.BestStreak {
		t.Fatalf("recognition did not converge to recorded facts: %+v %v", after, err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM donor_staff_audit WHERE actor_id=$1 AND target=$2
      AND action='donor.recognition_recomputed' AND details->>'reason'=$3
      AND details->>'before_current'='999' AND (details->>'after_current')::int=$4`, actor, fixture.donorID.String(), reason, baseline.CurrentStreak.Int32).Scan(&count); err != nil || count != 1 {
		t.Fatalf("missing repair audit: %d %v", count, err)
	}
	var gifts int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_gifts WHERE donor_id=$1", fixture.donorID).Scan(&gifts); err != nil ||
		gifts != 1 {
		t.Fatalf("repair changed payment history: %d %v", gifts, err)
	}
}

func TestRepairReasonIsTrimmedBeforeItsLengthIsChecked(t *testing.T) {
	for name, c := range map[string]struct {
		reason string
		want   string
		ok     bool
	}{
		"padded":          {" \u00a0 Repair after DAD-123 \u3000", "Repair after DAD-123", true},
		"only spaces":     {"\u00a0\u00a0\u00a0\u00a0\u00a0\u00a0\u00a0\u00a0\u00a0\u00a0\u00a0\u00a0", "", false},
		"short once trim": {"short\u00a0\u00a0\u00a0\u00a0\u00a0\u00a0\u00a0\u00a0", "short", false},
		"too long":        {strings.Repeat("x", 1001), strings.Repeat("x", 1001), false},
		"control":         {"a reason with a \x07 bell", "a reason with a \x07 bell", false},
	} {
		got, ok := repairReason(c.reason)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: repairReason = %q, %v; want %q, %v", name, got, ok, c.want, c.ok)
		}
	}
}
