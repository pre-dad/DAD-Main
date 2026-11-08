package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/identity"
	"dad/donor-api/internal/mail"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5/pgtype"
)

func changeFixture(t *testing.T) (EmailChangeService, EmailChangeWorker, pgtype.UUID, string, string, *recordingMail) {
	t.Helper()
	pool := database(t)
	id, email := donor(t, pool)
	accountCleanup(t, pool, id)
	session := claimProof(t, link(t, pool, id, time.Now()))
	t.Cleanup(func() {
		for _, sql := range []string{"DELETE FROM donor_identity_events WHERE donor_id=$1", "DELETE FROM donor_email_changes WHERE donor_id=$1"} {
			if _, err := pool.Exec(context.Background(), sql, id); err != nil {
				t.Error(err)
			}
		}
	})
	transport := &recordingMail{}
	w := EmailChangeWorker{
		DB:        as(t, dbroles.EmailChangeWorker),
		Settings:  mail.Settings{Host: "smtp.fixture", Port: 587, Sender: "sender@example.org", EncryptionKeys: emailTestKeys},
		Origin:    "https://donor.example.org",
		Transport: transport,
	}
	return EmailChangeService{DB: as(t, dbroles.API), EncryptionKeys: emailTestKeys}, w, id, email, session, transport
}

func TestEmailChangeRequestedBeforeAKeyRotationIsStillSent(t *testing.T) {
	s, w, id, _, session, transport := changeFixture(t)
	ctx := context.Background()
	if err := s.Request(ctx, session, "rotated-"+id.String()+"@example.org"); err != nil {
		t.Fatal(err)
	}
	w.Settings.EncryptionKeys = rotatedTestKeys(t)
	if worked, err := w.DeliverNext(ctx, time.Now()); err != nil || !worked || len(transport.messages) != 1 {
		t.Fatal("proof encrypted under the previous key was not sent", worked, err)
	}
	if stage, err := s.Confirm(ctx, tokenFromMail(t, transport.messages[0])); err != nil || stage != "NEW" {
		t.Fatal("sent proof does not confirm", stage, err)
	}
}

func TestEmailChangeRequiresBothAddressesAndRevokesAccess(t *testing.T) {
	s, w, id, old, session, transport := changeFixture(t)
	pool := database(t)
	ctx := context.Background()
	newEmail := "new-" + id.String() + "@example.org"
	unusedLogin := link(t, pool, id, time.Now())
	if err := s.Request(ctx, session, newEmail); err != nil {
		t.Fatal(err)
	}
	if worked, err := w.DeliverNext(ctx, time.Now()); err != nil || !worked {
		t.Fatal(worked, err)
	}
	if len(transport.messages) != 1 || transport.messages[0].To != old {
		t.Fatal("first proof was not sent to current address")
	}
	first := tokenFromMail(t, transport.messages[0])
	if stage, err := s.Confirm(ctx, first); err != nil || stage != "NEW" {
		t.Fatal(stage, err)
	}
	if _, err := s.Confirm(ctx, first); !errors.Is(err, ErrDenied) {
		t.Fatal("current proof replay accepted", err)
	}
	user, err := authenticate(ctx, queries.New(s.DB), session, time.Now())
	if err != nil || user.Email != old {
		t.Fatal("identity changed before second confirmation", err)
	}
	if worked, err := w.DeliverNext(ctx, time.Now()); err != nil || !worked {
		t.Fatal(worked, err)
	}
	if len(transport.messages) != 2 || transport.messages[1].To != newEmail {
		t.Fatal("second proof sent to wrong address")
	}
	second := tokenFromMail(t, transport.messages[1])
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() { _, err := s.Confirm(ctx, second); results <- err })
	}
	wg.Wait()
	close(results)
	succeeded, denied := 0, 0
	for err := range results {
		if err == nil {
			succeeded++
		} else if errors.Is(err, ErrDenied) {
			denied++
		} else {
			t.Fatal(err)
		}
	}
	if succeeded != 1 || denied != 1 {
		t.Fatal("confirmation was not single use")
	}
	if _, err = authenticate(ctx, queries.New(s.DB), session, time.Now()); !errors.Is(err, ErrDenied) {
		t.Fatal("old session survived", err)
	}
	login, err := queries.New(pool).LoginLink(ctx, HashToken(unusedLogin))
	if err != nil || !login.ConsumedAt.Valid {
		t.Fatal("old sign-in proof survived", err)
	}
	donor, err := queries.New(pool).DonorByEmail(ctx, newEmail)
	if err != nil || donor.ID != id {
		t.Fatal("identity not preserved", err)
	}
	var events int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM donor_identity_events WHERE donor_id=$1", id).Scan(&events); err != nil ||
		events != 3 {
		t.Fatal("identity audit incomplete", err)
	}
}

func TestEmailChangeConflictsExpiryReplacementAndDeliveryRetry(t *testing.T) {
	s, w, id, old, session, transport := changeFixture(t)
	pool := database(t)
	ctx := context.Background()
	_, occupied := donor(t, pool)
	if err := s.Request(ctx, "invalid", occupied); !errors.Is(err, ErrDenied) {
		t.Fatal("anonymous change allowed", err)
	}
	if err := s.Request(ctx, session, occupied); err != nil {
		t.Fatal("address ownership leaked before verification", err)
	}
	transport.failure = errors.New("lost acknowledgement")
	if _, err := w.DeliverNext(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	first := tokenFromMail(t, transport.messages[0])
	transport.failure = nil
	if _, err := w.DeliverNext(ctx, time.Now().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(transport.messages) != 2 || tokenFromMail(t, transport.messages[1]) != first ||
		transport.messages[0].ID != transport.messages[1].ID {
		t.Fatal("delivery retry changed proof or message ID")
	}
	if _, err := s.Confirm(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := w.DeliverNext(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Confirm(ctx, tokenFromMail(t, transport.messages[2])); !errors.Is(err, ErrEmailChangeConflict) {
		t.Fatal("existing donor was merged", err)
	}
	d, err := authenticate(ctx, queries.New(s.DB), session, time.Now())
	if err != nil || d.Email != old {
		t.Fatal("conflict changed identity", err)
	}
	if err = s.Request(ctx, session, "fresh-"+id.String()+"@example.org"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Confirm(ctx, tokenFromMail(t, transport.messages[2])); !errors.Is(err, ErrDenied) {
		t.Fatal("superseded request remained active", err)
	}
	if _, err = w.DeliverNext(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	last := tokenFromMail(t, transport.messages[len(transport.messages)-1])
	if _, err = pool.Exec(ctx, "UPDATE donor_email_changes SET expires_at=now()-interval '1 second' WHERE donor_id=$1", id); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Confirm(ctx, last); !errors.Is(err, ErrDenied) {
		t.Fatal("expired proof accepted", err)
	}
}

func TestEmailChangeRemovesPasskeys(t *testing.T) {
	s, w, id, _, session, transport := changeFixture(t)
	pool := database(t)
	ctx := context.Background()
	passkeys := donorPasskeyService(t)
	passkeyDonorFor(t, pool, id)
	key := newAuthenticator(t, donorPasskeyTestOrigin, donorPasskeyTestRPID)
	if err := registerPasskey(t, passkeys, session, key, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Request(ctx, session, "passkeys-"+id.String()+"@example.org"); err != nil {
		t.Fatal(err)
	}
	for stage, want := range []string{"NEW", "COMPLETED"} {
		if worked, err := w.DeliverNext(ctx, time.Now()); err != nil || !worked {
			t.Fatal(worked, err)
		}
		if got, err := s.Confirm(ctx, tokenFromMail(t, transport.messages[stage])); err != nil || got != want {
			t.Fatal(got, err)
		}
	}
	var left int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_passkeys WHERE donor_id=$1", id).Scan(&left); err != nil || left != 0 {
		t.Fatal("passkeys survived the email change", left, err)
	}
	if _, err := donorSignIn(t, passkeys, key, Client{}); !errors.Is(err, ErrPasskeyDenied) {
		t.Fatal("removed passkey signed in", err)
	}
}

func TestEmailChangeRequestNormalizesAndRefusesTheCurrentAddress(t *testing.T) {
	s, w, id, old, session, transport := changeFixture(t)
	pool := database(t)
	ctx := context.Background()
	if err := s.Request(ctx, session, "  "+strings.ToUpper(old)+" "); !errors.Is(err, ErrEmailChangeConflict) {
		t.Fatalf("the current address in another case was accepted as new: %v", err)
	}
	for _, bad := range []string{"", "no-at-sign", "two@@example.org", "a b@example.org"} {
		if err := s.Request(ctx, session, bad); !errors.Is(err, identity.ErrEmail) {
			t.Fatalf("request for %q: %v; want identity.ErrEmail", bad, err)
		}
	}
	var requests int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_email_changes WHERE donor_id=$1", id).Scan(&requests); err != nil ||
		requests != 0 {
		t.Fatalf("%d change requests stored for refused addresses, err %v", requests, err)
	}
	mixed := "New-" + id.String() + "@Example.ORG"
	if err := s.Request(ctx, session, mixed); err != nil {
		t.Fatal(err)
	}
	for stage, want := range []string{"NEW", "COMPLETED"} {
		if worked, err := w.DeliverNext(ctx, time.Now()); err != nil || !worked {
			t.Fatal(worked, err)
		}
		if got, err := s.Confirm(ctx, tokenFromMail(t, transport.messages[stage])); err != nil || got != want {
			t.Fatal(got, err)
		}
	}
	if transport.messages[1].To != strings.ToLower(mixed) {
		t.Fatalf("new-address proof sent to %q, want the lower-cased address", transport.messages[1].To)
	}
	if found, err := queries.New(pool).DonorByEmail(ctx, strings.ToLower(mixed)); err != nil || found.ID != id {
		t.Fatal("account not stored under the normalized address", err)
	}
}

func TestEmailChangeRefusesAnAddressTakenByAnotherRoleBeforeCompletion(t *testing.T) {
	s, w, id, old, session, transport := changeFixture(t)
	pool := database(t)
	ctx := context.Background()
	cleanPartners(t, pool)
	partner := approvedPartner(t, pool, "Late Claimant", "LATECLAIM", 0, 0)
	target := "late-" + id.String() + "@example.org"
	if err := s.Request(ctx, session, target); err != nil {
		t.Fatal(err)
	}
	for stage := range 2 {
		if worked, err := w.DeliverNext(ctx, time.Now()); err != nil || !worked {
			t.Fatal(worked, err)
		}
		if stage == 1 {
			break
		}
		if got, err := s.Confirm(ctx, tokenFromMail(t, transport.messages[0])); err != nil || got != "NEW" {
			t.Fatal(got, err)
		}
	}
	userID := mustNewID(t)
	mustExec(t, pool, `INSERT INTO donor_partner_users (id, partner_id, email, name, role, accepted_at, created_at)
		VALUES ($1,$2,$3,'Late','VIEWER',now(),now())`, userID, partner, target)
	t.Cleanup(func() { cleanExec(t, "DELETE FROM donor_partner_users WHERE id=$1", userID) })
	if _, err := s.Confirm(ctx, tokenFromMail(t, transport.messages[1])); !errors.Is(err, ErrEmailChangeConflict) {
		t.Fatalf("an address claimed by a partner login meanwhile was taken: %v", err)
	}
	if d, err := authenticate(ctx, queries.New(s.DB), session, time.Now()); err != nil || d.Email != old {
		t.Fatal("refused change altered the account or its session", err)
	}
}

func TestEmailChangeIsRefusedForADisabledDonor(t *testing.T) {
	s, w, id, old, session, transport := changeFixture(t)
	pool := database(t)
	ctx := context.Background()
	if err := s.Request(ctx, session, "disabled-"+id.String()+"@example.org"); err != nil {
		t.Fatal(err)
	}
	if worked, err := w.DeliverNext(ctx, time.Now()); err != nil || !worked {
		t.Fatal(worked, err)
	}
	mustExec(t, pool, "UPDATE donors SET disabled=true WHERE id=$1", id)
	if _, err := s.Confirm(ctx, tokenFromMail(t, transport.messages[0])); !errors.Is(err, ErrDenied) {
		t.Fatalf("a disabled donor's proof was accepted: %v", err)
	}
	var email string
	if err := pool.QueryRow(ctx, "SELECT email FROM donors WHERE id=$1", id).Scan(&email); err != nil || email != old {
		t.Fatal("disabled donor's email changed", email, err)
	}
}
