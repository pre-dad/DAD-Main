package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"dad/donor-api/internal/credentials"
	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/dbroles/dbrolestest"
	"dad/donor-api/internal/httpapi"
	"dad/donor-api/internal/mail"
	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/store"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stripe/stripe-go/v87"
)

type journeyGateway struct{}

func (journeyGateway) ExpireCheckout(context.Context, string, string) (string, error) {
	return "expired", nil
}

func (journeyGateway) CreateCheckout(_ context.Context, _ []byte, key string) (payments.RemoteCheckout, error) {
	live := false
	return payments.RemoteCheckout{
		ID:       "cs_test_" + strings.TrimPrefix(key, "dad-checkout-"),
		URL:      "https://checkout.stripe.com/c/test",
		Livemode: &live,
	}, nil
}

// journeyCharges reports each payment intent's card charge as succeeding at a fixed time.
type journeyCharges struct{ at time.Time }

func (g journeyCharges) RetrievePaymentIntent(_ context.Context, id string) (payments.Object, error) {
	raw, err := json.Marshal(
		map[string]any{
			"id":       id,
			"status":   "succeeded",
			"livemode": false,
			"latest_charge": map[string]any{
				"id":             "ch_" + id,
				"payment_intent": id,
				"status":         "succeeded",
				"paid":           true,
				"livemode":       false,
				"created":        g.at.Unix(),
			},
		},
	)
	if err != nil {
		return nil, err
	}
	return payments.DecodeObject(raw)
}

func (journeyCharges) RetrievePaymentMethod(context.Context, string) (payments.Object, error) {
	return nil, payments.ErrProcessorUnavailable
}

func (journeyCharges) RetrieveRefund(context.Context, string) (payments.Object, error) {
	return nil, payments.ErrProcessorUnavailable
}

func (journeyCharges) RetrieveSubscription(context.Context, string) (payments.Object, error) {
	return nil, payments.ErrProcessorUnavailable
}

type journeyMail struct{ message mail.Message }

func (m *journeyMail) Send(_ context.Context, message mail.Message) error {
	m.message = message
	return nil
}

func TestHTTPReferralVerificationCheckoutWebhookAndReceipt(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("TEST_DATABASE_URL")
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil || dsn == "" ||
		(config.ConnConfig.Database != "donor_go" && !strings.HasPrefix(config.ConnConfig.Database, "donor_go_")) {
		t.Fatal("dedicated migrated donor_go database required")
	}
	pool, err := store.Open(ctx, dsn, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	owner, err := store.NewID()
	if err != nil {
		t.Fatal(err)
	}
	requestID, err := store.NewID()
	if err != nil {
		t.Fatal(err)
	}
	ownerEmail := "journey-owner-" + owner.String() + "@example.org"
	guestEmail := "journey-guest-" + owner.String() + "@example.org"
	eventID := "evt_journey_" + owner.String()
	defer func() {
		// Scope every cleanup to this journey's generated identities, including the
		// new donor whose identity is created by signup before checkout.
		for _, sql := range []string{
			"DELETE FROM donor_messages WHERE donor_id IN (SELECT id FROM donors WHERE email=ANY($1::text[]))",
			"DELETE FROM donor_login_emails WHERE donor_id IN (SELECT id FROM donors WHERE email=ANY($1::text[]))",
			"DELETE FROM donor_gifts WHERE donor_id IN (SELECT id FROM donors WHERE email=ANY($1::text[]))",
			"DELETE FROM donor_billing_observations WHERE period_id IN (SELECT id FROM donor_billing_periods WHERE donor_id IN (SELECT id FROM donors WHERE email=ANY($1::text[])))",
			"DELETE FROM donor_billing_periods WHERE donor_id IN (SELECT id FROM donors WHERE email=ANY($1::text[]))",
			"DELETE FROM donor_recurring_reservations WHERE donor_id IN (SELECT id FROM donors WHERE email=ANY($1::text[]))",
			"DELETE FROM donor_subscription_leases WHERE donor_id IN (SELECT id FROM donors WHERE email=ANY($1::text[]))",
			"DELETE FROM donor_subscriptions WHERE donor_id IN (SELECT id FROM donors WHERE email=ANY($1::text[]))",
			"DELETE FROM donor_stripe_checkouts WHERE checkout_id IN (SELECT id FROM donor_checkouts WHERE donor_id IN (SELECT id FROM donors WHERE email=ANY($1::text[])))",
			"DELETE FROM donor_checkouts WHERE donor_id IN (SELECT id FROM donors WHERE email=ANY($1::text[]))",
			"DELETE FROM donor_metric_projections WHERE donor_id IN (SELECT id FROM donors WHERE email=ANY($1::text[]))",
			"DELETE FROM donor_referral_touches WHERE referrer_id IN (SELECT id FROM donors WHERE email=ANY($1::text[]))",
			"DELETE FROM donor_preference_events WHERE donor_id IN (SELECT id FROM donors WHERE email=ANY($1::text[]))",
			"DELETE FROM donor_preferences WHERE donor_id IN (SELECT id FROM donors WHERE email=ANY($1::text[]))",
			"DELETE FROM donor_session_events WHERE donor_id IN (SELECT id FROM donors WHERE email=ANY($1::text[]))",
			"DELETE FROM donor_sessions WHERE donor_id IN (SELECT id FROM donors WHERE email=ANY($1::text[]))",
			"DELETE FROM donor_login_links WHERE donor_id IN (SELECT id FROM donors WHERE email=ANY($1::text[]))",
			"DELETE FROM donors WHERE email=ANY($1::text[])",
		} {
			if _, err := pool.Exec(ctx, sql, []string{ownerEmail, guestEmail}); err != nil {
				t.Error(err)
			}
		}
		if _, err := pool.Exec(ctx, "DELETE FROM donor_stripe_events WHERE stripe_event_id=$1", eventID); err != nil {
			t.Error(err)
		}
	}()
	proof := store.HashToken("public-test-proof-" + owner.String())
	if _, err = pool.Exec(ctx, "INSERT INTO donors(id,email) VALUES($1,$2)", owner, ownerEmail); err != nil {
		t.Fatal(err)
	}
	proofID, _ := store.NewID()
	if _, err = pool.Exec(ctx, "INSERT INTO donor_login_links(id,donor_id,token_hash,expires_at) VALUES($1,$2,$3,$4)", proofID, owner, store.HashToken(proof), time.Now().Add(15*time.Minute)); err != nil {
		t.Fatal(err)
	}
	const origin = "https://donor.example.org"
	const secret = "whsec_public_journey_fixture"
	api := dbrolestest.Open(t, dbroles.API)
	services := httpapi.Services{
		Origin:          origin,
		EmailConfigured: true,
		Account:         store.AccountService{DB: api},
		Passwords:       store.PasswordService{DB: api},
		Donor:           store.DonorService{DB: api, Origin: origin},
		Checkout:        store.CheckoutService{DB: api, Origin: origin, Gateway: journeyGateway{}},
		Dashboard:       store.DashboardReader{DB: api},
		Webhook:         payments.Receiver{Secret: secret, Inbox: store.StripeInbox{DB: api}},
	}
	server := httptest.NewTLSServer(httpapi.NewServer("", services).Handler)
	defer server.Close()
	browser := func() *http.Client {
		client := server.Client()
		copy := *client
		copy.Jar, _ = cookiejar.New(nil)
		copy.Timeout = 10 * time.Second
		return &copy
	}
	referrer, guest := browser(), browser()
	call := func(client *http.Client, method, path string, body any, headers map[string]string, status int) []byte {
		t.Helper()
		var data []byte
		if raw, ok := body.([]byte); ok {
			data = raw
		} else if body != nil {
			var err error
			data, err = json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
		}
		req, err := http.NewRequest(method, server.URL+path, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", origin)
		req.Header.Set("Content-Type", "application/json")
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		result, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != status {
			t.Fatalf("%s %s: got %d want %d: %s", method, path, response.StatusCode, status, result)
		}
		if response.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("private response cached")
		}
		return result
	}
	call(referrer, "POST", "/donor/claim", map[string]string{"token": proof}, nil, 200)
	call(guest, "GET", "/donor/me/profile", nil, nil, 401)
	call(
		referrer,
		"POST",
		"/donor/me/sharing",
		map[string]string{"public_name": "Friendly donor"},
		map[string]string{"Origin": "https://other.example"},
		403,
	)
	body := call(referrer, "POST", "/donor/me/sharing", map[string]string{"public_name": "Friendly donor"}, nil, 200)
	var sharing store.Sharing
	if err = json.Unmarshal(body, &sharing); err != nil || sharing.Link == nil {
		t.Fatal(err)
	}
	call(referrer, "GET", "/donor/me/referral-code.svg", nil, nil, 200)
	path := strings.TrimPrefix(*sharing.Link, origin+"/r/")
	body = call(guest, "POST", "/donor/referrals/"+path, map[string]string{"kind": "IRL"}, nil, 200)
	if !bytes.Contains(body, []byte(`"captured":true`)) || bytes.Contains(body, []byte(ownerEmail)) {
		t.Fatal("referral capture privacy or state mismatch")
	}
	call(
		referrer,
		"POST",
		"/donor/me/preferences",
		map[string]any{
			"version":                 0,
			"referral_emails":         true,
			"marketing_emails":        false,
			"push_referral_converted": true,
			"push_streak_at_risk":     true,
			"push_streak_milestone":   true,
		},
		nil,
		200,
	)
	call(
		referrer,
		"POST",
		"/donor/me/preferences",
		map[string]any{
			"version":                 0,
			"referral_emails":         false,
			"marketing_emails":        false,
			"push_referral_converted": true,
			"push_streak_at_risk":     true,
			"push_streak_milestone":   true,
		},
		nil,
		409,
	)
	checkoutBody := map[string]any{"email": guestEmail, "amount_minor": 3000, "frequency": "MONTHLY"}
	call(guest, "POST", "/donor/checkouts", checkoutBody, map[string]string{"Idempotency-Key": requestID.String()}, 401)
	call(
		guest,
		"POST",
		"/donor/signup",
		map[string]string{"name": "Verified Donor", "email": guestEmail, "zip": "10001", "city": "New York"},
		nil,
		200,
	)
	transport := &journeyMail{}
	keys, err := credentials.ParseKeyring("MDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDA=")
	if err != nil {
		t.Fatal(err)
	}
	email := store.LoginEmailWorker{
		DB:        dbrolestest.Open(t, dbroles.LoginEmailWorker),
		Settings:  mail.Settings{Host: "smtp.fixture", Port: 587, Sender: "sender@example.org", EncryptionKeys: keys},
		Origin:    origin,
		Transport: transport,
	}
	if worked, err := email.DeliverNext(ctx, time.Now().Add(time.Second)); err != nil || !worked {
		t.Fatal(worked, err)
	}
	_, tail, _ := strings.Cut(transport.message.Body, "#token=")
	claim, _, _ := strings.Cut(tail, "\n")
	if transport.message.To != guestEmail || claim == "" {
		t.Fatal("pre-checkout verification email missing")
	}
	claim, _, _ = strings.Cut(claim, "&")
	call(guest, "POST", "/donor/claim", map[string]string{"token": claim}, nil, 200)
	call(guest, "GET", "/donor/me/profile", nil, nil, 200)
	body = call(guest, "POST", "/donor/checkouts", checkoutBody, map[string]string{"Idempotency-Key": requestID.String()}, 201)
	var checkout payments.CheckoutResult
	if err = json.Unmarshal(body, &checkout); err != nil {
		t.Fatal(err)
	}
	duplicate := call(guest, "POST", "/donor/checkouts", checkoutBody, map[string]string{"Idempotency-Key": requestID.String()}, 201)
	if !bytes.Equal(body, duplicate) {
		t.Fatal("checkout retry changed response")
	}
	now := time.Now()
	invoiceID := "in_" + requestID.String()
	start := now.Add(-24 * time.Hour)
	invoiceObject := map[string]any{
		"id":               invoiceID,
		"status":           "paid",
		"currency":         "usd",
		"amount_paid":      3000,
		"amount_due":       3000,
		"total":            3000,
		"starting_balance": 0,
		"ending_balance":   0,
		"billing_reason":   "subscription_cycle",
		"parent": map[string]any{
			"subscription_details": map[string]any{
				"subscription": "sub_" + checkout.CheckoutID,
				"metadata":     map[string]any{"dad_checkout_id": checkout.CheckoutID},
			},
		},
		"status_transitions": map[string]any{"paid_at": start.Add(time.Hour).Unix()},
		"lines": map[string]any{
			"has_more": false,
			"data": []any{
				map[string]any{
					"amount":   3000,
					"currency": "usd",
					"period":   map[string]any{"start": start.Unix(), "end": start.Add(30 * 24 * time.Hour).Unix()},
					"parent": map[string]any{
						"type":                      "subscription_item_details",
						"subscription_item_details": map[string]any{"subscription": "sub_" + checkout.CheckoutID, "proration": false},
					},
				},
			},
		},
		"payments": map[string]any{
			"has_more": false,
			"data": []any{
				map[string]any{
					"status":      "paid",
					"amount_paid": 3000,
					"currency":    "usd",
					"invoice":     invoiceID,
					"livemode":    false,
					"payment":     map[string]any{"type": "payment_intent", "payment_intent": "pi_" + invoiceID},
				},
			},
		},
	}
	raw, err := json.Marshal(
		map[string]any{
			"id":          eventID,
			"object":      "event",
			"type":        "invoice.payment_succeeded",
			"api_version": payments.APIVersion,
			"livemode":    false,
			"created":     now.Unix(),
			"data": map[string]any{
				"object": invoiceObject,
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	signature := fmt.Sprintf("t=%d,v1=%x", now.Unix(), stripe.ComputeSignature(now, raw, secret))
	for i := 0; i < 2; i++ {
		call(guest, "POST", "/donor/stripe/webhook", raw, map[string]string{"Stripe-Signature": signature}, 200)
	}
	if worked, err := (store.EventWorker{DB: dbrolestest.Open(t, dbroles.StripeWorker), Gateway: journeyCharges{at: now}}).ProcessNext(ctx); err != nil ||
		!worked {
		t.Fatal(worked, err)
	}
	body = call(referrer, "GET", "/donor/me/dashboard", nil, nil, 200)
	var dashboard struct {
		Referrals struct {
			Link, IRL int
			Total     int64 `json:"total_minor"`
		}
	}
	if err = json.Unmarshal(body, &dashboard); err != nil || dashboard.Referrals.Link != 0 || dashboard.Referrals.IRL != 1 ||
		dashboard.Referrals.Total != 3000 {
		t.Fatal("guest credit missing", string(body), err)
	}
	body = call(guest, "GET", "/donor/me/gifts", nil, nil, 200)
	var gifts store.GiftsPage
	if err = json.Unmarshal(body, &gifts); err != nil || len(gifts.Gifts) != 1 || gifts.Gifts[0].AmountMinor != 3000 {
		t.Fatal("gift list mismatch", err)
	}
	giftID := gifts.Gifts[0].ID.String()
	body = call(guest, "GET", "/donor/me/receipts/"+giftID, nil, nil, 200)
	var receipt store.Receipt
	if err = json.Unmarshal(body, &receipt); err != nil || receipt.TaxReceipt || receipt.Gift.Net != 3000 {
		t.Fatal("receipt mismatch", err)
	}
	call(referrer, "GET", "/donor/me/receipts/"+giftID, nil, nil, 404)
	call(guest, "GET", "/donor/me/gifts?offset=-1", nil, nil, 422)
	call(guest, "POST", "/donor/logout-all", nil, nil, 200)
	call(guest, "GET", "/donor/me/gifts", nil, nil, 401)
	var guestID pgtype.UUID
	if err = pool.QueryRow(ctx, "SELECT id FROM donors WHERE email=$1", guestEmail).Scan(&guestID); err != nil {
		t.Fatal(err)
	}
}
