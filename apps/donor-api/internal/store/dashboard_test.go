package store

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestDashboardAggregateNeverInventsTotalsFromIncompleteOrOverflowingFacts(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	now := time.Now().UTC()
	owner, first, second := testID(t), testID(t), testID(t)
	for _, id := range []pgtype.UUID{owner, first, second} {
		mustExec(t, tx, "INSERT INTO donors(id,email) VALUES($1,$2)", id, id.String()+"@example.org")
		mustExec(
			t,
			tx,
			"INSERT INTO donor_metric_projections(id,donor_id,net_minor,current_streak,computed_at,next_refresh_at) VALUES($1,$2,0,0,$3,$4)",
			testID(t),
			id,
			now,
			now.Add(time.Hour),
		)
	}
	mustExec(
		t,
		tx,
		"UPDATE donor_metric_projections SET referrer_id=$1,referral_kind='LINK',net_minor=3000 WHERE donor_id=$2",
		owner,
		first,
	)
	mustExec(
		t,
		tx,
		"UPDATE donor_metric_projections SET referrer_id=$1,referral_kind='IRL',net_minor=9000 WHERE donor_id=$2",
		owner,
		second,
	)
	// The dashboard reads the owner's denormalized totals, which projectFacts
	// writes from the referred rows: here LINK 3000 and IRL 9000.
	mustExec(
		t,
		tx,
		"UPDATE donor_metric_projections SET referred_link_count=1,referred_irl_count=1,referred_link_minor=3000,referred_irl_minor=9000 WHERE donor_id=$1",
		owner,
	)
	q := queries.New(tx)
	result, err := dashboard(ctx, q, queries.Donor{ID: owner}, now)
	if err != nil {
		t.Fatal(err)
	}
	referrals := result.Referrals
	if referrals == nil || referrals.TotalMinor != 12000 || referrals.Link != 1 || referrals.IRL != 1 {
		t.Fatal("wrong split", referrals)
	}
	// Incomplete referred facts must still never surface a total. These are
	// caught by the readiness gate (a bounded bool_and over the referred rows),
	// not by the totals read: a referred row that is unprojectable (NULL net),
	// stale (next_refresh_at not ahead of now), or of an unknown kind flips
	// ready false, so dashboard() withholds the referrals even though the
	// owner's denormalized columns still hold the earlier good totals.
	for _, modification := range []string{
		"net_minor=NULL", "next_refresh_at=computed_at", "referral_kind=NULL",
	} {
		mustExec(t, tx, "SAVEPOINT invalid_projection")
		mustExec(t, tx, "UPDATE donor_metric_projections SET "+modification+" WHERE donor_id=$1", first)
		result, err = dashboard(ctx, q, queries.Donor{ID: owner}, now)
		if err != nil || result.Referrals != nil {
			t.Fatal("incomplete aggregate exposed", modification, result, err)
		}
		mustExec(t, tx, "ROLLBACK TO SAVEPOINT invalid_projection")
	}
	// Stored minors that overflow int64 together must not surface a sum. The
	// per-channel bigint columns cannot overflow on their own; the dashboard
	// guard is pinned directly.
	for _, sameChannel := range []bool{false, true} {
		mustExec(t, tx, "SAVEPOINT overflow_projection")
		mustExec(t, tx, "UPDATE donor_metric_projections SET net_minor=$1 WHERE donor_id=$2", int64(math.MaxInt64), first)
		if sameChannel {
			mustExec(t, tx, "UPDATE donor_metric_projections SET referral_kind='LINK' WHERE donor_id=$1", second)
		}
		mustExec(
			t,
			tx,
			"UPDATE donor_metric_projections SET referred_link_minor=$1,referred_irl_minor=$1 WHERE donor_id=$2",
			int64(math.MaxInt64),
			owner,
		)
		result, err = dashboard(ctx, q, queries.Donor{ID: owner}, now)
		if err != nil || result.Referrals != nil {
			t.Fatal("overflowing aggregate exposed", result, err)
		}
		mustExec(t, tx, "ROLLBACK TO SAVEPOINT overflow_projection")
	}
}

// A referred donor whose gift is collected but not yet projected holds back
// the referrer's totals; one who only started a checkout does not.
func TestDashboardWaitsForReferredGiftsToBeProjected(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	now := time.Now().UTC()
	owner, giver, browser := testID(t), testID(t), testID(t)
	for _, id := range []pgtype.UUID{owner, giver, browser} {
		mustExec(t, tx, "INSERT INTO donors(id,email) VALUES($1,$2)", id, id.String()+"@example.org")
		mustExec(
			t,
			tx,
			"INSERT INTO donor_metric_projections(id,donor_id,computed_at,next_refresh_at) VALUES($1,$2,$3,$3)",
			testID(t),
			id,
			now,
		)
	}
	mustExec(
		t,
		tx,
		"UPDATE donor_metric_projections SET net_minor=0,current_streak=0,next_refresh_at=$2 WHERE donor_id=$1",
		owner,
		now.Add(time.Hour),
	)
	expires := now.Add(24 * time.Hour)
	for _, id := range []pgtype.UUID{giver, browser} {
		mustExec(
			t,
			tx,
			"INSERT INTO donor_checkouts(id,donor_id,amount_minor,frequency,referrer_id,referral_kind,referral_expires_at) VALUES($1,$2,3000,'MONTHLY',$3,'LINK',$4)",
			id,
			id,
			owner,
			expires,
		)
	}
	mustExec(
		t,
		tx,
		"INSERT INTO donor_gifts(id,donor_id,checkout_id,stripe_object_id,amount_minor,currency,frequency,paid_at,referrer_id,referral_kind) VALUES($1,$2,$2,$3,3000,'USD','MONTHLY',$4,$5,'LINK')",
		testID(t),
		giver,
		"pi_"+giver.String(),
		now,
		owner,
	)
	q := queries.New(tx)
	if result, err := dashboard(ctx, q, queries.Donor{ID: owner}, now); err != nil || result.Referrals != nil {
		t.Fatalf("totals shown while a referred gift awaits projection: %+v %v", result.Referrals, err)
	}
	mustExec(
		t,
		tx,
		"UPDATE donor_metric_projections SET referrer_id=$1,referral_kind='LINK',net_minor=3000,next_refresh_at=$3 WHERE donor_id=$2",
		owner,
		giver,
		now.Add(time.Hour),
	)
	// Projecting the giver credits the owner's totals; set them directly.
	mustExec(t, tx, "UPDATE donor_metric_projections SET referred_link_count=1,referred_link_minor=3000 WHERE donor_id=$1", owner)
	result, err := dashboard(ctx, q, queries.Donor{ID: owner}, now)
	if err != nil || result.Referrals == nil || result.Referrals.Link != 1 || result.Referrals.TotalMinor != 3000 {
		t.Fatalf("an unpaid referred checkout held back the totals: %+v %v", result.Referrals, err)
	}
}

// The web dashboard reads these exact fields.
func TestDashboardJSONContract(t *testing.T) {
	at := time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)
	grace := at.Add(14 * 24 * time.Hour)
	for _, c := range []struct {
		board DonorDashboard
		want  string
	}{
		{DonorDashboard{Mode: "test", State: "preparing"}, `{"mode":"test","state":"preparing"}`},
		{DonorDashboard{Mode: "test", State: "ready", ComputedAt: at}, `{"computed_at":"2027-01-02T03:04:05Z","founding_donor":false,"mode":"test","referrals":null,"state":"ready","streak":null,"rank":null,"rankings_opted_out":false,"network":null,"impact":{"given_minor":null,"currency":"","initiatives_funded":0,"events_volunteered":0}}`},
		{DonorDashboard{Mode: "test", State: "ready", ComputedAt: at, FoundingDonor: true,
			Referrals: &ReferralSummary{Link: 1, IRL: 2, LinkMinor: 300, IRLMinor: 400, TotalMinor: 700, Currency: "USD"},
			Streak:    &StreakSummary{Current: 3, AtRisk: true, GraceEndsAt: &grace}},
			`{"computed_at":"2027-01-02T03:04:05Z","founding_donor":true,"mode":"test","referrals":{"currency":"USD","irl":2,"irl_minor":400,"link":1,"link_minor":300,"total_minor":700},"state":"ready","streak":{"at_risk":true,"current":3,"grace_ends_at":"2027-01-16T03:04:05Z"},"rank":null,"rankings_opted_out":false,"network":null,"impact":{"given_minor":null,"currency":"","initiatives_funded":0,"events_volunteered":0}}`},
	} {
		got, err := json.Marshal(c.board)
		if err != nil || string(got) != c.want {
			t.Fatalf("got %s %v\nwant %s", got, err, c.want)
		}
	}
}

func TestDashboardSeparatesRankingOptOutFromNoRankYet(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	now := time.Now().UTC()
	optedIn := rankDonor(t, pool, 5, now.Add(-30*24*time.Hour), "Austin", false)
	optedOut := rankDonor(t, pool, 5, now.Add(-30*24*time.Hour), "Austin", true)
	q := queries.New(as(t, dbroles.API))
	for id, want := range map[pgtype.UUID]bool{optedIn: false, optedOut: true} {
		result, err := dashboard(ctx, q, queries.Donor{ID: id}, now)
		if err != nil || result.Rank != nil || result.RankingsOptedOut != want {
			t.Fatalf(
				"donor %s: rank=%+v opted_out=%v err=%v; want no rank and opted_out=%v",
				id,
				result.Rank,
				result.RankingsOptedOut,
				err,
				want,
			)
		}
	}
}

func TestDashboardIsPreparingUntilTheDonorIsProjected(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	id := testID(t)
	mustExec(t, tx, "INSERT INTO donors(id,email) VALUES($1,$2)", id, id.String()+"@example.org")
	result, err := dashboard(ctx, queries.New(tx), queries.Donor{ID: id}, time.Now().UTC())
	if err != nil || result.State != "preparing" || result.Referrals != nil || result.Streak != nil {
		t.Fatalf("unprojected donor: %+v %v", result, err)
	}
}

func TestDashboardShowsStreakAndImpactOnlyFromFreshProjections(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	now := time.Now().UTC().Truncate(time.Microsecond)
	grace := now.Add(48 * time.Hour)
	id := testID(t)
	mustExec(t, tx, "INSERT INTO donors(id,email) VALUES($1,$2)", id, id.String()+"@example.org")
	mustExec(
		t,
		tx,
		`INSERT INTO donor_metric_projections(id,donor_id,net_minor,current_streak,at_risk,grace_ends_at,streak_since,computed_at,next_refresh_at)
		VALUES($1,$2,4200,3,true,$3,$4,$5,$6)`,
		testID(t),
		id,
		grace,
		now.Add(-10*24*time.Hour),
		now,
		now.Add(time.Hour),
	)
	q := queries.New(tx)
	result, err := dashboard(ctx, q, queries.Donor{ID: id}, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Streak == nil || result.Streak.Current != 3 || !result.Streak.AtRisk || result.Streak.GraceEndsAt == nil ||
		!result.Streak.GraceEndsAt.Equal(grace) || result.Streak.Ladder == nil || result.Streak.Ladder.Days < 10 {
		t.Fatalf("fresh streak: %+v", result.Streak)
	}
	if result.Impact.GivenMinor == nil || *result.Impact.GivenMinor != 4200 || result.Impact.Currency != "USD" ||
		result.Impact.InitiativesFunded != 0 || result.Impact.EventsVolunteered != 0 || result.Network != nil || result.FoundingDonor {
		t.Fatalf("impact for a donor with no activity: %+v network=%+v founding=%v", result.Impact, result.Network, result.FoundingDonor)
	}

	// A projection no longer fresh must not claim the streak is alive, but the
	// donor's own net giving remains true and stays visible.
	mustExec(t, tx, "UPDATE donor_metric_projections SET next_refresh_at=$2 WHERE donor_id=$1", id, now)
	if result, err = dashboard(ctx, q, queries.Donor{ID: id}, now); err != nil || result.Streak != nil || result.Impact.GivenMinor == nil {
		t.Fatalf("stale projection: streak=%+v given=%v err=%v", result.Streak, result.Impact.GivenMinor, err)
	}

	// No streak at all, and net giving not yet computed: neither is invented.
	mustExec(
		t,
		tx,
		"UPDATE donor_metric_projections SET next_refresh_at=$2,current_streak=NULL,net_minor=NULL WHERE donor_id=$1",
		id,
		now.Add(time.Hour),
	)
	if result, err = dashboard(ctx, q, queries.Donor{ID: id}, now); err != nil || result.Streak != nil || result.Impact.GivenMinor != nil {
		t.Fatalf("incomplete projection: streak=%+v given=%v err=%v", result.Streak, result.Impact.GivenMinor, err)
	}
}
