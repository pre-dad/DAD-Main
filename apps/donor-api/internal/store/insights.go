package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"dad/donor-api/internal/store/queries"
	"dad/donor-api/internal/uicatalog"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// Insight spec states. A spec moves draft to pending to live to retired, or
// from pending to rejected; at most one is live.
const (
	InsightDraft    = "draft"
	InsightPending  = "pending"
	InsightLive     = "live"
	InsightRetired  = "retired"
	InsightRejected = "rejected"
)

const (
	maxInsightSpecBytes = 64 << 10
	maxRejectReason     = 500
)

var (
	// ErrInsightState is a transition the spec's current state does not allow.
	ErrInsightState = errors.New("insight spec is not in a state that allows this")
	// ErrInsightTwoPerson refuses an approval or rejection by the spec's author.
	ErrInsightTwoPerson = fmt.Errorf("%w: a different staff member must decide this spec", ErrStaffForbidden)
	// ErrInsightNotAuthor refuses a draft change by anyone but its author.
	ErrInsightNotAuthor = fmt.Errorf("%w: only the author can change or submit a draft", ErrStaffForbidden)
)

// InsightInvalidError carries every reason the UI catalog refused a spec. It
// is an ErrStaffInput.
type InsightInvalidError struct {
	Errors []uicatalog.Error
}

func (e *InsightInvalidError) Error() string        { return "invalid insight spec" }
func (e *InsightInvalidError) Is(target error) bool { return target == ErrStaffInput }

func checkInsightSpec(raw []byte) error {
	if len(raw) == 0 || len(raw) > maxInsightSpecBytes {
		return &InsightInvalidError{Errors: []uicatalog.Error{{Code: "SPEC_SIZE", Message: "A spec is 1 byte to 64 KiB of JSON"}}}
	}
	result, err := uicatalog.Validate(raw)
	if errors.Is(err, uicatalog.ErrNotJSON) {
		return &InsightInvalidError{
			Errors: []uicatalog.Error{{Code: "SPEC_NOT_JSON", Message: "A spec must be one well-formed JSON array"}},
		}
	}
	if err != nil {
		return err
	}
	if !result.OK {
		return &InsightInvalidError{Errors: result.Errors}
	}
	return nil
}

// InsightSpecSummary is a spec without its content, for lists.
type InsightSpecSummary struct {
	ID             pgtype.UUID `json:"id"`
	State          string      `json:"state"`
	CatalogVersion int32       `json:"catalog_version"`
	AuthorID       pgtype.UUID `json:"author_id"`
	AuthorName     string      `json:"author_name"`
	ApproverID     pgtype.UUID `json:"approver_id"`
	ApproverName   *string     `json:"approver_name"`
	RejectReason   *string     `json:"reject_reason"`
	CreatedAt      time.Time   `json:"created_at"`
	UpdatedAt      time.Time   `json:"updated_at"`
	SubmittedAt    *time.Time  `json:"submitted_at"`
	DecidedAt      *time.Time  `json:"decided_at"`
	RetiredAt      *time.Time  `json:"retired_at"`
}

// InsightSpec is one spec with its content.
type InsightSpec struct {
	InsightSpecSummary
	Spec json.RawMessage `json:"spec"`
}

type InsightSpecPage struct {
	Specs      []InsightSpecSummary `json:"specs"`
	NextCursor *string              `json:"next_cursor"`
}

type InsightService struct{ Staff *StaffService }

func (s InsightService) List(ctx context.Context, token, state, after string) (InsightSpecPage, error) {
	switch state {
	case "", InsightDraft, InsightPending, InsightLive, InsightRetired, InsightRejected:
	default:
		return InsightSpecPage{}, ErrStaffInput
	}
	at, id, err := keysetBounds(after, true)
	if err != nil {
		return InsightSpecPage{}, err
	}
	return staffRead(ctx, s.Staff, token, "content.publish", func(tx pgx.Tx, _ queries.DonorStaff, _ time.Time) (InsightSpecPage, error) {
		rows, err := queries.New(tx).InsightSpecsPage(ctx, queries.InsightSpecsPageParams{State: state, CursorAt: at, CursorID: id})
		if err != nil {
			return InsightSpecPage{}, err
		}
		rows, next := keysetPage(rows, func(row queries.InsightSpecsPageRow) *string { return encodeCursor(row.CreatedAt.Time, row.ID) })
		page := InsightSpecPage{Specs: make([]InsightSpecSummary, 0, len(rows)), NextCursor: next}
		for _, row := range rows {
			page.Specs = append(page.Specs, InsightSpecSummary{
				ID:             row.ID,
				State:          row.State,
				CatalogVersion: row.CatalogVersion,
				AuthorID:       row.AuthorID,
				AuthorName:     row.AuthorName,
				ApproverID:     row.ApproverID,
				ApproverName:   textPointer(row.ApproverName),
				RejectReason:   textPointer(row.RejectReason),
				CreatedAt:      row.CreatedAt.Time,
				UpdatedAt:      row.UpdatedAt.Time,
				SubmittedAt:    timePointer(row.SubmittedAt),
				DecidedAt:      timePointer(row.DecidedAt),
				RetiredAt:      timePointer(row.RetiredAt),
			})
		}
		return page, nil
	})
}

func (s InsightService) Get(ctx context.Context, token string, id pgtype.UUID) (InsightSpec, error) {
	return staffRead(ctx, s.Staff, token, "content.publish", func(tx pgx.Tx, _ queries.DonorStaff, _ time.Time) (InsightSpec, error) {
		return readInsight(ctx, queries.New(tx), id)
	})
}

func readInsight(ctx context.Context, q *queries.Queries, id pgtype.UUID) (InsightSpec, error) {
	row, err := q.InsightSpecByID(ctx, id)
	if err != nil {
		return InsightSpec{}, err
	}
	return InsightSpec{
		InsightSpecSummary: InsightSpecSummary{
			ID:             row.ID,
			State:          row.State,
			CatalogVersion: row.CatalogVersion,
			AuthorID:       row.AuthorID,
			AuthorName:     row.AuthorName,
			ApproverID:     row.ApproverID,
			ApproverName:   textPointer(row.ApproverName),
			RejectReason:   textPointer(row.RejectReason),
			CreatedAt:      row.CreatedAt.Time,
			UpdatedAt:      row.UpdatedAt.Time,
			SubmittedAt:    timePointer(row.SubmittedAt),
			DecidedAt:      timePointer(row.DecidedAt),
			RetiredAt:      timePointer(row.RetiredAt),
		},
		Spec: json.RawMessage(row.Spec),
	}, nil
}

func specDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Create saves a new draft authored by the caller.
func (s InsightService) Create(ctx context.Context, token string, spec []byte) (InsightSpec, error) {
	return staffWrite(
		ctx,
		s.Staff,
		token,
		"content.publish",
		func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (InsightSpec, error) {
			if err := checkInsightSpec(spec); err != nil {
				return InsightSpec{}, err
			}
			id, err := NewID()
			if err != nil {
				return InsightSpec{}, err
			}
			q := queries.New(tx)
			err = q.InsertInsightSpec(ctx, queries.InsertInsightSpecParams{
				ID: id, Spec: spec, CatalogVersion: int32(uicatalog.Version()), AuthorID: actor.ID, At: timestamp(now),
			})
			if err != nil {
				return InsightSpec{}, err
			}
			details := map[string]any{"sha256": specDigest(spec), "catalog_version": uicatalog.Version()}
			if err = auditStaff(ctx, q, actor.ID, "content.insight_created", id.String(), details, now); err != nil {
				return InsightSpec{}, err
			}
			return readInsight(ctx, q, id)
		},
	)
}

// transition locks a spec, requires it to be in one of from, and runs change.
func (s InsightService) transition(
	ctx context.Context,
	token string,
	id pgtype.UUID,
	from string,
	change func(q *queries.Queries, actor queries.DonorStaff, spec queries.InsightSpecForUpdateRow, now time.Time) error,
) (InsightSpec, error) {
	out, err := staffWrite(
		ctx,
		s.Staff,
		token,
		"content.publish",
		func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (InsightSpec, error) {
			q := queries.New(tx)
			row, err := q.InsightSpecForUpdate(ctx, id)
			if err != nil {
				return InsightSpec{}, err
			}
			if row.State != from {
				return InsightSpec{}, ErrInsightState
			}
			if err = change(q, actor, row, now); err != nil {
				return InsightSpec{}, err
			}
			return readInsight(ctx, q, id)
		},
	)
	if pg, ok := errors.AsType[*pgconn.PgError](err); ok && pg.Code == uniqueViolation {
		return InsightSpec{}, ErrInsightState
	}
	return out, err
}

// Update replaces a draft's content. Only its author may.
func (s InsightService) Update(ctx context.Context, token string, id pgtype.UUID, spec []byte) (InsightSpec, error) {
	return s.transition(
		ctx,
		token,
		id,
		InsightDraft,
		func(q *queries.Queries, actor queries.DonorStaff, row queries.InsightSpecForUpdateRow, now time.Time) error {
			if row.AuthorID != actor.ID {
				return ErrInsightNotAuthor
			}
			if err := checkInsightSpec(spec); err != nil {
				return err
			}
			err := q.UpdateInsightDraft(
				ctx,
				queries.UpdateInsightDraftParams{ID: id, Spec: spec, CatalogVersion: int32(uicatalog.Version()), UpdatedAt: timestamp(now)},
			)
			if err != nil {
				return err
			}
			details := map[string]any{"sha256": specDigest(spec), "catalog_version": uicatalog.Version()}
			return auditStaff(ctx, q, actor.ID, "content.insight_updated", id.String(), details, now)
		},
	)
}

// Submit asks for approval of a draft. Only its author may; the content is
// validated again so that nothing invalid reaches an approver.
func (s InsightService) Submit(ctx context.Context, token string, id pgtype.UUID) (InsightSpec, error) {
	return s.transition(
		ctx,
		token,
		id,
		InsightDraft,
		func(q *queries.Queries, actor queries.DonorStaff, row queries.InsightSpecForUpdateRow, now time.Time) error {
			if row.AuthorID != actor.ID {
				return ErrInsightNotAuthor
			}
			stored, err := q.InsightSpecByID(ctx, id)
			if err != nil {
				return err
			}
			if err = checkInsightSpec(stored.Spec); err != nil {
				return err
			}
			if err = q.SubmitInsightSpec(ctx, queries.SubmitInsightSpecParams{ID: id, SubmittedAt: timestamp(now)}); err != nil {
				return err
			}
			return auditStaff(ctx, q, actor.ID, "content.insight_submitted", id.String(), nil, now)
		},
	)
}

// Approve makes a pending spec live and retires the one it replaces. The
// approver must not be the author.
func (s InsightService) Approve(ctx context.Context, token string, id pgtype.UUID) (InsightSpec, error) {
	return s.transition(
		ctx,
		token,
		id,
		InsightPending,
		func(q *queries.Queries, actor queries.DonorStaff, row queries.InsightSpecForUpdateRow, now time.Time) error {
			if row.AuthorID == actor.ID {
				return ErrInsightTwoPerson
			}
			stored, err := q.InsightSpecByID(ctx, id)
			if err != nil {
				return err
			}
			if err = checkInsightSpec(stored.Spec); err != nil {
				return err
			}
			previous, err := q.RetireLiveInsightSpec(ctx, queries.RetireLiveInsightSpecParams{ActorID: actor.ID, At: timestamp(now)})
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if err = q.ApproveInsightSpec(ctx, queries.ApproveInsightSpecParams{ID: id, ActorID: actor.ID, At: timestamp(now)}); err != nil {
				return err
			}
			details := map[string]any{"replaced": nil}
			if previous.Valid {
				details["replaced"] = previous.String()
				if err = auditStaff(ctx, q, actor.ID, "content.insight_retired", previous.String(), map[string]any{"replaced_by": id.String()}, now); err != nil {
					return err
				}
			}
			return auditStaff(ctx, q, actor.ID, "content.insight_approved", id.String(), details, now)
		},
	)
}

// Reject sends a pending spec back with a reason. The rejecter must not be the
// author.
func (s InsightService) Reject(ctx context.Context, token string, id pgtype.UUID, reason string) (InsightSpec, error) {
	reason = strings.TrimSpace(reason)
	return s.transition(
		ctx,
		token,
		id,
		InsightPending,
		func(q *queries.Queries, actor queries.DonorStaff, row queries.InsightSpecForUpdateRow, now time.Time) error {
			if row.AuthorID == actor.ID {
				return ErrInsightTwoPerson
			}
			if n := utf8.RuneCountInString(reason); n < 1 || n > maxRejectReason || !plainText(reason, true) {
				return ErrStaffInput
			}
			err := q.RejectInsightSpec(
				ctx,
				queries.RejectInsightSpecParams{ID: id, ActorID: actor.ID, At: timestamp(now), Reason: text(reason)},
			)
			if err != nil {
				return err
			}
			return auditStaff(ctx, q, actor.ID, "content.insight_rejected", id.String(), map[string]any{"reason": reason}, now)
		},
	)
}

// Retire takes the live spec down. Anyone who may publish content can, so a
// bad layout is never held up waiting for a second person.
func (s InsightService) Retire(ctx context.Context, token string, id pgtype.UUID) (InsightSpec, error) {
	return s.transition(
		ctx,
		token,
		id,
		InsightLive,
		func(q *queries.Queries, actor queries.DonorStaff, _ queries.InsightSpecForUpdateRow, now time.Time) error {
			if err := q.RetireInsightSpec(ctx, queries.RetireInsightSpecParams{ID: id, ActorID: actor.ID, At: timestamp(now)}); err != nil {
				return err
			}
			return auditStaff(ctx, q, actor.ID, "content.insight_retired", id.String(), nil, now)
		},
	)
}

// InsightsPanel is the live spec as the dashboard carries it.
type InsightsPanel struct {
	CatalogVersion int32           `json:"catalog_version"`
	SpecID         pgtype.UUID     `json:"spec_id"`
	Spec           json.RawMessage `json:"spec"`
}

// liveInsights returns the live spec, or nil when none is live, or when it
// does not pass today's validation, in which case donors see no panel.
func liveInsights(ctx context.Context, q *queries.Queries) (*InsightsPanel, error) {
	row, err := q.LiveInsightSpec(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if int(row.CatalogVersion) > uicatalog.Version() {
		slog.WarnContext(ctx, "live insight spec targets a newer catalog", "spec", row.ID.String(), "catalog_version", row.CatalogVersion)
		return nil, nil
	}
	if err = checkInsightSpec(row.Spec); err != nil {
		var invalid *InsightInvalidError
		if !errors.As(err, &invalid) {
			return nil, err
		}
		slog.ErrorContext(
			ctx,
			"live insight spec failed validation",
			"spec",
			row.ID.String(),
			"problems",
			len(invalid.Errors),
			"first",
			invalid.Errors[0].Code,
		)
		return nil, nil
	}
	return &InsightsPanel{CatalogVersion: row.CatalogVersion, SpecID: row.ID, Spec: json.RawMessage(row.Spec)}, nil
}
