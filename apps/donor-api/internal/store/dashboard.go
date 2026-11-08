package store

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"dad/donor-api/internal/recognition"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DonorDashboard is the web dashboard contract.
type DonorDashboard struct {
	ComputedAt    time.Time        `json:"computed_at"`
	FoundingDonor bool             `json:"founding_donor"`
	Mode          string           `json:"mode"`
	Referrals     *ReferralSummary `json:"referrals"`
	State         string           `json:"state"`
	Streak        *StreakSummary   `json:"streak"`
	Rank          *Ranking         `json:"rank"`
	// RankingsOptedOut tells an opted-out donor from an opted-in one who has no
	// rank yet; both have a null Rank.
	RankingsOptedOut bool          `json:"rankings_opted_out"`
	Network          *NetworkCard  `json:"network"`
	Impact           ImpactSummary `json:"impact"`
}

// ImpactSummary is the donor's own impact totals (PRD I-1, I-3): what they have
// given since joining, net of refunds, the distinct initiatives they have been
// shown (A-4), and the events they checked in at.
type ImpactSummary struct {
	GivenMinor        *int64 `json:"given_minor"`
	Currency          string `json:"currency"`
	InitiativesFunded int64  `json:"initiatives_funded"`
	EventsVolunteered int64  `json:"events_volunteered"`
}

type ReferralSummary struct {
	Currency   string `json:"currency"`
	IRL        int64  `json:"irl"`
	IRLMinor   int64  `json:"irl_minor"`
	Link       int64  `json:"link"`
	LinkMinor  int64  `json:"link_minor"`
	TotalMinor int64  `json:"total_minor"`
}

type StreakSummary struct {
	AtRisk      bool       `json:"at_risk"`
	Current     int32      `json:"current"`
	GraceEndsAt *time.Time `json:"grace_ends_at"`
	// Ladder is the day-based streak card (PRD S-2/S-3): days since the run
	// began, the milestone ladder, days to the next milestone, and the 30-day bar.
	Ladder *recognition.StreakLadder `json:"ladder,omitempty"`
}

// NetworkCard is the dashboard headline for the donor's referral network
// (PRD I-2): the number of currently-active donors they brought in and that
// network's combined annualized giving. The full invitee list and milestone
// progress live on GET /donor/me/network.
type NetworkCard struct {
	ActiveCount     int64  `json:"active_count"`
	AnnualizedMinor int64  `json:"annualized_minor"`
	Currency        string `json:"currency"`
}

// MarshalJSON reports a dashboard still being prepared by its state alone.
func (d DonorDashboard) MarshalJSON() ([]byte, error) {
	if d.State == "preparing" {
		return json.Marshal(struct {
			Mode  string `json:"mode"`
			State string `json:"state"`
		}{d.Mode, d.State})
	}
	type dashboard DonorDashboard
	return json.Marshal(dashboard(d))
}

// dashboard reads the donor's projections, which must come from one snapshot:
// a worker may update several of them while the queries run.
func dashboard(ctx context.Context, q *queries.Queries, donor queries.Donor, now time.Time) (DonorDashboard, error) {
	id := donor.ID
	row, err := q.Projection(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return DonorDashboard{Mode: "test", State: "preparing"}, nil
	}
	if err != nil {
		return DonorDashboard{}, err
	}
	referred, err := q.ReferralTotals(ctx, queries.ReferralTotalsParams{ReferrerID: id, NextRefreshAt: timestamp(now)})
	if err != nil {
		return DonorDashboard{}, err
	}
	missing, err := q.MissingReferredProjection(ctx, id)
	if err != nil {
		return DonorDashboard{}, err
	}
	linkAmount, linkErr := referred.LinkMinor.Int64Value()
	irlAmount, irlErr := referred.IrlMinor.Int64Value()
	ready := !missing && referred.Ready && linkErr == nil && irlErr == nil && linkAmount.Valid && irlAmount.Valid
	linkMinor, irlMinor := linkAmount.Int64, irlAmount.Int64
	result := DonorDashboard{Mode: "test", State: "ready", ComputedAt: row.ComputedAt.Time, FoundingDonor: row.FoundingAwardedAt.Valid}
	if ready && irlMinor <= math.MaxInt64-linkMinor {
		result.Referrals = &ReferralSummary{
			Link:       referred.LinkCount,
			IRL:        referred.IrlCount,
			LinkMinor:  linkMinor,
			IRLMinor:   irlMinor,
			TotalMinor: linkMinor + irlMinor,
			Currency:   "USD",
		}
	}
	if row.NextRefreshAt.Time.After(now) && row.CurrentStreak.Valid {
		result.Streak = &StreakSummary{Current: row.CurrentStreak.Int32, AtRisk: row.AtRisk}
		if row.GraceEndsAt.Valid {
			result.Streak.GraceEndsAt = &row.GraceEndsAt.Time
		}
		var since *time.Time
		if row.StreakSince.Valid {
			since = &row.StreakSince.Time
		}
		ladder, err := configuredLadder(ctx, q, recognition.Streak{Since: since}, now, recognition.Zone(donor.Timezone.String))
		if err != nil {
			return DonorDashboard{}, err
		}
		result.Streak.Ladder = &ladder
	}
	// Rank line under the streak (S-4): the donor's own position only.
	rank, err := ForDonor(ctx, q, id)
	if err != nil {
		return DonorDashboard{}, err
	}
	result.Rank = rank
	preference, err := q.DonorPreference(ctx, id)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return DonorDashboard{}, err
	}
	result.RankingsOptedOut = preference.RankingsOptedOut
	// Network headline (I-2): active-network count + annualized giving.
	net, err := q.NetworkTotals(ctx, id)
	if err != nil {
		return DonorDashboard{}, err
	}
	if net.ActiveCount > 0 {
		result.Network = &NetworkCard{ActiveCount: net.ActiveCount, AnnualizedMinor: net.AnnualizedMinor, Currency: "USD"}
	}
	result.Impact.Currency = "USD"
	if row.NetMinor.Valid {
		result.Impact.GivenMinor = &row.NetMinor.Int64
	}
	if result.Impact.InitiativesFunded, err = q.DistinctInitiativeExposures(ctx, id); err != nil {
		return DonorDashboard{}, err
	}
	volunteered, err := q.DonorVolunteerStats(ctx, id)
	if err != nil {
		return DonorDashboard{}, err
	}
	result.Impact.EventsVolunteered = volunteered.Events
	return result, nil
}

type DashboardReader struct{ DB *pgxpool.Pool }

func (reader DashboardReader) DonorDashboard(ctx context.Context, token string) (DonorDashboard, error) {
	return donorRead(ctx, reader.DB, token, func(q *queries.Queries, d queries.Donor, now time.Time) (DonorDashboard, error) {
		return dashboard(ctx, q, d, now)
	})
}
