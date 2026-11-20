package store

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"time"
	"unicode/utf8"

	"dad/donor-api/internal/identity"
	"dad/donor-api/internal/recognition"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrEmployeeLink means the company code matches no approved partner.
	ErrEmployeeLink = errors.New("unknown company code")
	// ErrEmployerLinked means the donor is already an employee of another partner.
	ErrEmployerLinked = errors.New("already linked to another employer")
	// ErrTeam is an invalid team (bad name, or a team in another partner).
	ErrTeam = errors.New("invalid team")
)

// EmployeeService is employee giving for all three audiences: the donor's
// company code and team, the partner's k-anonymised aggregates, and the staff
// matching ledger. Staff is nil when the back office is not configured.
type EmployeeService struct {
	DB    *pgxpool.Pool
	Staff *StaffService
}

// PartnerOurPeople is the Our People view (P-13). While fewer than
// minimumGroup employees are linked every figure is zero and Suppressed is set
// (EM-3). MatchedMinor is this calendar year's match owed (EM-6, EM-7).
type PartnerOurPeople struct {
	StreakVerified   bool  `json:"streak_verified"`
	Enrolled         int64 `json:"enrolled"`
	WorkforceSize    int32 `json:"workforce_size"`
	GivenMinor       int64 `json:"given_minor"`
	MatchedMinor     int64 `json:"matched_minor"`
	AvgStreakDays    int64 `json:"avg_streak_days"`
	VolunteerMinutes int64 `json:"volunteer_minutes"`
	Teams            int64 `json:"teams"`
	Suppressed       bool  `json:"suppressed"`
}

// EmployeeTeamRow is one team on the company leaderboard (EM-5).
// AnnualizedMinor is the combined active giving it is ranked by.
type EmployeeTeamRow struct {
	MetricsVerified  bool   `json:"metrics_verified"`
	TeamID           string `json:"team_id"`
	Name             string `json:"name"`
	ActiveGivers     int64  `json:"active_givers"`
	AnnualizedMinor  int64  `json:"annualized_minor"`
	AvgStreakDays    int64  `json:"avg_streak_days"`
	VolunteerMinutes int64  `json:"volunteer_minutes"`
}

// DonorTeam is the donor's employer, their team, and their company's
// leaderboard (EM-2, EM-4, EM-5), for /donor/me/team.
type DonorTeam struct {
	PartnerID    string            `json:"partner_id"`
	Organization string            `json:"organization"`
	TeamID       string            `json:"team_id,omitempty"`
	TeamName     string            `json:"team_name,omitempty"`
	Leaderboard  []EmployeeTeamRow `json:"leaderboard"`
}

// MatchStatus is the §8 matching view (EM-6/EM-7): owed this calendar year and
// received offline. No money moves.
type MatchStatus struct {
	Year             int32 `json:"year"`
	RateBP           int32 `json:"match_rate_bp"`
	CapMinor         int64 `json:"match_cap_minor"`
	GiftsMinor       int64 `json:"gifts_minor"`
	OwedMinor        int64 `json:"owed_minor"`
	PaidMinor        int64 `json:"paid_minor"`
	OutstandingMinor int64 `json:"outstanding_minor"`
}

// Enroll links the authenticated donor to the partner whose company code they
// entered (EM-2). The code is normalised to upper case. Re-entering the same
// employer's code is idempotent; one employer per donor, so another
// employer's code is refused. Returns the partner organisation name.
func (s EmployeeService) Enroll(ctx context.Context, token, code string) (string, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if !companyCode.MatchString(code) {
		return "", ErrEmployeeLink
	}
	return donorWrite(ctx, s.DB, donorOwner(token), func(tx pgx.Tx, d queries.Donor, now time.Time) (string, error) {
		q := queries.New(tx)
		partnerID, err := q.PartnerByCompanyCode(ctx, text(code))
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrEmployeeLink
		}
		if err != nil {
			return "", err
		}
		if err = q.UpsertEmployeeLink(ctx, queries.UpsertEmployeeLinkParams{
			DonorID: d.ID, PartnerID: partnerID, EnrolledAt: timestamp(now),
		}); err != nil {
			return "", err
		}
		link, err := q.EmployeeLink(ctx, d.ID)
		if err != nil {
			return "", err
		}
		if link.PartnerID != partnerID {
			return "", ErrEmployerLinked
		}
		org, err := q.PartnerOrganizationByID(ctx, partnerID)
		if err != nil {
			return "", err
		}
		return org, nil
	})
}

// Team returns the donor's employer, team and company leaderboard, or a zero
// value when unlinked.
func (s EmployeeService) Team(ctx context.Context, token string) (DonorTeam, error) {
	return donorRead(ctx, s.DB, token, func(q *queries.Queries, d queries.Donor, now time.Time) (DonorTeam, error) {
		link, err := q.EmployeeLink(ctx, d.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			return DonorTeam{Leaderboard: []EmployeeTeamRow{}}, nil
		}
		if err != nil {
			return DonorTeam{}, err
		}
		org, err := q.PartnerOrganizationByID(ctx, link.PartnerID)
		if err != nil {
			return DonorTeam{}, err
		}
		out := DonorTeam{PartnerID: link.PartnerID.String(), Organization: org}
		if out.Leaderboard, err = teamLeaderboard(ctx, q, link.PartnerID, now); err != nil {
			return DonorTeam{}, err
		}
		if link.TeamID.Valid {
			team, err := q.TeamByID(ctx, link.TeamID)
			if err != nil {
				return DonorTeam{}, err
			}
			out.TeamID, out.TeamName = team.ID.String(), team.Name
		}
		return out, nil
	})
}

// JoinTeam places the donor on a named team of their own employer, creating the
// team on first use (EM-4). One team per employee.
func (s EmployeeService) JoinTeam(ctx context.Context, token, name string) (string, error) {
	name = identity.TrimSpace(name)
	if n := utf8.RuneCountInString(name); n == 0 || n > 80 || !plainText(name, false) {
		return "", ErrTeam
	}
	return donorWrite(ctx, s.DB, donorOwner(token), func(tx pgx.Tx, d queries.Donor, now time.Time) (string, error) {
		q := queries.New(tx)
		link, err := q.EmployeeLink(ctx, d.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrEmployeeLink
		}
		if err != nil {
			return "", err
		}
		teamID, err := teamByName(ctx, q, link.PartnerID, name)
		if err != nil {
			return "", err
		}
		if err = q.SetEmployeeTeam(ctx, queries.SetEmployeeTeamParams{DonorID: d.ID, TeamID: teamID}); err != nil {
			return "", err
		}
		return teamID.String(), awardBadges(ctx, q, d, now)
	})
}

// teamByName finds a partner's team by name or creates it, returning its id.
func teamByName(ctx context.Context, q *queries.Queries, partnerID pgtype.UUID, name string) (pgtype.UUID, error) {
	newID, err := NewID()
	if err != nil {
		return pgtype.UUID{}, err
	}
	id, err := q.InsertTeam(ctx, queries.InsertTeamParams{
		ID: newID, PartnerID: partnerID, Name: name, CreatedAt: timestamp(time.Now().UTC()),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return q.TeamIDByName(ctx, queries.TeamIDByNameParams{PartnerID: partnerID, Name: name})
	}
	if err == nil {
		err = recordServerEvent(ctx, q, pgtype.UUID{}, "team_created", "team-created:"+id.String(), time.Now().UTC())
	}
	return id, err
}

// calendarYear bounds a matching year: the cap resets each
// calendar year (EM-6), in the zone DAD's tax years use.
func calendarYear(year int) (time.Time, time.Time) {
	start := time.Date(year, 1, 1, 0, 0, 0, 0, recognition.NewYork)
	return start, start.AddDate(1, 0, 0)
}

// matchOwed is a year's gifts times the match rate, capped (EM-6). A cap of
// zero means no cap.
func matchOwed(giftsMinor int64, rateBP int32, capMinor int64) (int64, error) {
	if giftsMinor < 0 || rateBP < 0 || capMinor < 0 {
		return 0, errors.New("invalid matching terms or gifts")
	}
	owed := new(big.Int).Mul(big.NewInt(giftsMinor), big.NewInt(int64(rateBP)))
	owed.Quo(owed, big.NewInt(10000))
	if capMinor > 0 && owed.Cmp(big.NewInt(capMinor)) > 0 {
		return capMinor, nil
	}
	if !owed.IsInt64() {
		return 0, errors.New("matching amount exceeds supported range")
	}
	return owed.Int64(), nil
}

// OurPeople returns the calling partner's Our People totals (P-13). Any
// partner role may view; individuals are never returned (EM-3).
func (s EmployeeService) OurPeople(ctx context.Context, token string) (PartnerOurPeople, error) {
	return partnerRead(ctx, s.DB, token, func(q *queries.Queries, actor PartnerActor) (PartnerOurPeople, error) {
		now := time.Now().UTC()
		start, end := calendarYear(now.In(recognition.NewYork).Year())
		version, err := currentRulesVersion(ctx, q)
		if err != nil {
			return PartnerOurPeople{}, err
		}
		row, err := q.PartnerOurPeople(ctx, queries.PartnerOurPeopleParams{
			RulesVersion: version,
			PartnerID:    actor.PartnerID, YearStart: timestamp(start), YearEnd: timestamp(end), AsOf: timestamp(now),
		})
		if err != nil {
			return PartnerOurPeople{}, err
		}
		if row.Enrolled < minimumGroup {
			return PartnerOurPeople{WorkforceSize: row.WorkforceSize, Suppressed: true}, nil
		}
		matched, err := matchOwed(row.YearGivenMinor, row.MatchRateBp, row.MatchCapMinor)
		if err != nil {
			return PartnerOurPeople{}, err
		}
		return PartnerOurPeople{
			Enrolled: row.Enrolled, WorkforceSize: row.WorkforceSize, GivenMinor: row.GivenMinor,
			MatchedMinor:   matched,
			StreakVerified: row.StreakVerified, AvgStreakDays: row.AvgStreakDays, VolunteerMinutes: row.VolunteerMinutes, Teams: row.Teams,
		}, nil
	})
}

func teamLeaderboard(ctx context.Context, q *queries.Queries, partnerID pgtype.UUID, now time.Time) ([]EmployeeTeamRow, error) {
	version, err := currentRulesVersion(ctx, q)
	if err != nil {
		return nil, err
	}
	rows, err := q.TeamLeaderboard(
		ctx,
		queries.TeamLeaderboardParams{PartnerID: partnerID, AsOf: timestamp(now), Minimum: minimumGroup, RulesVersion: version},
	)
	if err != nil {
		return nil, err
	}
	out := make([]EmployeeTeamRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, EmployeeTeamRow{
			MetricsVerified: r.MetricsVerified, TeamID: r.TeamID.String(), Name: r.Name, ActiveGivers: r.ActiveGivers, AnnualizedMinor: r.AnnualizedMinor,
			AvgStreakDays: r.AvgStreakDays, VolunteerMinutes: r.VolunteerMinutes,
		})
	}
	return out, nil
}

// Teams returns the calling partner's team leaderboard (EM-5).
func (s EmployeeService) Teams(ctx context.Context, token string) ([]EmployeeTeamRow, error) {
	return partnerRead(ctx, s.DB, token, func(q *queries.Queries, actor PartnerActor) ([]EmployeeTeamRow, error) {
		return teamLeaderboard(ctx, q, actor.PartnerID, time.Now().UTC())
	})
}

// Match computes the matching status for a partner in a calendar year (EM-6):
// owed = linked-employee gifts (including pre-link) × rate, capped at the annual
// cap; outstanding = owed − received. Staff-only (partner.manage).
func (s EmployeeService) Match(ctx context.Context, token string, partnerID pgtype.UUID, year int32) (MatchStatus, error) {
	if s.Staff == nil {
		return MatchStatus{}, ErrStaffDenied
	}
	return staffRead(ctx, s.Staff, token, partnerPermission, func(tx pgx.Tx, actor queries.DonorStaff, _ time.Time) (MatchStatus, error) {
		q := queries.New(tx)
		terms, err := q.PartnerMatchTerms(ctx, partnerID)
		if err != nil {
			return MatchStatus{}, err
		}
		start, end := calendarYear(int(year))
		gifts, err := q.LinkedEmployeeGiftsInYear(ctx, queries.LinkedEmployeeGiftsInYearParams{
			PartnerID: partnerID, YearStart: timestamp(start), YearEnd: timestamp(end),
		})
		if err != nil {
			return MatchStatus{}, err
		}
		paid, err := q.MatchPaymentsInYear(ctx, queries.MatchPaymentsInYearParams{PartnerID: partnerID, Year: year})
		if err != nil {
			return MatchStatus{}, err
		}
		owed, err := matchOwed(gifts, terms.MatchRateBp, terms.MatchCapMinor)
		if err != nil {
			return MatchStatus{}, err
		}
		outstanding := max(owed-paid, 0)
		return MatchStatus{
			Year: year, RateBP: terms.MatchRateBp, CapMinor: terms.MatchCapMinor,
			GiftsMinor: gifts, OwedMinor: owed, PaidMinor: paid, OutstandingMinor: outstanding,
		}, nil
	})
}

// RecordMatchPayment records an offline match payment received (EM-7). No money
// moves. Staff-only.
func (s EmployeeService) RecordMatchPayment(
	ctx context.Context,
	token string,
	partnerID pgtype.UUID,
	year int32,
	amountMinor int64,
	requestID pgtype.UUID,
) error {
	if amountMinor <= 0 || !ValidRequestID(requestID) {
		return ErrStaffInput
	}
	if s.Staff == nil {
		return ErrStaffDenied
	}
	_, err := staffWrite(
		ctx,
		s.Staff,
		token,
		partnerPermission,
		func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (struct{}, error) {
			q := queries.New(tx)
			if _, err := q.PartnerMatchTerms(ctx, partnerID); err != nil {
				return struct{}{}, err
			}
			inserted, err := q.InsertMatchPayment(ctx, queries.InsertMatchPaymentParams{
				ID: requestID, PartnerID: partnerID, Year: year, AmountMinor: amountMinor,
				ReceivedAt: timestamp(now), RecordedBy: actor.ID,
			})
			if err != nil {
				return struct{}{}, err
			}
			if inserted == 0 {
				matches, err := q.MatchPaymentMatches(
					ctx,
					queries.MatchPaymentMatchesParams{ID: requestID, PartnerID: partnerID, Year: year, AmountMinor: amountMinor},
				)
				if err != nil {
					return struct{}{}, err
				}
				if !matches {
					return struct{}{}, ErrStaffConflict
				}
				return struct{}{}, nil
			}
			return struct{}{}, auditStaff(
				ctx,
				q,
				actor.ID,
				"partner.match_payment",
				partnerID.String(),
				map[string]any{"payment_id": requestID.String(), "amount_minor": amountMinor, "year": year},
				now,
			)
		},
	)
	return err
}

// maxWorkforceSize bounds the self-reported workforce size to a plausible headcount.
const maxWorkforceSize = 1_000_000

// SetWorkforce updates a partner's workforce size (EM-9). Partner admin only.
func (s EmployeeService) SetWorkforce(ctx context.Context, token string, size int32) error {
	if size < 0 || size > maxWorkforceSize {
		return ErrPartnerApplication
	}
	_, err := partnerAdminWrite(ctx, s.DB, token, func(tx pgx.Tx, actor PartnerActor, _ time.Time) (struct{}, error) {
		return struct{}{}, queries.New(tx).
			SetPartnerWorkforce(ctx, queries.SetPartnerWorkforceParams{ID: actor.PartnerID, WorkforceSize: size})
	})
	return err
}
