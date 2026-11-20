package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"dad/donor-api/internal/badges"
	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/push"
	"dad/donor-api/internal/recognition"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
)

const (
	WordingBadge     = "BADGE"
	WordingMilestone = "MILESTONE"
)

// WordingService lets content staff change the words on badges and streak
// milestones (PRD §8 Content). How and when they are earned stays in code.
type WordingService struct{ Staff *StaffService }

// WordingEntry is one badge or milestone's wording. For a badge, Text is the
// rule shown on it while unearned; for a milestone, the notification body.
type WordingEntry struct {
	Key          string `json:"key"`
	Title        string `json:"title"`
	Text         string `json:"text"`
	DefaultTitle string `json:"default_title"`
	DefaultText  string `json:"default_text"`
	Custom       bool   `json:"custom"`
}

// RecognitionWording is every badge and milestone with its default and staff
// wording, as shown to content staff.
type RecognitionWording struct {
	Badges     []WordingEntry `json:"badges"`
	Milestones []WordingEntry `json:"milestones"`
}

// WordingChange sets one entry's wording. An empty Title and Text restores the
// default.
type WordingChange struct {
	Kind  string
	Key   string
	Title string
	Text  string
}

func (s WordingService) Wording(ctx context.Context, token string) (RecognitionWording, error) {
	return staffRead(
		ctx,
		s.Staff,
		token,
		"content.publish",
		func(tx pgx.Tx, _ queries.DonorStaff, _ time.Time) (RecognitionWording, error) {
			q := queries.New(tx)
			custom := map[string]map[string]queries.RecognitionWordingRow{}
			for _, kind := range []string{WordingBadge, WordingMilestone} {
				rows, err := q.RecognitionWording(ctx, kind)
				if err != nil {
					return RecognitionWording{}, err
				}
				custom[kind] = map[string]queries.RecognitionWordingRow{}
				for _, row := range rows {
					custom[kind][row.Key] = row
				}
			}
			defaults, err := defaultBadgeWording(ctx, q)
			if err != nil {
				return RecognitionWording{}, err
			}
			entry := func(kind, key, title, text string) WordingEntry {
				e := WordingEntry{Key: key, Title: title, Text: text, DefaultTitle: title, DefaultText: text}
				if row, ok := custom[kind][key]; ok {
					e.Title, e.Text, e.Custom = row.Title, row.Body, true
				}
				return e
			}
			out := RecognitionWording{
				Badges:     make([]WordingEntry, 0, len(badges.Catalog)),
				Milestones: make([]WordingEntry, 0, len(recognition.LadderMilestones)),
			}
			for _, b := range defaults {
				out.Badges = append(out.Badges, entry(WordingBadge, b.Code, b.Title, b.Rule))
			}
			for _, days := range recognition.LadderMilestones {
				title, body := milestoneWording(days)
				out.Milestones = append(out.Milestones, entry(WordingMilestone, strconv.FormatInt(days, 10), title, body))
			}
			return out, nil
		},
	)
}

func (s WordingService) SetWording(ctx context.Context, token string, change WordingChange) error {
	_, err := staffWrite(
		ctx,
		s.Staff,
		token,
		"content.publish",
		func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (struct{}, error) {
			if !validWordingKey(change.Kind, change.Key) {
				return struct{}{}, ErrStaffInput
			}
			reset := change.Title == "" && change.Text == ""
			if !reset && (!wordingText(change.Title, 60) || !wordingText(change.Text, 200)) {
				return struct{}{}, ErrStaffInput
			}
			q := queries.New(tx)
			var err error
			if reset {
				err = q.ResetRecognitionWording(ctx, queries.ResetRecognitionWordingParams{Kind: change.Kind, Key: change.Key})
			} else {
				err = q.SetRecognitionWording(ctx, queries.SetRecognitionWordingParams{
					Kind: change.Kind, Key: change.Key, Title: change.Title, Body: change.Text, ActorID: actor.ID, UpdatedAt: timestamp(now),
				})
			}
			if err != nil {
				return struct{}{}, err
			}
			return struct{}{}, auditStaff(ctx, q, actor.ID, "content.wording_changed", change.Kind+":"+change.Key,
				map[string]string{"title": change.Title, "text": change.Text}, now)
		},
	)
	return err
}

// Only the named milestones can be reworded; the yearly ones after the last
// use the default wording.
func validWordingKey(kind, key string) bool {
	switch kind {
	case WordingBadge:
		_, ok := badges.Lookup(key)
		return ok
	case WordingMilestone:
		days, err := strconv.ParseInt(key, 10, 64)
		return err == nil && strconv.FormatInt(days, 10) == key && slices.Contains(recognition.LadderMilestones, days)
	}
	return false
}

func wordingText(s string, limit int) bool {
	return plainText(s, false) && strings.TrimSpace(s) == s && s != "" && utf8.RuneCountInString(s) <= limit
}

// milestoneWording is the default notification for a streak milestone.
func milestoneWording(days int64) (title, body string) {
	length, span := strconv.FormatInt(days, 10)+"-day", strconv.FormatInt(days, 10)+" days"
	if years := days / 365; days%365 == 0 {
		length, span = strconv.FormatInt(years, 10)+"-year", strconv.FormatInt(years, 10)+" years"
		if years == 1 {
			span = "a year"
		}
	}
	return "Your " + length + " giving streak", "You have kept your giving streak going for " + span + ". Thank you for showing up every day."
}

// customMilestone replaces a milestone notification's wording with staff's,
// when they have set it.
func customMilestone(ctx context.Context, q *queries.Queries, job queries.DonorPushNotification, n *push.Notification) error {
	data, err := payments.DecodeObject(job.Context)
	if err != nil {
		return err
	}
	days, err := data.Int("days")
	if err != nil {
		return payments.ErrFacts
	}
	row, err := q.MilestoneWording(ctx, strconv.FormatInt(days, 10))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	n.Title, n.Body = row.Title, row.Body
	return nil
}

// badgeWording returns the catalog with staff wording applied.
func badgeWording(ctx context.Context, q *queries.Queries) ([]badges.Badge, error) {
	catalog, err := defaultBadgeWording(ctx, q)
	if err != nil {
		return nil, err
	}
	rows, err := q.RecognitionWording(ctx, WordingBadge)
	if err != nil {
		return nil, err
	}
	custom := make(map[string]queries.RecognitionWordingRow, len(rows))
	for _, row := range rows {
		custom[row.Key] = row
	}
	for i, b := range catalog {
		if row, ok := custom[b.Code]; ok {
			catalog[i].Title, catalog[i].Rule = row.Title, row.Body
		}
	}
	return catalog, nil
}

// defaultBadgeWording returns the catalog with each rule sentence describing
// the threshold staff have configured.
func defaultBadgeWording(ctx context.Context, q *queries.Queries) ([]badges.Badge, error) {
	rules, err := recognitionRules(ctx, q)
	if err != nil {
		return nil, err
	}
	catalog := slices.Clone(badges.Catalog)
	for i, b := range catalog {
		for _, r := range rules.Badges {
			if r.Code == b.Code && r.Threshold != defaultBadgeRules[b.Code].Threshold {
				catalog[i].Rule = tunedBadgeRule(r, b.Rule)
			}
		}
	}
	return catalog, nil
}

func tunedBadgeRule(r BadgeRule, fallback string) string {
	switch r.Signal {
	case "referrals":
		return fmt.Sprintf("Bring in %d %s", r.Threshold, plural(r.Threshold, "person who completes a gift", "people who complete a gift"))
	case "streak_days":
		return fmt.Sprintf("Reach a %d-day streak", r.Threshold)
	case "checkins":
		return fmt.Sprintf("Check in at %d %s", r.Threshold, plural(r.Threshold, "event", "events"))
	case "initiatives":
		return fmt.Sprintf("Fund %d distinct %s", r.Threshold, plural(r.Threshold, "initiative", "initiatives"))
	case "city_rank":
		return fmt.Sprintf("Rank in the top %d in your city", r.Threshold)
	case "national_percentile":
		return fmt.Sprintf("Rank in the top %d%% nationally", r.Threshold)
	}
	return fallback
}

func plural(n int64, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
