package store

import (
	"context"
	"errors"
	"testing"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/push"
	"dad/donor-api/internal/store/queries"
)

// TestStaffRewordsBadgesAndMilestones covers PRD §8 Content: staff change the
// words on a badge and a milestone notification, and can restore the default.
func TestStaffRewordsBadgesAndMilestones(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	staff := &StaffService{DB: as(t, dbroles.API)}
	_, content := staffFixture(t, pool, "SUPER_ADMIN")
	_, growth := staffFixture(t, pool, "OP_GROWTH")
	wording := WordingService{Staff: staff}
	t.Cleanup(func() { mustExec(t, pool, "DELETE FROM donor_recognition_wording") })

	badge := WordingChange{Kind: WordingBadge, Key: "WT", Title: "Top Team", Text: "Your team wins the month"}
	if err := wording.SetWording(ctx, growth, badge); !errors.Is(err, ErrStaffForbidden) {
		t.Fatalf("growth operator rewording a badge: %v; want ErrStaffForbidden", err)
	}
	for name, change := range map[string]WordingChange{
		"unknown badge":     {Kind: WordingBadge, Key: "NOPE", Title: "A", Text: "B"},
		"yearly milestone":  {Kind: WordingMilestone, Key: "1825", Title: "A", Text: "B"},
		"padded key":        {Kind: WordingMilestone, Key: "030", Title: "A", Text: "B"},
		"title only":        {Kind: WordingBadge, Key: "WT", Title: "A"},
		"control character": {Kind: WordingBadge, Key: "WT", Title: "A\nB", Text: "B"},
	} {
		if err := wording.SetWording(ctx, content, change); !errors.Is(err, ErrStaffInput) {
			t.Errorf("%s: %v; want ErrStaffInput", name, err)
		}
	}
	if err := wording.SetWording(ctx, content, badge); err != nil {
		t.Fatal(err)
	}
	milestone := WordingChange{Kind: WordingMilestone, Key: "30", Title: "A month of giving", Text: "Thirty days of showing up."}
	if err := wording.SetWording(ctx, content, milestone); err != nil {
		t.Fatal(err)
	}

	_, donorToken := signedInDonor(t, pool)
	views, err := BadgeService{DB: pool, Staff: staff}.Badges(ctx, donorToken)
	if err != nil {
		t.Fatal(err)
	}
	if last := views[len(views)-1]; last.Code != "WT" || last.Title != badge.Title || last.Rule != badge.Text {
		t.Fatalf("donor sees %+v, want the staff wording", last)
	}

	f := newPushFixture(t)
	appDevice(t, pool, f.donor, push.IOS, iosToken, f.now.Add(1<<40))
	apns := &fakeProvider{}
	f.deliver(t, PushWorker{DB: as(t, dbroles.PushWorker), Providers: map[string]push.Provider{push.IOS: apns}}, f.now)
	if len(apns.notes) != 1 || apns.notes[0].Title != milestone.Title || apns.notes[0].Body != milestone.Text {
		t.Fatalf("milestone notification %+v, want the staff wording", apns.notes)
	}

	if err := wording.SetWording(ctx, content, WordingChange{Kind: WordingBadge, Key: "WT"}); err != nil {
		t.Fatal(err)
	}
	all, err := wording.Wording(ctx, content)
	if err != nil {
		t.Fatal(err)
	}
	wt, month := all.Badges[len(all.Badges)-1], all.Milestones[1]
	if wt.Custom || wt.Title != "Winning Team" || !month.Custom || month.Title != milestone.Title ||
		month.DefaultTitle != "Your 30-day giving streak" {
		t.Fatalf("after reset: badge %+v, milestone %+v", wt, month)
	}
	var audited int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_staff_audit WHERE action='content.wording_changed' AND target IN ('BADGE:WT','MILESTONE:30')").
		Scan(&audited); err != nil ||
		audited < 3 {
		t.Fatalf("%d wording changes audited, err %v; want 3", audited, err)
	}
}

func TestTunedThresholdKeepsStaffWordingAndPluralizesDefaults(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	staff := &StaffService{DB: as(t, dbroles.API)}
	_, admin := staffFixture(t, pool, "SUPER_ADMIN")
	wording := WordingService{Staff: staff}
	t.Cleanup(func() {
		mustExec(t, pool, "DELETE FROM donor_recognition_wording")
		mustExec(t, pool, "DELETE FROM donor_badge_rules")
		mustExec(t, pool, "DELETE FROM donor_milestone_rules")
	})

	rules, err := wording.Rules(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range rules.Badges {
		switch b.Code {
		case "REF3", "V3":
			rules.Badges[i].Threshold = 1
		case "REF12":
			rules.Badges[i].Threshold = 2
		}
	}
	if err = wording.SetRules(ctx, admin, rules); err != nil {
		t.Fatal(err)
	}
	custom := WordingChange{Kind: WordingBadge, Key: "REF12", Title: "Connector", Text: "Introduce a couple of friends"}
	if err = wording.SetWording(ctx, admin, custom); err != nil {
		t.Fatal(err)
	}

	all, err := wording.Wording(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string]WordingEntry{}
	for _, e := range all.Badges {
		entries[e.Key] = e
	}
	if e := entries["REF3"]; e.Custom || e.Text != "Bring in 1 person who completes a gift" || e.DefaultText != e.Text {
		t.Fatalf("tuned referral default %+v", e)
	}
	if e := entries["V3"]; e.Text != "Check in at 1 event" || e.DefaultText != e.Text {
		t.Fatalf("tuned check-in default %+v", e)
	}
	if e := entries["REF12"]; !e.Custom || e.Text != custom.Text || e.DefaultText != "Bring in 2 people who complete a gift" {
		t.Fatalf("custom wording on a tuned badge %+v", e)
	}

	catalog, err := badgeWording(ctx, queries.New(pool))
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range catalog {
		switch b.Code {
		case "REF12":
			if b.Title != custom.Title || b.Rule != custom.Text {
				t.Fatalf("tuning overwrote staff wording: %+v", b)
			}
		case "V3":
			if b.Rule != "Check in at 1 event" {
				t.Fatalf("tuned rule %q", b.Rule)
			}
		}
	}
}
