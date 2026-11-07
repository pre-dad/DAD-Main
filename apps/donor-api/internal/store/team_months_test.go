package store

import (
	"context"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"github.com/jackc/pgx/v5/pgtype"
)

// TestWinningTeamByCombinedActiveGiving covers Appendix A's Winning Team: when a
// month closes, the team with the most combined active giving wins, and its
// members earn the badge once.
func TestWinningTeamByCombinedActiveGiving(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	partner := approvedPartner(t, pool, "Team Org", "TEAMMONTH", 0, 0)
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
				`INSERT INTO donor_metric_projections(id,donor_id,current_streak,best_streak,computed_at,next_refresh_at,active,current_monthly_minor)
VALUES($1,$2,1,1,now(),now(),$3,3000)`,
				testID(t),
				donorID,
				i < active,
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
