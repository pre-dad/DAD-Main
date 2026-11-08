package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/payments"
	"github.com/jackc/pgx/v5"
)

// TestControlCharactersAreRejectedBeforeStorage covers inputs PostgreSQL
// cannot store, which must be refused as invalid rather than fail as errors.
func TestControlCharactersAreRejectedBeforeStorage(t *testing.T) {
	s, token, _ := donorServiceFixture(t)
	ctx := context.Background()
	if _, err := s.SaveSharing(ctx, token, "Ann\x00", false); !errors.Is(err, ErrPublicNameCharacters) {
		t.Fatalf("NUL public name: got %v, want ErrPublicNameCharacters", err)
	}
	if _, err := s.SaveSharing(ctx, token, "ann@example.org", false); !errors.Is(err, ErrPublicName) {
		t.Fatalf("email public name: got %v, want an ErrPublicName", err)
	}
	if _, err := s.ReferrerName(ctx, "code\x00"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("NUL referral code: got %v, want not found", err)
	}
	capture, err := s.CaptureReferral(ctx, "code\x00", "LINK", "", time.Now())
	if err != nil || capture.Token != "" {
		t.Fatalf("NUL referral capture: got %+v %v, want nothing captured", capture, err)
	}
	_, support := staffFixture(t, database(t), "OP_PARTNERSHIPS")
	if _, err = (&StaffService{DB: s.DB}).SearchDonors(ctx, support, "a\x00", ""); !errors.Is(err, ErrStaffInput) {
		t.Fatalf("NUL search: got %v, want ErrStaffInput", err)
	}
}

func TestDuplicateStaffInviteIsDistinct(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	_, admin := staffFixture(t, pool, "SUPER_ADMIN")
	var email string
	_, other := staffFixture(t, pool, "OP_PARTNERSHIPS")
	if err := pool.QueryRow(context.Background(), "SELECT s.email FROM donor_staff s JOIN donor_staff_sessions x ON x.staff_id=s.id WHERE x.token_hash=$1", HashToken(other)).Scan(&email); err != nil {
		t.Fatal(err)
	}
	_, err := (&StaffService{DB: api}).Invite(context.Background(), admin, email, "Again", "OP_PARTNERSHIPS")
	if !errors.Is(err, ErrStaffExists) || !errors.Is(err, ErrStaffConflict) {
		t.Fatalf("got %v, want ErrStaffExists", err)
	}
}

// TestStaffAuthorizationPrecedesPaymentConfiguration keeps an unconfigured
// processor from answering callers who may not act at all.
func TestStaffAuthorizationPrecedesPaymentConfiguration(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	_, finance := staffFixture(t, pool, "SUPER_ADMIN")
	_, support := staffFixture(t, pool, "OP_PARTNERSHIPS")
	s := StaffFinanceService{Staff: &StaffService{DB: api}}
	request := StaffFinancialRequest{
		RequestID: testID(t),
		DonorID:   testID(t),
		TargetID:  testID(t),
		Reason:    "Donor asked by phone",
		Amount:    100,
	}
	for _, c := range []struct {
		token string
		want  error
	}{{"not-a-session", ErrStaffDenied}, {support, ErrStaffForbidden}, {finance, payments.ErrUnconfigured}} {
		if _, err := s.Refund(context.Background(), c.token, request); !errors.Is(err, c.want) {
			t.Errorf("refund: got %v, want %v", err, c.want)
		}
		if _, err := s.Cancel(context.Background(), c.token, request); !errors.Is(err, c.want) {
			t.Errorf("cancel: got %v, want %v", err, c.want)
		}
	}
}
