package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/store/queries"
)

func TestDeleteAccountErasesPersonalDataAndKeepsLedger(t *testing.T) {
	s, token, id := donorServiceFixture(t)
	pool := database(t)
	ctx := context.Background()

	// Give the donor identity fields, an OIDC identity, and a retained gift.
	if _, err := pool.Exec(ctx,
		"UPDATE donors SET public_name='Dana', zip='94103', city='SF', timezone='America/Los_Angeles', phone='+14155550123', referral_code='dana', company_code='ACME' WHERE id=$1", id,
	); err != nil {
		t.Fatal(err)
	}
	mustExec(
		t,
		pool,
		"INSERT INTO donor_phone_codes(phone,code_hash,expires_at) VALUES('+14155550123',$1,now()+interval '10 minutes')",
		HashToken("123456"),
	)
	t.Cleanup(func() { cleanExec(t, "DELETE FROM donor_phone_codes WHERE phone='+14155550123'") })
	oidcID := mustNewID(t)
	if _, err := pool.Exec(ctx,
		"INSERT INTO donor_oidc_identities (id, donor_id, provider, subject) VALUES ($1,$2,'GOOGLE','sub-del')", oidcID, id,
	); err != nil {
		t.Fatal(err)
	}
	cleanup(t, "DELETE FROM donor_oidc_identities WHERE donor_id=$1", id)
	// A gift is the retained legal record.
	checkoutID, giftID := mustNewID(t), mustNewID(t)
	if _, err := pool.Exec(ctx,
		"INSERT INTO donor_checkouts(id,donor_id,amount_minor,frequency) VALUES($1,$2,3000,'MONTHLY')", checkoutID, id,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO donor_gifts(id,donor_id,checkout_id,stripe_object_id,amount_minor,currency,frequency,paid_at) VALUES($1,$2,$3,$4,3000,'USD','MONTHLY',now())",
		giftID, id, checkoutID, "pi_"+giftID.String(),
	); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanExec(t, "DELETE FROM donor_gifts WHERE donor_id=$1", id)
		cleanExec(t, "DELETE FROM donor_checkouts WHERE donor_id=$1", id)
		cleanExec(t, "DELETE FROM donor_deletions WHERE donor_id=$1", id)
	})

	// An email change, an app session naming its device, an employer, and the
	// only seat at an event someone else is waiting for.
	changeID := mustNewID(t)
	mustExec(t, pool, `INSERT INTO donor_email_changes (id, donor_id, old_email, new_email, stage, token_hash, expires_at)
		VALUES ($1, $2, 'old@example.org', 'new@example.org', 'COMPLETED', $3, now())`, changeID, id, HashToken(changeID.String()))
	t.Cleanup(func() { cleanExec(t, "DELETE FROM donor_email_changes WHERE donor_id=$1", id) })
	mustExec(t, pool, `INSERT INTO donor_sessions (id, donor_id, token_hash, expires_at, client, device_name, platform,
			refresh_family_hash, refresh_hash, refresh_expires_at)
		VALUES ($1, $2, $3, now() + interval '1 day', 'APP', 'Dana''s iPhone', 'IOS', $4, $5, now() + interval '1 day')`,
		mustNewID(t), id, HashToken("app-"+id.String()), HashToken("family-"+id.String()), HashToken("refresh-"+id.String()))
	deliveredID, pendingID := mustNewID(t), mustNewID(t)
	mustExec(t, pool, `INSERT INTO donor_messages (id, donor_id, kind, dedup_key, context, available_at, completed_at, subject, body)
		VALUES ($1, $2, 'REFERRAL', $3, '{"name":"Sam"}', now(), now(), 'Sam joined', 'Sam gave $30')`, deliveredID, id, "delivered-"+id.String())
	mustExec(t, pool, `INSERT INTO donor_messages (id, donor_id, kind, dedup_key, context, available_at)
		VALUES ($1, $2, 'REFERRAL', $3, '{"name":"Sam"}', now())`, pendingID, id, "pending-"+id.String())
	t.Cleanup(func() { cleanExec(t, "DELETE FROM donor_messages WHERE donor_id=$1", id) })
	partnerID := approvedPartner(t, pool, "Epsilon Co", "EPSILON", 0, 0)
	mustExec(t, pool, "INSERT INTO donor_partner_employees (donor_id, partner_id) VALUES ($1, $2)", id, partnerID)
	cleanEvents(t, pool)
	_, adminToken := staffFixture(t, pool, "SUPER_ADMIN")
	events := EventService{Staff: &StaffService{DB: as(t, dbroles.API)}}
	seat := eventInput("one-seat", "San Francisco", "94103", "VOLUNTEER", 48*time.Hour)
	seat.Capacity = 1
	eventID, err := events.Create(ctx, adminToken, seat)
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Publish(ctx, adminToken, eventID); err != nil {
		t.Fatal(err)
	}
	reservations := ReservationService{DB: as(t, dbroles.API)}
	waiting, waitingToken := signedInDonor(t, pool)
	pushCleanupDonor(t, pool, waiting)
	if r, err := reservations.Reserve(ctx, token, eventID); err != nil || r.Status != reservationReserved {
		t.Fatalf("reserve: %+v %v", r, err)
	}
	if r, err := reservations.Reserve(ctx, waitingToken, eventID); err != nil || r.Status != reservationWaitlisted {
		t.Fatalf("waitlist: %+v %v", r, err)
	}

	if err := s.DeleteAccount(ctx, token); err != nil {
		t.Fatal(err)
	}

	// Personal data scrubbed; email is a non-routable placeholder; disabled.
	var email string
	var publicName, phone, zip, city, tz, refCode, companyCode *string
	var disabled bool
	if err := pool.QueryRow(ctx,
		"SELECT email, public_name, phone, zip, city, timezone, referral_code, company_code, disabled FROM donors WHERE id=$1", id,
	).Scan(&email, &publicName, &phone, &zip, &city, &tz, &refCode, &companyCode, &disabled); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(email, "@deleted.invalid") {
		t.Errorf("email = %q; want a @deleted.invalid placeholder", email)
	}
	for name, v := range map[string]*string{
		"public_name": publicName, "phone": phone, "zip": zip, "city": city,
		"timezone": tz, "referral_code": refCode, "company_code": companyCode,
	} {
		if v != nil {
			t.Errorf("%s = %q; want cleared", name, *v)
		}
	}
	if !disabled {
		t.Error("account not disabled after deletion")
	}
	var phoneCodes int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_phone_codes WHERE phone='+14155550123'").Scan(&phoneCodes); err != nil ||
		phoneCodes != 0 {
		t.Errorf("deleted account retained %d phone codes (%v)", phoneCodes, err)
	}

	// OIDC identity removed.
	var oidcCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_oidc_identities WHERE donor_id=$1", id).Scan(&oidcCount); err != nil ||
		oidcCount != 0 {
		t.Errorf("oidc identities = %d, err %v; want 0", oidcCount, err)
	}

	var oldEmail, newEmail string
	if err := pool.QueryRow(ctx, "SELECT old_email, new_email FROM donor_email_changes WHERE id=$1", changeID).
		Scan(&oldEmail, &newEmail); err != nil || strings.Contains(oldEmail+newEmail, "example.org") {
		t.Errorf("email change kept addresses %q, %q (%v)", oldEmail, newEmail, err)
	}
	var devices int
	if err := pool.QueryRow(ctx, "SELECT count(device_name) FROM donor_sessions WHERE donor_id=$1", id).Scan(&devices); err != nil ||
		devices != 0 {
		t.Errorf("sessions kept %d device names (%v)", devices, err)
	}
	var delivered, pending string
	if err := pool.QueryRow(ctx, "SELECT coalesce(subject,'')||coalesce(body,'')||context::text FROM donor_messages WHERE id=$1", deliveredID).
		Scan(&delivered); err != nil ||
		delivered != "{}" {
		t.Errorf("delivered message kept content %q (%v)", delivered, err)
	}
	if err := pool.QueryRow(ctx, "SELECT context::text FROM donor_messages WHERE id=$1", pendingID).Scan(&pending); err != nil ||
		!strings.Contains(pending, "Sam") {
		t.Errorf("pending message context = %q (%v); want it kept for delivery", pending, err)
	}
	var employers int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_partner_employees WHERE donor_id=$1", id).Scan(&employers); err != nil ||
		employers != 0 {
		t.Errorf("employer link kept (%d, %v)", employers, err)
	}
	var waitingStatus string
	if err := pool.QueryRow(ctx, "SELECT status FROM donor_event_reservations WHERE event_id=$1 AND donor_id=$2", eventID, waiting).
		Scan(&waitingStatus); err != nil || waitingStatus != reservationReserved {
		t.Errorf("the waitlisted donor is %q (%v) after the seat holder deleted their account", waitingStatus, err)
	}

	// The gift ledger is retained.
	var giftCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_gifts WHERE donor_id=$1", id).Scan(&giftCount); err != nil || giftCount != 1 {
		t.Errorf("gifts = %d, err %v; want 1 (ledger retained)", giftCount, err)
	}

	// The deletion is audited.
	var auditCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_deletions WHERE donor_id=$1", id).Scan(&auditCount); err != nil ||
		auditCount != 1 {
		t.Errorf("deletion audit rows = %d, err %v; want 1", auditCount, err)
	}

	// The session no longer authenticates.
	if _, err := s.Profile(ctx, token); err == nil {
		t.Error("session still valid after deletion; want denied")
	}

	// A disabled anonymised donor still reads back by id for the ledger's FK.
	if _, err := queries.New(pool).LockDonor(ctx, id); err != nil {
		t.Errorf("anonymised donor row unreadable: %v", err)
	}
}

// TestDeleteAccountRefusedWhileGivingRenews keeps a deleted donor from being
// charged: deletion waits until the monthly gift is cancelled.
func TestDeleteAccountRefusedWhileGivingRenews(t *testing.T) {
	s, token, id := donorServiceFixture(t)
	pool := database(t)
	ctx := context.Background()
	checkout, attempt, subscription := mustNewID(t), mustNewID(t), mustNewID(t)
	t.Cleanup(func() {
		cleanExec(t, "DELETE FROM donor_subscriptions WHERE donor_id=$1", id)
		cleanExec(t, "DELETE FROM donor_stripe_checkouts WHERE id=$1", attempt)
		cleanExec(t, "DELETE FROM donor_checkouts WHERE donor_id=$1", id)
		cleanExec(t, "DELETE FROM donor_deletions WHERE donor_id=$1", id)
	})
	mustExec(t, pool, "INSERT INTO donor_checkouts(id,donor_id,amount_minor,frequency) VALUES($1,$2,3000,'MONTHLY')", checkout, id)
	mustExec(
		t,
		pool,
		"INSERT INTO donor_stripe_checkouts(id,checkout_id,request_hash,request_params,status) VALUES($1,$2,$3,'{}','COMPLETE')",
		attempt,
		checkout,
		attempt.String(),
	)

	if err := s.DeleteAccount(ctx, token); !errors.Is(err, ErrGivingActive) {
		t.Fatalf("delete with checkout complete but subscription unsynced: %v", err)
	}
	mustExec(t, pool, "UPDATE donor_stripe_checkouts SET stripe_subscription_id=$2 WHERE id=$1", attempt, "sub_"+attempt.String())
	mustExec(
		t,
		pool,
		"INSERT INTO donor_subscriptions(id,attempt_id,donor_id,status,period_end,observed_at) VALUES($1,$2,$3,'active',now()+interval '20 days',now())",
		subscription,
		attempt,
		id,
	)
	if err := s.DeleteAccount(ctx, token); !errors.Is(err, ErrGivingActive) {
		t.Fatalf("delete with a renewing subscription: %v", err)
	}
	var email string
	if err := pool.QueryRow(ctx, "SELECT email FROM donors WHERE id=$1", id).Scan(&email); err != nil ||
		strings.HasSuffix(email, "@deleted.invalid") {
		t.Fatalf("a refused deletion changed the donor: %q %v", email, err)
	}

	mustExec(t, pool, "UPDATE donor_subscriptions SET stops_at=period_end WHERE id=$1", subscription)
	if err := s.DeleteAccount(ctx, token); err != nil {
		t.Fatalf("delete once the gift is cancelled at period end: %v", err)
	}
}

// TestDeleteAccountWaitsForAnOpenCheckout keeps a checkout paid after the
// account was deleted from starting a subscription nobody can stop.
func TestDeleteAccountWaitsForAnOpenCheckout(t *testing.T) {
	s, token, id := donorServiceFixture(t)
	pool := database(t)
	ctx := context.Background()
	checkout, attempt := mustNewID(t), mustNewID(t)
	t.Cleanup(func() {
		cleanExec(t, "DELETE FROM donor_stripe_checkouts WHERE id=$1", attempt)
		cleanExec(t, "DELETE FROM donor_checkouts WHERE donor_id=$1", id)
		cleanExec(t, "DELETE FROM donor_deletions WHERE donor_id=$1", id)
	})
	mustExec(t, pool, "INSERT INTO donor_checkouts(id,donor_id,amount_minor,frequency) VALUES($1,$2,3000,'MONTHLY')", checkout, id)
	mustExec(t, pool, "INSERT INTO donor_stripe_checkouts(id,checkout_id,request_hash,request_params,status) VALUES($1,$2,$3,'{}','OPEN')",
		attempt, checkout, attempt.String())
	if err := s.DeleteAccount(ctx, token); !errors.Is(err, ErrGivingActive) {
		t.Fatalf("delete during an open checkout: %v", err)
	}
	mustExec(t, pool, "UPDATE donor_stripe_checkouts SET created_at=now()-interval '3 hours' WHERE id=$1", attempt)
	if err := s.DeleteAccount(ctx, token); err != nil {
		t.Fatalf("delete once the checkout has long expired: %v", err)
	}
}
