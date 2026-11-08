package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fullRewriteOracle is the ranking logic from before the refresh was chunked: one
// window pass over the whole eligible population. The chunked refresh must
// leave donor_rankings holding exactly what it computes.
const fullRewriteOracle = `
WITH eligible AS (
  SELECT d.id AS donor_id, d.city AS city,
         ($1::timestamptz AT TIME ZONE coalesce(d.timezone,'America/New_York'))::date
           - (p.streak_since AT TIME ZONE coalesce(d.timezone,'America/New_York'))::date AS streak,
         p.streak_since AS since
  FROM donors d
  JOIN donor_metric_projections p ON p.donor_id = d.id
  LEFT JOIN donor_preferences pref ON pref.donor_id = d.id
  WHERE d.disabled = false AND p.current_streak > 0 AND p.streak_since IS NOT NULL
    AND p.next_refresh_at > $1::timestamptz AND COALESCE(pref.rankings_opted_out, false) = false
)
SELECT donor_id::text,
       GREATEST(1, LEAST(100, CEIL(PERCENT_RANK() OVER (ORDER BY streak DESC, since ASC) * 100)::int)),
       coalesce(city, ''),
       coalesce(CASE WHEN city IS NOT NULL THEN RANK() OVER (PARTITION BY city ORDER BY streak DESC, since ASC)::int END, 0),
       (count(*) OVER ())::int,
       CASE WHEN city IS NOT NULL THEN (count(*) OVER (PARTITION BY city))::int ELSE 0 END
FROM eligible`

type rankLine struct {
	percentile, rank, national, cityPop int
	city                                string
}

func oracleRanks(t *testing.T, pool *pgxpool.Pool, now time.Time) map[string]rankLine {
	t.Helper()
	rows, err := pool.Query(context.Background(), fullRewriteOracle, now)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]rankLine{}
	for rows.Next() {
		var id string
		var l rankLine
		if err := rows.Scan(&id, &l.percentile, &l.city, &l.rank, &l.national, &l.cityPop); err != nil {
			t.Fatal(err)
		}
		out[id] = l
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func storedRanks(t *testing.T, pool *pgxpool.Pool) map[string]rankLine {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT r.donor_id::text, r.national_percentile, coalesce(r.city, ''), coalesce(r.city_rank, 0),
  coalesce((SELECT population FROM donor_ranking_populations WHERE scope='NATIONAL'), 0),
  coalesce((SELECT population FROM donor_ranking_populations WHERE scope='CITY' AND city=r.city), 0)
FROM donor_rankings r`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]rankLine{}
	for rows.Next() {
		var id string
		var l rankLine
		if err := rows.Scan(&id, &l.percentile, &l.city, &l.rank, &l.national, &l.cityPop); err != nil {
			t.Fatal(err)
		}
		out[id] = l
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func rowVersions(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(), "SELECT donor_id::text, xmin::text FROM donor_rankings")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, xmin string
		if err := rows.Scan(&id, &xmin); err != nil {
			t.Fatal(err)
		}
		out[id] = xmin
	}
	return out
}

func requireRanks(t *testing.T, pool *pgxpool.Pool, now time.Time) map[string]rankLine {
	t.Helper()
	want, got := oracleRanks(t, pool, now), storedRanks(t, pool)
	if len(got) != len(want) {
		t.Fatalf("donor_rankings holds %d rows, full rewrite computes %d", len(got), len(want))
	}
	for id, w := range want {
		if got[id] != w {
			t.Fatalf("donor %s ranked %+v, full rewrite computes %+v", id, got[id], w)
		}
	}
	return got
}

// seedRankingPopulation seeds donors over three cities and no city, with tied
// streaks, and returns their ids in seeding order.
func seedRankingPopulation(t *testing.T, pool *pgxpool.Pool, now time.Time) []pgtype.UUID {
	t.Helper()
	var ids []pgtype.UUID
	cities := []string{"Austin", "Boise", "Chicago", ""}
	for i := range 23 {
		streakDays := 5 + (i%6)*10
		ids = append(ids, rankDonor(t, pool, 1, now.Add(-time.Duration(streakDays*24+i%3)*time.Hour), cities[i%len(cities)], i%11 == 4))
	}
	return ids
}

func resetRankings(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	mustExec(t, pool, "DELETE FROM donor_rankings")
	mustExec(t, pool, "DELETE FROM donor_ranking_populations")
	mustExec(t, pool, "DELETE FROM donor_projection_schedules WHERE id='rankings'")
	cleanup(t, "DELETE FROM donor_badges WHERE code IN ('NAT2','CITY50')")
	cleanup(t, "DELETE FROM donor_rankings")
	cleanup(t, "DELETE FROM donor_ranking_populations")
	cleanup(t, "DELETE FROM donor_projection_schedules WHERE id='rankings'")
}

func TestChunkedRefreshMatchesFullRewriteAcrossChurn(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	now := time.Now().UTC()
	resetRankings(t, pool)
	rankBadgeRules(t, pool, 1)
	ids := seedRankingPopulation(t, pool, now)
	svc := RankingService{DB: as(t, dbroles.ProjectionWorker), ChunkSize: 4}
	if err := svc.Refresh(ctx, now); err != nil {
		t.Fatal(err)
	}
	first := requireRanks(t, pool, now)
	if len(first) < 15 {
		t.Fatalf("seeded population too small to exercise chunking: %d", len(first))
	}

	// Churn: an opt-out, a disabled donor, a lapsed projection, a longer streak
	// that reorders ties, a city move, and newcomers.
	mustExec(t, pool, `INSERT INTO donor_preferences(id,donor_id,version,referral_emails,marketing_emails,rankings_opted_out)
		VALUES($1,$2,1,false,false,true) ON CONFLICT (donor_id) DO UPDATE SET rankings_opted_out=true`, mustNewID(t), ids[0])
	mustExec(t, pool, "UPDATE donors SET disabled=true WHERE id=$1", ids[1])
	t.Cleanup(func() { pool.Exec(ctx, "UPDATE donors SET disabled=false WHERE id=$1", ids[1]) })
	mustExec(t, pool, "UPDATE donor_metric_projections SET next_refresh_at=$2 WHERE donor_id=$1", ids[2], now.Add(-time.Second))
	mustExec(t, pool, "UPDATE donor_metric_projections SET streak_since=$2 WHERE donor_id=$1", ids[3], now.Add(-400*24*time.Hour))
	mustExec(t, pool, "UPDATE donors SET city='Boise' WHERE id=$1", ids[5])
	mustExec(t, pool, "UPDATE donors SET city=NULL WHERE id=$1", ids[6])
	rankDonor(t, pool, 1, now.Add(-33*24*time.Hour), "Denver", false)
	rankDonor(t, pool, 1, now.Add(-33*24*time.Hour), "Austin", false)
	if err := svc.Refresh(ctx, now); err != nil {
		t.Fatal(err)
	}
	second := requireRanks(t, pool, now)
	if _, ok := second[uuidText(t, ids[0])]; ok {
		t.Fatal("opted-out donor kept a ranking row")
	}
	if _, ok := second[uuidText(t, ids[2])]; ok {
		t.Fatal("donor with a lapsed projection kept a ranking row")
	}

	// Emptying the population clears the table and the population sizes.
	mustExec(t, pool, "UPDATE donor_metric_projections SET next_refresh_at=$1", now.Add(-time.Second))
	t.Cleanup(func() {
		pool.Exec(
			ctx,
			"UPDATE donor_metric_projections SET next_refresh_at=now()+interval '1 hour' WHERE next_refresh_at=$1",
			now.Add(-time.Second),
		)
	})
	if err := svc.Refresh(ctx, now); err != nil {
		t.Fatal(err)
	}
	if got := requireRanks(t, pool, now); len(got) != 0 {
		t.Fatalf("empty population left %d rows", len(got))
	}
	var populations int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_ranking_populations").Scan(&populations); err != nil || populations != 0 {
		t.Fatalf("empty population left %d population rows (%v)", populations, err)
	}
}

func uuidText(t *testing.T, id pgtype.UUID) string {
	t.Helper()
	value, err := id.Value()
	if err != nil {
		t.Fatal(err)
	}
	return value.(string)
}

func TestUnchangedPopulationWritesNoRankingRows(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	now := time.Now().UTC()
	resetRankings(t, pool)
	seedRankingPopulation(t, pool, now)
	svc := RankingService{DB: as(t, dbroles.ProjectionWorker), ChunkSize: 4}
	if err := svc.Refresh(ctx, now); err != nil {
		t.Fatal(err)
	}
	before := rowVersions(t, pool)
	if len(before) == 0 {
		t.Fatal("nothing was ranked")
	}
	if err := svc.Refresh(ctx, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	for id, xmin := range rowVersions(t, pool) {
		if before[id] != xmin {
			t.Fatalf("ranking row %s was rewritten although its rank did not change", id)
		}
	}
	requireRanks(t, pool, now.Add(time.Minute))
}

func TestRankingRefreshFailingMidwayLeavesUsableRowsAndConverges(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	t0 := time.Now().UTC()
	resetRankings(t, pool)
	rankBadgeRules(t, pool, 1)
	ids := seedRankingPopulation(t, pool, t0)
	svc := RankingService{DB: as(t, dbroles.ProjectionWorker), ChunkSize: 4}
	if worked, err := svc.RefreshIfDue(ctx, t0); err != nil || !worked {
		t.Fatalf("first refresh: %t %v", worked, err)
	}
	old := requireRanks(t, pool, t0)

	t1 := t0.Add(RankingInterval)
	mustExec(t, pool, "UPDATE donor_metric_projections SET streak_since=$2 WHERE donor_id=$1", ids[3], t0.Add(-500*24*time.Hour))
	mustExec(t, pool, "UPDATE donor_metric_projections SET streak_since=$2 WHERE donor_id=$1", ids[9], t0.Add(-450*24*time.Hour))
	boom := errors.New("injected failure")
	failing := svc
	failing.afterChunk = func(written int) error {
		if written >= 8 {
			return boom
		}
		return nil
	}
	if _, err := failing.RefreshIfDue(ctx, t1); !errors.Is(err, boom) {
		t.Fatalf("refresh error = %v, want the injected failure", err)
	}
	want := oracleRanks(t, pool, t1)
	for id, got := range storedRanks(t, pool) {
		current := want[id]
		if o := old[id]; got != o && (got.percentile != current.percentile || got.rank != current.rank || got.city != current.city) {
			t.Fatalf("donor %s holds %+v, neither the previous %+v nor the new %+v rank", id, got, old[id], want[id])
		}
	}
	if worked, err := svc.RefreshIfDue(ctx, t1); err != nil || !worked {
		t.Fatalf("a failed refresh must stay due: worked %t, err %v", worked, err)
	}
	requireRanks(t, pool, t1)
	if worked, err := svc.RefreshIfDue(ctx, t1); err != nil || worked {
		t.Fatalf("a completed refresh ran again before it was due: %t %v", worked, err)
	}
}
