package store

import (
	"context"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/store/queries"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestWinningTeamByCombinedActiveGiving covers Appendix A's Winning Team: when a
// month closes, the team with the most combined active giving wins, and its
// members earn the badge once.
func TestWinningTeamByCombinedActiveGiving(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	version, err := currentRulesVersion(ctx, queries.New(pool))
	if err != nil {
		t.Fatal(err)
	}
	partner := approvedPartner(t, pool, "Team Org", "TEAMMONTH", 0, 0)
	cleanup(
		t,
		"DELETE FROM donor_badges WHERE code='WT' AND donor_id IN (SELECT donor_id FROM donor_partner_employees WHERE partner_id=$1)",
		partner,
	)
	team := func(name string, members, active int) (pgtype.UUID, []pgtype.UUID) {
		id := testID(t)
		mustExec(t, pool, "INSERT INTO donor_partner_teams(id,partner_id,name) VALUES($1,$2,$3)", id, partner, name)
		var donors []pgtype.UUID
		for i := range members {
			donorID, _ := donor(t, pool)
			mustExec(t, pool, "INSERT INTO donor_partner_employees(donor_id,partner_id,team_id) VALUES($1,$2,$3)", donorID, partner, id)
			mustExec(
				t,
				pool,
				`INSERT INTO donor_metric_projections(id,donor_id,current_streak,best_streak,computed_at,next_refresh_at,active,current_monthly_minor,rules_version)
VALUES($1,$2,1,1,'2026-10-31','2026-11-02',$3,3000,$4)`,
				testID(t),
				donorID,
				i < active,
				version,
			)
			donors = append(donors, donorID)
		}
		return id, donors
	}
	largeID, large := team("Large", 12, 6)
	_, small := team("Small", 5, 5)
	team("Tiny", 2, 2)

	months := TeamMonthService{DB: as(t, dbroles.ProjectionWorker)}
	now := time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC)
	if _, err := months.CloseDue(ctx, now); err != nil {
		t.Fatal(err)
	}
	if _, err := months.CloseDue(ctx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	var closed, ranked int
	var winner pgtype.UUID
	if err := pool.QueryRow(ctx, `SELECT count(*), max(winner_team_id::text)::uuid,
  (SELECT count(*) FROM donor_team_month_standings WHERE partner_id=$1)
FROM donor_team_months WHERE partner_id=$1 AND month='2026-10-01'`, partner).Scan(&closed, &winner, &ranked); err != nil {
		t.Fatal(err)
	}
	if winner != largeID {
		t.Fatalf("winner %v, want the team with the most combined active giving %v", winner, largeID)
	}
	if closed != 1 || ranked != 2 {
		t.Fatalf("closed %d months with %d ranked teams; want October once, without the team below the minimum size", closed, ranked)
	}
	earned := func(donors []pgtype.UUID) (n int) {
		for _, d := range donors {
			var c int
			if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_badges WHERE donor_id=$1 AND code='WT'", d).Scan(&c); err != nil {
				t.Fatal(err)
			}
			n += c
		}
		return n
	}
	if got := earned(large); got != len(large) {
		t.Fatalf("%d of %d members of the team with the most combined active giving earned Winning Team", got, len(large))
	}
	if got := earned(small); got != 0 {
		t.Fatalf("%d members of the smaller, lower-total team earned Winning Team", got)
	}
}

func TestTeamMonthWaitsForVerifiedMetrics(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	partner := approvedPartner(t, pool, "Pending Team", "PENDING-TEAM", 0, 0)
	team := mustNewID(t)
	mustExec(t, pool, "INSERT INTO donor_partner_teams(id,partner_id,name) VALUES($1,$2,'Pending')", team, partner)
	var first pgtype.UUID
	for range 5 {
		id := linkDonorWithGift(t, pool, partner, 3000, time.Now().UTC(), 1)
		first = id
		mustExec(t, pool, "UPDATE donor_partner_employees SET team_id=$2 WHERE donor_id=$1", id, team)
		mustExec(t, pool, "UPDATE donor_metric_projections SET active=true,current_monthly_minor=3000 WHERE donor_id=$1", id)
	}
	t.Cleanup(func() {
		cleanExec(
			t,
			"DELETE FROM donor_badges WHERE code='WT' AND donor_id IN (SELECT donor_id FROM donor_partner_employees WHERE partner_id=$1)",
			partner,
		)
	})
	mustExec(t, pool, "UPDATE donor_metric_projections SET refresh_error='InvalidFacts' WHERE donor_id=$1", first)
	months := TeamMonthService{DB: as(t, dbroles.ProjectionWorker)}
	now := time.Now().UTC()
	if _, err := months.CloseDue(ctx, now); err != nil {
		t.Fatal(err)
	}
	var closed int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_team_months WHERE partner_id=$1", partner).Scan(&closed); err != nil ||
		closed != 0 {
		t.Fatalf("closed %d months with unverified metrics: %v", closed, err)
	}
	mustExec(t, pool, "UPDATE donor_metric_projections SET refresh_error=NULL WHERE donor_id=$1", first)
	if _, err := months.CloseDue(ctx, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_team_months WHERE partner_id=$1 AND winner_team_id=$2", partner, team).Scan(&closed); err != nil ||
		closed != 1 {
		t.Fatalf("verified winner: closed=%d err=%v", closed, err)
	}
}

// closableTeam gives an approved partner one team of the minimum size whose
// members' metrics are fresh at now, so the partner's month can close.
func closableTeam(t *testing.T, pool *pgxpool.Pool, code string, now time.Time) pgtype.UUID {
	t.Helper()
	version, err := currentRulesVersion(context.Background(), queries.New(pool))
	if err != nil {
		t.Fatal(err)
	}
	partner := approvedPartner(t, pool, "Month "+code, code, 0, 0)
	cleanup(
		t,
		"DELETE FROM donor_badges WHERE code='WT' AND donor_id IN (SELECT donor_id FROM donor_partner_employees WHERE partner_id=$1)",
		partner,
	)
	team := testID(t)
	mustExec(t, pool, "INSERT INTO donor_partner_teams(id,partner_id,name) VALUES($1,$2,'Team')", team, partner)
	for range minimumGroup {
		donorID, _ := donor(t, pool)
		mustExec(t, pool, "INSERT INTO donor_partner_employees(donor_id,partner_id,team_id) VALUES($1,$2,$3)", donorID, partner, team)
		mustExec(
			t,
			pool,
			`INSERT INTO donor_metric_projections(id,donor_id,current_streak,best_streak,computed_at,next_refresh_at,active,current_monthly_minor,rules_version)
VALUES($1,$2,1,1,$3,$4,true,3000,$5)`,
			testID(t),
			donorID,
			now.Add(-time.Hour),
			now.Add(24*time.Hour),
			version,
		)
	}
	return partner
}

// drainTeamMonths closes due months until none remain, failing the test
// instead of hanging if the queue never empties.
func drainTeamMonths(t *testing.T, months TeamMonthService, now time.Time) {
	t.Helper()
	for range 500 {
		n, err := months.CloseDue(context.Background(), now)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
	t.Fatal("team months still closing after 500 batches")
}

func TestTeamMonthClosedIsTheMonthBeforeNewYorkNow(t *testing.T) {
	pool := database(t)
	months := TeamMonthService{DB: as(t, dbroles.ProjectionWorker)}
	for name, tc := range map[string]struct {
		now  time.Time
		want string
	}{
		"new year in New York closes December": {time.Date(2027, 1, 1, 5, 0, 0, 0, time.UTC), "2026-12-01"},
		"still October in New York":            {time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC), "2026-09-01"},
		"midnight November in New York":        {time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC), "2026-10-01"},
	} {
		t.Run(name, func(t *testing.T) {
			partner := closableTeam(t, pool, "MONTH-"+testID(t).String()[24:], tc.now)
			drainTeamMonths(t, months, tc.now)
			rows, err := pool.Query(context.Background(), "SELECT month::text FROM donor_team_months WHERE partner_id=$1", partner)
			if err != nil {
				t.Fatal(err)
			}
			got, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("closed months %v, want only %s", got, tc.want)
			}
		})
	}
}

func TestTeamMonthClosingProceedsInBatchesWithoutStarvingPartners(t *testing.T) {
	pool := database(t)
	months := TeamMonthService{DB: as(t, dbroles.ProjectionWorker)}
	now := time.Date(2027, 3, 1, 6, 0, 0, 0, time.UTC)
	var partners []pgtype.UUID
	for range teamMonthBatch + 5 {
		partners = append(partners, closableTeam(t, pool, "BATCH-"+testID(t).String()[24:], now))
	}
	first, err := months.CloseDue(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if first != teamMonthBatch {
		t.Fatalf("first pass closed %d partners, want the batch of %d", first, teamMonthBatch)
	}
	drainTeamMonths(t, months, now)
	var closed int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM donor_team_months WHERE partner_id=ANY($1) AND month='2027-02-01'", partners).Scan(&closed); err != nil ||
		closed != len(partners) {
		t.Fatalf("closed %d of %d partners after draining: %v", closed, len(partners), err)
	}
}
