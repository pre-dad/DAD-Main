package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"dad/donor-api/internal/badges"
	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestCofundersKeepIndependentCommitmentsAndVerifiedSpend(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	_, admin := staffFixture(t, pool, "SUPER_ADMIN")
	a := approvedPartner(t, pool, "Cofunder A", "COFUNDA", 0, 0)
	b := approvedPartner(t, pool, "Cofunder B", "COFUNDB", 0, 0)
	cleanInitiatives(t, pool)
	api := as(t, dbroles.API)
	staff := &StaffService{DB: api}
	s := InitiativeService{Staff: staff}
	id, err := s.Create(ctx, admin, withBoardApproval(t, pool, validInitiative("cofunded")))
	if err != nil {
		t.Fatal(err)
	}
	partners := PartnerApplicationService{DB: api, Staff: staff}
	for _, p := range []struct {
		id     pgtype.UUID
		amount int64
	}{{a, 10000}, {b, 20000}} {
		if err := partners.LinkInitiative(ctx, admin, p.id, id, p.amount); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Publish(ctx, admin, id); err != nil {
		t.Fatal(err)
	}
	for _, p := range []struct {
		id     pgtype.UUID
		amount int64
	}{{a, 10000}, {b, 20000}} {
		rows, err := queries.New(api).StaffPartnerInitiatives(ctx, p.id)
		if err != nil || len(rows) != 1 || rows[0].CommittedMinor != p.amount {
			t.Fatalf("cofunder %s: %+v %v", p.id, rows, err)
		}
	}
	reader := InitiativeReader{DB: api}
	draft := SpendInput{Category: "Delivery", AmountMinor: 5000}
	if err := s.SetSpend(ctx, admin, id, draft); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishSpend(ctx, admin, id, draft.Category); !errors.Is(err, ErrStaffInput) {
		t.Fatalf("unverified publication: %v", err)
	}
	read, err := reader.Spend(ctx, id)
	if err != nil || read.TotalMinor != 0 {
		t.Fatalf("draft leaked: %+v %v", read, err)
	}
	draft.VerificationNote = "Reconciled against approved invoices"
	draft.VerifiedOn = "2026-10-01"
	if err := s.SetSpend(ctx, admin, id, draft); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishSpend(ctx, admin, id, draft.Category); err != nil {
		t.Fatal(err)
	}
	draft.AmountMinor = 9000
	if err := s.SetSpend(ctx, admin, id, draft); err != nil {
		t.Fatal(err)
	}
	read, err = reader.Spend(ctx, id)
	if err != nil || read.TotalMinor != 5000 {
		t.Fatalf("draft edit replaced published snapshot: %+v %v", read, err)
	}
	if err := s.PublishSpend(ctx, admin, id, draft.Category); err != nil {
		t.Fatal(err)
	}
	read, err = reader.Spend(ctx, id)
	if err != nil || read.TotalMinor != 9000 {
		t.Fatalf("published snapshot: %+v %v", read, err)
	}
}

func TestReasonedStreakResetSurvivesRecompute(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	_, admin := staffFixture(t, pool, "SUPER_ADMIN")
	_, operator := staffFixture(t, pool, "OP_GROWTH")
	service, _, f, _ := refundFixture(t)
	q := queries.New(pool)
	before, err := q.Projection(ctx, f.donorID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanExec(t, "DELETE FROM donor_streak_resets WHERE donor_id=$1", f.donorID) })
	reason := "Reset requested following documented recognition review"
	if err := service.Staff.ResetStreak(ctx, operator, f.donorID, reason); !errors.Is(err, ErrStaffForbidden) {
		t.Fatalf("operator reset: %v", err)
	}
	if err := service.Staff.ResetStreak(ctx, admin, f.donorID, reason); err != nil {
		t.Fatal(err)
	}
	if err := service.Staff.RecomputeRecognition(ctx, admin, f.donorID, "Recheck durable administrative reset"); err != nil {
		t.Fatal(err)
	}
	after, err := q.Projection(ctx, f.donorID)
	if err != nil || !after.CurrentStreak.Valid || after.CurrentStreak.Int32 != 0 || after.NetMinor != before.NetMinor ||
		after.BestStreak != before.BestStreak {
		t.Fatalf("reset did not persist or changed history: %+v %v", after, err)
	}
}

func TestRecognitionRulesAndArtwork(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	_, admin := staffFixture(t, pool, "SUPER_ADMIN")
	_, growth := staffFixture(t, pool, "OP_GROWTH")
	s := WordingService{Staff: &StaffService{DB: as(t, dbroles.API)}}
	t.Cleanup(func() {
		mustExec(t, pool, "DELETE FROM donor_badge_rules")
		mustExec(t, pool, "DELETE FROM donor_milestone_rules")
	})
	rules, err := s.Rules(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	rules.Milestones = []int64{5, 30, 100, 365}
	for i := range rules.Badges {
		if rules.Badges[i].Code == "V" {
			rules.Badges[i].Artwork = partnerTestLogo(t, 2, 2)
		}
	}
	if err := s.SetRules(ctx, growth, rules); !errors.Is(err, ErrStaffForbidden) {
		t.Fatalf("growth changed rules: %v", err)
	}
	if err := s.SetRules(ctx, admin, rules); err != nil {
		t.Fatal(err)
	}
	saved, err := s.Rules(ctx, admin)
	if err != nil || saved.Milestones[0] != 5 {
		t.Fatalf("rules not saved: %+v %v", saved, err)
	}
	for _, b := range saved.Badges {
		ranked := b.Code == "NAT2" || b.Code == "CITY50"
		if ranked != (b.MinPopulation == badges.MinRankPopulation) || (!ranked && b.MinPopulation != 1) {
			t.Fatalf("badge %s saved minimum population %d", b.Code, b.MinPopulation)
		}
	}
	for _, c := range []struct {
		code       string
		population int64
	}{{"CITY50", 0}, {"NAT2", 1000001}, {"7", 5}} {
		edited := slices.Clone(saved.Badges)
		for i := range edited {
			if edited[i].Code == c.code {
				edited[i].MinPopulation = c.population
			}
		}
		if err := s.SetRules(ctx, admin, RecognitionRules{Badges: edited, Milestones: saved.Milestones}); !errors.Is(err, ErrStaffInput) {
			t.Fatalf("%s accepted minimum population %d: %v", c.code, c.population, err)
		}
	}
	if !ruleEarned(BadgeRule{Signal: "referrals", Threshold: 2}, badges.Signals{ConvertedReferrals: 2}) ||
		ruleEarned(BadgeRule{Signal: "referrals", Threshold: 3}, badges.Signals{ConvertedReferrals: 2}) {
		t.Fatal("custom threshold not applied")
	}
}

func TestPasswordRequiresProofAndCurrentSecret(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	id, token := signedInDonor(t, pool)
	api := as(t, dbroles.API)
	s := PasswordService{DB: api}
	q := queries.New(pool)
	var email string
	if err := pool.QueryRow(ctx, "SELECT email FROM donors WHERE id=$1", id).Scan(&email); err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, "UPDATE donor_sessions SET password_verified_at=now()-interval '16 minutes' WHERE token_hash=$1", HashToken(token))
	password := "a long and unique test password"
	if err := s.Set(ctx, token, "", password); !errors.Is(err, ErrDenied) || !errors.Is(err, ErrCurrentPassword) {
		t.Fatalf("password without fresh proof: %v", err)
	}
	if err := q.MarkPasswordReauthenticated(ctx, queries.MarkPasswordReauthenticatedParams{TokenHash: HashToken(token), PasswordVerifiedAt: timestamp(time.Now().UTC())}); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, token, "", password); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SignIn(ctx, email, "wrong password", Client{}); !errors.Is(err, ErrSignInCode) {
		t.Fatalf("wrong password: %v", err)
	}
	tokens, err := s.SignIn(ctx, email, password, Client{})
	if err != nil || tokens.Access == "" {
		t.Fatalf("password login: %+v %v", tokens, err)
	}
	if err := s.Set(ctx, tokens.Access, "", "new long replacement password"); !errors.Is(err, ErrDenied) ||
		!errors.Is(err, ErrCurrentPassword) {
		t.Fatalf("change without proof: %v", err)
	}
	if err := s.Set(ctx, tokens.Access, "a typo of the current one", "new long replacement password"); !errors.Is(err, ErrCurrentPassword) {
		t.Fatalf("change with a wrong current password: %v", err)
	}
	if err := s.Set(ctx, tokens.Access, password, "new long replacement password"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SignIn(ctx, email, password, Client{}); !errors.Is(err, ErrSignInCode) {
		t.Fatalf("old password survived: %v", err)
	}
}
