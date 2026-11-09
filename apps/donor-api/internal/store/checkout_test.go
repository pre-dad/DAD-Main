package store

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type checkoutCall struct {
	params []byte
	key    string
}

type checkoutGateway struct {
	mu     sync.Mutex
	calls  []checkoutCall
	fail   bool
	remote *payments.RemoteCheckout
	pool   *pgxpool.Pool
	// expired lists the sessions ExpireCheckout was asked to close. It reports
	// expireStatus, "expired" when unset, or expireErr if that is set. Each
	// beforeExpire hook runs first, once, as the processor call begins.
	expired      []string
	expireStatus string
	expireErr    error
	beforeExpire func()
}

func (g *checkoutGateway) ExpireCheckout(_ context.Context, session, _ string) (string, error) {
	g.mu.Lock()
	hook := g.beforeExpire
	g.beforeExpire = nil
	g.mu.Unlock()
	if hook != nil {
		hook()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.expired = append(g.expired, session)
	if g.expireErr != nil {
		return "", g.expireErr
	}
	return cmp.Or(g.expireStatus, "expired"), nil
}

func (g *checkoutGateway) CreateCheckout(ctx context.Context, params []byte, key string) (payments.RemoteCheckout, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, checkoutCall{append([]byte(nil), params...), key})
	// A separate connection must see the immutable request before the network call.
	var exists bool
	if err := g.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM donor_stripe_checkouts WHERE id::text=$1)", key[len("dad-checkout-"):]).Scan(&exists); err != nil ||
		!exists {
		return payments.RemoteCheckout{}, errors.New("uncommitted checkout")
	}
	if g.fail {
		g.fail = false
		return payments.RemoteCheckout{}, errors.New("lost provider acknowledgement")
	}
	if g.remote != nil {
		return *g.remote, nil
	}
	live := false
	return payments.RemoteCheckout{ID: "cs_test_" + key, URL: "https://checkout.stripe.com/c/" + key, Livemode: &live}, nil
}

func checkoutFixture(t *testing.T) (CheckoutService, payments.CheckoutRequest, pgtype.UUID, *checkoutGateway) {
	t.Helper()
	pool := database(t)
	id, email := donor(t, pool)
	t.Cleanup(func() {
		ctx := context.Background()
		for _, sql := range []string{
			"DELETE FROM donor_recurring_reservations WHERE donor_id=$1",
			"DELETE FROM donor_stripe_checkouts WHERE checkout_id IN (SELECT id FROM donor_checkouts WHERE donor_id=$1)",
			"DELETE FROM donor_checkouts WHERE donor_id=$1",
			"DELETE FROM donor_referral_touches WHERE referrer_id=$1",
		} {
			if _, err := pool.Exec(ctx, sql, id); err != nil {
				t.Error(err)
			}
		}
	})
	gateway := &checkoutGateway{pool: pool}
	return CheckoutService{
		DB:      as(t, dbroles.API),
		Gateway: gateway,
		Origin:  "http://localhost:3000",
	}, payments.CheckoutRequest{
		Session:   sessionFor(t, pool, id),
		RequestID: testID(t),
		Email:     email,
		Amount:    3000,
		Frequency: "MONTHLY",
	}, id, gateway
}

func TestCheckoutRetriesAndReservations(t *testing.T) {
	service, request, _, gateway := checkoutFixture(t)
	ctx := context.Background()
	gateway.fail = true
	if _, err := service.BeginCheckout(ctx, request); !errors.Is(err, payments.ErrProcessorUnavailable) {
		t.Fatal("lost response not retryable")
	}
	service.Origin = "https://changed.example.org"
	result, err := service.BeginCheckout(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(gateway.calls) != 2 || !bytes.Equal(gateway.calls[0].params, gateway.calls[1].params) ||
		gateway.calls[0].key != gateway.calls[1].key {
		t.Fatal("retry parameters or key changed")
	}
	var params map[string]any
	if err = json.Unmarshal(gateway.calls[0].params, &params); err != nil {
		t.Fatal(err)
	}
	if params["customer"] != nil || params["customer_email"] != request.Email || params["mode"] != "subscription" ||
		params["payment_method_types"] != nil ||
		params["allowed_payment_method_types"].([]any)[0] != "card" {
		t.Fatalf("wrong guest parameters: %v", params)
	}
	// Stripe accepts an expiry 30 minutes to 24 hours ahead; a shorter one frees the reservation sooner.
	if expires := time.Unix(int64(params["expires_at"].(float64)), 0); expires.Before(time.Now().Add(55*time.Minute)) ||
		expires.After(time.Now().Add(65*time.Minute)) {
		t.Fatalf("checkout expires at %v, want about an hour from now", expires)
	}
	repeated, err := service.BeginCheckout(ctx, request)
	if err != nil || repeated != result || len(gateway.calls) != 2 {
		t.Fatal("completed creation not reused")
	}
	changed := request
	changed.Amount = 9000
	if _, err = service.BeginCheckout(ctx, changed); !errors.Is(err, payments.ErrCheckoutConflict) {
		t.Fatal("changed request reused key")
	}
	changed = request
	changed.RequestID = testID(t)
	if _, err = service.BeginCheckout(ctx, changed); !errors.Is(err, payments.ErrCheckoutConflict) {
		t.Fatal("a checkout begun moments ago was expired")
	}
	// Once abandoned, a restart replaces the session rather than being refused
	// or handed the earlier session's URL.
	mustExec(t, database(t), "UPDATE donor_stripe_checkouts SET created_at=now()-interval '1 minute' WHERE id=$1", request.RequestID)
	replacement, err := service.BeginCheckout(ctx, changed)
	if err != nil || replacement == result || len(gateway.expired) != 1 || len(gateway.calls) != 3 {
		t.Fatalf("restart did not replace the session: %+v %v, expired %v", replacement, err, gateway.expired)
	}
}

func TestCheckoutConcurrentMonthlyReservation(t *testing.T) {
	service, request, _, gateway := checkoutFixture(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	results := make(chan error, 2)
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		r := request
		r.RequestID = testID(t)
		wg.Add(1)
		go func() { defer wg.Done(); <-start; _, err := service.BeginCheckout(ctx, r); results <- err }()
	}
	close(start)
	wg.Wait()
	close(results)
	success, conflicts := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, payments.ErrCheckoutConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflicts != 1 || len(gateway.calls) != 1 {
		t.Fatal("concurrent request escaped monthly reservation")
	}
}

func TestCheckoutUncertainWindowAndDisabledDonor(t *testing.T) {
	service, request, id, gateway := checkoutFixture(t)
	pool := database(t)
	ctx := context.Background()
	gateway.fail = true
	if _, err := service.BeginCheckout(ctx, request); err == nil {
		t.Fatal("expected lost response")
	}
	if _, err := pool.Exec(ctx, "UPDATE donor_stripe_checkouts SET created_at=$2 WHERE id=$1", request.RequestID, time.Now().Add(-23*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.BeginCheckout(ctx, request); !errors.Is(err, payments.ErrCheckoutConflict) {
		t.Fatal("old uncertain request retried")
	}
	if len(gateway.calls) != 1 {
		t.Fatal("old request reached provider")
	}
	if _, err := pool.Exec(ctx, "UPDATE donors SET disabled=true WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	request.RequestID = testID(t)
	if _, err := service.BeginCheckout(ctx, request); !errors.Is(err, ErrDenied) {
		t.Fatal("disabled donor checked out")
	}
}

func TestCheckoutAttributionSnapshotAndSelfReferral(t *testing.T) {
	service, request, id, gateway := checkoutFixture(t)
	pool := database(t)
	ctx := context.Background()
	referrer, _ := donor(t, pool)
	// Registered after referrer cleanup so dependent rows disappear first.
	t.Cleanup(func() {
		for _, sql := range []string{"DELETE FROM donor_recurring_reservations WHERE donor_id=$1", "DELETE FROM donor_stripe_checkouts WHERE checkout_id IN (SELECT id FROM donor_checkouts WHERE donor_id=$1)", "DELETE FROM donor_checkouts WHERE donor_id=$1"} {
			if _, err := pool.Exec(ctx, sql, id); err != nil {
				t.Error(err)
			}
		}
		if _, err := pool.Exec(ctx, "DELETE FROM donor_referral_touches WHERE referrer_id=$1", referrer); err != nil {
			t.Error(err)
		}
	})
	token := "test-visitor"
	touch := testID(t)
	if _, err := pool.Exec(ctx, `INSERT INTO donor_referral_touches (id,token_hash,referrer_id,kind,touched_at,expires_at) VALUES ($1,$2,$3,'IRL',$4,$5)`, touch, HashToken(token), referrer, time.Now(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	request.Visitor = &token
	gateway.fail = true
	if _, err := service.BeginCheckout(ctx, request); err == nil {
		t.Fatal("expected lost response")
	}
	if _, err := pool.Exec(ctx, "UPDATE donor_referral_touches SET expires_at=$2 WHERE id=$1", touch, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.BeginCheckout(ctx, request); err != nil {
		t.Fatal(err)
	}
	var params map[string]any
	if err := json.Unmarshal(gateway.calls[1].params, &params); err != nil {
		t.Fatal(err)
	}
	metadata := params["subscription_data"].(map[string]any)["metadata"].(map[string]any)
	if metadata["dad_referrer_id"] != referrer.String() || metadata["dad_referral_kind"] != "IRL" {
		t.Fatal("snapshot referral was lost on retry")
	}
	if _, err := pool.Exec(ctx, "UPDATE donor_referral_touches SET referrer_id=$2,expires_at=$3 WHERE id=$1", touch, id, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Release the prior monthly reservation so a fresh checkout can proceed; the
	// point of this call is that a self-referring touch is not attributed.
	if _, err := pool.Exec(ctx, "DELETE FROM donor_recurring_reservations WHERE donor_id=$1", id); err != nil {
		t.Fatal(err)
	}
	request.RequestID = testID(t)
	if _, err := service.BeginCheckout(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(gateway.calls[2].params, &params); err != nil {
		t.Fatal(err)
	}
	if params["metadata"].(map[string]any)["dad_referrer_id"] != nil {
		t.Fatal("self referral accepted")
	}
}

func TestUnexpectedProviderCheckoutIsNotAcknowledged(t *testing.T) {
	service, request, _, gateway := checkoutFixture(t)
	pool := database(t)
	ctx := context.Background()
	live := true
	gateway.remote = &payments.RemoteCheckout{ID: "cs_live_invalid", URL: "https://checkout.stripe.com/c/test", Livemode: &live}
	if _, err := service.BeginCheckout(ctx, request); !errors.Is(err, payments.ErrProcessorUnavailable) {
		t.Fatal("live response accepted")
	}
	attempt, err := queries.New(pool).StripeCheckout(ctx, request.RequestID)
	if err != nil || attempt.HostedUrl.Valid || attempt.StripeSessionID.Valid || attempt.Status != "PENDING" {
		t.Fatal("untrusted checkout acknowledged")
	}
}

func TestConcurrentRetrySharesOneDraftAndProviderKey(t *testing.T) {
	service, request, id, gateway := checkoutFixture(t)
	pool := database(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	results := make(chan payments.CheckoutResult, 4)
	failures := make(chan error, 4)
	start := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			value, err := service.BeginCheckout(ctx, request)
			results <- value
			failures <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first payments.CheckoutResult
	for result := range results {
		if first.CheckoutID == "" {
			first = result
		} else if result != first {
			t.Fatal("concurrent retry split checkout")
		}
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_checkouts WHERE donor_id=$1", id).Scan(&count); err != nil || count != 1 {
		t.Fatal("concurrent retry created duplicate drafts")
	}
	for _, call := range gateway.calls {
		if call.key != gateway.calls[0].key || !bytes.Equal(call.params, gateway.calls[0].params) {
			t.Fatal("concurrent retry changed Stripe request")
		}
	}
}

func TestCheckoutRequiresTheVerifiedSessionOwnersEmail(t *testing.T) {
	for _, kind := range []string{"anonymous", "invalid", "expired", "revoked", "unclaimed", "disabled", "other donor", "unknown email", "staff email"} {
		t.Run(kind, func(t *testing.T) {
			service, request, id, gateway := checkoutFixture(t)
			pool := database(t)
			switch kind {
			case "anonymous":
				request.Session = ""
			case "invalid":
				request.Session = "invalid-session"
			case "expired":
				mustExec(t, pool, "UPDATE donor_sessions SET expires_at=now()-interval '1 minute' WHERE donor_id=$1", id)
			case "revoked":
				mustExec(t, pool, "UPDATE donor_sessions SET revoked_at=now() WHERE donor_id=$1", id)
			case "unclaimed":
				mustExec(t, pool, "UPDATE donors SET claimed_at=NULL WHERE id=$1", id)
			case "disabled":
				mustExec(t, pool, "UPDATE donors SET disabled=true WHERE id=$1", id)
			case "other donor":
				_, request.Email = donor(t, pool)
			case "unknown email":
				request.Email = testID(t).String() + "@example.org"
			case "staff email":
				request.Email = "staff@example.org"
			}
			if _, err := service.BeginCheckout(context.Background(), request); !errors.Is(err, ErrDenied) {
				t.Fatalf("%s checkout: %v; want ErrDenied", kind, err)
			}
			var count int
			if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM donor_checkouts WHERE donor_id=$1", id).Scan(&count); err != nil ||
				count != 0 ||
				len(gateway.calls) != 0 {
				t.Fatalf("unauthorized checkout left %d rows and %d provider calls: %v", count, len(gateway.calls), err)
			}
		})
	}
}

func TestCheckoutRetryBelongsToTheDonorNotAReusedEmail(t *testing.T) {
	service, request, id, gateway := checkoutFixture(t)
	ctx := context.Background()
	if _, err := service.BeginCheckout(ctx, request); err != nil {
		t.Fatal(err)
	}
	pool := database(t)
	other, _ := donor(t, pool)
	token := sessionFor(t, pool, other)
	mustExec(t, pool, "UPDATE donors SET email=$1 WHERE id=$2", testID(t).String()+"@example.org", id)
	mustExec(t, pool, "UPDATE donors SET email=$1 WHERE id=$2", request.Email, other)
	request.Session = token
	if _, err := service.BeginCheckout(ctx, request); !errors.Is(err, ErrDenied) {
		t.Fatalf("a new owner of an address retrieved the previous donor's checkout: %v", err)
	}
	if len(gateway.calls) != 1 {
		t.Fatal("unauthorized retry reached the processor")
	}
}
