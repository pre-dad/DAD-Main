package store

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/recognition"
	"dad/donor-api/internal/store/queries"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRankingScheduleSurvivesEmptyPopulationAndSerializesWorkers(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	mustExec(t, pool, "DELETE FROM donor_projection_schedules WHERE id='rankings'")
	cleanup(t, "DELETE FROM donor_projection_schedules WHERE id='rankings'")
	cleanup(t, "DELETE FROM donor_rankings")
	svc := RankingService{DB: as(t, dbroles.ProjectionWorker)}
	now := time.Now().UTC()
	var wg sync.WaitGroup
	results := make(chan bool, 8)
	errors := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			worked, err := svc.RefreshIfDue(ctx, now)
			results <- worked
			errors <- err
		})
	}
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	count := 0
	for worked := range results {
		if worked {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("concurrent workers refreshed %d times, want one", count)
	}
	if worked, err := svc.RefreshIfDue(ctx, now.Add(RankingInterval-time.Microsecond)); err != nil || worked {
		t.Fatalf("empty population refreshed before due: %t %v", worked, err)
	}
	if worked, err := svc.RefreshIfDue(ctx, now.Add(RankingInterval)); err != nil || !worked {
		t.Fatalf("scheduled refresh did not run: %t %v", worked, err)
	}
}

// rankDonor seeds a donor with a projection carrying a streak and start, and an
// optional city + opt-out, for ranking tests.
func rankDonor(t *testing.T, pool *pgxpool.Pool, streak int32, since time.Time, city string, optedOut bool) pgtype.UUID {
	t.Helper()
	ctx := context.Background()
	id, _ := donor(t, pool)
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DELETE FROM donor_rankings WHERE donor_id=$1", id)
		pool.Exec(context.Background(), "DELETE FROM donor_preferences WHERE donor_id=$1", id)
	})
	if city != "" {
		if _, err := pool.Exec(ctx, "UPDATE donors SET city=$2 WHERE id=$1", id, city); err != nil {
			t.Fatal(err)
		}
	}
	pid, _ := NewID()
	if _, err := pool.Exec(ctx,
		`INSERT INTO donor_metric_projections (id,donor_id,best_streak,current_streak,streak_since,computed_at,next_refresh_at)
		 VALUES ($1,$2,$3,$3,$4,$5,$6)`,
		pid, id, streak, since, time.Now().UTC(), time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if optedOut {
		prefID, _ := NewID()
		if _, err := pool.Exec(ctx,
			`INSERT INTO donor_preferences (id,donor_id,version,referral_emails,marketing_emails,rankings_opted_out)
			 VALUES ($1,$2,1,true,true,true)`, prefID, id); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func TestRankingsUseLocalDaysAndImmediateOptOut(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	now := time.Now().UTC()
	longer := rankDonor(t, pool, 2, now.Add(-61*24*time.Hour), "Austin", false)
	shorter := rankDonor(t, pool, 3, now.Add(-60*24*time.Hour), "Austin", false)
	stale := rankDonor(t, pool, 10, now.Add(-300*24*time.Hour), "Austin", false)
	mustExec(t, pool, "UPDATE donor_metric_projections SET next_refresh_at=$2 WHERE donor_id=$1", stale, now.Add(-time.Second))
	svc := RankingService{DB: as(t, dbroles.ProjectionWorker)}
	cleanup(t, "DELETE FROM donor_badges WHERE code IN ('NAT2','CITY50')")
	cleanup(t, "DELETE FROM donor_rankings")
	if err := svc.Refresh(ctx, now); err != nil {
		t.Fatal(err)
	}
	q := queries.New(as(t, dbroles.API))
	for id, want := range map[pgtype.UUID]int{longer: 1, shorter: 2} {
		rank, err := ForDonor(ctx, q, id)
		if err != nil || rank == nil || rank.CityRank == nil || *rank.CityRank != want {
			t.Fatalf("daily ranking for %s = %+v (%v), want city rank %d", id, rank, err, want)
		}
	}
	if rank, err := ForDonor(ctx, q, stale); err != nil || rank != nil {
		t.Fatalf("stale recognition was ranked: %+v %v", rank, err)
	}
	pref := mustNewID(t)
	mustExec(t, pool, `INSERT INTO donor_preferences(id,donor_id,version,referral_emails,marketing_emails,rankings_opted_out)
		VALUES($1,$2,1,false,false,true)`, pref, longer)
	if rank, err := ForDonor(ctx, q, longer); err != nil || rank != nil {
		t.Fatalf("opt-out waited for the next rollup: %+v %v", rank, err)
	}
}

func TestSingleEligibleDonorIsTopPercentile(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id := rankDonor(t, pool, 1, now.Add(-7*24*time.Hour), "Austin", false)
	cleanup(t, "DELETE FROM donor_badges WHERE code IN ('NAT2','CITY50')")
	cleanup(t, "DELETE FROM donor_rankings")
	if err := (RankingService{DB: as(t, dbroles.ProjectionWorker)}).Refresh(ctx, now); err != nil {
		t.Fatal(err)
	}
	rank, err := ForDonor(ctx, queries.New(as(t, dbroles.API)), id)
	if err != nil || rank == nil || rank.NationalPercentile != 1 {
		t.Fatalf("single eligible donor rank = %+v %v; want top 1%%", rank, err)
	}
}

func TestRankingRefreshPercentileCityRankAndOptOut(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	now := time.Now().UTC()
	local := now.In(recognition.NewYork)
	base := time.Date(local.Year(), local.Month(), local.Day()-300, 12, 0, 0, 0, recognition.NewYork)

	// Clean any pre-existing ranking rows for isolation.
	t.Cleanup(func() { pool.Exec(ctx, "DELETE FROM donor_rankings") })
	if _, err := pool.Exec(ctx, "DELETE FROM donor_rankings"); err != nil {
		t.Fatal(err)
	}

	// Top streak in city A, with a tie broken by earlier start.
	top := rankDonor(t, pool, 17, base.AddDate(0, 0, -200), "Austin", false)
	tieEarlier := rankDonor(t, pool, 10, base.Add(-time.Hour), "Austin", false)
	tieLater := rankDonor(t, pool, 10, base, "Austin", false)
	_ = rankDonor(t, pool, 4, base.AddDate(0, 0, 200), "Austin", false)
	// An opted-out donor with the highest streak must be excluded entirely.
	optedOut := rankDonor(t, pool, 100, base.AddDate(0, 0, -1000), "Austin", true)

	cleanup(t, "DELETE FROM donor_badges WHERE code IN ('NAT2','CITY50')")
	rankBadgeRules(t, pool, 1)
	svc := RankingService{DB: as(t, dbroles.ProjectionWorker)}
	if err := svc.Refresh(ctx, now); err != nil {
		t.Fatal(err)
	}
	held := func(id pgtype.UUID) string {
		t.Helper()
		var codes string
		if err := pool.QueryRow(ctx, "SELECT coalesce(string_agg(code, ',' ORDER BY code), '') FROM donor_badges WHERE donor_id=$1", id).Scan(&codes); err != nil {
			t.Fatal(err)
		}
		return codes
	}
	if got := held(top); got != "CITY50,NAT2" {
		t.Fatalf("top donor holds %q after the rollup, want Top 50 City and Top 2%%", got)
	}
	if got := held(optedOut); got != "" {
		t.Fatalf("opted-out donor earned ranking badges %q", got)
	}

	cityRank := func(id pgtype.UUID) int {
		t.Helper()
		var r pgtype.Int4
		if err := pool.QueryRow(ctx, "SELECT city_rank FROM donor_rankings WHERE donor_id=$1", id).Scan(&r); err != nil {
			t.Fatalf("no ranking row for donor: %v", err)
		}
		return int(r.Int32)
	}

	// Top streak ranks #1 in the city.
	if got := cityRank(top); got != 1 {
		t.Fatalf("top donor city_rank=%d want 1", got)
	}
	// Tie on 300 local days: the earlier start ranks ahead (rank 2 vs 3).
	if e, l := cityRank(tieEarlier), cityRank(tieLater); e != 2 || l != 3 {
		t.Fatalf("tie-break failed: earlier=%d later=%d, want 2 and 3", e, l)
	}

	// The opted-out donor has no ranking row at all.
	var exists bool
	if err := pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM donor_rankings WHERE donor_id=$1)", optedOut).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("opted-out donor was ranked")
	}

	// The top donor's national percentile is the best bucket (1).
	var pct int
	if err := pool.QueryRow(ctx, "SELECT national_percentile FROM donor_rankings WHERE donor_id=$1", top).Scan(&pct); err != nil {
		t.Fatal(err)
	}
	if pct != 1 {
		t.Fatalf("top donor percentile: %d, want 1", pct)
	}
}

// rankBadgeRules saves the rank badges' default thresholds with minPopulation
// as the population each must be ranked among, as staff would.
func rankBadgeRules(t *testing.T, pool *pgxpool.Pool, minPopulation int64) {
	t.Helper()
	actor, _ := staffFixture(t, pool, "SUPER_ADMIN")
	cleanup(t, "DELETE FROM donor_badge_rules WHERE code IN ('NAT2','CITY50')")
	mustExec(t, pool, `INSERT INTO donor_badge_rules(code,signal,threshold,min_population,updated_at,actor_id)
VALUES ('NAT2','national_percentile',2,$1,now(),$2),('CITY50','city_rank',50,$1,now(),$2)
ON CONFLICT (code) DO UPDATE SET min_population=EXCLUDED.min_population`, minPopulation, actor)
}

// A rank among fewer donors than a rank badge's minimum population is shown
// but earns nothing, on the rollup or on read; staff can lower the minimum.
func TestRankBadgesWaitForTheirMinimumPopulation(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	now := time.Now().UTC()
	cleanup(t, "DELETE FROM donor_badges WHERE code IN ('NAT2','CITY50')")
	cleanup(t, "DELETE FROM donor_rankings")
	mustExec(t, pool, "DELETE FROM donor_rankings")
	mustExec(t, pool, "DELETE FROM donor_badge_rules WHERE code IN ('NAT2','CITY50')")
	id := rankDonor(t, pool, 30, now.Add(-30*24*time.Hour), "Boise", false)
	_ = rankDonor(t, pool, 3, now.Add(-3*24*time.Hour), "Boise", false)
	token := sessionFor(t, pool, id)
	svc := RankingService{DB: as(t, dbroles.ProjectionWorker)}
	held := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_badges WHERE donor_id=$1 AND code IN ('NAT2','CITY50')", id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := svc.Refresh(ctx, now); err != nil {
		t.Fatal(err)
	}
	rank, err := ForDonor(ctx, queries.New(pool), id)
	if err != nil || rank == nil || rank.NationalPercentile != 1 || rank.CityRank == nil || *rank.CityRank != 1 {
		t.Fatalf("the rank line is hidden in a small population: %+v %v", rank, err)
	}
	views, err := (BadgeService{DB: pool}).Badges(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if badgeView(t, views, "NAT2").Earned || badgeView(t, views, "CITY50").Earned || held() != 0 {
		t.Fatal("a rank among two donors earned a rank badge under the default minimum")
	}
	rankBadgeRules(t, pool, 3)
	if err := svc.Refresh(ctx, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if held() != 0 {
		t.Fatal("a rank among two donors earned a rank badge needing three")
	}
	rankBadgeRules(t, pool, 2)
	if err := svc.Refresh(ctx, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if held() != 2 {
		t.Fatal("lowering the minimum to the population did not award both rank badges")
	}
}

// badgeView finds one badge in the donor's badge list.
func badgeView(t *testing.T, views []BadgeView, code string) BadgeView {
	t.Helper()
	for _, v := range views {
		if v.Code == code {
			return v
		}
	}
	t.Fatalf("badge %s missing from the list", code)
	return BadgeView{}
}

func TestCityBadgeRecordsTheCityItWasEarnedIn(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id := rankDonor(t, pool, 1, now.Add(-7*24*time.Hour), "Chicago", false)
	token := sessionFor(t, pool, id)
	cleanup(t, "DELETE FROM donor_badges WHERE code IN ('NAT2','CITY50')")
	cleanup(t, "DELETE FROM donor_rankings")
	rankBadgeRules(t, pool, 1)
	if err := (RankingService{DB: as(t, dbroles.ProjectionWorker)}).Refresh(ctx, now); err != nil {
		t.Fatal(err)
	}
	var stored pgtype.Text
	if err := pool.QueryRow(ctx, "SELECT city FROM donor_badges WHERE donor_id=$1 AND code='CITY50'", id).Scan(&stored); err != nil {
		t.Fatalf("the rollup did not award the city badge: %v", err)
	}
	if !stored.Valid || stored.String != "Chicago" {
		t.Fatalf("rollup recorded city %+v, want Chicago", stored)
	}
	mustExec(t, pool, "UPDATE donors SET city='Denver' WHERE id=$1", id)
	b := BadgeService{DB: pool}
	views, err := b.Badges(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	city := badgeView(t, views, "CITY50")
	if !city.Earned || city.City == nil || *city.City != "Chicago" || city.DisplayTitle != "Top 50 in Chicago" || city.Code != "CITY50" {
		t.Fatalf("after moving, the city badge reads %+v; want it kept as Top 50 in Chicago", city)
	}
	for _, v := range views {
		if v.Code != "CITY50" && v.City != nil {
			t.Fatalf("badge %s carries city %q; only the city badge has one", v.Code, *v.City)
		}
		if v.Code != "CITY50" && v.DisplayTitle != v.Title {
			t.Fatalf("badge %s display title %q differs from its title %q", v.Code, v.DisplayTitle, v.Title)
		}
	}
}

func TestCityBadgeEarnedOnReadRecordsTheRankedCity(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id := rankDonor(t, pool, 1, now.Add(-7*24*time.Hour), "Austin", false)
	token := sessionFor(t, pool, id)
	cleanup(t, "DELETE FROM donor_badges WHERE code IN ('NAT2','CITY50')")
	cleanup(t, "DELETE FROM donor_rankings")
	rankBadgeRules(t, pool, 1)
	if err := (RankingService{DB: as(t, dbroles.ProjectionWorker)}).Refresh(ctx, now); err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, "DELETE FROM donor_badges WHERE donor_id=$1", id)
	views, err := (BadgeService{DB: pool}).Badges(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	city := badgeView(t, views, "CITY50")
	if !city.Earned || city.City == nil || *city.City != "Austin" || city.DisplayTitle != "Top 50 in Austin" {
		t.Fatalf("badge earned on read = %+v, want Top 50 in Austin", city)
	}
	if first := badgeView(t, views, "NAT2"); first.City != nil {
		t.Fatalf("a non-city badge carries city %q", *first.City)
	}
}

func TestCityBadgeBackfillTakesTheDonorsCurrentCity(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	migration, err := os.ReadFile("../../db/migrations/0079_badge_city.sql")
	if err != nil {
		t.Fatal(err)
	}
	text := string(migration)
	start := strings.Index(text, "UPDATE donor_badges")
	end := start + strings.Index(text[start:], ";") + 1
	if start < 0 || end <= start {
		t.Fatal("backfill statement not found in the migration")
	}
	earned := rankDonor(t, pool, 1, time.Now().UTC().Add(-time.Hour), "Boston", false)
	granted := rankDonor(t, pool, 1, time.Now().UTC().Add(-time.Hour), "Boston", false)
	other := rankDonor(t, pool, 1, time.Now().UTC().Add(-time.Hour), "Boston", false)
	cleanup(t, "DELETE FROM donor_badges WHERE donor_id IN ($1,$2,$3)", earned, granted, other)
	mustExec(
		t,
		pool,
		"INSERT INTO donor_badges(donor_id,code,source,earned_at) VALUES($1,'CITY50','AUTO',now()),($2,'CITY50','GRANT',now()),($3,'7','AUTO',now())",
		earned,
		granted,
		other,
	)
	mustExec(t, pool, text[start:end])
	cities := map[pgtype.UUID]pgtype.Text{}
	for _, d := range []pgtype.UUID{earned, granted, other} {
		var c pgtype.Text
		if err := pool.QueryRow(ctx, "SELECT city FROM donor_badges WHERE donor_id=$1", d).Scan(&c); err != nil {
			t.Fatal(err)
		}
		cities[d] = c
	}
	if c := cities[earned]; !c.Valid || c.String != "Boston" {
		t.Fatalf("auto-earned city badge backfilled to %+v, want Boston", c)
	}
	if cities[granted].Valid || cities[other].Valid {
		t.Fatalf("backfill touched a granted or non-city badge: granted %+v other %+v", cities[granted], cities[other])
	}
}
