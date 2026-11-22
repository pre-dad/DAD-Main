// Package recognition calculates donor recognition from verified financial facts.
// It does not ingest processor events or treat checkout attempts as payments.
package recognition

import (
	"cmp"
	"errors"
	"reflect"
	"slices"
	"time"
	_ "time/tzdata"
)

var (
	ErrInvalid    = errors.New("invalid financial facts")
	ErrIncomplete = errors.New("incomplete billing history")
)

const Grace = 7 * 24 * time.Hour

// NewYork is the zone of the Founding Donor cutoff and receipt tax years.
var NewYork = mustLoadLocation("America/New_York")

// FoundingCutoff is exclusive: a first gift must succeed before it. Per the R1
// PRD (Appendix A #1), Founding Donor status requires a gift before January 2027.
var FoundingCutoff = time.Date(2027, 1, 1, 0, 0, 0, 0, NewYork)

func mustLoadLocation(name string) *time.Location {
	zone, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return zone
}

type Candidate struct {
	ReferrerID string    `json:"referrer_id"`
	Kind       string    `json:"kind"`
	ExpiresAt  time.Time `json:"expires_at"`
}

type Gift struct {
	ID        string     `json:"id"`
	DonorID   string     `json:"donor_id"`
	Amount    int64      `json:"amount_minor"`
	PaidAt    time.Time  `json:"paid_at"`
	Frequency string     `json:"frequency"`
	Candidate *Candidate `json:"candidate"`
	PeriodID  string     `json:"period_id"`
	Currency  string     `json:"currency"`
}

type Refund struct {
	ID        string    `json:"id"`
	GiftID    string    `json:"gift_id"`
	Amount    int64     `json:"amount_minor"`
	Status    string    `json:"status"`
	UpdatedAt time.Time `json:"updated_at"`
	Currency  string    `json:"currency"`
}

type Period struct {
	ID      string    `json:"id"`
	DonorID string    `json:"donor_id"`
	DueAt   time.Time `json:"due_at"`
	EndsAt  time.Time `json:"ends_at"`
	Status  string    `json:"status"`
}

type Relationship struct {
	DonorID     string  `json:"donor_id"`
	FirstGiftID string  `json:"first_gift_id"`
	ReferrerID  *string `json:"referrer_id"`
	Kind        *string `json:"kind"`
}

// Streak is a donor's giving streak. Since is the due date of the current
// run's first period, which identifies the run; it is nil with no run.
type Streak struct {
	Current     int64      `json:"current"`
	Best        int64      `json:"best"`
	AtRisk      bool       `json:"at_risk"`
	GraceEndsAt *time.Time `json:"grace_ends_at"`
	Since       *time.Time `json:"since"`
}

func utc(t time.Time) time.Time { return t.Round(0).UTC() }

func cleanGifts(input []Gift) ([]Gift, error) {
	seen := map[string]Gift{}
	result := []Gift{}
	for _, g := range input {
		if g.ID == "" || g.DonorID == "" || g.Amount <= 0 || g.Currency != "USD" || g.PaidAt.IsZero() ||
			(g.Frequency != "MONTHLY" && g.Frequency != "ONE_TIME") || (g.Frequency == "ONE_TIME" && g.PeriodID != "") {
			return nil, ErrInvalid
		}
		g.PaidAt = utc(g.PaidAt)
		if g.Candidate != nil {
			c := *g.Candidate
			if c.ReferrerID == "" || (c.Kind != "LINK" && c.Kind != "IRL") || c.ExpiresAt.IsZero() {
				return nil, ErrInvalid
			}
			c.ExpiresAt = utc(c.ExpiresAt)
			g.Candidate = &c
		}
		if old, ok := seen[g.ID]; ok {
			if !reflect.DeepEqual(old, g) {
				return nil, ErrInvalid
			}
			continue
		}
		seen[g.ID] = g
		result = append(result, g)
	}
	return result, nil
}

// NetGifts returns each gift paid by asOf, keyed by gift ID, net of the
// refunds that had succeeded by then. A refund updated after asOf is invalid,
// so callers pass only the refunds known at asOf.
func NetGifts(input []Gift, refunds []Refund, asOf time.Time) (map[string]int64, error) {
	if asOf.IsZero() {
		return nil, ErrInvalid
	}
	gifts, err := cleanGifts(input)
	if err != nil {
		return nil, err
	}
	byID := map[string]Gift{}
	net := map[string]int64{}
	seen := map[string]Refund{}
	for _, g := range gifts {
		byID[g.ID] = g
		if !g.PaidAt.After(asOf) {
			net[g.ID] = g.Amount
		}
	}
	for _, r := range refunds {
		if r.ID == "" || r.Amount <= 0 || r.Currency != "USD" || r.UpdatedAt.IsZero() {
			return nil, ErrInvalid
		}
		switch r.Status {
		case "PENDING", "REQUIRES_ACTION", "SUCCEEDED", "FAILED", "CANCELED":
		default:
			return nil, ErrInvalid
		}
		r.UpdatedAt = utc(r.UpdatedAt)
		if old, ok := seen[r.ID]; ok {
			if old != r {
				return nil, ErrInvalid
			}
			continue
		}
		seen[r.ID] = r
		g, ok := byID[r.GiftID]
		if !ok || r.UpdatedAt.After(asOf) || r.UpdatedAt.Before(g.PaidAt) || r.Amount > g.Amount {
			return nil, ErrInvalid
		}
		if r.Status == "SUCCEEDED" {
			if net[g.ID] < r.Amount {
				return nil, ErrInvalid
			}
			net[g.ID] -= r.Amount
		}
	}
	return net, nil
}

// Relationships returns each donor's first gift paid by asOf and the
// referral it converted, if its window was still open.
func Relationships(input []Gift, asOf time.Time) (map[string]Relationship, error) {
	if asOf.IsZero() {
		return nil, ErrInvalid
	}
	gifts, err := cleanGifts(input)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(gifts, func(a, b Gift) int {
		return cmp.Or(a.PaidAt.Compare(b.PaidAt), cmp.Compare(a.ID, b.ID))
	})
	result := map[string]Relationship{}
	for _, g := range gifts {
		if _, ok := result[g.DonorID]; ok || g.PaidAt.After(asOf) {
			continue
		}
		r := Relationship{DonorID: g.DonorID, FirstGiftID: g.ID}
		if c := g.Candidate; c != nil && c.ReferrerID != g.DonorID && g.PaidAt.Before(c.ExpiresAt) {
			r.ReferrerID = &c.ReferrerID
			r.Kind = &c.Kind
		}
		result[g.DonorID] = r
	}
	return result, nil
}

// FoundingQualifiedAt returns when donorID's first gift before the cutoff
// succeeded, if one had by asOf.
func FoundingQualifiedAt(input []Gift, donorID string, asOf time.Time) (time.Time, bool, error) {
	if asOf.IsZero() {
		return time.Time{}, false, ErrInvalid
	}
	gifts, err := cleanGifts(input)
	if err != nil {
		return time.Time{}, false, err
	}
	var first time.Time
	for _, g := range gifts {
		if g.DonorID == donorID && !g.PaidAt.After(asOf) && g.PaidAt.Before(FoundingCutoff) && (first.IsZero() || g.PaidAt.Before(first)) {
			first = g.PaidAt
		}
	}
	return first, !first.IsZero(), nil
}

// GivingStreak counts consecutive paid monthly periods as of asOf. A renewal
// that is due but not yet observed keeps the current count for the grace
// window, after which the history is incomplete until the invoice arrives.
func GivingStreak(
	input []Gift,
	refunds []Refund,
	inputPeriods []Period,
	donorID string,
	asOf time.Time,
	previousBest int64,
	stops []time.Time,
) (Streak, error) {
	if previousBest < 0 {
		return Streak{}, ErrInvalid
	}
	gifts, err := cleanGifts(input)
	if err != nil {
		return Streak{}, err
	}
	net, err := NetGifts(gifts, refunds, asOf)
	if err != nil {
		return Streak{}, err
	}
	periods, err := donorPeriods(inputPeriods, donorID)
	if err != nil {
		return Streak{}, err
	}
	paid, err := periodPayments(gifts, net, periods, donorID)
	if err != nil {
		return Streak{}, err
	}
	stopped, err := renewalBoundaries(stops)
	if err != nil {
		return Streak{}, err
	}
	elapsed := []Period{}
	for _, p := range periods {
		if !p.DueAt.After(asOf) {
			elapsed = append(elapsed, p)
		}
	}
	if len(elapsed) == 0 {
		return Streak{Best: previousBest}, nil
	}
	next := elapsed[len(elapsed)-1].EndsAt
	ended := stopped[next]
	renewalDue := !next.After(asOf)
	if renewalDue && !ended && !asOf.Before(next.Add(Grace)) {
		return Streak{}, ErrIncomplete
	}
	result := countStreak(elapsed, paid, net, asOf, stopped)
	result.Best = max(previousBest, result.Best)
	if renewalDue && ended {
		result.Current = 0
		result.AtRisk = false
		result.GraceEndsAt = nil
		result.Since = nil
	}
	return result, nil
}

// donorPeriods validates every period and returns donorID's in due order.
func donorPeriods(input []Period, donorID string) ([]Period, error) {
	seen := map[string]Period{}
	periods := []Period{}
	for _, p := range input {
		duration := p.EndsAt.Sub(p.DueAt)
		if p.ID == "" || p.DonorID == "" || p.DueAt.IsZero() || p.EndsAt.IsZero() || duration < 27*24*time.Hour ||
			duration > 32*24*time.Hour ||
			(p.Status != "EXPECTED" && p.Status != "FAILED") {
			return nil, ErrInvalid
		}
		p.DueAt = utc(p.DueAt)
		p.EndsAt = utc(p.EndsAt)
		if old, ok := seen[p.ID]; ok {
			if old != p {
				return nil, ErrInvalid
			}
			continue
		}
		seen[p.ID] = p
		if p.DonorID == donorID {
			periods = append(periods, p)
		}
	}
	slices.SortFunc(periods, func(a, b Period) int { return a.DueAt.Compare(b.DueAt) })
	for i := 1; i < len(periods); i++ {
		if periods[i-1].EndsAt.After(periods[i].DueAt) {
			return nil, ErrInvalid
		}
	}
	return periods, nil
}

// periodPayments groups donorID's settled monthly gifts by billing period.
func periodPayments(gifts []Gift, net map[string]int64, periods []Period, donorID string) (map[string][]Gift, error) {
	known := map[string]bool{}
	for _, p := range periods {
		known[p.ID] = true
	}
	result := map[string][]Gift{}
	for _, g := range gifts {
		if g.DonorID != donorID || g.Frequency != "MONTHLY" {
			continue
		}
		if _, ok := net[g.ID]; !ok {
			continue
		}
		if !known[g.PeriodID] {
			return nil, ErrIncomplete
		}
		result[g.PeriodID] = append(result[g.PeriodID], g)
	}
	return result, nil
}

func renewalBoundaries(stops []time.Time) (map[time.Time]bool, error) {
	stopped := map[time.Time]bool{}
	for _, t := range stops {
		if t.IsZero() {
			return nil, ErrInvalid
		}
		stopped[utc(t)] = true
	}
	return stopped, nil
}

func countStreak(
	elapsed []Period,
	payments map[string][]Gift,
	net map[string]int64,
	asOf time.Time,
	stopped map[time.Time]bool,
) Streak {
	result := Streak{}
	var last, lastPaid time.Time
	for _, p := range elapsed {
		// A cancellation that ended renewals at the previous period's end, or a
		// gap before this period, starts a new run.
		if stopped[last] || !last.Equal(p.DueAt) {
			result.Current = 0
			result.Since = nil
		}
		var paid time.Time
		for _, g := range payments[p.ID] {
			if net[g.ID] > 0 && (paid.IsZero() || g.PaidAt.Before(paid)) {
				paid = g.PaidAt
			}
		}
		result.AtRisk = false
		result.GraceEndsAt = nil
		deadline := p.DueAt.Add(Grace)
		switch {
		case !paid.IsZero():
			if !paid.Before(deadline) || (!lastPaid.IsZero() && paid.Before(lastPaid)) {
				result.Current = 0
			}
			if result.Current == 0 {
				result.Since = &p.DueAt
			}
			result.Current++
			lastPaid = paid
			result.Best = max(result.Best, result.Current)
		case len(payments[p.ID]) > 0 || !asOf.Before(deadline):
			result.Current = 0
			result.Since = nil
			lastPaid = time.Time{}
		case result.Current > 0:
			result.AtRisk = true
			result.GraceEndsAt = &deadline
		}
		last = p.EndsAt
	}
	return result
}

// LadderMilestones are the streak milestones in days (PRD S-3): a fixed early
// set, then every 365 days after the last.
var LadderMilestones = []int64{7, 30, 100, 365, 500, 1000, 1095, 1460}

// ladderBarDays is the width of the streak card's segmented bar (PRD S-2).
const ladderBarDays = 30

// StreakLadder is the streak card's display data (PRD S-2/S-3): the current
// streak length in days, the next milestone and how many days remain, the last
// milestone reached, and a 30-day segmented bar (most recent day last).
type StreakLadder struct {
	Days          int64  `json:"days"`
	LastMilestone int64  `json:"last_milestone"`
	NextMilestone *int64 `json:"next_milestone"`
	DaysToNext    *int64 `json:"days_to_next"`
	Bar           []bool `json:"bar"`
}

// nextMilestone returns the first milestone strictly greater than days.
// Past the fixed set the ladder continues every 365 days (PRD S-3 "+365").
func nextMilestone(days int64, milestones []int64) int64 {
	for _, m := range milestones {
		if m > days {
			return m
		}
	}
	last := milestones[len(milestones)-1]
	return last + ((days-last)/365+1)*365
}

// lastMilestone returns the highest milestone at or below days, or 0.
func lastMilestone(days int64, milestones []int64) int64 {
	var reached int64
	for _, m := range milestones {
		if m <= days {
			reached = m
		}
	}
	if days >= milestones[len(milestones)-1] {
		last := milestones[len(milestones)-1]
		reached = last + (days-last)/365*365
	}
	return reached
}

// Ladder derives the streak card from a streak and the moment it is read.
// Days counts the midnights in zone, the donor's own, since the run began, so
// a streak grows by one at local midnight (§10 Time). A streak with no run
// shows zero days, no reached milestone, the first milestone as next, and an
// empty bar. The bar marks, for each of the last 30 days ending today, whether
// the streak was already running on that day.
func (s Streak) Ladder(asOf time.Time, zone *time.Location) StreakLadder {
	return s.LadderWithMilestones(asOf, zone, LadderMilestones)
}

// LadderWithMilestones is Ladder over a staff-configured milestone ladder.
func (s Streak) LadderWithMilestones(asOf time.Time, zone *time.Location, milestones []int64) StreakLadder {
	if s.Since == nil {
		next := milestones[0]
		return StreakLadder{NextMilestone: &next, DaysToNext: &next, Bar: make([]bool, ladderBarDays)}
	}
	days := max(civilDay(asOf, zone)-civilDay(*s.Since, zone), 0)
	ladder := StreakLadder{Days: days, LastMilestone: lastMilestone(days, milestones)}
	next := nextMilestone(days, milestones)
	toNext := next - days
	ladder.NextMilestone = &next
	ladder.DaysToNext = &toNext
	bar := make([]bool, ladderBarDays)
	for i := range bar {
		// bar[ladderBarDays-1] is today; bar[0] is 29 days ago.
		bar[i] = days >= int64(ladderBarDays-1-i)
	}
	ladder.Bar = bar
	return ladder
}

// Reaches is the local midnight in zone at which the run is days long. The
// streak must have a start: Since is not nil.
func (s Streak) Reaches(days int64, zone *time.Location) time.Time {
	start := s.Since.In(zone)
	return time.Date(start.Year(), start.Month(), start.Day()+int(days), 0, 0, 0, 0, zone)
}

// civilDay numbers t's calendar date in zone.
func civilDay(t time.Time, zone *time.Location) int64 {
	local := t.In(zone)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC).Unix() / 86400
}

// Zone is the IANA zone a donor's streak and daily rotation roll over in,
// New York when their ZIP gives none.
func Zone(name string) *time.Location {
	if name == "" {
		return NewYork
	}
	zone, err := time.LoadLocation(name)
	if err != nil {
		return NewYork
	}
	return zone
}
