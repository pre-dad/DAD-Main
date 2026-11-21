package store

import (
	"context"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5/pgtype"
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

// TestUnverifiedPartnersDoNotCrowdOutLaterOnes puts more unverified partners
// ahead of a closable one, by id, than a batch holds.
func TestUnverifiedPartnersDoNotCrowdOutLaterOnes(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	id := func(n byte) pgtype.UUID {
		return pgtype.UUID{Bytes: [16]byte{0, 0, 0, 0, 0, 0, 0x40, 0, 0x80, 0, 0, 0, 0, 0, 0xf0, n}, Valid: true}
	}
	for n := range byte(teamMonthBatch + 1) {
		partner := approvedPartnerWithID(t, pool, id(n), "Stuck Org", "STUCK-TM-"+string('A'+n), 0, 0)
		team := mustNewID(t)
		mustExec(t, pool, "INSERT INTO donor_partner_teams(id,partner_id,name) VALUES($1,$2,'Stuck')", team, partner)
		var first pgtype.UUID
		for range minimumGroup {
			first = linkDonorWithGift(t, pool, partner, 3000, time.Now().UTC(), 1)
			mustExec(t, pool, "UPDATE donor_partner_employees SET team_id=$2 WHERE donor_id=$1", first, team)
		}
		mustExec(t, pool, "UPDATE donor_metric_projections SET refresh_error='InvalidFacts' WHERE donor_id=$1", first)
	}
	ready := approvedPartnerWithID(t, pool, id(200), "Ready Org", "READY-TM", 0, 0)
	team := mustNewID(t)
	mustExec(t, pool, "INSERT INTO donor_partner_teams(id,partner_id,name) VALUES($1,$2,'Ready')", team, ready)
	solo, _ := donor(t, pool)
	mustExec(t, pool, "INSERT INTO donor_partner_employees(donor_id,partner_id,team_id) VALUES($1,$2,$3)", solo, ready, team)

	closed, err := (TeamMonthService{DB: as(t, dbroles.ProjectionWorker)}).CloseDue(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	var recorded int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_team_months WHERE partner_id=$1", ready).Scan(&recorded); err != nil ||
		recorded != 1 || closed < 1 {
		t.Fatalf(
			"the closable partner behind %d unverified ones was not closed: recorded=%d closed=%d err=%v",
			teamMonthBatch+1,
			recorded,
			closed,
			err,
		)
	}
}
