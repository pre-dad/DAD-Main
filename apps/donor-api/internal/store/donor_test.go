package store

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5/pgtype"
)

func donorServiceFixture(t *testing.T) (DonorService, string, pgtype.UUID) {
	t.Helper()
	pool := database(t)
	api := as(t, dbroles.API)
	id, _ := donor(t, pool)
	accountCleanup(t, pool, id)
	t.Cleanup(func() {
		for _, sql := range []string{"DELETE FROM donor_referral_touches WHERE referrer_id=$1", "DELETE FROM donor_preference_events WHERE donor_id=$1", "DELETE FROM donor_preferences WHERE donor_id=$1"} {
			if _, err := pool.Exec(context.Background(), sql, id); err != nil {
				t.Error(err)
			}
		}
	})
	proof := link(t, pool, id, time.Now())
	tokens, err := (AccountService{DB: api}).ClaimAccount(context.Background(), proof, "", Client{})
	if err != nil {
		t.Fatal(err)
	}
	return DonorService{DB: api, Origin: "https://donor.example.org"}, tokens.Access, id
}

func TestPreferenceConcurrencyAndConsentAudit(t *testing.T) {
	s, token, id := donorServiceFixture(t)
	pool := database(t)
	ctx := context.Background()
	profile, err := s.Profile(ctx, token)
	if err != nil || profile.Version != 0 || profile.ReferralEmails || profile.MarketingEmails || profile.ConsentVersion != consentVersion {
		t.Fatal(profile, err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			_, err := s.SavePreferences(ctx, token, Preferences{Version: 0, ReferralEmails: true}.Replace())
			results <- err
		}()
	}
	close(start)
	success, conflict := 0, 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			success++
		} else if errors.Is(err, ErrPreferenceConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatal("lost update", success, conflict)
	}
	var count int
	var version int
	var consent, source string
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_preference_events WHERE donor_id=$1", id).Scan(&count); err != nil ||
		count != 1 {
		t.Fatal("duplicate/missing consent event", err)
	}
	if err := pool.QueryRow(ctx, "SELECT version,consent_version,source FROM donor_preference_events WHERE donor_id=$1", id).Scan(&version, &consent, &source); err != nil ||
		version != 1 ||
		consent != consentVersion ||
		source != "account" {
		t.Fatal("audit mismatch", err)
	}
	if err = s.SignOutEverywhere(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Profile(ctx, token); !errors.Is(err, ErrDenied) {
		t.Fatal("revoked session accepted", err)
	}
}

func TestSharingRotationAppliesLastTouchAndExpiry(t *testing.T) {
	first, token, id := donorServiceFixture(t)
	pool := database(t)
	second, other, secondID := donorServiceFixture(t)
	ctx := context.Background()
	now := time.Now()
	before, err := first.Sharing(ctx, token)
	if err != nil || before.PublicName != nil || before.Link != nil || before.IRLLink != nil || before.ReferralCode != nil {
		t.Fatal(before, err)
	}
	for _, name := range []string{" ", "email@example.org", "name\ncontrol", strings.Repeat("x", 81)} {
		if _, err := first.SaveSharing(ctx, token, name, false); !errors.Is(err, ErrPublicName) {
			t.Fatal("invalid public name", err)
		}
	}
	a, err := first.SaveSharing(ctx, token, "  First  ", false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := second.SaveSharing(ctx, other, "Second", false)
	if err != nil {
		t.Fatal(err)
	}
	if *a.PublicName != "First" || *a.IRLLink != *a.Link+"?via=irl" {
		t.Fatal("wrong share links")
	}
	code := strings.TrimPrefix(*a.Link, first.Origin+"/r/")
	if a.ReferralCode == nil || *a.ReferralCode != code {
		t.Fatalf("referral_code %v is not the link's code %q", a.ReferralCode, code)
	}
	touch, err := first.CaptureReferral(ctx, code, "LINK", "", now)
	if err != nil || touch.Token == "" || *touch.PublicName != "First" {
		t.Fatal(touch, err)
	}
	later, err := first.CaptureReferral(ctx, strings.TrimPrefix(*b.Link, second.Origin+"/r/"), "IRL", touch.Token, now.Add(10*24*time.Hour))
	if err != nil || later.Token != touch.Token || *later.PublicName != "Second" {
		t.Fatal("last touch not applied", later, err)
	}
	saved, err := queries.New(pool).ReferralTouchByToken(ctx, HashToken(touch.Token))
	if err != nil || saved.Kind != "IRL" ||
		!saved.ExpiresAt.Time.Equal(now.Add(10*24*time.Hour+30*24*time.Hour).Truncate(time.Microsecond)) {
		t.Fatal("last-touch attribution type/expiry not updated", err)
	}
	rotated, err := first.SaveSharing(ctx, token, "First", true)
	if err != nil || *rotated.Link == *a.Link {
		t.Fatal("code not rotated", err)
	}
	fresh, err := first.CaptureReferral(ctx, code, "IRL", "", now)
	if err != nil || fresh.Token != "" {
		t.Fatal("retired code captured new visitor", err)
	}
	retained, err := first.CaptureReferral(ctx, code, "IRL", touch.Token, now)
	if err != nil || retained.Token != touch.Token {
		t.Fatal("rotation invalidated captured touch", err)
	}
	renewed, err := first.CaptureReferral(
		ctx,
		strings.TrimPrefix(*rotated.Link, first.Origin+"/r/"),
		"IRL",
		touch.Token,
		now.Add(41*24*time.Hour),
	)
	if err != nil || renewed.Token == touch.Token || renewed.Token == "" {
		t.Fatal("expired token repurposed", err)
	}
	if _, err = pool.Exec(ctx, "UPDATE donors SET disabled=true WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	if _, err = first.Sharing(ctx, token); !errors.Is(err, ErrDenied) {
		t.Fatal("disabled donor can share", err)
	}
	// The token now points at Second via last-touch; a disabled referrer's name
	// must not be disclosed on a subsequent capture.
	if _, err = pool.Exec(ctx, "UPDATE donors SET disabled=true WHERE id=$1", secondID); err != nil {
		t.Fatal(err)
	}
	existing, err := first.CaptureReferral(ctx, code, "LINK", touch.Token, now)
	if err != nil || existing.PublicName != nil {
		t.Fatal("disabled donor name disclosed", err)
	}
}

func TestConcurrentSharingKeepsOnePermanentCode(t *testing.T) {
	s, token, _ := donorServiceFixture(t)
	results := make(chan Sharing, 2)
	failures := make(chan error, 2)
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			value, err := s.SaveSharing(context.Background(), token, "Sam", false)
			results <- value
			failures <- err
		}()
	}
	close(start)
	a, b := <-results, <-results
	for i := 0; i < 2; i++ {
		if err := <-failures; err != nil {
			t.Fatal(err)
		}
	}
	if a.Link == nil || b.Link == nil || *a.Link != *b.Link {
		t.Fatal("concurrent sharing returned different codes")
	}
}

func TestSaveProfileDerivesTimezoneAndValidates(t *testing.T) {
	s, token, _ := donorServiceFixture(t)
	ctx := context.Background()

	// A new donor's role is DONOR and identity fields are empty.
	profile, err := s.Profile(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if profile.AccountRole != "DONOR" {
		t.Fatalf("new donor role = %q; want DONOR", profile.AccountRole)
	}
	if profile.Zip != nil || profile.City != nil || profile.Timezone != nil || profile.Phone != nil {
		t.Fatalf("new donor identity fields not empty: %+v", profile)
	}

	// A valid Chicago ZIP derives America/Chicago; city and phone are stored as given.
	profile, err = s.SaveProfile(ctx, token, ProfileUpdate{Zip: "60601", City: "Chicago", Phone: "+13125550123"})
	if err != nil {
		t.Fatal(err)
	}
	if profile.Zip == nil || *profile.Zip != "60601" {
		t.Errorf("zip = %v; want 60601", profile.Zip)
	}
	if profile.Timezone == nil || *profile.Timezone != "America/Chicago" {
		t.Errorf("timezone = %v; want America/Chicago", profile.Timezone)
	}
	if profile.City == nil || *profile.City != "Chicago" {
		t.Errorf("city = %v; want Chicago", profile.City)
	}
	if profile.Phone == nil || *profile.Phone != "+13125550123" {
		t.Errorf("phone = %v; want +13125550123", profile.Phone)
	}
	// Role is untouched by a profile save (immutability, PRD §3).
	if profile.AccountRole != "DONOR" {
		t.Errorf("role changed to %q after profile save", profile.AccountRole)
	}

	// Switching to a San Francisco ZIP re-derives the timezone.
	profile, err = s.SaveProfile(ctx, token, ProfileUpdate{Zip: "94103"})
	if err != nil {
		t.Fatal(err)
	}
	if profile.Timezone == nil || *profile.Timezone != "America/Los_Angeles" {
		t.Errorf("timezone = %v; want America/Los_Angeles", profile.Timezone)
	}
	// Omitted city/phone are cleared (every save is a full replace).
	if profile.City != nil || profile.Phone != nil {
		t.Errorf("city/phone not cleared on replace: city=%v phone=%v", profile.City, profile.Phone)
	}

	// A blank ZIP clears both the ZIP and the derived timezone together.
	profile, err = s.SaveProfile(ctx, token, ProfileUpdate{Zip: ""})
	if err != nil {
		t.Fatal(err)
	}
	if profile.Zip != nil || profile.Timezone != nil {
		t.Errorf("blank zip did not clear zip/timezone: zip=%v tz=%v", profile.Zip, profile.Timezone)
	}

	// Invalid inputs are rejected with the right typed errors, and nothing is written.
	for _, tc := range []struct {
		name   string
		update ProfileUpdate
		want   error
	}{
		{"bad zip", ProfileUpdate{Zip: "123"}, ErrZip},
		{"unresolvable zip", ProfileUpdate{Zip: "00000"}, ErrZip},
		{"bad phone", ProfileUpdate{Phone: "5550123"}, ErrPhone},
		{"long city", ProfileUpdate{City: strings.Repeat("x", 81)}, ErrCity},
	} {
		if _, err := s.SaveProfile(ctx, token, tc.update); !errors.Is(err, tc.want) {
			t.Errorf("%s: error = %v; want %v", tc.name, err, tc.want)
		}
	}
}

// TestVolunteerPreferenceRoundTrip covers E-1: the volunteer opt-in and the
// chosen interests are stored on the versioned preference record, mirrored to
// the consent-event table, and read back; the default is off with an empty
// (non-null) interests set.
func TestVolunteerPreferenceRoundTrip(t *testing.T) {
	s, token, id := donorServiceFixture(t)
	pool := database(t)
	ctx := context.Background()

	profile, err := s.Profile(ctx, token)
	if err != nil || profile.Volunteer || profile.VolunteerInterests == nil || len(profile.VolunteerInterests) != 0 {
		t.Fatalf("default volunteer prefs: %+v %v", profile.Preferences, err)
	}

	saved, err := s.SavePreferences(ctx, token, Preferences{
		Version: 0, PushReferralConverted: true,
		Volunteer: true, VolunteerInterests: []string{"food-bank", "mentoring"},
	}.Replace())
	if err != nil || saved.Version != 1 || !saved.Volunteer ||
		len(saved.VolunteerInterests) != 2 || saved.VolunteerInterests[0] != "food-bank" || saved.VolunteerInterests[1] != "mentoring" {
		t.Fatalf("saved volunteer prefs: %+v %v", saved.Preferences, err)
	}

	// The consent-event mirror records the change.
	var vol bool
	var interests []string
	if err = pool.QueryRow(ctx,
		"SELECT volunteer,volunteer_interests FROM donor_preference_events WHERE donor_id=$1 AND version=1", id,
	).Scan(&vol, &interests); err != nil || !vol || len(interests) != 2 {
		t.Fatalf("consent event mirror: %v %v %v", vol, interests, err)
	}

	// Turning volunteer back off clears the pool membership but the record keeps versioning.
	off, err := s.SavePreferences(ctx, token, Preferences{Version: 1, PushReferralConverted: true}.Replace())
	if err != nil || off.Version != 2 || off.Volunteer || len(off.VolunteerInterests) != 0 {
		t.Fatalf("opt back out: %+v %v", off.Preferences, err)
	}
}

// TestPreferenceChangeKeepsOmittedChoices covers a client that predates a
// choice: saving without it must not reset it.
func TestPreferenceChangeKeepsOmittedChoices(t *testing.T) {
	s, token, _ := donorServiceFixture(t)
	ctx := context.Background()
	on, off := true, false
	interests := []string{"mentoring"}
	if _, err := s.SavePreferences(ctx, token, PreferenceChange{
		Version: 0, PushStreakLapsed: &off, RankingsOptedOut: &on, NetworkVisible: &off,
		Volunteer: &on, VolunteerInterests: &interests,
	}); err != nil {
		t.Fatal(err)
	}
	saved, err := s.SavePreferences(ctx, token, PreferenceChange{Version: 1, MarketingEmails: true})
	if err != nil || saved.Version != 2 || !saved.MarketingEmails || saved.PushStreakLapsed || !saved.PushBadgeEarned ||
		!saved.RankingsOptedOut || saved.NetworkVisible || !saved.Volunteer ||
		len(saved.VolunteerInterests) != 1 || saved.VolunteerInterests[0] != "mentoring" {
		t.Fatalf("omitted choices changed: %+v %v", saved.Preferences, err)
	}
}

func TestPreferenceSavesRefuseStaleAndOutOfRangeVersionsWithoutAuditing(t *testing.T) {
	s, token, id := donorServiceFixture(t)
	pool := database(t)
	ctx := context.Background()
	for _, version := range []int32{-1, 1, math.MaxInt32} {
		if _, err := s.SavePreferences(ctx, token, PreferenceChange{Version: version, MarketingEmails: true}); !errors.Is(
			err,
			ErrPreferenceConflict,
		) {
			t.Fatalf("version %d against an unsaved record: %v", version, err)
		}
	}
	if _, err := s.SavePreferences(ctx, token, PreferenceChange{Version: 0, MarketingEmails: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SavePreferences(ctx, token, PreferenceChange{Version: 0, MarketingEmails: false}); !errors.Is(
		err,
		ErrPreferenceConflict,
	) {
		t.Fatalf("a save from before the last one overwrote it: %v", err)
	}
	profile, err := s.Profile(ctx, token)
	if err != nil || profile.Version != 1 || !profile.MarketingEmails {
		t.Fatalf("refused save changed the record: %+v %v", profile.Preferences, err)
	}
	var events int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM donor_preference_events WHERE donor_id=$1", id).Scan(&events); err != nil ||
		events != 1 {
		t.Fatalf("%d consent events for one accepted save: %v", events, err)
	}
}

func TestCaptureReferralRefusesUnknownChannelsAndMalformedCodes(t *testing.T) {
	s, token, _ := donorServiceFixture(t)
	ctx := context.Background()
	now := time.Now()
	shared, err := s.SaveSharing(ctx, token, "Sam", false)
	if err != nil {
		t.Fatal(err)
	}
	code := *shared.ReferralCode
	if _, err = s.CaptureReferral(ctx, code, "EMAIL", "", now); !errors.Is(err, payments.ErrFacts) {
		t.Fatalf("an unknown channel was recorded: %v", err)
	}
	for _, bad := range []string{"", "has space", "semi;colon", strings.Repeat("a", 65), "tab\tcode"} {
		if capture, err := s.CaptureReferral(ctx, bad, "LINK", "", now); err != nil || capture.Token != "" || capture.PublicName != nil {
			t.Fatalf("code %q made a visitor: %+v %v", bad, capture, err)
		}
	}
	touch, err := s.CaptureReferral(ctx, code, "LINK", "", now)
	if err != nil || touch.Token == "" {
		t.Fatal(touch, err)
	}
	// A malformed code never replaces the earlier touch; it keeps the visitor's token while it lasts.
	kept, err := s.CaptureReferral(ctx, "has space", "IRL", touch.Token, now.Add(time.Hour))
	if err != nil || kept.Token != touch.Token {
		t.Fatalf("a malformed code dropped the visitor's touch: %+v %v", kept, err)
	}
	expired, err := s.CaptureReferral(ctx, "has space", "IRL", touch.Token, now.Add(referralWindow+time.Hour))
	if err != nil || expired.Token != "" {
		t.Fatalf("an expired touch was kept alive by a malformed code: %+v %v", expired, err)
	}
}

func TestGiftPagesRefuseOffsetsThatCannotBePaged(t *testing.T) {
	s, token, _ := donorServiceFixture(t)
	ctx := context.Background()
	for _, offset := range []int64{-1, math.MaxInt64, math.MaxInt64 - pageSize + 1} {
		if _, err := s.Gifts(ctx, token, offset); !errors.Is(err, payments.ErrFacts) {
			t.Errorf("offset %d: %v; want payments.ErrFacts", offset, err)
		}
	}
	if gifts, err := s.Gifts(ctx, token, math.MaxInt64-pageSize); err != nil || len(gifts.Gifts) != 0 || gifts.NextOffset != nil {
		t.Fatalf("the last representable offset: %+v %v", gifts, err)
	}
}

func TestPageKeepsAFullPageAndReportsWhereTheNextBegins(t *testing.T) {
	rows := make([]int, pageSize+1)
	if got, next := page(rows, 100); len(got) != pageSize || next == nil || *next != 100+pageSize {
		t.Fatalf("look-ahead row: %d rows, next %v", len(got), next)
	}
	if got, next := page(rows[:pageSize], 100); len(got) != pageSize || next != nil {
		t.Fatalf("exactly one full page must not promise another: %d rows, next %v", len(got), next)
	}
	if got, next := page([]int(nil), 0); len(got) != 0 || next != nil {
		t.Fatalf("empty page: %d rows, next %v", len(got), next)
	}
}
