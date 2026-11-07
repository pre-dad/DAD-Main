package store

import (
	"context"
	"encoding/base64"
	"slices"
	"time"

	"dad/donor-api/internal/badges"
	"dad/donor-api/internal/recognition"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
)

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
	"F":      {Signal: "founding", Threshold: 1},
	"1D":     {Signal: "first_gift", Threshold: 1},
	"7":      {Signal: "streak_days", Threshold: 7},
	"30":     {Signal: "streak_days", Threshold: 30},
	"100":    {Signal: "streak_days", Threshold: 100},
	"Y1":     {Signal: "streak_days", Threshold: 365},
	"1K":     {Signal: "streak_days", Threshold: 1000},
	"Y3":     {Signal: "streak_days", Threshold: 1095},
	"CB":     {Signal: "comeback", Threshold: 1},
	"UP":     {Signal: "raised_amount", Threshold: 1},
	"REF1":   {Signal: "referrals", Threshold: 1},
	"REF3":   {Signal: "referrals", Threshold: 3},
	"REF12":  {Signal: "referrals", Threshold: 12},
	"TC":     {Signal: "referrals", Threshold: 20},
	"IRL":    {Signal: "irl_referral", Threshold: 1},
	"V":      {Signal: "volunteer", Threshold: 1},
	"SU":     {Signal: "checkins", Threshold: 1},
	"V3":     {Signal: "checkins", Threshold: 3},
	"V10":    {Signal: "checkins", Threshold: 10},
	"INIT7":  {Signal: "initiatives", Threshold: 7},
	"CITY50": {Signal: "city_rank", Threshold: 50, MinPopulation: badges.MinRankPopulation},
	"NAT2":   {Signal: "national_percentile", Threshold: 2, MinPopulation: badges.MinRankPopulation},
	"TP":     {Signal: "team", Threshold: 1},
	"WT":     {Signal: "monthly_winner", Threshold: 1},
}

func recognitionRules(ctx context.Context, q *queries.Queries) (RecognitionRules, error) {
	out := RecognitionRules{Milestones: slices.Clone(recognition.LadderMilestones), Badges: make([]BadgeRule, 0, len(badges.Catalog))}
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
		out.Badges = append(out.Badges, rule)
	}
	ms, err := q.MilestoneRules(ctx)
	if err != nil {
		return out, err
	}
	if len(ms) > 0 {
		out.Milestones = ms[0]
	}
	return out, nil
}

func configuredLadder(
	ctx context.Context,
	q *queries.Queries,
	run recognition.Streak,
	now time.Time,
	zone *time.Location,
) (recognition.StreakLadder, error) {
	rules, err := recognitionRules(ctx, q)
	if err != nil {
		return recognition.StreakLadder{}, err
	}
	return run.LadderWithMilestones(now, zone, rules.Milestones), nil
}

func ruleEarned(r BadgeRule, s badges.Signals) bool {
	var n int64
	switch r.Signal {
	case "streak_days":
		n = s.StreakDays
	case "referrals":
		n = s.ConvertedReferrals
	case "checkins":
		n = s.EventCheckins
	case "initiatives":
		n = s.DistinctInitiatives
	case "city_rank":
		return s.CityRank != nil && int64(*s.CityRank) <= r.Threshold && int64(s.CityPopulation) >= r.MinPopulation
	case "national_percentile":
		return s.NationalTopPercent != nil && int64(*s.NationalTopPercent) <= r.Threshold && int64(s.NationalPopulation) >= r.MinPopulation
	case "founding":
		return s.FoundingQualified
	case "first_gift":
		return s.FirstGiftMade
	case "comeback":
		return s.ComebackReached30
	case "raised_amount":
		return s.RaisedAmount
	case "irl_referral":
		return s.HasIRLReferral
	case "volunteer":
		return s.VolunteerOptIn
	case "team":
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
	for _, b := range in.Badges {
		def, ok := defaultBadgeRules[b.Code]
		if !ok || seen[b.Code] || b.Signal != def.Signal || b.Threshold < 1 || b.Threshold > 1000000 {
			return ErrStaffInput
		}
		seen[b.Code] = true
		if b.Signal == "national_percentile" && b.Threshold > 100 {
			return ErrStaffInput
		}
		ranked := b.Signal == "city_rank" || b.Signal == "national_percentile"
		if b.MinPopulation < 1 || b.MinPopulation > 1000000 || (!ranked && b.MinPopulation != 1) {
			return ErrStaffInput
		}
		if b.Signal != "streak_days" && b.Signal != "referrals" && b.Signal != "checkins" && b.Signal != "initiatives" &&
			b.Signal != "city_rank" &&
			b.Signal != "national_percentile" &&
			b.Threshold != 1 {
			return ErrStaffInput
		}
		if b.Artwork != "" {
			if _, err := cleanLogo(b.Artwork); err != nil {
				return ErrStaffInput
			}
		}
	}
	_, err := staffWrite(
		ctx,
		s.Staff,
		token,
		"content.publish",
		func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (struct{}, error) {
			q := queries.New(tx)
			for _, b := range in.Badges {
				art := b.Artwork
				if art != "" {
					png, _ := cleanLogo(art)
					art = "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
				}
				if err := q.SetBadgeRule(ctx, queries.SetBadgeRuleParams{Code: b.Code, Signal: b.Signal, Threshold: b.Threshold, MinPopulation: b.MinPopulation, Artwork: art, UpdatedAt: timestamp(now), ActorID: actor.ID}); err != nil {
					return struct{}{}, err
				}
			}
			if err := q.SetMilestoneRules(ctx, queries.SetMilestoneRulesParams{Days: in.Milestones, UpdatedAt: timestamp(now), ActorID: actor.ID}); err != nil {
				return struct{}{}, err
			}
			if err := q.RefreshRecognitionSchedule(ctx, timestamp(now)); err != nil {
				return struct{}{}, err
			}
			return struct{}{}, auditStaff(
				ctx,
				q,
				actor.ID,
				"content.recognition_rules_changed",
				"recognition",
				map[string]any{"milestones": in.Milestones},
				now,
			)
		},
	)
	return err
}
