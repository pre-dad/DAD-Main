package store

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"dad/donor-api/internal/identity"
	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// maxRefundMinor bounds a staff refund. It is deliberately wider than the
// donor gift ceiling (maxGiftMinor): a refund may settle a historical gift
// recorded before the ceiling applied.
const maxRefundMinor = 99_999_999

type StaffFinanceService struct {
	Staff   *StaffService
	Gateway payments.StaffFinanceGateway
	Origin  string
}

type StaffRefund struct {
	AmountMinor int64        `json:"amount_minor,omitempty"`
	Currency    string       `json:"currency,omitempty"`
	Mode        string       `json:"mode"`
	RefundID    *pgtype.UUID `json:"refund_id,omitempty"`
	RequestID   pgtype.UUID  `json:"request_id"`
	Status      string       `json:"status"`
}

type StaffCancellation struct {
	Mode         string        `json:"mode"`
	RequestID    pgtype.UUID   `json:"request_id"`
	State        string        `json:"state"`
	Subscription *Subscription `json:"subscription"`
}

type StaffFinancialRequest struct {
	RequestID, DonorID, TargetID pgtype.UUID
	Reason                       string
	Amount                       int64
	PeriodEnd                    time.Time
}

// recurring is the donor recurring service, through which staff cancel and
// preview monthly gifts.
func (s StaffFinanceService) recurring() RecurringService {
	return RecurringService{DB: s.Staff.DB, Gateway: s.Gateway, Origin: s.Origin}
}

func staffGift(ctx context.Context, q *queries.Queries, donor, id pgtype.UUID) (queries.DonorGift, error) {
	gift, err := q.StaffGift(ctx, queries.StaffGiftParams{DonorID: donor, ID: id})
	if err == nil && !gift.StripePaymentIntentID.Valid {
		err = payments.ErrFacts
	}
	return gift, err
}

// refundSnapshot is the processor's refund list fetched without holding the
// donor lock, with the count of local refund observations it was read after.
type refundSnapshot struct {
	gift         queries.DonorGift
	observations int64
	rows         []payments.Object
}

// currentRefunds returns the processor's refunds for the locked gift. The
// snapshot is used only if no refund state was recorded after it was read.
func (s StaffFinanceService) currentRefunds(
	ctx context.Context,
	q *queries.Queries,
	gift queries.DonorGift,
	snapshot refundSnapshot,
) ([]payments.Object, int64, error) {
	observations, err := q.GiftRefundObservations(ctx, gift.ID)
	if err != nil {
		return nil, 0, err
	}
	if observations != snapshot.observations || gift.ID != snapshot.gift.ID ||
		gift.StripePaymentIntentID != snapshot.gift.StripePaymentIntentID {
		return nil, 0, errRefetch
	}
	return refundBalance(gift, snapshot.rows)
}

func (s StaffFinanceService) refunds(ctx context.Context, gift queries.DonorGift) ([]payments.Object, int64, error) {
	if s.Gateway == nil {
		return nil, 0, payments.ErrUnconfigured
	}
	rows, err := s.Gateway.ListPaymentRefunds(ctx, gift.StripePaymentIntentID.String)
	if err != nil {
		return nil, 0, err
	}
	return refundBalance(gift, rows)
}

// refundBalance validates the processor's refunds of gift and returns the
// amount still refundable.
func refundBalance(gift queries.DonorGift, rows []payments.Object) ([]payments.Object, int64, error) {
	ids := make(map[string]bool, len(rows))
	remaining := gift.AmountMinor
	for _, row := range rows {
		id := row.Text("id")
		amount, err := row.Int("amount")
		if err != nil || !strings.HasPrefix(id, "re_") || ids[id] || row.Text("payment_intent") != gift.StripePaymentIntentID.String ||
			row.Text("currency") != "usd" ||
			amount <= 0 {
			return nil, 0, payments.ErrFacts
		}
		ids[id] = true
		switch row.Text("status") {
		case "succeeded", "pending", "requires_action":
			if amount > remaining {
				return nil, 0, payments.ErrFacts
			}
			remaining -= amount
		case "failed", "canceled":
		default:
			return nil, 0, payments.ErrFacts
		}
	}
	return rows, remaining, nil
}

func (s StaffFinanceService) stage(ctx context.Context, token, kind string, r StaffFinancialRequest) error {
	r.Reason = identity.TrimSpace(r.Reason)
	if !ValidRequestID(r.RequestID) || !plainText(r.Reason, true) || utf8.RuneCountInString(r.Reason) < 5 ||
		utf8.RuneCountInString(r.Reason) > 500 {
		return ErrStaffInput
	}
	permission := "gift.cancel"
	var amount pgtype.Int8
	var period pgtype.Timestamptz
	if kind == "REFUND" {
		permission = "gift.refund"
		if r.Amount < 1 || r.Amount > maxRefundMinor {
			return ErrStaffInput
		}
		amount = pgtype.Int8{Int64: r.Amount, Valid: true}
	} else if kind == "CANCEL" && !r.PeriodEnd.IsZero() {
		period = timestamp(r.PeriodEnd)
	} else {
		return ErrStaffInput
	}
	_, err := staffDonorWrite(
		ctx,
		s.Staff,
		token,
		permission,
		r.DonorID,
		func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (bool, error) {
			q := queries.New(tx)
			command, err := q.StaffActionByID(ctx, r.RequestID)
			if err == nil {
				if command.ActorID != actor.ID || command.DonorID != r.DonorID || command.Kind != kind || command.TargetID != r.TargetID ||
					command.Reason != r.Reason ||
					command.AmountMinor != amount ||
					command.PeriodEnd.Valid != period.Valid ||
					(period.Valid && !command.PeriodEnd.Time.Equal(period.Time)) {
					return false, ErrStaffConflict
				}
				return true, nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return false, err
			}
			if kind == "REFUND" {
				gift, err := staffGift(ctx, q, r.DonorID, r.TargetID)
				if err != nil {
					return false, err
				}
				if r.Amount > gift.AmountMinor {
					return false, ErrStaffConflict
				}
				pending, err := q.PendingStaffRefund(ctx, r.TargetID)
				if err != nil {
					return false, err
				}
				if pending {
					return false, ErrStaffConflict
				}
			} else {
				if _, err = ownedAttempt(ctx, q, r.DonorID, r.TargetID); err != nil {
					return false, err
				}
				if _, err = q.CancellationByID(ctx, r.RequestID); err == nil {
					return false, ErrStaffConflict
				}
				if !errors.Is(err, pgx.ErrNoRows) {
					return false, err
				}
			}
			if err = q.InsertStaffAction(ctx, queries.InsertStaffActionParams{ID: r.RequestID, ActorID: actor.ID, DonorID: r.DonorID, Kind: kind, TargetID: r.TargetID, Reason: r.Reason, AmountMinor: amount, PeriodEnd: period, CreatedAt: timestamp(now)}); err != nil {
				return false, err
			}
			return true, auditStaff(
				ctx,
				q,
				actor.ID,
				"gift.action_authorized",
				r.RequestID.String(),
				map[string]string{"kind": kind, "target_id": r.TargetID.String(), "reason": r.Reason},
				now,
			)
		},
	)
	return err
}

// configured authorizes token before reporting whether payments are
// configured, so an unauthorized caller learns nothing about the deployment.
func (s StaffFinanceService) configured(ctx context.Context, token, permission string) error {
	if _, err := requireStaff(ctx, queries.New(s.Staff.DB), token, permission, time.Now().UTC()); err != nil {
		return err
	}
	if s.Gateway == nil {
		return payments.ErrUnconfigured
	}
	return nil
}

func (s StaffFinanceService) Refund(ctx context.Context, token string, r StaffFinancialRequest) (StaffRefund, error) {
	if err := s.configured(ctx, token, "gift.refund"); err != nil {
		return StaffRefund{}, err
	}
	if err := s.stage(ctx, token, "REFUND", r); err != nil {
		return StaffRefund{}, err
	}
	result, err := s.submitRefund(ctx, token, r)
	if err != nil {
		return StaffRefund{}, s.auditFailure(ctx, token, "gift.refund", "gift.refund_failed", r, err)
	}
	return result, nil
}

// submitRefund creates the authorized refund with the processor without
// holding the donor lock, then records it under the lock.
func (s StaffFinanceService) submitRefund(ctx context.Context, token string, r StaffFinancialRequest) (StaffRefund, error) {
	return withFreshSnapshot(
		func() (refundSnapshot, error) { return s.createRefund(ctx, r) },
		func(snapshot refundSnapshot) (StaffRefund, error) {
			return staffDonorWrite(
				ctx,
				s.Staff,
				token,
				"gift.refund",
				r.DonorID,
				func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (StaffRefund, error) {
					q := queries.New(tx)
					command, err := q.StaffActionByID(ctx, r.RequestID)
					if err != nil {
						return StaffRefund{}, err
					}
					gift, err := staffGift(ctx, q, r.DonorID, r.TargetID)
					if err != nil {
						return StaffRefund{}, err
					}
					notSubmitted := StaffRefund{RequestID: command.ID, Status: "NOT_SUBMITTED", Mode: "test"}
					if command.Status == "ABANDONED" {
						return notSubmitted, nil
					}
					rows, remaining, err := s.currentRefunds(ctx, q, gift, snapshot)
					if err != nil {
						return StaffRefund{}, err
					}
					current, err := commandRefund(rows, command.ID)
					if err != nil {
						return StaffRefund{}, err
					}
					if current == nil {
						if command.StripeRefundID.Valid || now.Sub(command.CreatedAt.Time) >= commandRetryWindow || r.Amount <= remaining {
							return StaffRefund{}, ErrStaffConflict
						}
						if err = q.SaveStaffAction(ctx, queries.SaveStaffActionParams{ID: command.ID, Status: "ABANDONED", CompletedAt: timestamp(now)}); err != nil {
							return StaffRefund{}, err
						}
						if err = auditStaff(ctx, q, actor.ID, "gift.refund_not_submitted", command.ID.String(), map[string]string{"reason": "available_amount_changed"}, now); err != nil {
							return StaffRefund{}, err
						}
						return notSubmitted, nil
					}
					amount, err := current.Int("amount")
					metadata, metaErr := current.Object("metadata")
					if err != nil || metaErr != nil || amount != r.Amount || metadata.Text("dad_staff_action") != command.ID.String() ||
						(command.StripeRefundID.Valid && command.StripeRefundID.String != current.Text("id")) {
						return StaffRefund{}, payments.ErrFacts
					}
					if err = recordRefund(ctx, q, gift, current, pgtype.UUID{}, command.ID, now); err != nil {
						return StaffRefund{}, err
					}
					refund, err := q.RefundByStripeID(ctx, current.Text("id"))
					if err != nil {
						return StaffRefund{}, err
					}
					if err = q.SaveStaffAction(ctx, queries.SaveStaffActionParams{ID: command.ID, StripeRefundID: text(refund.StripeRefundID), Status: "OBSERVED", CompletedAt: timestamp(now)}); err != nil {
						return StaffRefund{}, err
					}
					if err = auditStaff(ctx, q, actor.ID, "gift.refund_observed", command.ID.String(), map[string]any{"status": refund.Status, "amount_minor": refund.AmountMinor, "reason": command.Reason}, now); err != nil {
						return StaffRefund{}, err
					}
					if _, err = refreshProjection(ctx, tx, r.DonorID, now); err != nil {
						return StaffRefund{}, err
					}
					return StaffRefund{
						RequestID:   command.ID,
						RefundID:    &refund.ID,
						Status:      refund.Status,
						AmountMinor: refund.AmountMinor,
						Currency:    "USD",
						Mode:        "test",
					}, nil
				},
			)
		}, ErrStaffConflict,
	)
}

// createRefund reads the gift's processor refunds without holding the donor
// lock and creates the command's refund unless the processor already has it or
// the gift no longer has enough left. Only one refund command per gift can be
// outstanding, so no other DAD refund competes for the balance.
func (s StaffFinanceService) createRefund(ctx context.Context, r StaffFinancialRequest) (refundSnapshot, error) {
	q := queries.New(s.Staff.DB)
	command, err := q.StaffActionByID(ctx, r.RequestID)
	if err != nil || command.Status == "ABANDONED" {
		return refundSnapshot{}, err
	}
	gift, err := q.StaffGiftSnapshot(ctx, queries.StaffGiftSnapshotParams{DonorID: r.DonorID, ID: r.TargetID})
	if err != nil {
		return refundSnapshot{}, err
	}
	if !gift.StripePaymentIntentID.Valid {
		return refundSnapshot{}, payments.ErrFacts
	}
	observations, err := q.GiftRefundObservations(ctx, gift.ID)
	if err != nil {
		return refundSnapshot{}, err
	}
	rows, remaining, err := s.refunds(ctx, gift)
	if err != nil {
		return refundSnapshot{}, err
	}
	snapshot := refundSnapshot{gift: gift, observations: observations, rows: rows}
	current, err := commandRefund(rows, command.ID)
	if err != nil || current != nil || command.StripeRefundID.Valid || time.Since(command.CreatedAt.Time) >= commandRetryWindow ||
		r.Amount > remaining {
		return snapshot, err
	}
	created, err := s.Gateway.CreateRefund(ctx, gift.StripePaymentIntentID.String, r.Amount, command.ID.String())
	if err != nil {
		return refundSnapshot{}, err
	}
	snapshot.rows = append(rows, created)
	return snapshot, nil
}

// commandRefund returns the processor refund created for a staff command.
func commandRefund(rows []payments.Object, command pgtype.UUID) (payments.Object, error) {
	var current payments.Object
	for _, row := range rows {
		metadata, err := row.Object("metadata")
		if err == nil && metadata.Text("dad_staff_action") == command.String() {
			if current != nil {
				return nil, payments.ErrFacts
			}
			current = row
		}
	}
	return current, nil
}

func (s StaffFinanceService) Cancel(ctx context.Context, token string, r StaffFinancialRequest) (StaffCancellation, error) {
	if err := s.configured(ctx, token, "gift.cancel"); err != nil {
		return StaffCancellation{}, err
	}
	if err := s.stage(ctx, token, "CANCEL", r); err != nil {
		return StaffCancellation{}, err
	}
	owner := func(ctx context.Context, tx pgx.Tx) (queries.Donor, error) {
		q := queries.New(tx)
		_, donor, err := lockStaffDonor(ctx, q, token, "gift.cancel", r.DonorID)
		if err != nil {
			return donor, err
		}
		command, err := q.StaffActionByID(ctx, r.RequestID)
		if err != nil {
			return donor, err
		}
		if command.Status == "ABANDONED" {
			return donor, ErrStaffConflict
		}
		return donor, nil
	}
	recurring := s.recurring()
	snapshot, err := recurring.refresh(ctx, owner, r.DonorID)
	if err != nil {
		return StaffCancellation{}, s.auditFailure(ctx, token, "gift.cancel", "gift.cancellation_failed", r, err)
	}
	result, err := recurring.cancel(
		ctx,
		owner,
		snapshot,
		CancellationRequest{RequestID: r.RequestID, AttemptID: r.TargetID, PeriodEnd: r.PeriodEnd},
	)
	if err != nil {
		return StaffCancellation{}, s.auditFailure(ctx, token, "gift.cancel", "gift.cancellation_failed", r, err)
	}
	_, err = staffDonorWrite(
		ctx,
		s.Staff,
		token,
		"gift.cancel",
		r.DonorID,
		func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (bool, error) {
			q := queries.New(tx)
			command, err := q.StaffActionByID(ctx, r.RequestID)
			if err != nil {
				return false, err
			}
			if command.CompletedAt.Valid {
				return true, nil
			}
			if err = q.SaveStaffAction(ctx, queries.SaveStaffActionParams{ID: command.ID, Status: "OBSERVED", CompletedAt: timestamp(now)}); err != nil {
				return false, err
			}
			return true, auditStaff(
				ctx,
				q,
				actor.ID,
				"gift.cancellation_confirmed",
				command.ID.String(),
				map[string]string{"reason": command.Reason, "period_end": command.PeriodEnd.Time.UTC().Format(time.RFC3339)},
				now,
			)
		},
	)
	if err != nil {
		return StaffCancellation{}, s.auditFailure(ctx, token, "gift.cancel", "gift.cancellation_failed", r, err)
	}
	return StaffCancellation{Mode: result.Mode, RequestID: r.RequestID, State: result.State, Subscription: result.Subscription}, nil
}

// auditFailure records a staff action's failed outcome in its own transaction,
// because the failed operation's transaction rolled back. It returns cause,
// joined with any error recording it.
func (s StaffFinanceService) auditFailure(
	ctx context.Context,
	token, permission, action string,
	r StaffFinancialRequest,
	cause error,
) error {
	if errors.Is(cause, ErrStaffDenied) || errors.Is(cause, ErrStaffForbidden) {
		return cause
	}
	outcome := "error"
	switch {
	case errors.Is(cause, payments.ErrProcessorUnavailable), errors.Is(cause, payments.ErrUnconfigured):
		outcome = "processor_unavailable"
	case errors.Is(cause, payments.ErrFacts):
		outcome = "invalid_processor_facts"
	case errors.Is(cause, ErrStaffConflict), errors.Is(cause, ErrSubscriptionConflict):
		outcome = "conflict"
	}
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := staffRead(auditCtx, s.Staff, token, permission, func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (bool, error) {
		details := map[string]string{"target_id": r.TargetID.String(), "reason": identity.TrimSpace(r.Reason), "outcome": outcome}
		return true, auditStaff(auditCtx, queries.New(tx), actor.ID, action, r.RequestID.String(), details, now)
	})
	if err != nil {
		return errors.Join(cause, err)
	}
	return cause
}
