package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"dad/donor-api/internal/recognition"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// InitiativeReader is the donor-facing, unauthenticated read of published
// initiative data. It only ever returns data for a PUBLISHED
// initiative — a draft or unknown id reads as "not found" — so the draft side
// is never observable to donors.
type InitiativeReader struct{ DB queries.DBTX }

// ErrInitiativeNotPublished is returned when an initiative does not exist or is
// not published; both map to 404 so the draft state is not observable.
var ErrInitiativeNotPublished = errors.New("initiative not published")

// SpendCategory is one category's total for an initiative, in minor units.
type SpendCategory struct {
	Category    string `json:"category"`
	AmountMinor int64  `json:"amount_minor"`
}

// InitiativeSpend is the category breakdown plus the total for one published
// initiative. Totals only — never line items (PRD §5.2).
type InitiativeSpend struct {
	InitiativeID string          `json:"initiative_id"`
	TotalMinor   int64           `json:"total_minor"`
	Categories   []SpendCategory `json:"categories"`
}

// Spend returns a published initiative's spend by category and its total.
func (reader InitiativeReader) Spend(ctx context.Context, id pgtype.UUID) (InitiativeSpend, error) {
	q := queries.New(reader.DB)
	initiative, err := q.InitiativeByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return InitiativeSpend{}, ErrInitiativeNotPublished
	}
	if err != nil {
		return InitiativeSpend{}, err
	}
	if initiative.PublishState != "PUBLISHED" {
		return InitiativeSpend{}, ErrInitiativeNotPublished
	}
	rows, err := q.InitiativeSpend(ctx, id)
	if err != nil {
		return InitiativeSpend{}, err
	}
	out := InitiativeSpend{InitiativeID: id.String(), Categories: make([]SpendCategory, 0, len(rows))}
	for _, row := range rows {
		out.TotalMinor += row.AmountMinor
		out.Categories = append(out.Categories, SpendCategory{Category: row.Category, AmountMinor: row.AmountMinor})
	}
	return out, nil
}

// maxShownHeadlineMetrics is how many of an initiative's (up to 6) published
// headline metrics the app shows (PRD §8).
const maxShownHeadlineMetrics = 4

// DonorInitiativeService is the signed-in donor's view of the daily rotation.
// It holds the donor's pool so it can record an exposure.
type DonorInitiativeService struct{ DB *pgxpool.Pool }

// TodayMetric is one shown headline metric of the day's initiative.
type TodayMetric struct {
	Label      string `json:"label"`
	Value      int64  `json:"value"`
	Unit       string `json:"unit"`
	VerifiedOn string `json:"verified_on"`
}

// TodayInitiative is the donor's initiative of the day, with the shown subset of
// its verified headline metrics and the donor's running distinct-shown count.
type TodayInitiative struct {
	InitiativeID  string        `json:"initiative_id"`
	Slug          string        `json:"slug"`
	Title         string        `json:"title"`
	Summary       string        `json:"summary"`
	Region        string        `json:"region"`
	Location      string        `json:"location"`
	Type          string        `json:"type"`
	StartedOn     string        `json:"started_on"`
	Metrics       []TodayMetric `json:"metrics"`
	DistinctShown int64         `json:"distinct_shown"`
}

// ErrNoInitiativeToday means no published, active initiative exists to rotate.
// It wraps pgx.ErrNoRows so the HTTP layer maps it to 404 like any empty read.
var ErrNoInitiativeToday = fmt.Errorf("no initiative to show today: %w", pgx.ErrNoRows)

// rotationIndex is the donor's slot in [0,n) on a local calendar day: each
// donor starts at their own offset and steps one initiative a day, so over any
// n consecutive days they see every active initiative once (A-1), while donors
// see different initiatives on the same day.
func rotationIndex(donorID pgtype.UUID, day int64, n int) int {
	if n <= 0 {
		return 0
	}
	sum := sha256.Sum256(donorID.Bytes[:])
	offset := binary.BigEndian.Uint64(sum[:8]) % uint64(n)
	return int((offset + uint64(day)%uint64(n)) % uint64(n))
}

// localDay numbers t's calendar date in zone.
func localDay(t time.Time, zone *time.Location) int64 {
	local := t.In(zone)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC).Unix() / 86400
}

// Today returns the donor's initiative of the day and records the exposure.
// The rotatable set is small and index-served, and the pick is an in-memory
// hash, so this is O(active set), never a scan of all initiatives (risk #6).
func (s DonorInitiativeService) Today(ctx context.Context, token string) (TodayInitiative, error) {
	return donorWrite(
		ctx,
		s.DB,
		donorOwner(token),
		func(tx pgx.Tx, donor queries.Donor, now time.Time) (TodayInitiative, error) {
			q := queries.New(tx)
			actives, err := q.RotatableInitiatives(ctx)
			if err != nil {
				return TodayInitiative{}, err
			}
			if len(actives) == 0 {
				return TodayInitiative{}, ErrNoInitiativeToday
			}
			zone := recognition.Zone(donor.Timezone.String)
			chosen := actives[rotationIndex(donor.ID, localDay(now, zone), len(actives))]

			day := pgtype.Date{Time: now.In(zone), Valid: true}
			if err = q.RecordInitiativeExposure(ctx, queries.RecordInitiativeExposureParams{
				DonorID: donor.ID, InitiativeID: chosen.ID, FirstShownOn: day,
			}); err != nil {
				return TodayInitiative{}, err
			}
			shown, err := q.DistinctInitiativeExposures(ctx, donor.ID)
			if err != nil {
				return TodayInitiative{}, err
			}
			rules, err := badgeRules(ctx, q)
			if err != nil {
				return TodayInitiative{}, err
			}
			for _, rule := range rules {
				if rule.Signal == "initiatives" && shown >= rule.Threshold {
					if err := earnBadge(ctx, q, donor.ID, rule.Code, pgtype.Text{}, now); err != nil {
						return TodayInitiative{}, err
					}
				}
			}
			metrics, err := q.PublishedInitiativeMetrics(ctx, chosen.ID)
			if err != nil {
				return TodayInitiative{}, err
			}
			out := TodayInitiative{
				InitiativeID: chosen.ID.String(), Slug: chosen.Slug, Title: chosen.Title,
				Summary: chosen.Summary, Region: chosen.Region, Location: chosen.Location, Type: chosen.Type,
				StartedOn: chosen.StartedOn.Time.Format(time.DateOnly), DistinctShown: shown,
				Metrics: make([]TodayMetric, 0, maxShownHeadlineMetrics),
			}
			for _, m := range metrics {
				if len(out.Metrics) >= maxShownHeadlineMetrics {
					break
				}
				verified := ""
				if m.VerifiedOn.Valid {
					verified = m.VerifiedOn.Time.Format(time.DateOnly)
				}
				out.Metrics = append(
					out.Metrics,
					TodayMetric{Label: m.Label, Value: m.Value, Unit: m.Unit, VerifiedOn: verified},
				)
			}
			return out, nil
		},
	)
}

// PublicInitiative is a published initiative as the public pages show it.
// Partner is the approved partner's name, or null when none is linked.
type PublicInitiative struct {
	ID        string  `json:"id"`
	Slug      string  `json:"slug"`
	Title     string  `json:"title"`
	Summary   string  `json:"summary"`
	Type      string  `json:"type"`
	Location  string  `json:"location"`
	Status    string  `json:"status"`
	StartedOn string  `json:"started_on"`
	Partner   *string `json:"partner"`
}

func publicInitiative(r queries.PublishedInitiativesRow) PublicInitiative {
	out := PublicInitiative{
		ID: r.ID.String(), Slug: r.Slug, Title: r.Title, Summary: r.Summary, Type: r.Type, Location: r.Location,
		Status: r.Status, StartedOn: r.StartedOn.Time.Format(time.DateOnly),
	}
	if r.Partner != "" {
		out.Partner = &r.Partner
	}
	return out
}

// Published lists every published initiative, active ones first.
func (reader InitiativeReader) Published(ctx context.Context) ([]PublicInitiative, error) {
	rows, err := queries.New(reader.DB).PublishedInitiatives(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]PublicInitiative, 0, len(rows))
	for _, r := range rows {
		out = append(out, publicInitiative(r))
	}
	return out, nil
}

// PublishedBySlug returns one published initiative.
func (reader InitiativeReader) PublishedBySlug(ctx context.Context, slug string) (PublicInitiative, error) {
	r, err := queries.New(reader.DB).PublishedInitiativeBySlug(ctx, slug)
	if errors.Is(err, pgx.ErrNoRows) {
		return PublicInitiative{}, ErrInitiativeNotPublished
	}
	if err != nil {
		return PublicInitiative{}, err
	}
	return publicInitiative(queries.PublishedInitiativesRow(r)), nil
}

// OverallSpend is spend by category across every published initiative, with
// the grand total. Totals only — never line items (PRD §5.2).
type OverallSpend struct {
	TotalMinor int64           `json:"total_minor"`
	Categories []SpendCategory `json:"categories"`
}

// OverallSpend sums each spend category across all published initiatives.
func (reader InitiativeReader) OverallSpend(ctx context.Context) (OverallSpend, error) {
	rows, err := queries.New(reader.DB).PublishedSpendByCategory(ctx)
	if err != nil {
		return OverallSpend{}, err
	}
	out := OverallSpend{Categories: make([]SpendCategory, 0, len(rows))}
	for _, row := range rows {
		out.TotalMinor += row.AmountMinor
		out.Categories = append(out.Categories, SpendCategory{Category: row.Category, AmountMinor: row.AmountMinor})
	}
	return out, nil
}
