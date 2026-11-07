package main

import (
	"cmp"
	"container/heap"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"dad/donor-api/internal/recognition"
)

// emailDomain marks seeded donors. The reserved .invalid TLD can never
// receive mail, and the guard treats any other address as real data.
const emailDomain = "loadtest.invalid"

const (
	day            = 24 * time.Hour
	history        = 730 * day // the oldest donor signed up two years ago
	referralWindow = 30 * day
)

// Shares of the population and per-event rates. They describe a referral-led
// launch: most donors give monthly at the preselected $30, a minority share a
// code, and sharing influence is heavy-tailed.
const (
	abandonedShare       = 0.08 // started checkout, never paid
	referredShare        = 0.45 // arrived through a referral touch
	lateTouchShare       = 0.03 // of referred: the window had lapsed by checkout
	irlShare             = 0.35
	claimShare           = 0.60 // givers who claim an account
	sharerShare          = 0.40 // claimed donors who choose a public name
	preferenceShare      = 0.30 // claimed donors who saved notification choices
	renewalFailure       = 0.05
	cancelHazard         = 0.015 // per renewal, cancelled by the donor at period end
	scheduledCancelShare = 0.02  // of live subscriptions, ending at the current period's end
	unreportedShare      = 0.15  // of renewals due in the last 7 days
	refundShare          = 0.015
	influenceAlpha       = 1.16 // Pareto shape: a few sharers refer hundreds
	influenceCeiling     = 3000
)

// Engagement layer: partners, employees, teams, events and initiatives (Phases
// 4–6). These exercise the leaderboard/rollup, near-city event and rotation
// reads under load (PRD §13 risk #6 / dev-plan 7.3). They are sized as small
// fixed sets scaled to the population so a 50k seed carries a realistic
// employer/event population without a second heavy generator.
const (
	partnerCount     = 40   // approved partner orgs
	teamsPerPartner  = 6    // teams each partner runs
	employeeShare    = 0.18 // of claimed donors, linked to an employer
	initiativeCount  = 120  // published, active initiatives
	eventCount       = 200  // published events, a mix of past and upcoming
	eventPastShare   = 0.55 // of events already finished (so check-ins accrue)
	reservedShare    = 0.22 // of claimed donors, holding at least one reservation
	checkInShare     = 0.70 // of reservations at a finished event, checked in
	eventWindow      = 120 * day
	eventDurationMin = 90 * time.Minute
	eventDurationMax = 4 * time.Hour
)

// Monthly gift amounts in cents by weight: the $1, $3 and $5 a day presets,
// a few other daily amounts, and 0 for any whole-dollar daily amount up to $30.
var monthlyAmounts = weighted[int64]{{3000, 60}, {9000, 14}, {15000, 10}, {6000, 8}, {30000, 3}, {90000, 1}, {0, 4}}

var names = []string{"Ada", "Amara", "Ben", "Carmen", "Chen", "Dara", "Eli", "Fatima", "Gabe", "Hana", "Ines", "Jamal",
	"Kai", "Lena", "Mateo", "Mira", "Nadia", "Omar", "Priya", "Quinn", "Rosa", "Sam", "Tariq", "Uma", "Vera", "Wes",
	"Xin", "Yara", "Zane", "Ana", "Bo", "Cleo", "Dev", "Emi", "Finn", "Gia", "Hugo", "Iris", "Jo", "Kofi"}

type weighted[T any] []struct {
	value  T
	weight float64
}

func (w weighted[T]) pick(r *rand.Rand) T {
	total := 0.0
	for _, option := range w {
		total += option.weight
	}
	x := r.Float64() * total
	for _, option := range w {
		if x -= option.weight; x < 0 {
			return option.value
		}
	}
	return w[len(w)-1].value
}

type donor struct {
	index      int
	id         pgtype.UUID
	email      string
	createdAt  time.Time
	claimedAt  time.Time // zero while unclaimed
	sharingAt  time.Time // when the public name and code were chosen
	publicName string
	code       string
	influence  float64
	preference bool
	city       string // zero unless the donor set a profile location
	zip        string // five-digit US ZIP, its SCF matching an event city
	timezone   string
	checkouts  []*checkout

	// Expected dashboard figures, derived from how the facts were generated
	// rather than by the projection code, so tests can compare the two.
	referred    map[string]referralTally // by referral kind
	streak      int
	atRisk      bool
	streakKnown bool
}

type referralTally struct{ count, minor int64 }

type checkout struct {
	id, attempt  pgtype.UUID
	donor        *donor
	amount       int64
	frequency    string
	createdAt    time.Time
	referrer     *donor
	kind         string
	expiresAt    time.Time
	status       string // attempt status: EXPIRED, COMPLETE or CANCELLED
	session      string
	subscription string
	periods      []*period
	gifts        []*gift
	sub          *subscription
	cancellation *cancellation
}

type period struct {
	id        pgtype.UUID
	invoice   string
	due, ends time.Time
	paid      bool
	failedAt  time.Time // zero unless a renewal attempt failed
	paidAt    time.Time
}

type gift struct {
	id            pgtype.UUID
	object        string
	paymentIntent string
	amount        int64
	frequency     string
	paidAt        time.Time
	period        *period
	referrer      *donor
	kind          string
	refund        *refund
}

type refund struct {
	id           pgtype.UUID
	stripeID     string
	amount       int64
	status       string
	observations []refundObservation
}

type refundObservation struct {
	status string
	at     time.Time
}

type subscription struct {
	id        pgtype.UUID
	status    string
	periodEnd time.Time
	stopsAt   time.Time // zero when renewals continue
	anchor    time.Time
	observed  time.Time
}

type cancellation struct {
	id        pgtype.UUID
	periodEnd time.Time
	created   time.Time
	completed time.Time
}

// touch is a referral visit, whether or not it led to a gift.
type touch struct {
	id        pgtype.UUID
	tokenHash string
	referrer  *donor
	kind      string
	at        time.Time
}

type population struct {
	now     time.Time
	donors  []*donor
	touches []touch

	partners    []*partner
	initiatives []*initiative
	events      []*event
}

// partner is an approved partner org (donor_partner_signups with approved_at),
// its teams, and the employees linked to it.
type partner struct {
	id          pgtype.UUID
	index       int
	num         int // global, offset-inclusive — unique across appended seeds
	name        string
	companyCode string
	workforce   int
	matchRateBP int
	matchCap    int64
	approvedAt  time.Time
	teams       []*team
	employees   []*employee
}

type team struct {
	id   pgtype.UUID
	name string
}

// employee links a donor to a partner (donor_partner_employees), optionally on
// a team. The donor keeps giving as themselves; the link only rolls their
// gifts up to the employer.
type employee struct {
	donor      *donor
	team       *team
	enrolledAt time.Time
}

// initiative is a published, active funded program (donor_initiatives) with a
// few published verified metrics, optionally run by a partner.
type initiative struct {
	id        pgtype.UUID
	index     int
	slug      string
	partner   *partner
	startedOn time.Time
	committed int64
	metrics   []initiativeMetric
}

type initiativeMetric struct {
	id    pgtype.UUID
	pos   int
	label string
	value int64
	unit  string
}

// event is a published event (donor_events) and the reservations against it.
type event struct {
	id           pgtype.UUID
	index        int
	slug         string
	kind         string
	city, zip    string
	startsAt     time.Time
	endsAt       time.Time
	capacity     int
	publishedAt  time.Time
	reservations []*reservation
}

// reservation is one donor's seat at an event (donor_event_reservations). A
// RESERVED/WAITLISTED donor may have checked in at a finished event.
type reservation struct {
	id         pgtype.UUID
	donor      *donor
	status     string
	reservedAt time.Time
	checkedIn  time.Time // zero unless the donor checked in
}

type generator struct {
	r      *rand.Rand
	now    time.Time
	offset int
	pop    *population
}

// generate builds a deterministic population of n donors as of now. offset
// numbers the donors after those an earlier run created, so appended
// populations get distinct emails and identifiers.
func generate(n, offset int, seed uint64, now time.Time) *population {
	now = now.UTC().Truncate(time.Second)
	g := &generator{r: rand.New(rand.NewPCG(seed, uint64(offset))), now: now, offset: offset}
	g.pop = &population{now: now}
	ages := make([]time.Duration, n)
	for i := range ages {
		// Squaring skews signups toward the present, as referral growth compounds.
		u := g.r.Float64()
		ages[i] = max(time.Hour, time.Duration(u*u*float64(history)).Truncate(time.Second))
	}
	slices.SortFunc(ages, func(a, b time.Duration) int { return int(b - a) })
	sharers := &sharerPool{}
	for i, age := range ages {
		d := &donor{index: offset + i, id: g.uuid(), createdAt: now.Add(-age)}
		d.email = fmt.Sprintf("donor-%07d@%s", d.index, emailDomain)
		g.pop.donors = append(g.pop.donors, d)
		g.join(d, sharers)
	}
	g.visits()
	g.expectReferrals()
	g.engagement()
	return g.pop
}

// join gives d a first checkout, possibly referred by an active sharer, and
// the giving history that followed.
func (g *generator) join(d *donor, sharers *sharerPool) {
	c := &checkout{id: g.uuid(), attempt: g.uuid(), donor: d, createdAt: d.createdAt}
	if g.r.Float64() < referredShare {
		lapsed := g.r.Float64() < lateTouchShare
		at := d.createdAt.Add(-g.seconds(min(25*day, time.Duration(g.r.ExpFloat64()*float64(18*time.Hour)))))
		if lapsed {
			at = d.createdAt.Add(-referralWindow - g.seconds(30*day))
		}
		sharers.activate(at)
		if referrer := sharers.pick(g.r, at); referrer != nil {
			kind := g.kind()
			g.pop.touches = append(g.pop.touches, touch{g.uuid(), g.hash(), referrer, kind, at})
			if !lapsed {
				c.referrer, c.kind, c.expiresAt = referrer, kind, at.Add(referralWindow)
			}
		}
	}
	d.checkouts = append(d.checkouts, c)
	switch x := g.r.Float64(); {
	case x < abandonedShare:
		c.amount, c.frequency, c.status = g.amount(monthlyAmounts), "MONTHLY", "EXPIRED"
		return
	default:
		c.amount, c.frequency, c.status = g.amount(monthlyAmounts), "MONTHLY", "COMPLETE"
		g.monthly(c)
	}
	first := c.gifts[0].paidAt
	if g.r.Float64() < claimShare {
		if at := first.Add(g.seconds(min(7*day, time.Duration(g.r.ExpFloat64()*float64(6*time.Hour))))); at.Before(g.now) {
			d.claimedAt = at
			d.preference = g.r.Float64() < preferenceShare
		}
	}
	if !d.claimedAt.IsZero() && g.r.Float64() < sharerShare {
		if at := d.claimedAt.Add(g.seconds(2 * day)); at.Before(g.now) {
			d.sharingAt, d.publicName, d.code = at, names[g.r.IntN(len(names))], g.code()
			d.influence = min(influenceCeiling, math.Pow(1-g.r.Float64(), -1/influenceAlpha))
			sharers.add(d)
		}
	}
	// Fully refunded renewals are checked by the recognition scenarios instead.
	d.streakKnown = true
	for _, gift := range d.gifts() {
		g.maybeRefund(gift)
		if r := gift.refund; gift.frequency == "MONTHLY" && r != nil && r.status == "SUCCEEDED" && r.amount == gift.amount {
			d.streakKnown = false
		}
	}
}

func (g *generator) addGift(c *checkout, gift *gift) {
	if c.referrer != nil && gift.paidAt.Before(c.expiresAt) {
		gift.referrer, gift.kind = c.referrer, c.kind
	}
	c.gifts = append(c.gifts, gift)
}

// monthly bills renewals on the subscription's anniversary until now, with
// failures recovered within or after grace, unrecovered failures, donor
// cancellations, and renewals the processor has not reported yet.
func (g *generator) monthly(c *checkout) {
	d := c.donor
	c.session, c.subscription = g.stripeID("cs_test_"), g.stripeID("sub_")
	anchor := c.createdAt.Add(g.seconds(5 * time.Minute))
	c.sub = &subscription{id: g.uuid(), status: "active", anchor: anchor}
	var lastEnds time.Time
	run := 0
	for k := 0; ; k++ {
		due, ends := addMonths(anchor, k), addMonths(anchor, k+1)
		if due.After(g.now) {
			break
		}
		continuous := due.Equal(lastEnds)
		if continuous && g.r.Float64() < cancelHazard {
			created := due.Add(-g.seconds(20 * day))
			c.cancellation = &cancellation{id: g.uuid(), periodEnd: due, created: created, completed: created.Add(time.Minute)}
			c.sub.stopsAt = due
			break
		}
		if continuous && g.now.Sub(due) < recognition.Grace && g.r.Float64() < unreportedShare {
			break
		}
		p := &period{id: g.uuid(), invoice: g.stripeID("in_"), due: due, ends: ends}
		processorCancels := false
		switch {
		case k == 0:
			p.paidAt = due.Add(g.seconds(time.Minute))
		case g.r.Float64() >= renewalFailure:
			p.paidAt = due.Add(time.Minute + g.seconds(90*time.Minute))
		default:
			p.failedAt = due.Add(g.seconds(time.Hour))
			switch x := g.r.Float64(); {
			case x < 0.60:
				p.paidAt = due.Add(day + g.seconds(12*day))
			case x < 0.75:
				p.paidAt = due.Add(15*day + g.seconds(10*day))
			default:
				processorCancels = g.r.Float64() < 0.5
			}
		}
		if p.failedAt.After(g.now) {
			p.failedAt = time.Time{}
		}
		if p.paidAt.After(g.now) {
			p.paidAt = time.Time{}
		}
		if p.paid = !p.paidAt.IsZero(); p.paid {
			g.addGift(
				c,
				&gift{
					id:            g.uuid(),
					object:        p.invoice,
					paymentIntent: g.stripeID("pi_"),
					amount:        c.amount,
					frequency:     "MONTHLY",
					paidAt:        p.paidAt,
					period:        p,
				},
			)
		}
		if !p.paid && p.failedAt.IsZero() {
			break // the processor has reported neither outcome yet
		}
		c.periods = append(c.periods, p)
		if !continuous {
			run = 0
		}
		d.atRisk = false
		switch {
		case p.paid && p.paidAt.Before(due.Add(recognition.Grace)):
			run++
		case p.paid:
			run = 1
		case !g.now.Before(due.Add(recognition.Grace)):
			run = 0
		case run > 0:
			d.atRisk = true
		}
		lastEnds = ends
		if processorCancels && !ends.After(g.now) {
			c.sub.status, c.sub.stopsAt = "canceled", ends
			break
		}
	}
	last := c.periods[len(c.periods)-1]
	c.sub.periodEnd, c.sub.observed = last.ends, cmp.Or(last.paidAt, last.failedAt)
	if c.sub.status == "active" && !last.paid {
		c.sub.status = "past_due"
	}
	if c.sub.stopsAt.IsZero() && last.ends.After(g.now) && g.r.Float64() < scheduledCancelShare {
		created := last.due.Add(g.seconds(g.now.Sub(last.due)))
		c.cancellation = &cancellation{id: g.uuid(), periodEnd: last.ends, created: created, completed: created.Add(time.Minute)}
		c.sub.stopsAt = last.ends
	}
	if !c.sub.stopsAt.IsZero() && !c.sub.stopsAt.After(g.now) {
		c.sub.status, c.status = "canceled", "CANCELLED"
		c.sub.periodEnd = c.sub.stopsAt
	}
	next := lastEnds
	stopped := !c.sub.stopsAt.IsZero() && !c.sub.stopsAt.After(g.now) && c.sub.stopsAt.Equal(next)
	if stopped {
		run, d.atRisk = 0, false
	}
	d.streak = run
}

func (g *generator) maybeRefund(gift *gift) {
	if g.r.Float64() >= refundShare {
		return
	}
	requested := gift.paidAt.Add(time.Hour + g.seconds(20*day))
	if requested.After(g.now) {
		return
	}
	amount := gift.amount
	if g.r.Float64() < 0.3 {
		amount = max(1, gift.amount/2)
	}
	r := &refund{id: g.uuid(), stripeID: g.stripeID("re_"), amount: amount, status: "PENDING"}
	r.observations = append(r.observations, refundObservation{"PENDING", requested})
	settled := requested.Add(time.Hour + g.seconds(5*day))
	if !settled.After(g.now) {
		r.status = weighted[string]{{"SUCCEEDED", 85}, {"FAILED", 5}, {"CANCELED", 2}, {"PENDING", 8}}.pick(g.r)
		if r.status != "PENDING" {
			r.observations = append(r.observations, refundObservation{r.status, settled})
		}
	}
	gift.refund = r
}

// visits adds the referral touches that did not lead to a gift: several per
// conversion, plus a few for every sharer.
func (g *generator) visits() {
	conversions := map[*donor]int{}
	for _, t := range g.pop.touches {
		conversions[t.referrer]++
	}
	for _, d := range g.pop.donors {
		if d.code == "" {
			continue
		}
		visits := g.r.IntN(3)
		for range conversions[d] {
			visits += int(g.r.ExpFloat64() * 6)
		}
		for range visits {
			at := d.sharingAt.Add(g.seconds(g.now.Sub(d.sharingAt)))
			g.pop.touches = append(g.pop.touches, touch{g.uuid(), g.hash(), d, g.kind(), at})
		}
	}
}

// expectReferrals tallies, for each referrer, the donors whose first gift
// carried their attribution, as the dashboard should report them.
func (g *generator) expectReferrals() {
	for _, d := range g.pop.donors {
		first := d.firstGift()
		if first == nil || first.referrer == nil {
			continue
		}
		r, net := first.referrer, d.net()
		if r.referred == nil {
			r.referred = map[string]referralTally{}
		}
		tally := r.referred[first.kind]
		tally.minor += net
		if net > 0 {
			tally.count++
		}
		r.referred[first.kind] = tally
	}
}

// engagement builds the partners, teams, employees, initiatives, events and
// reservations that the event, partner and employee reads serve. It runs after donors are known
// so links point at real claimed donors; everything is drawn from the same
// deterministic generator, so a seed reproduces the whole population.
func (g *generator) engagement() {
	g.locateDonors()
	g.makePartners()
	g.makeInitiatives()
	g.makeEvents()
	g.linkEmployees()
	g.reserveEvents()
}

// locatedShare of claimed donors set a profile location (a location is optional,
// so not every donor has one). A located donor's city/ZIP is drawn from the
// shared cities list, so the near-city event list returns the events seeded in
// that city.
const locatedShare = 0.7

// locateDonors gives a share of claimed donors a city, a five-digit ZIP whose
// SCF matches that city, and the city's timezone — the inputs the near-city
// event query and donor-local rotation read.
func (g *generator) locateDonors() {
	for _, d := range g.pop.donors {
		if d.claimedAt.IsZero() || g.r.Float64() >= locatedShare {
			continue
		}
		loc := cities[g.r.IntN(len(cities))]
		d.city = loc.city
		d.zip = loc.zip + fmt.Sprintf("%02d", g.r.IntN(100)) // five digits, SCF preserved
		d.timezone = loc.tz
	}
}

// orgNames and eventTitles are small vocabularies the names draw from; the
// index keeps every partner and event name unique.
var orgNames = []string{
	"Northwind", "Acme", "Globex", "Initech", "Umbrella", "Hooli", "Stark", "Wayne", "Soylent", "Wonka",
	"Cyberdyne", "Tyrell", "Oscorp", "Massive Dynamic", "Vandelay", "Dunder Mifflin", "Pied Piper", "Aperture",
	"Blue Sun", "Weyland",
}

var eventKinds = weighted[string]{{"VOLUNTEER", 60}, {"COMMUNITY", 30}, {"INVITE_ONLY", 10}}

// cities pairs a city label with a three-digit ZIP region (SCF) and its IANA
// timezone, matching the coarse geo model the near-city query uses. Seeded
// donors and events draw from this list so they share geography and the
// near-city list returns events.
var cities = []struct{ city, zip, tz string }{
	{"Chicago", "606", "America/Chicago"}, {"Austin", "787", "America/Chicago"}, {"Denver", "802", "America/Denver"},
	{"Seattle", "981", "America/Los_Angeles"}, {"Atlanta", "303", "America/New_York"}, {"Boston", "021", "America/New_York"},
	{"Phoenix", "850", "America/Phoenix"}, {"Miami", "331", "America/New_York"}, {"Portland", "972", "America/Los_Angeles"},
	{"Nashville", "372", "America/Chicago"},
}

func (g *generator) makePartners() {
	for i := range partnerCount {
		n := g.offset + i // unique across appended populations, like donor indices
		p := &partner{
			id:          g.uuid(),
			index:       i,
			num:         n,
			name:        fmt.Sprintf("%s %d", orgNames[i%len(orgNames)], n),
			companyCode: fmt.Sprintf("CODE%08d", n),
			workforce:   100 + g.r.IntN(5000),
			matchRateBP: weighted[int]{{0, 30}, {2500, 25}, {5000, 25}, {10000, 20}}.pick(g.r),
			approvedAt:  g.now.Add(-g.seconds(min(history, 400*day))),
		}
		if p.matchRateBP > 0 {
			p.matchCap = int64(1+g.r.IntN(50)) * 100000 // $1k–$50k cap in cents
		}
		for t := range teamsPerPartner {
			p.teams = append(p.teams, &team{id: g.uuid(), name: fmt.Sprintf("Team %c", 'A'+byte(t))})
		}
		g.pop.partners = append(g.pop.partners, p)
	}
}

func (g *generator) makeInitiatives() {
	units := []string{"PEOPLE", "USD_CENTS", "INITIATIVES", "PARTNERS"}
	for i := range initiativeCount {
		n := g.offset + i
		in := &initiative{
			id:        g.uuid(),
			index:     i,
			slug:      fmt.Sprintf("initiative-%08d", n),
			startedOn: g.now.Add(-g.seconds(min(history, 500*day))).Truncate(day),
			committed: int64(1+g.r.IntN(500)) * 100000,
		}
		// Two in three initiatives are run by a partner; the rest are internal.
		if g.r.IntN(3) != 0 {
			in.partner = g.pop.partners[g.r.IntN(len(g.pop.partners))]
		}
		for pos := range 2 + g.r.IntN(4) { // 2–5 published metrics
			in.metrics = append(in.metrics, initiativeMetric{
				id:    g.uuid(),
				pos:   pos,
				label: fmt.Sprintf("Metric %d", pos+1),
				value: int64(10 + g.r.IntN(100000)),
				unit:  units[pos%len(units)],
			})
		}
		g.pop.initiatives = append(g.pop.initiatives, in)
	}
}

func (g *generator) makeEvents() {
	for i := range eventCount {
		n := g.offset + i
		loc := cities[i%len(cities)]
		duration := eventDurationMin + g.seconds(eventDurationMax-eventDurationMin)
		var starts time.Time
		if g.r.Float64() < eventPastShare {
			// A finished event: ended between a day ago and the window's start.
			starts = g.now.Add(-day - g.seconds(eventWindow)).Add(-duration)
		} else {
			starts = g.now.Add(day + g.seconds(eventWindow))
		}
		ev := &event{
			id:          g.uuid(),
			index:       i,
			slug:        fmt.Sprintf("event-%08d", n),
			kind:        eventKinds.pick(g.r),
			city:        loc.city,
			zip:         loc.zip + fmt.Sprintf("%02d", g.r.IntN(100)),
			startsAt:    starts,
			endsAt:      starts.Add(duration),
			capacity:    weighted[int]{{0, 30}, {20, 25}, {50, 25}, {200, 20}}.pick(g.r),
			publishedAt: starts.Add(-30 * day),
		}
		if ev.publishedAt.After(g.now) {
			ev.publishedAt = g.now.Add(-g.seconds(10 * day))
		}
		g.pop.events = append(g.pop.events, ev)
	}
}

// linkEmployees ties a share of claimed donors to a partner and one of its
// teams. A donor belongs to at most one partner (the schema's PK is donor_id).
func (g *generator) linkEmployees() {
	if len(g.pop.partners) == 0 {
		return
	}
	for _, d := range g.pop.donors {
		if d.claimedAt.IsZero() || g.r.Float64() >= employeeShare {
			continue
		}
		p := g.pop.partners[g.r.IntN(len(g.pop.partners))]
		var tm *team
		if len(p.teams) > 0 && g.r.IntN(4) != 0 { // three in four are on a team
			tm = p.teams[g.r.IntN(len(p.teams))]
		}
		at := d.claimedAt.Add(g.seconds(max(time.Second, g.now.Sub(d.claimedAt))))
		p.employees = append(p.employees, &employee{donor: d, team: tm, enrolledAt: at})
	}
}

// reserveEvents books a share of claimed donors onto events, waitlisting past
// capacity and recording check-ins at finished events. One reservation per
// (event, donor): a donor reserves distinct events, so the unique constraint
// holds.
func (g *generator) reserveEvents() {
	if len(g.pop.events) == 0 {
		return
	}
	active := map[pgtype.UUID]int{} // event -> active (reserved+waitlisted) count
	for _, d := range g.pop.donors {
		if d.claimedAt.IsZero() || g.r.Float64() >= reservedShare {
			continue
		}
		seen := map[pgtype.UUID]bool{}
		for n := 1 + g.r.IntN(3); n > 0; n-- { // 1–3 reservations
			ev := g.pop.events[g.r.IntN(len(g.pop.events))]
			if seen[ev.id] {
				continue
			}
			seen[ev.id] = true
			r := &reservation{id: g.uuid(), donor: d, status: "RESERVED"}
			// Reserved any time from publish to the event start (or now).
			ceiling := ev.startsAt
			if g.now.Before(ceiling) {
				ceiling = g.now
			}
			if !ev.publishedAt.Before(ceiling) {
				continue
			}
			r.reservedAt = ev.publishedAt.Add(g.seconds(ceiling.Sub(ev.publishedAt)))
			if ev.capacity > 0 && active[ev.id] >= ev.capacity {
				r.status = "WAITLISTED"
			}
			active[ev.id]++
			if r.status == "RESERVED" && !ev.endsAt.After(g.now) && g.r.Float64() < checkInShare {
				r.checkedIn = ev.startsAt.Add(g.seconds(ev.endsAt.Sub(ev.startsAt)))
			}
			ev.reservations = append(ev.reservations, r)
		}
	}
}

// emails are the notification choices a donor with a preference saved.
func (d *donor) emails() (referral, marketing bool) { return d.index%3 != 0, d.index%4 == 0 }

func (d *donor) gifts() []*gift {
	var out []*gift
	for _, c := range d.checkouts {
		out = append(out, c.gifts...)
	}
	return out
}

func (d *donor) firstGift() *gift {
	var first *gift
	for _, g := range d.gifts() {
		if first == nil || g.paidAt.Before(first.paidAt) {
			first = g
		}
	}
	return first
}

func (d *donor) net() int64 {
	total := int64(0)
	for _, g := range d.gifts() {
		total += g.amount
		if g.refund != nil && g.refund.status == "SUCCEEDED" {
			total -= g.refund.amount
		}
	}
	return total
}

// addMonths returns t moved by months, on the same day of the month or the
// month's last day, the way the processor dates monthly renewals.
func addMonths(t time.Time, months int) time.Time {
	y, m, d := t.Date()
	first := time.Date(y, m+time.Month(months), 1, t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
	return first.AddDate(0, 0, min(d, first.AddDate(0, 1, -1).Day())-1)
}

func (g *generator) amount(choices weighted[int64]) int64 {
	if amount := choices.pick(g.r); amount != 0 {
		return amount
	}
	return int64(1+g.r.IntN(30)) * 3000
}

func (g *generator) kind() string {
	if g.r.Float64() < irlShare {
		return "IRL"
	}
	return "LINK"
}

// seconds returns a uniform whole-second duration in [0, limit).
func (g *generator) seconds(limit time.Duration) time.Duration {
	if limit < time.Second {
		return 0
	}
	return time.Duration(g.r.Int64N(int64(limit/time.Second))) * time.Second
}

func (g *generator) bytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(g.r.Uint32())
	}
	return b
}

func (g *generator) uuid() pgtype.UUID {
	id := pgtype.UUID{Valid: true}
	copy(id.Bytes[:], g.bytes(16))
	id.Bytes[6] = (id.Bytes[6] & 0x0f) | 0x40
	id.Bytes[8] = (id.Bytes[8] & 0x3f) | 0x80
	return id
}

// code has the shape of an issued referral code: 24 bytes of URL-safe base64.
func (g *generator) code() string { return base64.RawURLEncoding.EncodeToString(g.bytes(24)) }

// hash stands in for a visitor token's hash; nobody holds the token.
func (g *generator) hash() string { return hex.EncodeToString(g.bytes(32)) }

func (g *generator) stripeID(prefix string) string { return prefix + hex.EncodeToString(g.bytes(12)) }

// sharerPool samples referrers in proportion to influence among donors whose
// code existed at the time of the visit.
type sharerPool struct {
	pending pendingSharers
	active  []*donor
	totals  []float64
}

func (p *sharerPool) add(d *donor) { heap.Push(&p.pending, d) }

func (p *sharerPool) activate(at time.Time) {
	for len(p.pending) > 0 && p.pending[0].sharingAt.Before(at) {
		d := heap.Pop(&p.pending).(*donor)
		total := d.influence
		if n := len(p.totals); n > 0 {
			total += p.totals[n-1]
		}
		p.active, p.totals = append(p.active, d), append(p.totals, total)
	}
}

// pick returns a sharer whose code existed at the visit. Visits arrive
// roughly, not strictly, in time order, so a recently activated sharer may be
// too new and is drawn again.
func (p *sharerPool) pick(r *rand.Rand, at time.Time) *donor {
	for range 8 {
		if len(p.active) == 0 {
			return nil
		}
		d := p.active[sort.SearchFloat64s(p.totals, r.Float64()*p.totals[len(p.totals)-1])]
		if d.sharingAt.Before(at) {
			return d
		}
	}
	return nil
}

type pendingSharers []*donor

func (h pendingSharers) Len() int           { return len(h) }
func (h pendingSharers) Less(i, j int) bool { return h[i].sharingAt.Before(h[j].sharingAt) }
func (h pendingSharers) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *pendingSharers) Push(x any)        { *h = append(*h, x.(*donor)) }
func (h *pendingSharers) Pop() any {
	old := *h
	d := old[len(old)-1]
	*h = old[:len(old)-1]
	return d
}

// referrals is how many donors d's dashboard should count as referred.
func (d *donor) referrals() int64 {
	var n int64
	for _, tally := range d.referred {
		n += tally.count
	}
	return n
}
