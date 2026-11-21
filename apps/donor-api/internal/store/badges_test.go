package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dad/donor-api/internal/badges"
	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// earnedSet returns the codes a badge view list reports as earned.
func earnedSet(views []BadgeView) map[string]bool {
	m := map[string]bool{}
	for _, v := range views {
		if v.Earned {
			m[v.Code] = true
		}
	}
	return m
}

func TestBadgesAutoEarnPermanenceAndAdminGrantRevoke(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	donorID, donorToken := signedInDonor(t, pool)
	b := BadgeService{DB: pool, Staff: &StaffService{DB: as(t, dbroles.API)}}
	_, adminToken := staffFixture(t, pool, "SUPER_ADMIN")
	_, support := staffFixture(t, pool, "OP_PARTNERSHIPS")

	// With no projection/signals, only badges with no requirement are earned
	// (none) — the catalog still returns every badge as a view.
	views, err := b.Badges(ctx, donorToken)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != len(badges.Catalog) {
		t.Fatalf("expected %d badge views, got %d", len(badges.Catalog), len(views))
	}
	if len(earnedSet(views)) != 0 {
		t.Fatalf("no signals but badges earned: %v", earnedSet(views))
	}

	// Seed a projection that qualifies the donor for first-gift + founding +
	// a 100-day streak + 3 referrals. Then a read auto-earns them.
	seedBadgeProjection(t, pool, donorID, time.Now().UTC())
	views, err = b.Badges(ctx, donorToken)
	if err != nil {
		t.Fatal(err)
	}
	got := earnedSet(views)
	for _, code := range []string{"1D", "F", "7", "30", "100", "REF1", "REF3"} {
		if !got[code] {
			t.Fatalf("expected auto-earned %q; earned=%v", code, got)
		}
	}

	// Permanence (B-2): wipe the projection; the earned badges must remain.
	if _, err := pool.Exec(ctx, "DELETE FROM donor_metric_projections WHERE donor_id=$1", donorID); err != nil {
		t.Fatal(err)
	}
	views, err = b.Badges(ctx, donorToken)
	if err != nil {
		t.Fatal(err)
	}
	if !earnedSet(views)["100"] {
		t.Fatal("badge un-earned after signal removed (should be permanent)")
	}

	// Admin grant of an inert/not-yet-earned badge; SUPPORT is refused.
	if err := b.Grant(ctx, support, donorID, "V"); !errors.Is(err, ErrStaffForbidden) {
		t.Fatalf("SUPPORT granted a badge: %v", err)
	}
	if err := b.Grant(ctx, adminToken, donorID, "V"); err != nil {
		t.Fatal(err)
	}
	if err := b.Grant(ctx, adminToken, donorID, "NOT_A_BADGE"); !errors.Is(err, ErrStaffInput) {
		t.Fatalf("unknown badge code granted: %v", err)
	}
	if views, err = b.Badges(ctx, donorToken); err != nil {
		t.Fatal(err)
	}
	if !earnedSet(views)["V"] {
		t.Fatal("admin-granted badge not shown earned")
	}

	// Admin revoke: the badge reads as unearned afterward (B-4).
	if err := b.Revoke(ctx, adminToken, donorID, "V"); err != nil {
		t.Fatal(err)
	}
	if views, err = b.Badges(ctx, donorToken); err != nil {
		t.Fatal(err)
	}
	if earnedSet(views)["V"] {
		t.Fatal("revoked badge still shown earned")
	}
}

// seedBadgeProjection writes a projection row that qualifies the donor for the
// first-gift, founding, 100-day-streak and 3-referral badges.
func seedBadgeProjection(t *testing.T, pool *pgxpool.Pool, donorID pgtype.UUID, now time.Time) {
	t.Helper()
	id, _ := NewID()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO donor_metric_projections
		 (id, donor_id, first_gift_id, founding_awarded_at, best_streak, current_streak,
		  streak_since, referred_link_count, referred_irl_count,
		  computed_at, next_refresh_at)
		 VALUES ($1,$2,'gift-1',$3,4,4,$4,2,1,$3,$5)
		 ON CONFLICT (donor_id) DO UPDATE SET
		   first_gift_id=EXCLUDED.first_gift_id, founding_awarded_at=EXCLUDED.founding_awarded_at,
		   best_streak=EXCLUDED.best_streak, current_streak=EXCLUDED.current_streak,
		   streak_since=EXCLUDED.streak_since, referred_link_count=EXCLUDED.referred_link_count,
		   referred_irl_count=EXCLUDED.referred_irl_count, next_refresh_at=EXCLUDED.next_refresh_at`,
		id, donorID, now, now.AddDate(0, 0, -102), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
}

func TestExpiredProjectionCannotAwardNewStreakBadges(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	donorID, token := signedInDonor(t, pool)
	now := time.Now().UTC()
	seedBadgeProjection(t, pool, donorID, now)
	mustExec(t, pool, "UPDATE donor_metric_projections SET next_refresh_at=$2 WHERE donor_id=$1", donorID, now.Add(-time.Hour))
	mustExec(t, pool, "INSERT INTO donor_badges(donor_id,code,source,earned_at) VALUES($1,'7','AUTO',$2)", donorID, now.Add(-24*time.Hour))
	service := BadgeService{DB: as(t, dbroles.API)}
	views, err := service.Badges(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	earned := earnedSet(views)
	if earned["30"] || earned["100"] {
		t.Fatalf("expired continuity awarded new streak badges: %v", earned)
	}
	if !earned["7"] || !earned["1D"] || !earned["F"] {
		t.Fatalf("expired continuity erased permanent awards or verified first-gift facts: %v", earned)
	}
	mustExec(t, pool, "UPDATE donor_metric_projections SET next_refresh_at=$2 WHERE donor_id=$1", donorID, now.Add(time.Hour))
	views, err = service.Badges(ctx, token)
	if err != nil || !earnedSet(views)["100"] {
		t.Fatalf("fresh continuity did not award the streak badge: %v, %v", earnedSet(views), err)
	}
}

// Badges are earned when the donor's facts change, not when they open the
// badges page, so a perk works on its own: a Founding Donor whose projection
// is refreshed can see the invite-only Founding Donor Dinner straight away
// (B-3, E-5).
func TestBadgesEarnWithoutVisitingTheBadgesPage(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	cleanEvents(t, pool)
	_, adminToken := staffFixture(t, pool, "SUPER_ADMIN")
	events := EventService{Staff: &StaffService{DB: as(t, dbroles.API)}}
	in := eventInput("founders-dinner", "San Francisco", "94103", "INVITE_ONLY", 48*time.Hour)
	in.QualifyingPerk = "FOUNDING_EVENTS"
	eventID, err := events.Create(ctx, adminToken, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Publish(ctx, adminToken, eventID); err != nil {
		t.Fatal(err)
	}
	f := newWorkerFixture(t, "MONTHLY")
	pushCleanup(t, f.db, f.donorID)
	cleanup(t, "DELETE FROM donor_badges WHERE donor_id=$1", f.donorID)
	checkout := f.checkout(t)
	f.receive(
		t,
		"invoice.payment_succeeded",
		invoice(checkout.CheckoutID, "in_founder_"+f.donorID.String(), time.Now().Add(-time.Hour), true),
	)
	f.process(t)
	var held []string
	rows, err := pool.Query(ctx, "SELECT code FROM donor_badges WHERE donor_id=$1 ORDER BY code", f.donorID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			t.Fatal(err)
		}
		held = append(held, code)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(held, ",") != "1D,F" {
		t.Fatalf("after the first gift the donor holds %v, want First Day and Founding Donor", held)
	}
	visible, err := EventReader{DB: as(t, dbroles.API)}.InviteOnlyEvents(ctx, sessionFor(t, pool, f.donorID))
	if err != nil || len(visible) != 1 || visible[0].ID != eventID.String() {
		t.Fatalf("Founding Donor cannot see the founders' dinner: %+v %v", visible, err)
	}
}

// Comeback needs an earlier run: a first streak reaching 30 days is not one.
func TestComebackNeedsAnEarlierRun(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	now := time.Now().UTC()
	donorID, _ := signedInDonor(t, pool)
	seedBadgeProjection(t, pool, donorID, now)
	mustExec(t, pool, "UPDATE donor_metric_projections SET streak_since=$2, best_streak=1, current_streak=1 WHERE donor_id=$1",
		donorID, now.Add(-40*24*time.Hour))
	checkout := testID(t)
	mustExec(
		t,
		pool,
		"INSERT INTO donor_checkouts (id, donor_id, amount_minor, frequency) VALUES ($1,$2,3000,'MONTHLY')",
		checkout,
		donorID,
	)
	cleanup(t, "DELETE FROM donor_checkouts WHERE donor_id=$1", donorID)
	gift := func(paid time.Time) {
		id := testID(t)
		mustExec(t, pool, `INSERT INTO donor_gifts (id,donor_id,checkout_id,stripe_object_id,amount_minor,currency,frequency,paid_at)
			VALUES ($1,$2,$3,$4,3000,'USD','MONTHLY',$5)`, id, donorID, checkout, "pi_"+id.String(), paid)
	}
	cleanup(t, "DELETE FROM donor_gifts WHERE donor_id=$1", donorID)
	gift(now.Add(-40 * 24 * time.Hour))
	donor, err := queries.New(pool).LockDonor(ctx, donorID)
	if err != nil {
		t.Fatal(err)
	}
	signals, err := signalsFor(ctx, queries.New(pool), donor, now)
	if err != nil || signals.ComebackReached30 {
		t.Fatalf("a first run counted as a comeback: %+v %v", signals, err)
	}
	gift(now.Add(-200 * 24 * time.Hour))
	if signals, err = signalsFor(ctx, queries.New(pool), donor, now); err != nil || !signals.ComebackReached30 {
		t.Fatalf("a restarted 40-day run is not a comeback: %+v %v", signals, err)
	}
}
