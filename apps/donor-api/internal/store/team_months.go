package store

import (
	"context"
	"errors"
	"time"

	"dad/donor-api/internal/recognition"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const teamMonthBatch = 20

// TeamMonthService closes each partner's team month (Appendix A, Winning
// Team): it records the standings once the month has ended and awards the
// badge to the members of the team at #1.
type TeamMonthService struct{ DB *pgxpool.Pool }

// CloseDue closes the month before now for up to a batch of partners and
// reports how many it closed. Partners are paged by id, so ones whose metrics
// are not yet verified cannot crowd out those that can close. Each partner
// closes in its own transaction: a failure is reported after the rest have
// been tried, and the count covers only what committed.
func (s TeamMonthService) CloseDue(ctx context.Context, now time.Time) (int, error) {
	local := now.In(recognition.NewYork)
	month := pgtype.Date{Time: time.Date(local.Year(), local.Month()-1, 1, 0, 0, 0, 0, time.UTC), Valid: true}
	q := queries.New(s.DB)
	var errs []error
	closed := 0
	var after pgtype.UUID
	for closed < teamMonthBatch {
		partners, err := q.PartnersDueTeamMonth(ctx, queries.PartnersDueTeamMonthParams{Month: month, After: after, Batch: teamMonthBatch})
		if err != nil {
			return closed, errors.Join(append(errs, err)...)
		}
		for _, partner := range partners {
			won, err := transact(ctx, s.DB, pgx.TxOptions{}, func(tx pgx.Tx) (bool, error) {
				return closeTeamMonth(ctx, queries.New(tx), partner, month, now)
			})
			if err != nil {
				errs = append(errs, err)
			} else if won {
				closed++
			}
		}
		if len(partners) < teamMonthBatch {
			break
		}
		after = partners[len(partners)-1]
	}
	return closed, errors.Join(errs...)
}

// closeTeamMonth records one partner's standings. It reports false when the
// partner's metrics are not yet verified or another worker closed the month
// first. Standings rank current membership and projections as of the close,
// not a snapshot of the month's last instant, so a late close after an outage
// ranks later data. Ties break by team name, then id.
func closeTeamMonth(ctx context.Context, q *queries.Queries, partner pgtype.UUID, month pgtype.Date, now time.Time) (bool, error) {
	version, err := currentRulesVersion(ctx, q)
	if err != nil {
		return false, err
	}
	verified, err := q.PartnerTeamMetricsVerified(
		ctx,
		queries.PartnerTeamMetricsVerifiedParams{PartnerID: partner, AsOf: timestamp(now), Minimum: minimumGroup, RulesVersion: version},
	)
	if err != nil || !verified {
		return false, err
	}
	inserted, err := q.CloseTeamMonth(ctx, queries.CloseTeamMonthParams{PartnerID: partner, Month: month, ClosedAt: timestamp(now)})
	if err != nil || inserted == 0 {
		return false, err
	}
	standings, err := q.RecordTeamMonthStandings(
		ctx,
		queries.RecordTeamMonthStandingsParams{PartnerID: partner, Month: month, Minimum: minimumGroup},
	)
	if err != nil {
		return false, err
	}
	for _, team := range standings {
		// A month in which no team gave has no winner.
		if team.Rank != 1 || team.PerMemberMinor == 0 {
			continue
		}
		if err := q.SetTeamMonthWinner(ctx, queries.SetTeamMonthWinnerParams{PartnerID: partner, Month: month, WinnerTeamID: team.TeamID}); err != nil {
			return false, err
		}
		members, err := q.TeamMembers(ctx, team.TeamID)
		if err != nil {
			return false, err
		}
		for _, donor := range members {
			if err := earnBadge(ctx, q, donor, "WT", pgtype.Text{}, now); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}
