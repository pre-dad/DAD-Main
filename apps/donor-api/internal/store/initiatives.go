package store

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"

	"dad/donor-api/internal/identity"
	"dad/donor-api/internal/recognition"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// InitiativeService is the Impact Ops staff surface for initiatives and their
// headline metrics (PRD §8). An initiative or metric is editable while DRAFT
// and only becomes donor-visible once published, and a metric cannot be
// published until it carries a verification note and date (enforced in the DB
// and rechecked here for a clear error).
type InitiativeService struct{ Staff *StaffService }

const initiativePermission = "initiative.manage"

// Up to 6 headline metrics per initiative, of which the app shows the first 4
// (PRD §8).
const maxHeadlineMetrics = 6

// InitiativeInput is a staff create/update of an initiative's editable fields.
type InitiativeInput struct {
	Slug            string   `json:"slug"`
	Title           string   `json:"title"`
	Summary         string   `json:"summary"`
	Type            string   `json:"type"`
	Location        string   `json:"location"`
	Region          string   `json:"region"`
	Counties        []string `json:"counties"`
	StartedOn       string   `json:"started_on"`
	Status          string   `json:"status"`
	CommittedMinor  int64    `json:"committed_minor"`
	PartnerSignupID string   `json:"partner_signup_id"`
	// The board approval: a stored content source and the date it was given.
	// Both or neither; publishing requires them.
	BoardApprovalSourceID string `json:"board_approval_source_id"`
	BoardApprovedOn       string `json:"board_approved_on"`
}

// ErrBoardApprovalRequired refuses to publish an initiative, or to remove the
// approval of a published one, without a recorded board approval.
var ErrBoardApprovalRequired = errors.New("initiative has no board approval")

// MetricInput is a staff upsert of one headline metric at a position (0-5).
type MetricInput struct {
	Position         int32  `json:"position"`
	Label            string `json:"label"`
	Value            int64  `json:"value"`
	Unit             string `json:"unit"`
	VerificationNote string `json:"verification_note"`
	VerifiedOn       string `json:"verified_on"`
}

var initiativeSlug = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// validate checks an initiative input, returning ErrStaffInput on any problem.
func (in InitiativeInput) validate(now time.Time) (queries.InsertInitiativeParams, error) {
	var params queries.InsertInitiativeParams
	if len(in.Slug) > 100 || !initiativeSlug.MatchString(in.Slug) {
		return params, ErrStaffInput
	}
	for _, f := range []struct {
		value   string
		maximum int
	}{{in.Title, 150}, {in.Summary, 4000}, {in.Type, 40}, {in.Location, 200}, {in.Region, 100}} {
		if identity.TrimSpace(f.value) == "" || utf8.RuneCountInString(f.value) > f.maximum ||
			!plainText(f.value, f.maximum > 200) {
			return params, ErrStaffInput
		}
	}
	if in.Status != "ACTIVE" && in.Status != "COMPLETE" {
		return params, ErrStaffInput
	}
	if in.CommittedMinor < 0 {
		return params, ErrStaffInput
	}
	if len(in.Counties) > 500 {
		return params, ErrStaffInput
	}
	for _, county := range in.Counties {
		if identity.TrimSpace(county) == "" || utf8.RuneCountInString(county) > 100 || !plainText(county, false) {
			return params, ErrStaffInput
		}
	}
	started, err := time.Parse(time.DateOnly, in.StartedOn)
	if err != nil {
		return params, ErrStaffInput
	}
	var partner pgtype.UUID
	if in.PartnerSignupID != "" {
		if partner.Scan(in.PartnerSignupID) != nil || !partner.Valid {
			return params, ErrStaffInput
		}
	}
	var approvalSource pgtype.UUID
	var approvedOn pgtype.Date
	if (in.BoardApprovalSourceID == "") != (in.BoardApprovedOn == "") {
		return params, ErrStaffInput
	}
	if in.BoardApprovalSourceID != "" {
		if approvalSource.Scan(in.BoardApprovalSourceID) != nil || !approvalSource.Valid {
			return params, ErrStaffInput
		}
		day, err := time.Parse(time.DateOnly, in.BoardApprovedOn)
		if err != nil || in.BoardApprovedOn > now.In(recognition.NewYork).Format(time.DateOnly) {
			return params, ErrStaffInput
		}
		approvedOn = pgtype.Date{Time: day, Valid: true}
	}
	counties := in.Counties
	if counties == nil {
		counties = []string{}
	}
	params = queries.InsertInitiativeParams{
		Slug:                  in.Slug,
		Title:                 in.Title,
		Summary:               in.Summary,
		Type:                  in.Type,
		Location:              in.Location,
		Region:                in.Region,
		Counties:              counties,
		StartedOn:             pgtype.Date{Time: started, Valid: true},
		Status:                in.Status,
		CommittedMinor:        in.CommittedMinor,
		PartnerSignupID:       partner,
		BoardApprovalSourceID: approvalSource,
		BoardApprovedOn:       approvedOn,
		CreatedAt:             timestamp(now),
	}
	return params, nil
}

// Create inserts a new DRAFT initiative and returns its id.
func (s InitiativeService) Create(ctx context.Context, token string, in InitiativeInput) (pgtype.UUID, error) {
	return staffWrite(ctx, s.Staff, token, initiativePermission,
		func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (pgtype.UUID, error) {
			params, err := in.validate(now)
			if err != nil {
				return pgtype.UUID{}, err
			}
			id, err := NewID()
			if err != nil {
				return pgtype.UUID{}, err
			}
			params.ID = id
			q := queries.New(tx)
			if err = requireApprovedPartner(ctx, q, params.PartnerSignupID); err != nil {
				return pgtype.UUID{}, err
			}
			if err = requireApprovalSource(ctx, q, params.BoardApprovalSourceID); err != nil {
				return pgtype.UUID{}, err
			}
			if err = q.InsertInitiative(ctx, params); err != nil {
				return pgtype.UUID{}, err
			}
			if err = auditStaff(ctx, q, actor.ID, "initiative.created", id.String(), map[string]string{"slug": in.Slug}, now); err != nil {
				return pgtype.UUID{}, err
			}
			return id, nil
		})
}

// Update edits an existing initiative's fields. Publishing is separate.
func (s InitiativeService) Update(ctx context.Context, token string, id pgtype.UUID, in InitiativeInput) error {
	_, err := staffWrite(ctx, s.Staff, token, initiativePermission,
		func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (struct{}, error) {
			params, err := in.validate(now)
			if err != nil {
				return struct{}{}, err
			}
			q := queries.New(tx)
			existing, err := q.LockInitiative(ctx, id)
			if err != nil {
				return struct{}{}, err
			}
			if params.PartnerSignupID != existing.PartnerSignupID {
				if err = requireApprovedPartner(ctx, q, params.PartnerSignupID); err != nil {
					return struct{}{}, err
				}
			}
			if existing.PublishState == "PUBLISHED" && existing.BoardApprovalSourceID.Valid && !params.BoardApprovalSourceID.Valid {
				return struct{}{}, ErrBoardApprovalRequired
			}
			if err = requireApprovalSource(ctx, q, params.BoardApprovalSourceID); err != nil {
				return struct{}{}, err
			}
			if err = q.UpdateInitiative(ctx, queries.UpdateInitiativeParams{
				ID: id, Title: params.Title, Summary: params.Summary, Type: params.Type,
				Location: params.Location, Region: params.Region, Counties: params.Counties,
				StartedOn: params.StartedOn, Status: params.Status, CommittedMinor: params.CommittedMinor,
				PartnerSignupID:       params.PartnerSignupID,
				BoardApprovalSourceID: params.BoardApprovalSourceID, BoardApprovedOn: params.BoardApprovedOn,
				UpdatedAt: timestamp(now),
			}); err != nil {
				return struct{}{}, err
			}
			// INITIATIVE_COMPLETE partner email (§6.11) on the ACTIVE->COMPLETE
			// transition only, for a partner-owned initiative. Keyed per
			// initiative so a later edit of an already-complete initiative does
			// not re-notify.
			if existing.Status != "COMPLETE" && params.Status == "COMPLETE" {
				key := "partner-complete:" + id.String()
				if err = queueInitiativePartners(ctx, q, id, "INITIATIVE_COMPLETE", key, map[string]string{"initiative_id": id.String()}, now); err != nil {
					return struct{}{}, err
				}
			}
			return struct{}{}, auditStaff(ctx, q, actor.ID, "initiative.updated", id.String(), nil, now)
		})
	return err
}

// Publish makes a DRAFT initiative donor-visible. Publishing a published
// initiative is a no-op, so its published time is not reset.
func (s InitiativeService) Publish(ctx context.Context, token string, id pgtype.UUID) error {
	_, err := staffWrite(ctx, s.Staff, token, initiativePermission,
		func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (struct{}, error) {
			q := queries.New(tx)
			initiative, err := q.LockInitiative(ctx, id)
			if err != nil {
				return struct{}{}, err
			}
			if initiative.PublishState == "PUBLISHED" {
				return struct{}{}, nil
			}
			if !initiative.BoardApprovalSourceID.Valid {
				return struct{}{}, ErrBoardApprovalRequired
			}
			if err := q.PublishInitiative(ctx, queries.PublishInitiativeParams{ID: id, PublishedAt: timestamp(now)}); err != nil {
				return struct{}{}, err
			}
			return struct{}{}, auditStaff(ctx, q, actor.ID, "initiative.published", id.String(), nil, now)
		})
	return err
}

// SetMetric upserts one headline metric at its position. A metric carrying a
// verification note and date may later be published; the publish step is
// separate so an unverified draft metric can be staged without being shown.
func (s InitiativeService) SetMetric(
	ctx context.Context,
	token string,
	initiativeID pgtype.UUID,
	in MetricInput,
) error {
	_, err := staffWrite(ctx, s.Staff, token, initiativePermission,
		func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (struct{}, error) {
			if in.Position < 0 || in.Position >= maxHeadlineMetrics {
				return struct{}{}, ErrStaffInput
			}
			if identity.TrimSpace(in.Label) == "" || utf8.RuneCountInString(in.Label) > 150 ||
				!plainText(in.Label, false) {
				return struct{}{}, ErrStaffInput
			}
			if !slices.Contains(metricUnits, in.Unit) {
				return struct{}{}, ErrStaffInput
			}
			if utf8.RuneCountInString(in.VerificationNote) > 2000 ||
				(in.VerificationNote != "" && !plainText(in.VerificationNote, true)) {
				return struct{}{}, ErrStaffInput
			}
			var verified pgtype.Date
			if in.VerifiedOn != "" {
				day, err := time.Parse(time.DateOnly, in.VerifiedOn)
				if err != nil {
					return struct{}{}, ErrStaffInput
				}
				verified = pgtype.Date{Time: day, Valid: true}
			}
			q := queries.New(tx)
			if _, err := q.InitiativeByID(ctx, initiativeID); err != nil {
				return struct{}{}, err
			}
			id, err := NewID()
			if err != nil {
				return struct{}{}, err
			}
			if err = q.UpsertInitiativeMetric(ctx, queries.UpsertInitiativeMetricParams{
				ID: id, InitiativeID: initiativeID, Position: in.Position, Label: in.Label,
				Value: in.Value, Unit: in.Unit, VerificationNote: in.VerificationNote,
				VerifiedOn: verified, CreatedAt: timestamp(now),
			}); err != nil {
				return struct{}{}, err
			}
			return struct{}{}, auditStaff(
				ctx,
				q,
				actor.ID,
				"initiative.metric_set",
				initiativeID.String(),
				map[string]int32{"position": in.Position},
				now,
			)
		})
	return err
}

// PublishMetric makes a verified metric donor-visible. The DB constraint also
// refuses an unverified metric; this re-check maps the refusal to ErrStaffInput
// so the caller gets a clean 422 rather than a 500.
func (s InitiativeService) PublishMetric(
	ctx context.Context,
	token string,
	initiativeID pgtype.UUID,
	position int32,
) error {
	_, err := staffWrite(ctx, s.Staff, token, initiativePermission,
		func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (struct{}, error) {
			q := queries.New(tx)
			_, err := q.InitiativeByID(ctx, initiativeID)
			if err != nil {
				return struct{}{}, err
			}
			metrics, err := q.InitiativeMetrics(ctx, initiativeID)
			if err != nil {
				return struct{}{}, err
			}
			idx := slices.IndexFunc(
				metrics,
				func(m queries.DonorInitiativeMetric) bool { return m.Position == position },
			)
			if idx < 0 {
				return struct{}{}, ErrStaffInput
			}
			if m := metrics[idx]; !m.VerifiedOn.Valid || m.VerificationNote == "" {
				return struct{}{}, ErrStaffInput
			}
			if err = q.PublishInitiativeMetric(ctx, queries.PublishInitiativeMetricParams{InitiativeID: initiativeID, Position: position, UpdatedAt: timestamp(now)}); err != nil {
				return struct{}{}, err
			}
			// METRICS_VERIFIED partner email (§6.11): a verified figure for a
			// partner's initiative went live. Keyed per (initiative, position) so
			// republishing the same metric does not re-notify.
			key := "partner-metrics:" + initiativeID.String() + ":" + strconv.FormatInt(int64(position), 10)
			if err = queueInitiativePartners(ctx, q, initiativeID, "METRICS_VERIFIED", key, map[string]string{"initiative_id": initiativeID.String()}, now); err != nil {
				return struct{}{}, err
			}
			return struct{}{}, auditStaff(
				ctx,
				q,
				actor.ID,
				"initiative.metric_published",
				initiativeID.String(),
				map[string]int32{"position": position},
				now,
			)
		})
	return err
}

// DeleteMetric removes a headline metric by position.
func (s InitiativeService) DeleteMetric(
	ctx context.Context,
	token string,
	initiativeID pgtype.UUID,
	position int32,
) error {
	_, err := staffWrite(ctx, s.Staff, token, initiativePermission,
		func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (struct{}, error) {
			q := queries.New(tx)
			if err := q.DeleteInitiativeMetric(ctx, queries.DeleteInitiativeMetricParams{InitiativeID: initiativeID, Position: position}); err != nil {
				return struct{}{}, err
			}
			return struct{}{}, auditStaff(
				ctx,
				q,
				actor.ID,
				"initiative.metric_deleted",
				initiativeID.String(),
				map[string]int32{"position": position},
				now,
			)
		})
	return err
}

// SpendInput sets the running total for one spend category of an initiative.
// Spend is category totals only — never line items (PRD §5.2).
type SpendInput struct {
	Category         string `json:"category"`
	AmountMinor      int64  `json:"amount_minor"`
	VerificationNote string `json:"verification_note"`
	VerifiedOn       string `json:"verified_on"`
	PublishState     string `json:"publish_state"`
}

// SetSpend upserts the total for a spend category.
func (s InitiativeService) SetSpend(ctx context.Context, token string, initiativeID pgtype.UUID, in SpendInput) error {
	_, err := staffWrite(ctx, s.Staff, token, initiativePermission,
		func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (struct{}, error) {
			if identity.TrimSpace(in.Category) == "" || utf8.RuneCountInString(in.Category) > 100 ||
				!plainText(in.Category, false) {
				return struct{}{}, ErrStaffInput
			}
			if in.AmountMinor < 0 {
				return struct{}{}, ErrStaffInput
			}
			q := queries.New(tx)
			if _, err := q.InitiativeByID(ctx, initiativeID); err != nil {
				return struct{}{}, err
			}
			var verified pgtype.Date
			if len(in.VerificationNote) > 2000 || !plainText(in.VerificationNote, true) {
				return struct{}{}, ErrStaffInput
			}
			if in.VerifiedOn != "" {
				day, err := time.Parse(time.DateOnly, in.VerifiedOn)
				if err != nil {
					return struct{}{}, ErrStaffInput
				}
				verified = pgtype.Date{Time: day, Valid: true}
			}
			if err := q.SaveSpendDraft(ctx, queries.SaveSpendDraftParams{
				InitiativeID: initiativeID, Category: in.Category, AmountMinor: in.AmountMinor, VerificationNote: in.VerificationNote, VerifiedOn: verified, UpdatedAt: timestamp(now),
			}); err != nil {
				return struct{}{}, err
			}
			return struct{}{}, auditStaff(
				ctx,
				q,
				actor.ID,
				"initiative.spend_set",
				initiativeID.String(),
				map[string]string{"category": in.Category},
				now,
			)
		})
	return err
}

// DeleteSpend removes a spend category from an initiative.
func (s InitiativeService) DeleteSpend(
	ctx context.Context,
	token string,
	initiativeID pgtype.UUID,
	category string,
) error {
	_, err := staffWrite(ctx, s.Staff, token, initiativePermission,
		func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (struct{}, error) {
			q := queries.New(tx)
			if err := q.DeleteSpendDraft(ctx, queries.DeleteSpendDraftParams{InitiativeID: initiativeID, Category: category}); err != nil {
				return struct{}{}, err
			}
			if err := q.DeleteInitiativeSpend(ctx, queries.DeleteInitiativeSpendParams{InitiativeID: initiativeID, Category: category}); err != nil {
				return struct{}{}, err
			}
			return struct{}{}, auditStaff(
				ctx,
				q,
				actor.ID,
				"initiative.spend_deleted",
				initiativeID.String(),
				map[string]string{"category": category},
				now,
			)
		})
	return err
}

// requireApprovedPartner refuses a partner that is still a contact-form lead:
// donor_partner_signups holds both, and only an approved row is a partner org.
func requireApprovedPartner(ctx context.Context, q *queries.Queries, partner pgtype.UUID) error {
	if !partner.Valid {
		return nil
	}
	org, err := q.PartnerOrgByID(ctx, partner)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !org.ApprovedAt.Valid) {
		return ErrStaffInput
	}
	return err
}

// requireApprovalSource refuses a board approval citing no stored source.
func requireApprovalSource(ctx context.Context, q *queries.Queries, source pgtype.UUID) error {
	if !source.Valid {
		return nil
	}
	found, err := q.ContentSourceIDs(ctx, []pgtype.UUID{source})
	if err != nil {
		return err
	}
	if len(found) != 1 {
		return ErrStaffInput
	}
	return nil
}

func queueInitiativePartners(
	ctx context.Context,
	q *queries.Queries,
	id pgtype.UUID,
	kind, key string,
	data map[string]string,
	now time.Time,
) error {
	partners, err := q.InitiativePartners(ctx, id)
	if err != nil {
		return err
	}
	for _, p := range partners {
		if err = queuePartnerMessage(ctx, q, p.PartnerID, kind, key+":"+p.PartnerID.String(), data, now); err != nil {
			return err
		}
	}
	return nil
}

func (s InitiativeService) PublishSpend(ctx context.Context, token string, id pgtype.UUID, category string) error {
	_, err := staffWrite(
		ctx,
		s.Staff,
		token,
		initiativePermission,
		func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (struct{}, error) {
			q := queries.New(tx)
			if _, err := q.LockInitiative(ctx, id); err != nil {
				return struct{}{}, err
			}
			rowID, err := NewID()
			if err != nil {
				return struct{}{}, err
			}
			n, err := q.PublishSpendDraft(
				ctx,
				queries.PublishSpendDraftParams{ID: rowID, InitiativeID: id, Category: category, AsOf: timestamp(now)},
			)
			if err != nil {
				return struct{}{}, err
			}
			if n == 0 {
				return struct{}{}, ErrStaffInput
			}
			if err = q.DeleteSpendDraft(ctx, queries.DeleteSpendDraftParams{InitiativeID: id, Category: category}); err != nil {
				return struct{}{}, err
			}
			return struct{}{}, auditStaff(
				ctx,
				q,
				actor.ID,
				"initiative.spend_published",
				id.String(),
				map[string]string{"category": category},
				now,
			)
		},
	)
	return err
}

// metricUnits is the unit vocabulary of a headline metric.
var metricUnits = []string{"USD_CENTS", "PEOPLE", "INITIATIVES", "PARTNERS"}
