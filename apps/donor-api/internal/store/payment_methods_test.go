package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

type paymentUpdateFixture struct {
	f        *workerFixture
	id       pgtype.UUID
	checkout string
	session  string
	remote   map[string]any
	setup    map[string]any
	sub      map[string]any
}

func newPaymentUpdateFixture(t *testing.T) paymentUpdateFixture {
	t.Helper()
	f := newWorkerFixture(t, "MONTHLY")
	result := f.checkout(t)
	ctx := context.Background()
	id := testID(t)
	session := "cs_test_setup_" + id.String()
	setupID := "seti_" + id.String()
	var attempt pgtype.UUID
	if err := f.db.QueryRow(ctx, "UPDATE donor_stripe_checkouts SET stripe_subscription_id=$2 WHERE checkout_id=$1 RETURNING id", result.CheckoutID, "sub_"+result.CheckoutID).Scan(&attempt); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(ctx, `INSERT INTO donor_payment_updates(id,donor_id,attempt_id,customer_id,request_params,stripe_session_id,hosted_url,status) VALUES($1,$2,$3,'cus_fixture','{}',$4,'https://checkout.stripe.com/c/test','OPEN')`, id, f.donorID, attempt, session); err != nil {
		t.Fatal(err)
	}
	remote := map[string]any{
		"id":           session,
		"livemode":     false,
		"mode":         "setup",
		"customer":     "cus_fixture",
		"status":       "complete",
		"setup_intent": setupID,
		"metadata":     map[string]any{"dad_payment_update_id": id.String()},
	}
	setup := map[string]any{
		"id":                   setupID,
		"livemode":             false,
		"customer":             "cus_fixture",
		"status":               "succeeded",
		"usage":                "off_session",
		"payment_method_types": []string{"card"},
		"payment_method":       "pm_fixture",
		"metadata":             map[string]any{"dad_payment_update_id": id.String()},
	}
	sub := subscription(result.CheckoutID, time.Now().Add(-24*time.Hour))
	p := paymentUpdateFixture{f: f, id: id, checkout: result.CheckoutID, session: session, remote: remote, setup: setup, sub: sub}
	p.publish(t)
	return p
}

func (p paymentUpdateFixture) publish(t *testing.T) {
	p.f.gateway.sessions[p.session] = paymentObject(t, p.remote)
	p.f.gateway.setups[p.setup["id"].(string)] = paymentObject(t, p.setup)
	p.f.gateway.subscriptions["sub_"+p.checkout] = paymentObject(t, p.sub)
}

func (p paymentUpdateFixture) assertState(t *testing.T, status string) {
	t.Helper()
	var got string
	var completed, url bool
	var gifts int
	if err := p.f.db.QueryRow(context.Background(), "SELECT status,completed_at IS NOT NULL,hosted_url IS NOT NULL FROM donor_payment_updates WHERE id=$1", p.id).Scan(&got, &completed, &url); err != nil {
		t.Fatal(err)
	}
	if got != status || completed != (status == "APPLIED") || url == terminalPaymentUpdate(status) {
		t.Fatalf("unexpected update state %s completed=%v url=%v", got, completed, url)
	}
	if err := p.f.db.QueryRow(context.Background(), "SELECT count(*) FROM donor_gifts WHERE donor_id=$1", p.f.donorID).Scan(&gifts); err != nil ||
		gifts != 0 {
		t.Fatal("card setup generated a gift", err)
	}
}

func TestPaymentUpdateRecoversLostProviderAcknowledgement(t *testing.T) {
	p := newPaymentUpdateFixture(t)
	p.f.gateway.loseAcknowledgement = true
	event := p.f.receive(t, "checkout.session.completed", p.remote)
	p.f.process(t)
	assertEventProcessed(t, p.f, event, false)
	p.assertState(t, "OPEN")
	p.f.retryNow(t)
	p.f.process(t)
	assertEventProcessed(t, p.f, event, true)
	p.assertState(t, "APPLIED")
	if len(p.f.gateway.updateKeys) != 1 || p.f.gateway.updateKeys[0] != "dad-payment-apply-"+p.id.String() {
		t.Fatal("card mutation repeated or key changed")
	}
	event = p.f.receive(t, "checkout.session.completed", p.remote)
	p.f.process(t)
	assertEventProcessed(t, p.f, event, true)
	if len(p.f.gateway.updateKeys) != 1 {
		t.Fatal("duplicate event changed the card")
	}
}

func TestPaymentUpdateRequiresVerifiedOwnershipAndCardSetup(t *testing.T) {
	cases := map[string]func(*testing.T, *paymentUpdateFixture){
		"session owner": func(_ *testing.T, p *paymentUpdateFixture) { p.remote["customer"] = "cus_other" },
		"live setup":    func(_ *testing.T, p *paymentUpdateFixture) { p.setup["livemode"] = true },
		"setup owner": func(t *testing.T, p *paymentUpdateFixture) {
			p.setup["metadata"] = map[string]any{"dad_payment_update_id": testID(t).String()}
		},
		"wrong usage": func(_ *testing.T, p *paymentUpdateFixture) { p.setup["usage"] = "on_session" },
		"not card": func(_ *testing.T, p *paymentUpdateFixture) {
			p.setup["payment_method_types"] = []string{"us_bank_account"}
		},
		"not succeeded":      func(_ *testing.T, p *paymentUpdateFixture) { p.setup["status"] = "requires_action" },
		"subscription owner": func(_ *testing.T, p *paymentUpdateFixture) { p.sub["customer"] = "cus_other" },
		"not active":         func(_ *testing.T, p *paymentUpdateFixture) { p.sub["status"] = "incomplete" },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			p := newPaymentUpdateFixture(t)
			edit(t, &p)
			p.publish(t)
			event := p.f.receive(t, "checkout.session.completed", p.remote)
			p.f.process(t)
			assertEventProcessed(t, p.f, event, false)
			p.assertState(t, "OPEN")
			if len(p.f.gateway.updateKeys) != 0 {
				t.Fatal("unverified setup changed card")
			}
		})
	}
}

func TestPaymentUpdateExpiryCancellationAndIdempotencyAge(t *testing.T) {
	for _, mode := range []string{"expired", "canceled", "old", "old already applied"} {
		t.Run(mode, func(t *testing.T) {
			p := newPaymentUpdateFixture(t)
			kind := "checkout.session.completed"
			want := "OPEN"
			processed := true
			switch mode {
			case "expired":
				p.remote["status"] = "expired"
				kind = "checkout.session.expired"
				want = "EXPIRED"
			case "canceled":
				p.sub["status"] = "canceled"
				want = "CANCELLED"
			default:
				if _, err := p.f.db.Exec(context.Background(), "UPDATE donor_payment_updates SET created_at=$2 WHERE id=$1", p.id, time.Now().Add(-24*time.Hour)); err != nil {
					t.Fatal(err)
				}
				if mode == "old already applied" {
					p.sub["default_payment_method"] = "pm_fixture"
					want = "APPLIED"
				} else {
					processed = false
				}
			}
			p.publish(t)
			event := p.f.receive(t, kind, p.remote)
			p.f.process(t)
			assertEventProcessed(t, p.f, event, processed)
			p.assertState(t, want)
			if len(p.f.gateway.updateKeys) != 0 {
				t.Fatal("unsafe or unnecessary processor mutation")
			}
		})
	}
}
