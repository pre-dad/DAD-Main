package store

import (
	"context"
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
// reports how many it closed.
func (s TeamMonthService) CloseDue(ctx context.Context, now time.Time) (int, error) {
	local := now.In(recognition.NewYork)
	month := pgtype.Date{Time: time.Date(local.Year(), local.Month()-1, 1, 0, 0, 0, 0, time.UTC), Valid: true}
	closed := 0
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
		q := queries.New(tx)
		partners, err := q.PartnersDueTeamMonth(ctx, queries.PartnersDueTeamMonthParams{Month: month, Batch: teamMonthBatch})
		if err != nil {
			return err
		}
		for _, partner := range partners {
			won, err := closeTeamMonth(ctx, q, partner, month, now)
			if err != nil {
				return err
			}
			if won {
				closed++
			}
		}
		return nil
	})
	return closed, err
}

// closeTeamMonth records one partner's standings. It reports false when
// another worker closed the month first.
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
