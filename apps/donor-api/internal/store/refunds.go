package store

import (
	"context"
	"strings"
	"time"

	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5/pgtype"
)

// recordRefund stores current as one of gift's refunds, caused by exactly one of
// eventID (a webhook) or actionID (a staff action). The caller holds the donor then gift lock, and current was read from the
// processor after the gift's latest recorded refund state; webhook timestamps
// cannot order competing refund states reliably.
func recordRefund(
	ctx context.Context,
	q *queries.Queries,
	gift queries.DonorGift,
	current payments.Object,
	eventID, actionID pgtype.UUID,
	now time.Time,
) error {
	amount, err := current.Int("amount")
	status := strings.ToUpper(current.Text("status"))
	remoteID := current.Text("id")
	if err != nil || !strings.HasPrefix(remoteID, "re_") || current.Text("payment_intent") != gift.StripePaymentIntentID.String ||
		current.Text("currency") != "usd" ||
		amount <= 0 ||
		amount > gift.AmountMinor ||
		eventID.Valid == actionID.Valid {
		return payments.ErrFacts
	}
	switch status {
	case "PENDING", "REQUIRES_ACTION", "SUCCEEDED", "FAILED", "CANCELED":
	default:
		return payments.ErrFacts
	}
	id, err := NewID()
	if err != nil {
		return err
	}
	if err = q.InsertRefund(ctx, queries.InsertRefundParams{ID: id, GiftID: gift.ID, StripeRefundID: remoteID, AmountMinor: amount, Status: status, ObservedAt: timestamp(now)}); err != nil {
		return err
	}
	refund, err := q.RefundByStripeID(ctx, remoteID)
	if err != nil {
		return err
	}
	if refund.GiftID != gift.ID || refund.AmountMinor != amount {
		return payments.ErrFacts
	}
	if err = q.UpdateRefund(ctx, queries.UpdateRefundParams{ID: refund.ID, Status: status, ObservedAt: timestamp(now)}); err != nil {
		return err
	}
	amounts, err := q.SucceededRefundAmounts(ctx, gift.ID)
	if err != nil {
		return err
	}
	total := int64(0)
	for _, value := range amounts {
		if value > gift.AmountMinor-total {
			return payments.ErrFacts
		}
		total += value
	}
	observation, err := NewID()
	if err != nil {
		return err
	}
	return q.InsertRefundObservation(
		ctx,
		queries.InsertRefundObservationParams{
			ID:             observation,
			RefundID:       refund.ID,
			SourceEventID:  eventID,
			SourceActionID: actionID,
			Status:         status,
			ObservedAt:     timestamp(now),
		},
	)
}
