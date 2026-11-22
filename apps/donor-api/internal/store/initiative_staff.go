package store

import (
	"context"
	"time"

	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// StaffInitiative is an initiative as Impact Ops edits it, drafts included.
type StaffInitiative struct {
	InitiativeInput
	ID           string             `json:"id"`
	PublishState string             `json:"publish_state"`
	PublishedAt  pgtype.Timestamptz `json:"published_at"`
	InRotation   bool               `json:"in_rotation"`
	UpdatedAt    pgtype.Timestamptz `json:"updated_at"`
}

// StaffInitiativePage is one page of initiatives, with the offset of the next.
type StaffInitiativePage struct {
	Initiatives []StaffInitiative `json:"initiatives"`
	NextOffset  *int64            `json:"next_offset"`
}

// StaffInitiativeMetric is a headline metric with its publish state.
type StaffInitiativeMetric struct {
	MetricInput
	PublishState string `json:"publish_state"`
}

// StaffInitiativeDetail is the §8 initiative editor: the initiative, every
// headline metric whatever its state, and spend by category.
type StaffInitiativeDetail struct {
	StaffInitiative
	Metrics []StaffInitiativeMetric `json:"metrics"`
	Spend   []SpendInput            `json:"spend"`
}

func staffInitiative(i queries.DonorInitiative) StaffInitiative {
	partner := ""
	if i.PartnerSignupID.Valid {
		partner = i.PartnerSignupID.String()
	}
	approval, approvedOn := "", ""
	if i.BoardApprovalSourceID.Valid {
		approval, approvedOn = i.BoardApprovalSourceID.String(), i.BoardApprovedOn.Time.Format(time.DateOnly)
	}
	return StaffInitiative{
		InitiativeInput: InitiativeInput{
			Slug: i.Slug, Title: i.Title, Summary: i.Summary, Type: i.Type, Location: i.Location, Region: i.Region,
			Counties: i.Counties, StartedOn: i.StartedOn.Time.Format(time.DateOnly), Status: i.Status,
			CommittedMinor: i.CommittedMinor, PartnerSignupID: partner,
			BoardApprovalSourceID: approval, BoardApprovedOn: approvedOn,
		},
		ID: i.ID.String(), PublishState: i.PublishState, PublishedAt: i.PublishedAt, InRotation: i.InRotation, UpdatedAt: i.UpdatedAt,
	}
}

// List returns a page of initiatives of any publish state for staff.
func (s InitiativeService) List(ctx context.Context, token string, offset int64) (StaffInitiativePage, error) {
	if !pageOffsetValid(offset) {
		return StaffInitiativePage{}, ErrStaffInput
	}
	return staffRead(ctx, s.Staff, token, initiativePermission,
		func(tx pgx.Tx, _ queries.DonorStaff, _ time.Time) (StaffInitiativePage, error) {
			rows, err := queries.New(tx).StaffInitiativesPage(ctx, offset)
			if err != nil {
				return StaffInitiativePage{}, err
			}
			rows, next := page(rows, offset)
			out := StaffInitiativePage{Initiatives: make([]StaffInitiative, 0, len(rows)), NextOffset: next}
			for _, r := range rows {
				out.Initiatives = append(out.Initiatives, staffInitiative(r))
			}
			return out, nil
		})
}

func (s InitiativeService) Detail(ctx context.Context, token string, id pgtype.UUID) (StaffInitiativeDetail, error) {
	return staffRead(ctx, s.Staff, token, initiativePermission,
		func(tx pgx.Tx, _ queries.DonorStaff, _ time.Time) (StaffInitiativeDetail, error) {
			q := queries.New(tx)
			initiative, err := q.InitiativeByID(ctx, id)
			if err != nil {
				return StaffInitiativeDetail{}, err
			}
			metrics, err := q.InitiativeMetrics(ctx, id)
			if err != nil {
				return StaffInitiativeDetail{}, err
			}
			spend, err := q.StaffSpend(ctx, id)
			if err != nil {
				return StaffInitiativeDetail{}, err
			}
			out := StaffInitiativeDetail{
				StaffInitiative: staffInitiative(initiative),
				Metrics:         make([]StaffInitiativeMetric, 0, len(metrics)),
				Spend:           make([]SpendInput, 0, len(spend)),
			}
			for _, m := range metrics {
				verified := ""
				if m.VerifiedOn.Valid {
					verified = m.VerifiedOn.Time.Format(time.DateOnly)
				}
				out.Metrics = append(out.Metrics, StaffInitiativeMetric{
					MetricInput: MetricInput{
						Position: m.Position, Label: m.Label, Value: m.Value, Unit: m.Unit,
						VerificationNote: m.VerificationNote, VerifiedOn: verified,
					},
					PublishState: m.PublishState,
				})
			}
			for _, row := range spend {
				verified := ""
				if row.VerifiedOn.Valid {
					verified = row.VerifiedOn.Time.Format(time.DateOnly)
				}
				out.Spend = append(
					out.Spend,
					SpendInput{
						Category:         row.Category,
						AmountMinor:      row.AmountMinor,
						VerificationNote: row.VerificationNote,
						VerifiedOn:       verified,
						PublishState:     row.PublishState,
					},
				)
			}
			return out, nil
		})
}

// SetRotation includes or excludes an initiative from the daily rotation
// (§8 Content). Audited.
func (s InitiativeService) SetRotation(ctx context.Context, token string, id pgtype.UUID, inRotation bool) error {
	_, err := staffWrite(ctx, s.Staff, token, initiativePermission,
		func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (struct{}, error) {
			q := queries.New(tx)
			updated, err := q.SetInitiativeRotation(ctx, queries.SetInitiativeRotationParams{
				ID: id, InRotation: inRotation, UpdatedAt: timestamp(now),
			})
			if err != nil {
				return struct{}{}, err
			}
			if updated == 0 {
				return struct{}{}, pgx.ErrNoRows
			}
			return struct{}{}, auditStaff(ctx, q, actor.ID, "initiative.rotation_set", id.String(),
				map[string]bool{"in_rotation": inRotation}, now)
		})
	return err
}
