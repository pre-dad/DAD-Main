package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5/pgtype"
)

type refundGateway struct {
	payments.StaffFinanceGateway
	refund              payments.Object
	creates             int
	loseAcknowledgement bool
	beforeCreate        func()
	t                   *testing.T
}

func (g *refundGateway) ListPaymentRefunds(context.Context, string) ([]payments.Object, error) {
	if g.refund == nil {
		return nil, nil
	}
	return []payments.Object{g.refund}, nil
}

func (g *refundGateway) CreateRefund(_ context.Context, payment string, amount int64, command string) (payments.Object, error) {
	if g.beforeCreate != nil {
		g.beforeCreate()
	}
	g.creates++
	g.refund = paymentObject(
		g.t,
		map[string]any{
			"id":             "re_" + command,
			"payment_intent": payment,
			"currency":       "usd",
			"amount":         amount,
			"status":         "succeeded",
			"metadata":       map[string]string{"dad_staff_action": command},
		},
	)
	if g.loseAcknowledgement {
		g.loseAcknowledgement = false
		return nil, payments.ErrProcessorUnavailable
	}
	return g.refund, nil
}

// refundFixture is a donor with one paid $30 monthly gift and a finance
// service whose processor the test controls.
func refundFixture(t *testing.T) (StaffFinanceService, *refundGateway, *workerFixture, pgtype.UUID) {
	t.Helper()
	pool := database(t)
	ctx := context.Background()
	f := newWorkerFixture(t, "MONTHLY")
	checkout := f.checkout(t)
	invoiceID := "in_finance_" + f.donorID.String()
	f.receive(t, "invoice.payment_succeeded", invoice(checkout.CheckoutID, invoiceID, time.Now().Add(-48*time.Hour), true))
	f.process(t)
	var giftID pgtype.UUID
	if err := pool.QueryRow(ctx, "SELECT id FROM donor_gifts WHERE donor_id=$1", f.donorID).Scan(&giftID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, sql := range []string{
			"DELETE FROM donor_refund_observations WHERE source_action_id IN (SELECT id FROM donor_staff_actions WHERE donor_id=$1)",
			"DELETE FROM donor_staff_actions WHERE donor_id=$1",
		} {
			if _, err := pool.Exec(context.Background(), sql, f.donorID); err != nil {
				t.Error(err)
			}
		}
	})
	gateway := &refundGateway{t: t}
	return StaffFinanceService{
		Staff:   &StaffService{DB: as(t, dbroles.API)},
		Gateway: gateway,
		Origin:  "https://example.org",
	}, gateway, f, giftID
}

func TestStaffRefundLostAcknowledgementPermissionsAndAudit(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	_, finance := staffFixture(t, pool, "SUPER_ADMIN")
	_, support := staffFixture(t, pool, "OP_PARTNERSHIPS")
	s, gateway, f, giftID := refundFixture(t)
	gateway.loseAcknowledgement = true
	r := StaffFinancialRequest{
		RequestID: testID(t),
		DonorID:   f.donorID,
		TargetID:  giftID,
		Amount:    1000,
		Reason:    "Donor requested partial refund",
	}
	if _, err := s.Refund(ctx, support, r); !errors.Is(err, ErrStaffForbidden) {
		t.Fatal("support permitted refund", err)
	}
	if gateway.creates != 0 {
		t.Fatal("denied action reached processor")
	}
	if _, err := s.Refund(ctx, finance, r); !errors.Is(err, payments.ErrProcessorUnavailable) {
		t.Fatal("expected lost acknowledgement", err)
	}
	for range 2 {
		result, err := s.Refund(ctx, finance, r)
		if err != nil || result.Status != "SUCCEEDED" {
			t.Fatal("refund reconciliation failed", result, err)
		}
	}
	if gateway.creates != 1 {
		t.Fatal("retry duplicated processor refund")
	}
	projection, err := queries.New(pool).Projection(ctx, f.donorID)
	if err != nil || projection.NetMinor.Int64 != 2000 || projection.CurrentStreak.Int32 != 1 || !projection.FoundingAwardedAt.Valid {
		t.Fatal("partial refund changed recognition incorrectly", err)
	}
	var authorizations, observations int
	if err = pool.QueryRow(ctx, "SELECT count(*) FILTER (WHERE action='gift.action_authorized'),count(*) FILTER (WHERE action='gift.refund_observed') FROM donor_staff_audit WHERE target=$1", r.RequestID.String()).Scan(&authorizations, &observations); err != nil ||
		authorizations != 1 ||
		observations < 1 {
		t.Fatal("refund audit incomplete", err)
	}
	r.Amount = 2000
	if _, err = s.Refund(ctx, finance, r); !errors.Is(err, ErrStaffConflict) {
		t.Fatal("idempotency payload changed", err)
	}
}

func TestStaffRefundCallsProcessorOutsideDonorLock(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	ctx := context.Background()
	_, finance := staffFixture(t, pool, "SUPER_ADMIN")
	f := newWorkerFixture(t, "MONTHLY")
	checkout := f.checkout(t)
	f.receive(
		t,
		"invoice.payment_succeeded",
		invoice(checkout.CheckoutID, "in_lock_"+f.donorID.String(), time.Now().Add(-48*time.Hour), true),
	)
	f.process(t)
	var giftID pgtype.UUID
	if err := pool.QueryRow(ctx, "SELECT id FROM donor_gifts WHERE donor_id=$1", f.donorID).Scan(&giftID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, sql := range []string{
			"DELETE FROM donor_refund_observations WHERE source_action_id IN (SELECT id FROM donor_staff_actions WHERE donor_id=$1)",
			"DELETE FROM donor_staff_actions WHERE donor_id=$1",
		} {
			if _, err := pool.Exec(context.Background(), sql, f.donorID); err != nil {
				t.Error(err)
			}
		}
	})
	stalled := newGate(t)
	gateway := &refundGateway{t: t, beforeCreate: stalled.wait}
	s := StaffFinanceService{Staff: &StaffService{DB: api}, Gateway: gateway, Origin: "https://example.org"}
	r := StaffFinancialRequest{
		RequestID: testID(t),
		DonorID:   f.donorID,
		TargetID:  giftID,
		Amount:    1000,
		Reason:    "Donor requested partial refund",
	}
	var result StaffRefund
	err := whileStalled(t, stalled, func() error {
		var err error
		result, err = s.Refund(ctx, finance, r)
		return err
	}, func() { assertDonorUnlocked(t, pool, f.donorID) })
	if err != nil || result.Status != "SUCCEEDED" || gateway.creates != 1 {
		t.Fatal("refund not recorded", result, err)
	}
	var status string
	if err = pool.QueryRow(ctx, "SELECT status FROM donor_staff_actions WHERE id=$1", r.RequestID).Scan(&status); err != nil ||
		status != "OBSERVED" {
		t.Fatal("staff action not completed", status, err)
	}
}

func TestStaffRefundRefusesMoreThanTheGiftAndASecondWhileOneIsPending(t *testing.T) {
	ctx := context.Background()
	_, finance := staffFixture(t, database(t), "SUPER_ADMIN")
	s, gateway, f, giftID := refundFixture(t)
	request := func(amount int64) StaffFinancialRequest {
		return StaffFinancialRequest{
			RequestID: testID(t),
			DonorID:   f.donorID,
			TargetID:  giftID,
			Amount:    amount,
			Reason:    "Donor requested a refund",
		}
	}
	if _, err := s.Refund(ctx, finance, request(3001)); !errors.Is(err, ErrStaffConflict) || gateway.creates != 0 {
		t.Fatal("refund above the gift authorized", err, gateway.creates)
	}
	gateway.loseAcknowledgement = true
	if _, err := s.Refund(ctx, finance, request(1000)); !errors.Is(err, payments.ErrProcessorUnavailable) {
		t.Fatal(err)
	}
	if _, err := s.Refund(ctx, finance, request(1000)); !errors.Is(err, ErrStaffConflict) || gateway.creates != 1 {
		t.Fatal("second refund authorized while the first is unsettled", err, gateway.creates)
	}
}

func TestStaffRefundRejectsInvalidRequestsBeforeTheProcessor(t *testing.T) {
	ctx := context.Background()
	_, finance := staffFixture(t, database(t), "SUPER_ADMIN")
	s, gateway, f, giftID := refundFixture(t)
	valid := StaffFinancialRequest{
		RequestID: testID(t),
		DonorID:   f.donorID,
		TargetID:  giftID,
		Amount:    1000,
		Reason:    "Donor requested a refund",
	}
	badID := testID(t)
	badID.Bytes[6] = 0x10
	for name, mutate := range map[string]func(*StaffFinancialRequest){
		"non-v4 request id": func(r *StaffFinancialRequest) { r.RequestID = badID },
		"zero amount":       func(r *StaffFinancialRequest) { r.Amount = 0 },
		"negative amount":   func(r *StaffFinancialRequest) { r.Amount = -1 },
		"amount over cap":   func(r *StaffFinancialRequest) { r.Amount = maxRefundMinor + 1 },
		"short reason":      func(r *StaffFinancialRequest) { r.Reason = "no" },
		"blank reason":      func(r *StaffFinancialRequest) { r.Reason = "     " },
		"control character": func(r *StaffFinancialRequest) { r.Reason = "bad\x00reason here" },
	} {
		r := valid
		mutate(&r)
		if _, err := s.Refund(ctx, finance, r); !errors.Is(err, ErrStaffInput) {
			t.Errorf("%s: error = %v; want ErrStaffInput", name, err)
		}
	}
	other, _ := donor(t, database(t))
	r := valid
	r.DonorID = other
	if _, err := s.Refund(ctx, finance, r); err == nil {
		t.Error("refunded a gift through another donor")
	}
	if gateway.creates != 0 {
		t.Fatal("invalid request reached the processor", gateway.creates)
	}
}
