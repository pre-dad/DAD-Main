package store

import (
	"context"
	"time"

	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Staff previews read stored state; the refund or cancellation they lead to
// reads the processor again before acting.

type RefundQuote struct {
	AmountMinor    int64       `json:"amount_minor"`
	AvailableMinor int64       `json:"available_minor"`
	Currency       string      `json:"currency"`
	DonorID        pgtype.UUID `json:"donor_id"`
	GiftID         pgtype.UUID `json:"gift_id"`
	Mode           string      `json:"mode"`
	// ObservedAt is when a refund of the gift was last recorded, if ever.
	ObservedAt *time.Time `json:"observed_at,omitempty"`
}

// RefundQuote previews how much of a gift can still be refunded from its
// recorded refunds, of which pending ones still reserve their amount.
func (s StaffFinanceService) RefundQuote(ctx context.Context, token string, donorID, giftID pgtype.UUID) (RefundQuote, error) {
	return staffRead(ctx, s.Staff, token, "gift.refund", func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (RefundQuote, error) {
		q := queries.New(tx)
		gift, err := q.StaffGiftSnapshot(ctx, queries.StaffGiftSnapshotParams{DonorID: donorID, ID: giftID})
		if err != nil {
			return RefundQuote{}, err
		}
		if !gift.StripePaymentIntentID.Valid {
			return RefundQuote{}, payments.ErrFacts
		}
		refunds, err := q.ReceiptRefunds(ctx, gift.ID)
		if err != nil {
			return RefundQuote{}, err
		}
		quote := RefundQuote{
			GiftID:         gift.ID,
			DonorID:        gift.DonorID,
			AmountMinor:    gift.AmountMinor,
			AvailableMinor: gift.AmountMinor,
			Currency:       "USD",
			Mode:           "test",
		}
		for _, r := range refunds {
			if r.Status != "FAILED" && r.Status != "CANCELED" {
				quote.AvailableMinor -= r.AmountMinor
			}
			if quote.ObservedAt == nil || r.ObservedAt.Time.After(*quote.ObservedAt) {
				quote.ObservedAt = &r.ObservedAt.Time
			}
		}
		if quote.AvailableMinor < 0 {
			return RefundQuote{}, payments.ErrFacts
		}
		return quote, auditStaff(ctx, q, actor.ID, "gift.refund_preview", gift.ID.String(), nil, now)
	})
}

// CancellationQuote previews the donor's monthly gift for a staff
// cancellation.
func (s StaffFinanceService) CancellationQuote(ctx context.Context, token string, donorID pgtype.UUID) (RecurringDetails, error) {
	return staffRead(
		ctx,
		s.Staff,
		token,
		"gift.cancel",
		func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (RecurringDetails, error) {
			q := queries.New(tx)
			if _, err := q.StaffDonorByID(ctx, donorID); err != nil {
				return RecurringDetails{}, err
			}
			view, err := storedDetails(ctx, q, donorID)
			if err != nil || view.Subscription == nil {
				return RecurringDetails{Mode: "test"}, err
			}
			return view, auditStaff(ctx, q, actor.ID, "gift.cancel_preview", view.Subscription.ID, nil, now)
		},
	)
}
