package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/payments"
	"github.com/jackc/pgx/v5/pgtype"
)

func cancelFixture(t *testing.T) (StaffFinanceService, *recurringFixture, StaffFinancialRequest) {
	t.Helper()
	r := newRecurringFixture(t)
	s := StaffFinanceService{Staff: &StaffService{DB: as(t, dbroles.API)}, Gateway: r.sub, Origin: "https://donor.example.org"}
	return s, r, StaffFinancialRequest{
		RequestID: testID(t),
		DonorID:   r.donorID,
		TargetID:  r.attempt,
		Reason:    "Donor asked by phone to stop",
		PeriodEnd: r.end,
	}
}

func auditActions(t *testing.T, r *recurringFixture, target pgtype.UUID) map[string]string {
	t.Helper()
	rows, err := r.db.Query(
		context.Background(),
		"SELECT action,COALESCE(details->>'outcome','') FROM donor_staff_audit WHERE target=$1",
		target.String(),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	actions := map[string]string{}
	for rows.Next() {
		var action, outcome string
		if err = rows.Scan(&action, &outcome); err != nil {
			t.Fatal(err)
		}
		actions[action] = outcome
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return actions
}

func TestStaffCancelDeniesWithoutPermissionBeforeAnyRecordOrProcessorCall(t *testing.T) {
	pool := database(t)
	_, support := staffFixture(t, pool, "OP_PARTNERSHIPS")
	s, r, request := cancelFixture(t)
	if _, err := s.Cancel(context.Background(), support, request); !errors.Is(err, ErrStaffForbidden) {
		t.Fatal("support cancelled a gift", err)
	}
	if _, err := s.Cancel(context.Background(), "no-such-session", request); !errors.Is(err, ErrStaffDenied) {
		t.Fatal("unauthenticated cancel", err)
	}
	var actions int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM donor_staff_actions WHERE id=$1", request.RequestID).Scan(&actions); err != nil ||
		actions != 0 {
		t.Fatal("a denied cancel staged an action", actions, err)
	}
	if len(r.sub.keys) != 0 {
		t.Fatal("denied cancel reached the processor", r.sub.keys)
	}
}

func TestStaffCancelRefusesAnUnknownSubscriptionBeforeStagingAnything(t *testing.T) {
	pool := database(t)
	_, finance := staffFixture(t, pool, "SUPER_ADMIN")
	s, r, request := cancelFixture(t)
	request.TargetID = testID(t)
	if _, err := s.Cancel(context.Background(), finance, request); !errors.Is(err, ErrSubscriptionConflict) {
		t.Fatal("cancel of a subscription the donor does not own", err)
	}
	if len(r.sub.keys) != 0 {
		t.Fatal("unknown subscription reached the processor", r.sub.keys)
	}
	var actions int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM donor_staff_actions WHERE id=$1", request.RequestID).Scan(&actions); err != nil ||
		actions != 0 {
		t.Fatal("a refused cancel staged an action", actions, err)
	}
	if audited := auditActions(t, r, request.RequestID); len(audited) != 0 {
		t.Fatalf("a refused cancel was audited as authorized: %v", audited)
	}
}

func TestStaffCancelRepeatIsIdempotentAndRejectsAChangedRequest(t *testing.T) {
	pool := database(t)
	_, finance := staffFixture(t, pool, "SUPER_ADMIN")
	s, r, request := cancelFixture(t)
	ctx := context.Background()
	for range 3 {
		if _, err := s.Cancel(ctx, finance, request); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.sub.keys) != 1 {
		t.Fatalf("repeats reached the processor %d times", len(r.sub.keys))
	}
	var confirmations int
	if err := pool.QueryRow(
		ctx,
		"SELECT count(*) FROM donor_staff_audit WHERE target=$1 AND action='gift.cancellation_confirmed'",
		request.RequestID.String(),
	).Scan(&confirmations); err != nil || confirmations != 1 {
		t.Fatal("confirmation audited more than once", confirmations, err)
	}
	changed := request
	changed.Reason = "A different reason entirely"
	if _, err := s.Cancel(ctx, finance, changed); !errors.Is(err, ErrStaffConflict) {
		t.Fatal("request id reused with another reason", err)
	}
	changed = request
	changed.PeriodEnd = r.end.Add(-time.Hour)
	if _, err := s.Cancel(ctx, finance, changed); !errors.Is(err, ErrStaffConflict) {
		t.Fatal("request id reused with another boundary", err)
	}
}

func TestStaffCancelCallsProcessorOutsideDonorLock(t *testing.T) {
	pool := database(t)
	_, finance := staffFixture(t, pool, "SUPER_ADMIN")
	s, r, request := cancelFixture(t)
	stalled := r.sub.stall(t)
	err := whileStalled(t, stalled, func() error {
		_, err := s.Cancel(context.Background(), finance, request)
		return err
	}, func() { assertDonorUnlocked(t, pool, r.donorID) })
	if err != nil || len(r.sub.keys) != 1 {
		t.Fatal("cancel not recorded", err, r.sub.keys)
	}
}

func TestStaffCancelWritesAuthorizedAndConfirmedAuditRows(t *testing.T) {
	pool := database(t)
	_, finance := staffFixture(t, pool, "SUPER_ADMIN")
	s, r, request := cancelFixture(t)
	if _, err := s.Cancel(context.Background(), finance, request); err != nil {
		t.Fatal(err)
	}
	actions := auditActions(t, r, request.RequestID)
	for _, action := range []string{"gift.action_authorized", "gift.cancellation_confirmed"} {
		if _, found := actions[action]; !found {
			t.Fatalf("missing %s audit row: %v", action, actions)
		}
	}
	if _, failed := actions["gift.cancellation_failed"]; failed {
		t.Fatalf("a successful cancel audited a failure: %v", actions)
	}
}

func TestStaffCancelRefusesStaleStateWithoutTheProcessor(t *testing.T) {
	pool := database(t)
	_, finance := staffFixture(t, pool, "SUPER_ADMIN")
	s, r, request := cancelFixture(t)
	next := r.end.AddDate(0, 1, 0)
	r.changeRemotely(t, func(remote map[string]any) {
		subscriptionItem(remote)["current_period_start"] = r.end.Unix()
		subscriptionItem(remote)["current_period_end"] = next.Unix()
	})
	if _, err := s.Cancel(context.Background(), finance, request); !errors.Is(err, ErrSubscriptionConflict) {
		t.Fatal("cancel at a boundary Stripe has passed", err)
	}
	if len(r.sub.keys) != 0 {
		t.Fatal("stale cancel reached the processor", r.sub.keys)
	}
	if got := auditActions(t, r, request.RequestID)["gift.cancellation_failed"]; got != "conflict" {
		t.Fatalf("failure outcome %q, want conflict", got)
	}
	var status string
	if err := pool.QueryRow(context.Background(), "SELECT status FROM donor_staff_actions WHERE id=$1", request.RequestID).Scan(&status); err != nil ||
		status == "OBSERVED" {
		t.Fatal("a refused cancel was recorded as observed", status, err)
	}
}

func TestStaffCancelWithoutAProcessorFailsClosed(t *testing.T) {
	pool := database(t)
	_, finance := staffFixture(t, pool, "SUPER_ADMIN")
	s, _, request := cancelFixture(t)
	s.Gateway = nil
	if _, err := s.Cancel(context.Background(), finance, request); !errors.Is(err, payments.ErrUnconfigured) {
		t.Fatal(err)
	}
}
