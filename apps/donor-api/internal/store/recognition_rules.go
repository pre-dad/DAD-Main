package store

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"slices"
	"time"

	"dad/donor-api/internal/badges"
	"dad/donor-api/internal/recognition"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
)

// Signals a badge rule can key on. Those in thresholdedSignals count something
// staff can tune; the rest are earned or not, so their threshold stays 1.
const (
	signalStreakDays         = "streak_days"
	signalReferrals          = "referrals"
	signalCheckins           = "checkins"
	signalInitiatives        = "initiatives"
	signalCityRank           = "city_rank"
	signalNationalPercentile = "national_percentile"
	signalFounding           = "founding"
	signalFirstGift          = "first_gift"
	signalComeback           = "comeback"
	signalRaisedAmount       = "raised_amount"
	signalIRLReferral        = "irl_referral"
	signalVolunteer          = "volunteer"
	signalTeam               = "team"
	signalMonthlyWinner      = "monthly_winner"
)

// thresholdedSignals are the signals whose threshold staff may change.
var thresholdedSignals = []string{
	signalStreakDays, signalReferrals, signalCheckins, signalInitiatives, signalCityRank, signalNationalPercentile,
}

// rankedSignal reports whether a signal is a rank taken among a population.
func rankedSignal(signal string) bool {
	return signal == signalCityRank || signal == signalNationalPercentile
}

type BadgeRule struct {
	Code      string `json:"code"`
	Signal    string `json:"signal"`
	Threshold int64  `json:"threshold"`
	// MinPopulation is the fewest donors a rank must be taken among to earn
	// the badge; it is 1 for a badge that is not a rank.
	MinPopulation int64  `json:"min_population"`
	Artwork       string `json:"artwork"`
}

type RecognitionRules struct {
	Badges     []BadgeRule `json:"badges"`
	Milestones []int64     `json:"milestones"`
}

var defaultBadgeRules = map[string]BadgeRule{
	"F":      {Signal: signalFounding, Threshold: 1},
	"1D":     {Signal: signalFirstGift, Threshold: 1},
	"7":      {Signal: signalStreakDays, Threshold: 7},
	"30":     {Signal: signalStreakDays, Threshold: 30},
	"100":    {Signal: signalStreakDays, Threshold: 100},
	"Y1":     {Signal: signalStreakDays, Threshold: 365},
	"1K":     {Signal: signalStreakDays, Threshold: 1000},
	"Y3":     {Signal: signalStreakDays, Threshold: 1095},
	"CB":     {Signal: signalComeback, Threshold: 1},
	"UP":     {Signal: signalRaisedAmount, Threshold: 1},
	"REF1":   {Signal: signalReferrals, Threshold: 1},
	"REF3":   {Signal: signalReferrals, Threshold: 3},
	"REF12":  {Signal: signalReferrals, Threshold: 12},
	"TC":     {Signal: signalReferrals, Threshold: 20},
	"IRL":    {Signal: signalIRLReferral, Threshold: 1},
	"V":      {Signal: signalVolunteer, Threshold: 1},
	"SU":     {Signal: signalCheckins, Threshold: 1},
	"V3":     {Signal: signalCheckins, Threshold: 3},
	"V10":    {Signal: signalCheckins, Threshold: 10},
	"INIT7":  {Signal: signalInitiatives, Threshold: 7},
	"CITY50": {Signal: signalCityRank, Threshold: 50, MinPopulation: badges.MinRankPopulation},
	"NAT2":   {Signal: signalNationalPercentile, Threshold: 2, MinPopulation: badges.MinRankPopulation},
	"TP":     {Signal: signalTeam, Threshold: 1},
	"WT":     {Signal: signalMonthlyWinner, Threshold: 1},
}

func badgeRules(ctx context.Context, q *queries.Queries) ([]BadgeRule, error) {
	out := make([]BadgeRule, 0, len(badges.Catalog))
	rows, err := q.BadgeRules(ctx)
	if err != nil {
		return out, err
	}
	custom := map[string]BadgeRule{}
	for _, row := range rows {
		custom[row.Code] = BadgeRule{row.Code, row.Signal, row.Threshold, row.MinPopulation, row.Artwork}
	}
	for _, b := range badges.Catalog {
		rule := defaultBadgeRules[b.Code]
		rule.Code = b.Code
		rule.MinPopulation = max(rule.MinPopulation, 1)
		if c, ok := custom[b.Code]; ok {
			rule = c
		}
		out = append(out, rule)
	}
	return out, nil
}

func recognitionRules(ctx context.Context, q *queries.Queries) (RecognitionRules, error) {
	var out RecognitionRules
	var err error
	if out.Badges, err = badgeRules(ctx, q); err != nil {
		return out, err
	}
	out.Milestones, err = milestoneRules(ctx, q)
	return out, err
}

// milestoneRules returns the streak milestones staff configured, or the
// default ladder when they have not.
func milestoneRules(ctx context.Context, q *queries.Queries) ([]int64, error) {
	ms, err := q.MilestoneRules(ctx)
	if err != nil {
		return slices.Clone(recognition.LadderMilestones), err
	}
	if len(ms) > 0 {
		return ms[0], nil
	}
	return slices.Clone(recognition.LadderMilestones), nil
}

func configuredLadder(
	ctx context.Context,
	q *queries.Queries,
	run recognition.Streak,
	now time.Time,
	zone *time.Location,
) (recognition.StreakLadder, error) {
	milestones, err := milestoneRules(ctx, q)
	if err != nil {
		return recognition.StreakLadder{}, err
	}
	return run.LadderWithMilestones(now, zone, milestones), nil
}

func ruleEarned(r BadgeRule, s badges.Signals) bool {
	var n int64
	switch r.Signal {
	case signalStreakDays:
		n = s.StreakDays
	case signalReferrals:
		n = s.ConvertedReferrals
	case signalCheckins:
		n = s.EventCheckins
	case signalInitiatives:
		n = s.DistinctInitiatives
	case signalCityRank:
		return s.CityRank != nil && int64(*s.CityRank) <= r.Threshold && int64(s.CityPopulation) >= r.MinPopulation
	case signalNationalPercentile:
		return s.NationalTopPercent != nil && int64(*s.NationalTopPercent) <= r.Threshold && int64(s.NationalPopulation) >= r.MinPopulation
	case signalFounding:
		return s.FoundingQualified
	case signalFirstGift:
		return s.FirstGiftMade
	case signalComeback:
		return s.ComebackReached30
	case signalRaisedAmount:
		return s.RaisedAmount
	case signalIRLReferral:
		return s.HasIRLReferral
	case signalVolunteer:
		return s.VolunteerOptIn
	case signalTeam:
		return s.JoinedTeam
	default:
		return false
	}
	return n >= r.Threshold
}

func (s WordingService) Rules(ctx context.Context, token string) (RecognitionRules, error) {
	return staffRead(ctx, s.Staff, token, "content.publish", func(tx pgx.Tx, _ queries.DonorStaff, _ time.Time) (RecognitionRules, error) {
		return recognitionRules(ctx, queries.New(tx))
	})
}

func (s WordingService) SetRules(ctx context.Context, token string, in RecognitionRules) error {
	if len(in.Milestones) == 0 || len(in.Milestones) > 32 || !slices.IsSorted(in.Milestones) {
		return ErrStaffInput
	}
	for i, n := range in.Milestones {
		if n < 1 || n > 1000000 || (i > 0 && n == in.Milestones[i-1]) {
			return ErrStaffInput
		}
	}
	if len(in.Badges) != len(badges.Catalog) {
		return ErrStaffInput
	}
	seen := map[string]bool{}
	logos := map[string][]byte{}
	for _, b := range in.Badges {
		def, ok := defaultBadgeRules[b.Code]
		if !ok || seen[b.Code] || b.Signal != def.Signal || b.Threshold < 1 || b.Threshold > 1000000 {
			return ErrStaffInput
		}
		seen[b.Code] = true
		if b.Signal == signalNationalPercentile && b.Threshold > 100 {
			return ErrStaffInput
		}
		if b.MinPopulation < 1 || b.MinPopulation > 1000000 || (!rankedSignal(b.Signal) && b.MinPopulation != 1) {
			return ErrStaffInput
		}
		if !slices.Contains(thresholdedSignals, b.Signal) && b.Threshold != 1 {
			return ErrStaffInput
		}
		if b.Artwork != "" {
			png, err := cleanLogo(b.Artwork)
			if err != nil {
				return ErrStaffInput
			}
			logos[b.Code] = png
		}
	}
	_, err := staffWrite(
		ctx,
		s.Staff,
		token,
		"content.publish",
		func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (struct{}, error) {
			q := queries.New(tx)
			changes := make([]map[string]any, 0, len(in.Badges))
			for _, b := range in.Badges {
				change := map[string]any{"code": b.Code, "signal": b.Signal, "threshold": b.Threshold, "min_population": b.MinPopulation}
				art := ""
				if png, ok := logos[b.Code]; ok {
					art = "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
					sum := sha256.Sum256(png)
					change["artwork_sha256"] = hex.EncodeToString(sum[:])
				}
				changes = append(changes, change)
				if err := q.SetBadgeRule(ctx, queries.SetBadgeRuleParams{Code: b.Code, Signal: b.Signal, Threshold: b.Threshold, MinPopulation: b.MinPopulation, Artwork: art, UpdatedAt: timestamp(now), ActorID: actor.ID}); err != nil {
					return struct{}{}, err
				}
			}
			if err := q.SetMilestoneRules(ctx, queries.SetMilestoneRulesParams{Days: in.Milestones, UpdatedAt: timestamp(now), ActorID: actor.ID}); err != nil {
				return struct{}{}, err
			}
			if err := q.BumpRecognitionEpoch(ctx); err != nil {
				return struct{}{}, err
			}
			return struct{}{}, auditStaff(
				ctx,
				q,
				actor.ID,
				"content.recognition_rules_changed",
				"recognition",
				map[string]any{"milestones": in.Milestones, "badges": changes},
				now,
			)
		},
	)
	return err
}
