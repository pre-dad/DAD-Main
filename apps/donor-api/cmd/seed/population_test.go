package main

import (
	"math"
	"testing"
	"time"

	"dad/donor-api/internal/store"
)

var asOf = time.Date(2026, 10, 7, 15, 4, 5, 0, time.UTC)

func TestGenerateIsDeterministic(t *testing.T) {
	a, b, other := generate(500, 0, 9, asOf), generate(500, 0, 9, asOf), generate(500, 0, 10, asOf)
	differs := false
	for i := range a.donors {
		x, y := a.donors[i], b.donors[i]
		if x.id != y.id || x.email != y.email || x.code != y.code || len(x.gifts()) != len(y.gifts()) || x.streak != y.streak ||
			x.net() != y.net() {
			t.Fatalf("donor %d differs between runs with the same seed", i)
		}
		differs = differs || x.id != other.donors[i].id
	}
	if !differs || len(a.touches) != len(b.touches) {
		t.Fatal("seed does not determine the population")
	}
	appended := generate(10, 500, 9, asOf)
	if appended.donors[0].email != "donor-0000500@"+emailDomain || appended.donors[0].id == a.donors[0].id {
		t.Fatal("an appended population must continue the numbering with new identifiers")
	}
}

func TestPopulationHasALaunchShape(t *testing.T) {
	pop := generate(50000, 0, 1, asOf)
	var abandoned, monthly, monthly30, referred, sharers, referrers, heavy, periods, failed int
	var mostReferrals int64
	var longest, atRisk, cancelled, pastDue, refunds int
	for _, d := range pop.donors {
		switch first := d.checkouts[0]; {
		case first.status == "EXPIRED":
			abandoned++
		default:
			monthly++
			if first.amount == 3000 {
				monthly30++
			}
		}
		if g := d.firstGift(); g != nil && g.referrer != nil {
			referred++
		}
		if d.code != "" {
			sharers++
		}
		if n := d.referrals(); n > 0 {
			referrers++
			mostReferrals = max(mostReferrals, n)
			if n >= 100 {
				heavy++
			}
		}
		longest = max(longest, d.streak)
		if d.atRisk {
			atRisk++
		}
		for _, c := range d.checkouts {
			periods += len(c.periods)
			for _, p := range c.periods {
				if !p.failedAt.IsZero() {
					failed++
				}
			}
			for _, g := range c.gifts {
				if g.refund != nil {
					refunds++
				}
			}
			if c.sub != nil && c.sub.status == "canceled" {
				cancelled++
			} else if c.sub != nil && c.sub.status == "past_due" {
				pastDue++
			}
		}
	}
	share := func(n int) float64 { return float64(n) / float64(len(pop.donors)) }
	for name, check := range map[string]struct{ got, want, tolerance float64 }{
		"monthly":      {share(monthly), 1 - abandonedShare, 0.02},
		"abandoned":    {share(abandoned), abandonedShare, 0.01},
		"$30 monthly":  {float64(monthly30) / float64(monthly), 0.60, 0.02},
		"referred":     {share(referred), referredShare * (1 - abandonedShare) * (1 - lateTouchShare), 0.01},
		"sharers":      {share(sharers), (1 - abandonedShare) * claimShare * sharerShare, 0.01},
		"failed rates": {float64(failed) / float64(periods), renewalFailure, 0.01},
	} {
		if math.Abs(check.got-check.want) > check.tolerance {
			t.Errorf("%s share %.3f, want %.3f±%.3f", name, check.got, check.want, check.tolerance)
		}
	}
	if share(referrers) > 0.2 || heavy == 0 || mostReferrals < 200 {
		t.Errorf("referrals are not heavy-tailed: %d referrers, %d with 100+, most %d", referrers, heavy, mostReferrals)
	}
	if longest < 20 || atRisk == 0 || cancelled == 0 || pastDue == 0 || refunds == 0 {
		t.Errorf(
			"missing recognition edge cases: longest streak %d, %d at risk, %d cancelled, %d past due, %d refunds",
			longest,
			atRisk,
			cancelled,
			pastDue,
			refunds,
		)
	}
}

// TestFactsKeepTheSchemaAndDomainRules checks what the database and the
// recognition rules would otherwise reject or misread.
func TestFactsKeepTheSchemaAndDomainRules(t *testing.T) {
	pop := generate(20000, 0, 3, asOf)
	ids, codes, objects := map[[16]byte]bool{}, map[string]bool{}, map[string]bool{}
	unique := func(seen map[[16]byte]bool, id [16]byte) {
		if seen[id] {
			t.Fatalf("duplicate identifier %x", id)
		}
		seen[id] = true
	}
	for _, d := range pop.donors {
		unique(ids, d.id.Bytes)
		if d.code != "" {
			if codes[d.code] || len(d.code) != 32 || d.publicName == "" || d.claimedAt.IsZero() || !d.claimedAt.Before(d.sharingAt) {
				t.Fatalf("donor %d shares without a unique code after claiming", d.index)
			}
			codes[d.code] = true
		}
		monthly := 0
		for _, c := range d.checkouts {
			unique(ids, c.id.Bytes)
			if !store.ValidGiftAmount(c.amount) || (c.referrer == nil) != (c.kind == "") || c.referrer == d {
				t.Fatalf("checkout for donor %d breaks a checkout constraint", d.index)
			}
			if c.referrer != nil && !c.referrer.sharingAt.Before(c.expiresAt.Add(-referralWindow)) {
				t.Fatalf("donor %d was referred before the referrer had a code", d.index)
			}
			if c.frequency == "MONTHLY" && c.status != "EXPIRED" {
				monthly++
			}
			for i, p := range c.periods {
				if span := p.ends.Sub(p.due); span < 27*day || span > 32*day || p.due.After(pop.now) {
					t.Fatalf("period of %s for donor %d", span, d.index)
				}
				if i > 0 && p.due.Before(c.periods[i-1].ends) {
					t.Fatalf("overlapping periods for donor %d", d.index)
				}
				if p.paid == p.paidAt.IsZero() || (!p.paid && p.failedAt.IsZero()) {
					t.Fatalf("period recorded for donor %d without a reported outcome", d.index)
				}
			}
			for _, g := range c.gifts {
				if objects[g.object] || g.amount != c.amount || g.paidAt.After(pop.now) || g.paidAt.Before(c.createdAt) {
					t.Fatalf("gift for donor %d is not a valid collected payment", d.index)
				}
				objects[g.object] = true
				attributed := c.referrer != nil && g.paidAt.Before(c.expiresAt)
				if attributed != (g.referrer != nil) {
					t.Fatalf("gift for donor %d does not record attribution as of its charge", d.index)
				}
				if r := g.refund; r != nil {
					last := r.observations[len(r.observations)-1]
					if r.amount > g.amount || r.observations[0].at.Before(g.paidAt) || last.at.After(pop.now) || last.status != r.status {
						t.Fatalf("refund for donor %d is inconsistent", d.index)
					}
				}
			}
			if s := c.sub; s != nil && s.status == "canceled" && s.stopsAt.IsZero() {
				t.Fatalf("cancelled subscription for donor %d keeps renewing", d.index)
			}
		}
		if monthly > 1 || d.streak < 0 {
			t.Fatalf("donor %d has %d monthly subscriptions and streak %d", d.index, monthly, d.streak)
		}
	}
	for _, touch := range pop.touches {
		if touch.referrer.code == "" || touch.at.Before(touch.referrer.sharingAt) || touch.at.After(pop.now) {
			t.Fatal("a visit used a code that did not exist yet")
		}
	}
}

func TestAddMonthsKeepsTheAnniversaryOrMonthEnd(t *testing.T) {
	anchor := time.Date(2026, 1, 31, 9, 30, 0, 0, time.UTC)
	for months, want := range map[int]string{1: "2026-02-28", 2: "2026-03-31", 3: "2026-04-30", 13: "2027-02-28", 25: "2028-02-29"} {
		if got := addMonths(anchor, months).Format(time.DateOnly); got != want {
			t.Errorf("%d months after %s: %s, want %s", months, anchor.Format(time.DateOnly), got, want)
		}
	}
}

func TestReferralCodesFollowInfluence(t *testing.T) {
	pop := generate(5000, 0, 1, asOf)
	codes := pop.referralCodes(fixtureCodes)
	visits := map[string]int{}
	for _, code := range codes {
		visits[code]++
	}
	var top *donor
	for _, d := range pop.donors {
		if d.code != "" && (top == nil || d.influence > top.influence) {
			top = d
		}
	}
	if len(codes) != fixtureCodes || visits[top.code] < 10 || len(visits) < 100 {
		t.Fatalf("codes do not follow influence: %d codes, %d distinct, top sharer %d", len(codes), len(visits), visits[top.code])
	}
}

// TestEngagementLayerKeepsItsConstraints checks the partners/employees/events/
// reservations the engagement layer adds satisfy the schema and domain rules
// the event, partner and employee reads depend on.
func TestEngagementLayerKeepsItsConstraints(t *testing.T) {
	pop := generate(20000, 0, 7, asOf)
	if len(pop.partners) != partnerCount || len(pop.initiatives) != initiativeCount || len(pop.events) != eventCount {
		t.Fatalf("engagement sizes: %d partners, %d initiatives, %d events", len(pop.partners), len(pop.initiatives), len(pop.events))
	}

	ids := map[[16]byte]bool{}
	codes := map[string]bool{}
	unique := func(id [16]byte) {
		if ids[id] {
			t.Fatalf("duplicate engagement identifier %x", id)
		}
		ids[id] = true
	}

	oneEmployer := map[[16]byte]bool{} // a donor belongs to at most one partner
	var employees, onTeam int
	for _, p := range pop.partners {
		unique(p.id.Bytes)
		if p.companyCode == "" || codes[p.companyCode] || p.workforce < 0 || p.matchRateBP < 0 || p.matchCap < 0 {
			t.Fatalf("partner %d breaks a signup constraint", p.index)
		}
		codes[p.companyCode] = true
		if (p.matchRateBP == 0) != (p.matchCap == 0) {
			t.Fatalf("partner %d has a match rate/cap mismatch", p.index)
		}
		teamIDs := map[[16]byte]bool{}
		for _, tm := range p.teams {
			unique(tm.id.Bytes)
			teamIDs[tm.id.Bytes] = true
		}
		for _, e := range p.employees {
			if e.donor.claimedAt.IsZero() {
				t.Fatalf("an unclaimed donor was linked to partner %d", p.index)
			}
			if oneEmployer[e.donor.id.Bytes] {
				t.Fatalf("donor %d is linked to two partners", e.donor.index)
			}
			oneEmployer[e.donor.id.Bytes] = true
			if e.team != nil {
				if !teamIDs[e.team.id.Bytes] {
					t.Fatalf("employee of partner %d is on another partner's team", p.index)
				}
				onTeam++
			}
			if e.enrolledAt.Before(e.donor.claimedAt) || e.enrolledAt.After(pop.now) {
				t.Fatalf("employee enrolment for donor %d is out of range", e.donor.index)
			}
			employees++
		}
	}
	if employees == 0 || onTeam == 0 {
		t.Fatalf("no employees (%d) or none on a team (%d)", employees, onTeam)
	}

	var publishedMetrics int
	for _, in := range pop.initiatives {
		unique(in.id.Bytes)
		if len(in.metrics) < 2 || len(in.metrics) > 5 || in.committed < 0 {
			t.Fatalf("initiative %d breaks a metric-count or committed constraint", in.index)
		}
		for pos, m := range in.metrics {
			unique(m.id.Bytes)
			if m.pos != pos || m.pos < 0 || m.pos >= 6 {
				t.Fatalf("initiative %d metric position %d is out of range", in.index, m.pos)
			}
			publishedMetrics++
		}
	}
	if publishedMetrics == 0 {
		t.Fatal("no published initiative metrics")
	}

	var reservations, waitlisted, checkedIn, pastEvents, upcoming int
	for _, ev := range pop.events {
		unique(ev.id.Bytes)
		if !ev.endsAt.After(ev.startsAt) || ev.capacity < 0 || ev.publishedAt.After(pop.now) {
			t.Fatalf("event %d breaks a window/capacity/publish constraint", ev.index)
		}
		if ev.endsAt.After(pop.now) {
			upcoming++
		} else {
			pastEvents++
		}
		donors := map[[16]byte]bool{}
		for _, r := range ev.reservations {
			unique(r.id.Bytes)
			if donors[r.donor.id.Bytes] {
				t.Fatalf("donor %d has two reservations at event %d", r.donor.index, ev.index)
			}
			donors[r.donor.id.Bytes] = true
			if r.status != "RESERVED" && r.status != "WAITLISTED" {
				t.Fatalf("event %d reservation has status %q", ev.index, r.status)
			}
			if r.status == "WAITLISTED" {
				waitlisted++
				if ev.capacity == 0 {
					t.Fatalf("event %d has an uncapped waitlist", ev.index)
				}
			}
			if !r.checkedIn.IsZero() {
				checkedIn++
				if r.status != "RESERVED" || ev.endsAt.After(pop.now) {
					t.Fatalf("event %d has a check-in that is not a finished reservation", ev.index)
				}
				if r.checkedIn.Before(ev.startsAt) || r.checkedIn.After(ev.endsAt) {
					t.Fatalf("event %d check-in is outside the event window", ev.index)
				}
			}
			reservations++
		}
	}
	if reservations == 0 || waitlisted == 0 || checkedIn == 0 || pastEvents == 0 || upcoming == 0 {
		t.Fatalf(
			"missing engagement edge cases: %d reservations, %d waitlisted, %d checked in, %d past, %d upcoming events",
			reservations, waitlisted, checkedIn, pastEvents, upcoming,
		)
	}

	other := generate(20000, 0, 7, asOf)
	if len(other.partners) != len(pop.partners) || other.partners[0].id != pop.partners[0].id ||
		len(other.events[0].reservations) != len(pop.events[0].reservations) {
		t.Fatal("engagement layer is not reproducible from the seed")
	}
}

func TestAppendedEngagementLayersHaveUniqueIdentifiers(t *testing.T) {
	first, second := generate(10, 0, 3, asOf), generate(10, 10, 3, asOf)
	codes, slugs := map[string]bool{}, map[string]bool{}
	for _, pop := range []*population{first, second} {
		for _, p := range pop.partners {
			if codes[p.companyCode] {
				t.Fatalf("company code %s repeats across appended seeds", p.companyCode)
			}
			codes[p.companyCode] = true
		}
		for _, in := range pop.initiatives {
			if slugs[in.slug] {
				t.Fatalf("initiative slug %s repeats across appended seeds", in.slug)
			}
			slugs[in.slug] = true
		}
		for _, ev := range pop.events {
			if slugs[ev.slug] {
				t.Fatalf("event slug %s repeats across appended seeds", ev.slug)
			}
			slugs[ev.slug] = true
		}
	}
}
