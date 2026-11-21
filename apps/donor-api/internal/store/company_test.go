package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestCompanyCodeCaptureAndBind(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	ctx := context.Background()
	s := DonorService{DB: api, Origin: "https://donor.example.org"}
	now := time.Now().UTC()

	cleanup(t, "DELETE FROM donor_company_touches")

	// A malformed code with no prior touch captures nothing.
	if cap, err := s.CaptureCompanyCode(ctx, "!!bad!!", "", now); err != nil || cap.Token != "" {
		t.Fatalf("malformed capture = %+v, %v; want empty", cap, err)
	}

	// A valid code mints a visitor token and is normalised to upper case.
	first, err := s.CaptureCompanyCode(ctx, "acme-corp", "", now)
	if err != nil || first.Token == "" {
		t.Fatalf("first capture = %+v, %v", first, err)
	}
	var stored string
	if err := pool.QueryRow(ctx, "SELECT company_code FROM donor_company_touches WHERE token_hash=$1", HashToken(first.Token)).Scan(&stored); err != nil ||
		stored != "ACME-CORP" {
		t.Fatalf("stored code = %q, err %v; want ACME-CORP", stored, err)
	}

	// Last touch wins: a new code on the same token overwrites it, same token.
	second, err := s.CaptureCompanyCode(ctx, "GLOBEX", first.Token, now.Add(time.Minute))
	if err != nil || second.Token != first.Token {
		t.Fatalf("second capture = %+v, %v; want same token %q", second, err, first.Token)
	}
	if err := pool.QueryRow(ctx, "SELECT company_code FROM donor_company_touches WHERE token_hash=$1", HashToken(first.Token)).Scan(&stored); err != nil ||
		stored != "GLOBEX" {
		t.Fatalf("after last-touch = %q; want GLOBEX", stored)
	}

	// The retention pass purges expired touches, which can never bind.
	expired, err := s.CaptureCompanyCode(ctx, "OSCORP", "", now.Add(-2*referralWindow))
	if err != nil {
		t.Fatal(err)
	}
	retention := &TouchRetention{DB: as(t, dbroles.ProjectionWorker)}
	if _, err := retention.PurgeNext(ctx, now); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_company_touches WHERE token_hash=$1", HashToken(expired.Token)).Scan(&left); err != nil ||
		left != 0 {
		t.Fatalf("expired company touch kept: %d %v", left, err)
	}
}

func TestCompanyAttributionWaitsForAccountProof(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	donors := DonorService{DB: as(t, dbroles.API), Origin: "https://donor.example.org"}
	partnerID := approvedPartner(t, pool, "Globex", "GLOBEX-"+strings.ToUpper(mustNewID(t).String()[:6]), 0, 0)
	var code string
	if err := pool.QueryRow(ctx, "SELECT company_code FROM donor_partner_signups WHERE id=$1", partnerID).Scan(&code); err != nil {
		t.Fatal(err)
	}
	capture, err := donors.CaptureCompanyCode(ctx, code, "", time.Now().UTC())
	if err != nil || capture.Token == "" {
		t.Fatal(capture, err)
	}
	cleanup(t, "DELETE FROM donor_company_touches WHERE token_hash=$1", HashToken(capture.Token))

	checkout, request, donorID, _ := checkoutFixture(t)
	cleanup(t, "UPDATE donors SET company_code=NULL WHERE id=$1", donorID)
	if _, err := checkout.BeginCheckout(ctx, request); err != nil {
		t.Fatal(err)
	}
	account := AccountService{DB: as(t, dbroles.API)}
	if _, err := account.ClaimAccount(ctx, "invalid-proof", "", Client{CompanyToken: capture.Token}); !errors.Is(err, ErrDenied) {
		t.Fatalf("invalid account proof: %v", err)
	}
	var assigned bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM donor_partner_employees WHERE donor_id=$1)
		OR EXISTS(SELECT 1 FROM donors WHERE id=$1 AND company_code IS NOT NULL)`, donorID).Scan(&assigned); err != nil || assigned {
		t.Fatalf("guest checkout changed employer without account proof: %v %v", assigned, err)
	}
	proof := link(t, pool, donorID, time.Now().UTC())
	if _, err := account.ClaimAccount(ctx, proof, "", Client{CompanyToken: capture.Token}); err != nil {
		t.Fatal(err)
	}
	var linked pgtype.UUID
	var bound string
	if err := pool.QueryRow(ctx, `SELECT e.partner_id, d.company_code FROM donor_partner_employees e
		JOIN donors d ON d.id=e.donor_id WHERE e.donor_id=$1`, donorID).Scan(&linked, &bound); err != nil ||
		linked != partnerID || bound != code {
		t.Fatalf("verified claim linked %v with code %q (%v), want %v and %q", linked, bound, err, partnerID, code)
	}

	other, err := donors.CaptureCompanyCode(ctx, "INITECH", "", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	cleanup(t, "DELETE FROM donor_company_touches WHERE token_hash=$1", HashToken(other.Token))
	if _, err := bindCompanyCode(ctx, queries.New(pool), donorID, other.Token, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT company_code FROM donors WHERE id=$1", donorID).Scan(&bound); err != nil || bound != code {
		t.Fatalf("a later code replaced the donor's: %q %v", bound, err)
	}
}

func TestAppBindsItsCompanyTouchAfterSignIn(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	donors := DonorService{DB: as(t, dbroles.API), Origin: "https://donor.example.org"}
	partnerID := approvedPartner(t, pool, "Hooli", "HOOLI-"+strings.ToUpper(mustNewID(t).String()[:6]), 0, 0)
	var code string
	if err := pool.QueryRow(ctx, "SELECT company_code FROM donor_partner_signups WHERE id=$1", partnerID).Scan(&code); err != nil {
		t.Fatal(err)
	}
	capture, err := donors.CaptureCompanyCode(ctx, code, "", time.Now().UTC())
	if err != nil || capture.Token == "" {
		t.Fatal(capture, err)
	}
	cleanup(t, "DELETE FROM donor_company_touches WHERE token_hash=$1", HashToken(capture.Token))
	id, _ := donor(t, pool)
	cleanup(t, "DELETE FROM donor_partner_employees WHERE donor_id=$1", id)
	token := claimSession(t, pool, id, appClient).Access

	if _, err := donors.BindCompanyTouch(ctx, strings.Repeat("x", 43), capture.Token); !errors.Is(err, ErrDenied) {
		t.Fatalf("bound without a session: %v", err)
	}
	if bound, err := donors.BindCompanyTouch(ctx, token, strings.Repeat("u", 43)); err != nil || bound {
		t.Fatalf("an unknown token bound: %v %v", bound, err)
	}
	if bound, err := donors.BindCompanyTouch(ctx, token, capture.Token); err != nil || !bound {
		t.Fatalf("the captured touch did not bind: %v %v", bound, err)
	}
	var linked pgtype.UUID
	var bound string
	if err := pool.QueryRow(ctx, `SELECT e.partner_id, d.company_code FROM donor_partner_employees e
		JOIN donors d ON d.id=e.donor_id WHERE e.donor_id=$1`, id).Scan(&linked, &bound); err != nil ||
		linked != partnerID || bound != code {
		t.Fatalf("linked %v with code %q (%v), want %v and %q", linked, bound, err, partnerID, code)
	}
}
