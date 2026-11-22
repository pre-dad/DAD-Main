package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"

	"dad/donor-api/internal/identity"
	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/store"
)

type StaffFinanceSource interface {
	RefundQuote(context.Context, string, pgtype.UUID, pgtype.UUID) (store.RefundQuote, error)
	CancellationQuote(context.Context, string, pgtype.UUID) (store.RecurringDetails, error)
	Refund(context.Context, string, store.StaffFinancialRequest) (store.StaffRefund, error)
	Cancel(context.Context, string, store.StaffFinancialRequest) (store.StaffCancellation, error)
}

func financeStatus(err error) (int, string) {
	if errors.Is(err, payments.ErrFacts) || errors.Is(err, store.ErrSubscriptionConflict) {
		return http.StatusConflict, "Financial records require reconciliation. Refresh before confirming"
	}
	return staffStatus(err)
}

// validReason applies the store's own limit, measured after trimming, so the
// handler never admits or refuses what the store would decide otherwise.
func validReason(reason string) bool {
	n := utf8.RuneCountInString(identity.TrimSpace(reason))
	return n >= 5 && n <= 500
}

// Financial previews read stored state; actions call the processor, so they
// get its time budget.
func (a *api) finance(timeout time.Duration) service {
	return service{a.StaffFinance != nil, "Financial actions are temporarily unavailable", timeout, financeStatus}
}

func (a *api) refundQuote(w http.ResponseWriter, r *http.Request, session string) {
	donorID, ok := pathID(w, r, "donor_id")
	if !ok {
		return
	}
	giftID, ok := pathID(w, r, "gift_id")
	if !ok {
		return
	}
	a.finance(readTimeout).serve(w, r, func(ctx context.Context) (any, error) {
		return a.StaffFinance.RefundQuote(ctx, session, donorID, giftID)
	})
}

func (a *api) cancellationQuote(w http.ResponseWriter, r *http.Request, session string) {
	donorID, ok := pathID(w, r, "donor_id")
	if !ok {
		return
	}
	a.finance(readTimeout).serve(w, r, func(ctx context.Context) (any, error) {
		return a.StaffFinance.CancellationQuote(ctx, session, donorID)
	})
}

type refundBody struct {
	Amount int64  `json:"amount_minor"`
	Reason string `json:"reason"`
}

func (b refundBody) valid() bool {
	return b.Amount >= 1 && b.Amount <= maxAmountMinor && validReason(b.Reason)
}

func (a *api) refund(w http.ResponseWriter, r *http.Request, session string) {
	donorID, ok := pathID(w, r, "donor_id")
	if !ok {
		return
	}
	giftID, ok := pathID(w, r, "gift_id")
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	body, ok := decodeValid[refundBody](w, r)
	if !ok {
		return
	}
	request := store.StaffFinancialRequest{RequestID: key, DonorID: donorID, TargetID: giftID, Amount: body.Amount, Reason: body.Reason}
	a.finance(providerTimeout).serve(w, r, func(ctx context.Context) (any, error) { return a.StaffFinance.Refund(ctx, session, request) })
}

type staffCancelBody struct {
	ID     pgtype.UUID `json:"subscription_id"`
	End    time.Time   `json:"period_end"`
	Reason string      `json:"reason"`
}

func (b staffCancelBody) valid() bool { return b.ID.Valid && !b.End.IsZero() && validReason(b.Reason) }

func (a *api) cancelSubscription(w http.ResponseWriter, r *http.Request, session string) {
	donorID, ok := pathID(w, r, "donor_id")
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	body, ok := decodeValid[staffCancelBody](w, r)
	if !ok {
		return
	}
	request := store.StaffFinancialRequest{
		RequestID: key,
		DonorID:   donorID,
		TargetID:  body.ID,
		PeriodEnd: body.End.Truncate(time.Microsecond),
		Reason:    body.Reason,
	}
	a.finance(providerTimeout).serve(w, r, func(ctx context.Context) (any, error) { return a.StaffFinance.Cancel(ctx, session, request) })
}
