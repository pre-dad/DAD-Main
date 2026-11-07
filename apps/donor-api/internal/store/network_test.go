package store

import (
	"context"
	"testing"
	"time"

	"dad/donor-api/internal/store/queries"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// referredDonor seeds a donor referred by referrer, with a projection carrying
// active-state, current monthly amount, a converted first gift, and an optional
// opt-in to network visibility (with a public name).
func referredDonor(
	t *testing.T,
	pool *pgxpool.Pool,
	referrer pgtype.UUID,
	active bool,
	monthlyMinor int64,
	name string,
	visible bool,
) pgtype.UUID {
	t.Helper()
	ctx := context.Background()
	id, _ := donor(t, pool)
	t.Cleanup(func() { cleanExec(t, "DELETE FROM donor_preferences WHERE donor_id=$1", id) })
	if name != "" {
		if _, err := pool.Exec(ctx, "UPDATE donors SET public_name=$2 WHERE id=$1", id, name); err != nil {
			t.Fatal(err)
		}
	}
	pid, _ := NewID()
	if _, err := pool.Exec(ctx,
		`INSERT INTO donor_metric_projections
		 (id,donor_id,referrer_id,referral_kind,first_gift_id,best_streak,current_streak,
		  computed_at,next_refresh_at,current_monthly_minor,active)
		 VALUES ($1,$2,$3,'LINK',$4,1,1,$5,$6,$7,$8)`,
		pid, id, referrer, "gift_"+id.String(), time.Now().UTC(), time.Now().UTC().Add(time.Hour),
		monthlyMinor, active); err != nil {
		t.Fatal(err)
	}
	prefID, _ := NewID()
	if _, err := pool.Exec(ctx,
		`INSERT INTO donor_preferences (id,donor_id,version,referral_emails,marketing_emails,network_visible)
		 VALUES ($1,$2,1,true,true,$3)`, prefID, id, visible); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestNetworkTotalsActiveOnlyAnnualizedAndInviteeVisibility(t *testing.T) {
	pool := database(t)
	ctx := context.Background()

	referrer, _ := donor(t, pool)
	// The referrer needs a projection so the converted-referral count (N-2) reads.
	rpid, _ := NewID()
	if _, err := pool.Exec(ctx,
		`INSERT INTO donor_metric_projections
		 (id,donor_id,best_streak,current_streak,computed_at,next_refresh_at,referred_link_count)
		 VALUES ($1,$2,1,1,$3,$4,3)`,
		rpid, referrer, time.Now().UTC(), time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	// Two active referrals ($20/mo + $30/mo), one opted into visibility; one
	// lapsed referral (active=false) that must NOT count toward the total.
	_ = referredDonor(t, pool, referrer, true, 2000, "Ada L.", true)
	_ = referredDonor(t, pool, referrer, true, 3000, "", false)
	_ = referredDonor(t, pool, referrer, false, 5000, "Lapsed L.", true)
	// Visibility is opt-out: a referral who never saved a choice is listed.
	unset := referredDonor(t, pool, referrer, true, 1000, "Una S.", false)
	mustExec(t, pool, "DELETE FROM donor_preferences WHERE donor_id=$1", unset)

	// buildNetwork is the engine behind NetworkService.Network; drive it
	// directly against the referrer id (Network() only adds session auth).
	got, err := buildNetwork(ctx, queries.New(pool), referrer)
	if err != nil {
		t.Fatal(err)
	}

	if got.ActiveCount != 3 {
		t.Fatalf("active_count: got %d want 3 (lapsed excluded)", got.ActiveCount)
	}
	// (2000 + 3000 + 1000) * 12 = 72000.
	if got.AnnualizedMinor != 72000 {
		t.Fatalf("annualized_minor: got %d want 72000", got.AnnualizedMinor)
	}
	// N-3: only the opted-in active donor is listed by name; the other active
	// donor (not visible) is counted anonymously. The lapsed donor, though
	// visible, is inactive so it is neither listed as active nor in the anon
	// active tail.
	if len(got.Invitees) != 3 {
		t.Fatalf("invitees: got %d want 3 (every visible donor, regardless of active)", len(got.Invitees))
	}
	var named, namedActive int
	for _, iv := range got.Invitees {
		if iv.Name != "" {
			named++
		}
		if iv.Active {
			namedActive++
		}
	}
	if named != 3 || namedActive != 2 {
		t.Fatalf("invitee names: got named=%d active=%d want named=3 active=2", named, namedActive)
	}
	// 3 active total - 2 visible-active = 1 anonymous active.
	if got.AnonymousActive != 1 {
		t.Fatalf("anonymous_active: got %d want 1", got.AnonymousActive)
	}

	// N-2: 3 converted referrals -> next milestone is the 12 tier, 9 remaining.
	if got.NextMilestone == nil || got.NextMilestone.Target != 12 || got.NextMilestone.Remaining != 9 {
		t.Fatalf("next_milestone: got %+v want target=12 remaining=9", got.NextMilestone)
	}
}

func TestNetworkMilestoneLadderEndsAtTheLastTier(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	referrer, _ := donor(t, pool)
	mustExec(
		t,
		pool,
		`INSERT INTO donor_metric_projections(id,donor_id,best_streak,current_streak,computed_at,next_refresh_at,referred_link_count,referred_irl_count)
		 VALUES ($1,$2,1,1,now(),now()+interval '1 hour',12,0)`,
		testID(t),
		referrer,
	)
	for _, tt := range []struct {
		converted         int
		label             string
		target, remaining int64
	}{{12, "Team Captain", 20, 8}, {19, "Team Captain", 20, 1}} {
		mustExec(t, pool, "UPDATE donor_metric_projections SET referred_link_count=$2 WHERE donor_id=$1", referrer, tt.converted)
		got, err := buildNetwork(ctx, queries.New(pool), referrer)
		if err != nil || got.NextMilestone == nil ||
			*got.NextMilestone != (NetworkMilestone{Label: tt.label, Target: tt.target, Remaining: tt.remaining}) {
			t.Fatalf("%d converted: %+v %v", tt.converted, got.NextMilestone, err)
		}
	}
	mustExec(t, pool, "UPDATE donor_metric_projections SET referred_link_count=20 WHERE donor_id=$1", referrer)
	got, err := buildNetwork(ctx, queries.New(pool), referrer)
	if err != nil || got.NextMilestone != nil {
		t.Fatalf("every tier reached: %+v %v", got.NextMilestone, err)
	}
	if got.Invitees == nil || len(got.Invitees) != 0 || got.ActiveCount != 0 || got.AnonymousActive != 0 {
		t.Fatalf("a donor with no referrals: %+v", got)
	}
}
