package store

import (
	"context"
	"errors"
	"time"

	"dad/donor-api/internal/store/queries"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NetworkService serves a donor's referral network view (PRD §6.5 I-2,
// §6.7 N-1..N-3). All reads are a bounded lookup over the donor's own
// referrals, never a population scan.
type NetworkService struct{ DB *pgxpool.Pool }

// NetworkInvitee is one person the donor brought in, shown only when they opted
// in to visibility (N-3). Anonymous invitees are not listed; they are counted in
// AnonymousActive instead.
type NetworkInvitee struct {
	Name   string `json:"name"`
	Active bool   `json:"active"`
}

// NetworkSummary is the donor's network card.
type NetworkSummary struct {
	ActiveCount     int64             `json:"active_count"`
	AnnualizedMinor int64             `json:"annualized_minor"`
	NextMilestone   *NetworkMilestone `json:"next_milestone"`
	Invitees        []NetworkInvitee  `json:"invitees"`
	AnonymousActive int64             `json:"anonymous_active"`
}

// NetworkMilestone is the next referral tier the donor has not yet reached (N-2).
// Nil once every tier is reached.
type NetworkMilestone struct {
	Label     string `json:"label"`
	Target    int64  `json:"target"`
	Remaining int64  `json:"remaining"`
}

var milestoneLabels = map[string]string{
	"REF1":  "First Referral",
	"REF3":  "Community Builder",
	"REF12": "Super Connector",
	"TC":    "Team Captain",
}

// Network assembles the donor's network summary.
func (s NetworkService) Network(ctx context.Context, token string) (NetworkSummary, error) {
	return donorRead(ctx, s.DB, token, func(q *queries.Queries, d queries.Donor, _ time.Time) (NetworkSummary, error) {
		return buildNetwork(ctx, q, d.ID)
	})
}

func buildNetwork(ctx context.Context, q *queries.Queries, donorID pgtype.UUID) (NetworkSummary, error) {
	var out NetworkSummary
	totals, err := q.NetworkTotals(ctx, donorID)
	if err != nil {
		return out, err
	}
	out.ActiveCount = totals.ActiveCount
	out.AnnualizedMinor = totals.AnnualizedMinor

	// N-2: the converted-referral count drives the milestone ladder. Use the
	// lifetime converted count (link+irl), not the active count -- a referral who
	// later lapsed still counts toward "people you brought in".
	proj, err := q.Projection(ctx, donorID)
	if err == nil {
		converted := int64(proj.ReferredLinkCount + proj.ReferredIrlCount)
		rules, err := recognitionRules(ctx, q)
		if err != nil {
			return out, err
		}
		for _, rule := range rules.Badges {
			if rule.Signal != "referrals" || rule.Threshold <= converted {
				continue
			}
			if out.NextMilestone == nil || rule.Threshold < out.NextMilestone.Target {
				out.NextMilestone = &NetworkMilestone{
					Label:     milestoneLabels[rule.Code],
					Target:    rule.Threshold,
					Remaining: rule.Threshold - converted,
				}
			}
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}

	// N-3: opted-in invitees are listed by name; the rest of the active network
	// is reported anonymously.
	invitees, err := q.NetworkInviteeList(ctx, donorID)
	if err != nil {
		return out, err
	}
	var visibleActive int64
	out.Invitees = make([]NetworkInvitee, 0, len(invitees))
	for _, iv := range invitees {
		out.Invitees = append(out.Invitees, NetworkInvitee{Name: iv.PublicName.String, Active: iv.Active})
		if iv.Active {
			visibleActive++
		}
	}
	if anon := out.ActiveCount - visibleActive; anon > 0 {
		out.AnonymousActive = anon
	}
	return out, nil
}
