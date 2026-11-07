package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"dad/donor-api/internal/identity"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type DonorSummary struct {
	Claimed    bool        `json:"claimed"`
	Disabled   bool        `json:"disabled"`
	Email      string      `json:"email"`
	ID         pgtype.UUID `json:"id"`
	PublicName pgtype.Text `json:"public_name"`
}

type DonorSearch struct {
	Donors     []DonorSummary `json:"donors"`
	NextOffset *int64         `json:"next_offset"`
}

type StaffStreak struct {
	AtRisk     bool               `json:"at_risk"`
	Best       int32              `json:"best"`
	ComputedAt pgtype.Timestamptz `json:"computed_at"`
	Current    pgtype.Int4        `json:"current"`
}

type ReferrerLink struct {
	Email string      `json:"email"`
	ID    pgtype.UUID `json:"id"`
	Type  pgtype.Text `json:"type"`
}

type ReferredDonor struct {
	Email    string      `json:"email"`
	ID       pgtype.UUID `json:"id"`
	NetMinor pgtype.Int8 `json:"net_minor"`
	Type     pgtype.Text `json:"type"`
}

type DonorDetail struct {
	AsOf                time.Time                   `json:"as_of"`
	Claimed             bool                        `json:"claimed"`
	Email               string                      `json:"email"`
	FoundingDonor       bool                        `json:"founding_donor"`
	Gifts               []queries.DonorGiftsPageRow `json:"gifts"`
	ID                  pgtype.UUID                 `json:"id"`
	NextGiftsOffset     *int64                      `json:"next_gifts_offset"`
	NextReferralsOffset *int64                      `json:"next_referrals_offset"`
	PublicName          pgtype.Text                 `json:"public_name"`
	Referred            []ReferredDonor             `json:"referred"`
	Referrer            *ReferrerLink               `json:"referrer"`
	Streak              *StaffStreak                `json:"streak"`
	Subscription        *StaffSubscription          `json:"subscription"`
}

// StaffSubscription is the donor's monthly gift as last synced from the
// processor.
type StaffSubscription struct {
	AmountMinor int64      `json:"amount_minor"`
	NextRenewal time.Time  `json:"next_renewal"`
	ObservedAt  time.Time  `json:"observed_at"`
	Status      string     `json:"status"`
	StopsAt     *time.Time `json:"stops_at"`
}

type AuditEvent struct {
	Action  string             `json:"action"`
	ActorID pgtype.UUID        `json:"actor_id"`
	At      pgtype.Timestamptz `json:"at"`
	Details json.RawMessage    `json:"details"`
	ID      pgtype.UUID        `json:"id"`
	Target  string             `json:"target"`
}

type AuditPage struct {
	Events     []AuditEvent `json:"events"`
	NextOffset *int64       `json:"next_offset"`
}

type MessagePage struct {
	Messages   []queries.StaffMessagePageRow `json:"messages"`
	NextOffset *int64                        `json:"next_offset"`
}

type ActionPage struct {
	Actions    []queries.StaffActionPageRow `json:"actions"`
	NextOffset *int64                       `json:"next_offset"`
}

type PendingAction struct {
	Action *queries.StaffPendingActionRow `json:"action"`
}

func (s *StaffService) SearchDonors(ctx context.Context, token, term string, offset int64) (DonorSearch, error) {
	if !pageOffsetValid(offset) || !plainText(term, false) || utf8.RuneCountInString(term) > 320 {
		return DonorSearch{}, ErrStaffInput
	}
	return staffRead(ctx, s, token, "donor.read", func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (DonorSearch, error) {
		q := queries.New(tx)
		pattern := strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(identity.TrimSpace(term))
		rows, err := q.StaffDonorSearch(ctx, queries.StaffDonorSearchParams{Term: "%" + pattern + "%", PageOffset: offset})
		if err != nil {
			return DonorSearch{}, err
		}
		rows, next := page(rows, offset)
		out := DonorSearch{Donors: make([]DonorSummary, 0, len(rows)), NextOffset: next}
		for _, row := range rows {
			out.Donors = append(
				out.Donors,
				DonorSummary{
					ID:         row.ID,
					Email:      row.Email,
					PublicName: row.PublicName,
					Claimed:    row.ClaimedAt.Valid,
					Disabled:   row.Disabled,
				},
			)
		}
		if err = auditStaff(ctx, q, actor.ID, "donor.search", "donors", map[string]any{"offset": offset, "result_count": len(rows)}, now); err != nil {
			return DonorSearch{}, err
		}
		return out, nil
	})
}

func (s *StaffService) DonorDetail(
	ctx context.Context,
	token string,
	id pgtype.UUID,
	giftsOffset, referralsOffset int64,
) (DonorDetail, error) {
	if !pageOffsetValid(giftsOffset) || !pageOffsetValid(referralsOffset) {
		return DonorDetail{}, ErrStaffInput
	}
	return staffRead(ctx, s, token, "donor.read", func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (DonorDetail, error) {
		q := queries.New(tx)
		donor, err := q.StaffDonorByID(ctx, id)
		if err != nil {
			return DonorDetail{}, err
		}
		out := DonorDetail{ID: donor.ID, Email: donor.Email, PublicName: donor.PublicName, AsOf: now, Claimed: donor.ClaimedAt.Valid}
		projection, err := q.Projection(ctx, id)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return DonorDetail{}, err
		}
		if err == nil {
			out.FoundingDonor = projection.FoundingAwardedAt.Valid
			// A seeded projection has no facts yet; show it as updating rather than zero.
			if projection.NetMinor.Valid {
				out.Streak = &StaffStreak{
					Current:    projection.CurrentStreak,
					Best:       projection.BestStreak,
					AtRisk:     projection.AtRisk,
					ComputedAt: projection.ComputedAt,
				}
			}
			if projection.ReferrerID.Valid {
				row, err := q.StaffDonorByID(ctx, projection.ReferrerID)
				if err != nil {
					return DonorDetail{}, err
				}
				out.Referrer = &ReferrerLink{ID: row.ID, Email: row.Email, Type: projection.ReferralKind}
			}
		}
		if out.Subscription, err = storedSubscription(ctx, q, id, now); err != nil {
			return DonorDetail{}, err
		}
		gifts, err := q.DonorGiftsPage(ctx, queries.DonorGiftsPageParams{DonorID: id, PageOffset: giftsOffset})
		if err != nil {
			return DonorDetail{}, err
		}
		out.Gifts, out.NextGiftsOffset = page(gifts, giftsOffset)
		referrals, err := q.StaffReferredPage(ctx, queries.StaffReferredPageParams{ReferrerID: id, PageOffset: referralsOffset})
		if err != nil {
			return DonorDetail{}, err
		}
		referrals, out.NextReferralsOffset = page(referrals, referralsOffset)
		out.Referred = make([]ReferredDonor, 0, len(referrals))
		for _, row := range referrals {
			out.Referred = append(out.Referred, ReferredDonor{ID: row.ID, Email: row.Email, Type: row.ReferralKind, NetMinor: row.NetMinor})
		}
		if err = auditStaff(ctx, q, actor.ID, "donor.view", id.String(), nil, now); err != nil {
			return DonorDetail{}, err
		}
		return out, nil
	})
}

// storedSubscription returns the donor's last synced monthly gift, if any.
func storedSubscription(ctx context.Context, q *queries.Queries, donor pgtype.UUID, now time.Time) (*StaffSubscription, error) {
	details, err := storedDetails(ctx, q, donor)
	if err != nil || details.Subscription == nil {
		return nil, err
	}
	sub := details.Subscription
	return &StaffSubscription{
		AmountMinor: sub.AmountMinor,
		NextRenewal: sub.PeriodEnd,
		ObservedAt:  *details.ObservedAt,
		Status:      sub.Status,
		StopsAt:     sub.StopsAt,
	}, nil
}

func (s *StaffService) AuditEvents(ctx context.Context, token string, offset int64) (AuditPage, error) {
	if !pageOffsetValid(offset) {
		return AuditPage{}, ErrStaffInput
	}
	return staffRead(ctx, s, token, "audit.read", func(tx pgx.Tx, _ queries.DonorStaff, _ time.Time) (AuditPage, error) {
		rows, err := queries.New(tx).StaffAuditPage(ctx, offset)
		if err != nil {
			return AuditPage{}, err
		}
		rows, next := page(rows, offset)
		out := AuditPage{Events: make([]AuditEvent, 0, len(rows)), NextOffset: next}
		for _, row := range rows {
			out.Events = append(
				out.Events,
				AuditEvent{
					ID:      row.ID,
					ActorID: row.ActorID,
					Action:  row.Action,
					Target:  row.Target,
					At:      row.CreatedAt,
					Details: json.RawMessage(row.Details),
				},
			)
		}
		return out, nil
	})
}

func (s *StaffService) DonorMessages(ctx context.Context, token string, id pgtype.UUID, offset int64) (MessagePage, error) {
	if !pageOffsetValid(offset) {
		return MessagePage{}, ErrStaffInput
	}
	return staffRead(ctx, s, token, "donor.read", func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (MessagePage, error) {
		q := queries.New(tx)
		if _, err := q.StaffDonorByID(ctx, id); err != nil {
			return MessagePage{}, err
		}
		rows, err := q.StaffMessagePage(ctx, queries.StaffMessagePageParams{DonorID: id, PageOffset: offset})
		if err != nil {
			return MessagePage{}, err
		}
		out := MessagePage{}
		out.Messages, out.NextOffset = page(rows, offset)
		if err = auditStaff(ctx, q, actor.ID, "message.list", id.String(), nil, now); err != nil {
			return MessagePage{}, err
		}
		return out, nil
	})
}

func (s *StaffService) DonorActions(ctx context.Context, token string, id pgtype.UUID, offset int64) (ActionPage, error) {
	if !pageOffsetValid(offset) {
		return ActionPage{}, ErrStaffInput
	}
	return staffRead(ctx, s, token, "donor.read", func(tx pgx.Tx, _ queries.DonorStaff, _ time.Time) (ActionPage, error) {
		rows, err := queries.New(tx).StaffActionPage(ctx, queries.StaffActionPageParams{DonorID: id, PageOffset: offset})
		if err != nil {
			return ActionPage{}, err
		}
		out := ActionPage{}
		out.Actions, out.NextOffset = page(rows, offset)
		return out, nil
	})
}

func (s *StaffService) PendingAction(ctx context.Context, token string, donorID, targetID pgtype.UUID, kind string) (PendingAction, error) {
	permission := "gift.refund"
	if kind == "CANCEL" {
		permission = "gift.cancel"
	} else if kind != "REFUND" {
		return PendingAction{}, ErrStaffInput
	}
	return staffRead(ctx, s, token, permission, func(tx pgx.Tx, actor queries.DonorStaff, _ time.Time) (PendingAction, error) {
		row, err := queries.New(tx).
			StaffPendingAction(ctx, queries.StaffPendingActionParams{DonorID: donorID, ActorID: actor.ID, TargetID: targetID, Kind: kind})
		if errors.Is(err, pgx.ErrNoRows) {
			return PendingAction{}, nil
		}
		if err != nil {
			return PendingAction{}, err
		}
		return PendingAction{Action: &row}, nil
	})
}
