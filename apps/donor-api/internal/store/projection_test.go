package store

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/recognition"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type referralScenario struct {
	Name     string               `json:"name"`
	Op       string               `json:"operation"`
	AsOf     time.Time            `json:"as_of"`
	Gifts    []recognition.Gift   `json:"gifts"`
	Refunds  []recognition.Refund `json:"refunds"`
	Expected json.RawMessage      `json:"expected"`
}

type referralTotals struct {
	Link      int64 `json:"link_count"`
	IRL       int64 `json:"irl_count"`
	LinkMinor int64 `json:"link_minor"`
	IRLMinor  int64 `json:"irl_minor"`
}

// historyFixture writes scenario facts through the production gift, refund and
// projection paths inside a transaction the test rolls back.
type historyFixture struct {
	t       *testing.T
	tx      pgx.Tx
	q       *queries.Queries
	donors  map[string]pgtype.UUID
	objects map[string]string
}

func newHistoryFixture(t *testing.T, tx pgx.Tx) *historyFixture {
	return &historyFixture{t: t, tx: tx, q: queries.New(tx), donors: map[string]pgtype.UUID{}, objects: map[string]string{}}
}

func (h *historyFixture) donor(name string) pgtype.UUID {
	h.t.Helper()
	if id, ok := h.donors[name]; ok {
		return id
	}
	id := testID(h.t)
	if err := h.q.InsertGuest(context.Background(), queries.InsertGuestParams{ID: id, Email: id.String() + "@example.org"}); err != nil {
		h.t.Fatal(err)
	}
	h.donors[name] = id
	return id
}

func (h *historyFixture) gift(g recognition.Gift) {
	h.t.Helper()
	ctx := context.Background()
	donor := h.donor(g.DonorID)
	// Giving is MONTHLY-only (one-time removed, PRD §5.1); the checkout constraint
	// forbids any other frequency. Shared scenarios exercise referral/recognition
	// math, which is frequency-agnostic, so every seeded gift is recorded monthly.
	draft := queries.InsertCheckoutParams{
		ID:          testID(h.t),
		DonorID:     donor,
		AmountMinor: g.Amount,
		Frequency:   "MONTHLY",
		CreatedAt:   timestamp(g.PaidAt),
	}
	// Checkout never captures a donor's own referral touch, so the schema forbids it.
	if c := g.Candidate; c != nil && c.ReferrerID != g.DonorID {
		draft.ReferrerID = h.donor(c.ReferrerID)
		draft.ReferralKind = pgtype.Text{String: c.Kind, Valid: true}
		draft.ReferralExpiresAt = timestamp(c.ExpiresAt)
	}
	if err := h.q.InsertCheckout(ctx, draft); err != nil {
		h.t.Fatal(err)
	}
	checkout, err := h.q.CheckoutDraft(ctx, draft.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	object := "pi_" + testID(h.t).String()
	h.objects[g.ID] = object
	if err = recordGift(ctx, h.q, checkout, paidGift{objectID: object, amount: g.Amount, paid: g.PaidAt, paymentIntent: object}, g.PaidAt); err != nil {
		h.t.Fatal(err)
	}
}

func (h *historyFixture) refund(r recognition.Refund) {
	h.t.Helper()
	ctx := context.Background()
	gift, err := h.q.GiftByStripeObject(ctx, h.objects[r.GiftID])
	if err != nil {
		h.t.Fatal(err)
	}
	event := testID(h.t)
	if _, err = h.tx.Exec(ctx, `INSERT INTO donor_stripe_events (id,stripe_event_id,event_type,occurred_at,data) VALUES ($1,$2,'refund.updated',$3,'{}')`, event, "evt_"+event.String(), r.UpdatedAt); err != nil {
		h.t.Fatal(err)
	}
	current := paymentObject(
		h.t,
		map[string]any{
			"id":             "re_" + event.String(),
			"payment_intent": gift.StripePaymentIntentID.String,
			"amount":         r.Amount,
			"currency":       "usd",
			"status":         strings.ToLower(r.Status),
		},
	)
	if err = recordRefund(ctx, h.q, gift, current, event, pgtype.UUID{}, r.UpdatedAt); err != nil {
		h.t.Fatal(err)
	}
}

func TestSharedReferralScenariosAgainstSQLAggregate(t *testing.T) {
	data, err := os.ReadFile("../../testdata/recognition.json")
	if err != nil {
		t.Fatal(err)
	}
	var scenarios []referralScenario
	if err = json.Unmarshal(data, &scenarios); err != nil {
		t.Fatal(err)
	}
	pool := database(t)
	ran := 0
	for _, c := range scenarios {
		if c.Op != "referrals" {
			continue
		}
		ran++
		t.Run(c.Name, func(t *testing.T) {
			ctx := context.Background()
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			var expected map[string]referralTotals
			if err = json.Unmarshal(c.Expected, &expected); err != nil {
				t.Fatal(err)
			}
			h := newHistoryFixture(t, tx)
			for _, g := range c.Gifts {
				h.gift(g)
			}
			for _, r := range c.Refunds {
				h.refund(r)
			}
			names := make([]string, 0, len(h.donors))
			for name := range h.donors {
				names = append(names, name)
			}
			slices.Sort(names)
			actAs(t, tx, dbroles.ProjectionWorker)
			for _, name := range names {
				if _, err = refreshProjection(ctx, tx, h.donors[name], c.AsOf); err != nil {
					t.Fatal(name, err)
				}
			}
			actAs(t, tx, dbroles.API)
			for _, name := range names {
				want := expected[name]
				board, err := dashboard(ctx, h.q, queries.Donor{ID: h.donors[name]}, c.AsOf)
				if err != nil {
					t.Fatal(err)
				}
				referrals := board.Referrals
				if referrals == nil {
					t.Fatalf("%s: referral totals not ready: %+v", name, board)
				}
				got := referralTotals{
					Link:      referrals.Link,
					IRL:       referrals.IRL,
					LinkMinor: referrals.LinkMinor,
					IRLMinor:  referrals.IRLMinor,
				}
				if got != want {
					t.Fatalf("%s: got %+v want %+v", name, got, want)
				}
			}
		})
	}
	if ran == 0 {
		t.Fatal("missing referral scenarios")
	}
}

// oldReferralAggregate computes a referrer's four referral totals the way the
// pre-denormalization ReferralTotals query (and the 0016 backfill) did: a live
// FILTER aggregate over the referred rows. The denormalized read must equal it
// for every projection ordering; this is the oracle the correctness tests
// compare against.
func oldReferralAggregate(t *testing.T, tx pgx.Tx, referrer pgtype.UUID) referralTotals {
	t.Helper()
	var got referralTotals
	err := tx.QueryRow(context.Background(), `SELECT
count(*) FILTER (WHERE referral_kind='LINK' AND net_minor>0)::bigint,
count(*) FILTER (WHERE referral_kind='IRL' AND net_minor>0)::bigint,
coalesce(sum(net_minor) FILTER (WHERE referral_kind='LINK'),0)::bigint,
coalesce(sum(net_minor) FILTER (WHERE referral_kind='IRL'),0)::bigint
FROM donor_metric_projections WHERE referrer_id=$1`, referrer).Scan(&got.Link, &got.IRL, &got.LinkMinor, &got.IRLMinor)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// dashboardTotals reads a referrer's denormalized dashboard totals as the API
// role sees them, failing if the referrals are withheld.
func dashboardTotals(t *testing.T, tx pgx.Tx, id pgtype.UUID, now time.Time) referralTotals {
	t.Helper()
	board, err := dashboard(context.Background(), queries.New(tx), queries.Donor{ID: id}, now)
	if err != nil {
		t.Fatal(err)
	}
	if board.Referrals == nil {
		t.Fatalf("referral totals withheld for %s: %+v", id, board)
	}
	return referralTotals{
		Link:      board.Referrals.Link,
		IRL:       board.Referrals.IRL,
		LinkMinor: board.Referrals.LinkMinor,
		IRLMinor:  board.Referrals.IRLMinor,
	}
}

// TestDenormalizedReferralTotalsEqualTheOldAggregate pins the denormalized read
// to the OLD FILTER aggregate across the orderings and the refund zero-crossing
// that the shared-scenario test does not exercise. The shared test only ever
// projects the referrer first (alice sorts first); here we also project the
// referrer AFTER its referred donors — the ordering whose dropped incremental
// deltas the recompute backstop must reconcile.
func TestDenormalizedReferralTotalsEqualTheOldAggregate(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	asOf := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	window := asOf.AddDate(0, 1, 0)
	project := func(t *testing.T, tx pgx.Tx, h *historyFixture, name string) {
		t.Helper()
		if _, err := refreshProjection(ctx, tx, h.donor(name), asOf); err != nil {
			t.Fatal(name, err)
		}
	}
	referred := func(ref, kind string) *recognition.Candidate {
		return &recognition.Candidate{ReferrerID: ref, Kind: kind, ExpiresAt: window}
	}

	t.Run("varying counts, referrer projected first", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		h := newHistoryFixture(t, tx)
		h.gift(
			recognition.Gift{
				ID:        "l1",
				DonorID:   "link1",
				Amount:    3000,
				Currency:  "USD",
				Frequency: "MONTHLY",
				PaidAt:    asOf.Add(-72 * time.Hour),
				Candidate: referred("alice", "LINK"),
			},
		)
		h.gift(
			recognition.Gift{
				ID:        "l2",
				DonorID:   "link2",
				Amount:    5000,
				Currency:  "USD",
				Frequency: "MONTHLY",
				PaidAt:    asOf.Add(-71 * time.Hour),
				Candidate: referred("alice", "LINK"),
			},
		)
		h.gift(
			recognition.Gift{
				ID:        "i1",
				DonorID:   "irl1",
				Amount:    9000,
				Currency:  "USD",
				Frequency: "MONTHLY",
				PaidAt:    asOf.Add(-70 * time.Hour),
				Candidate: referred("alice", "IRL"),
			},
		)
		actAs(t, tx, dbroles.ProjectionWorker)
		for _, name := range []string{"alice", "link1", "link2", "irl1"} {
			project(t, tx, h, name)
		}
		actAs(t, tx, dbroles.API)
		if got, want := dashboardTotals(t, tx, h.donor("alice"), asOf), oldReferralAggregate(t, tx, h.donor("alice")); got != want {
			t.Fatalf("denormalized totals disagree with aggregate: got %+v want %+v", got, want)
		}
	})

	t.Run("referred projected before referrer", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		h := newHistoryFixture(t, tx)
		h.gift(
			recognition.Gift{
				ID:        "l1",
				DonorID:   "link1",
				Amount:    3000,
				Currency:  "USD",
				Frequency: "MONTHLY",
				PaidAt:    asOf.Add(-72 * time.Hour),
				Candidate: referred("alice", "LINK"),
			},
		)
		h.gift(
			recognition.Gift{
				ID:        "i1",
				DonorID:   "irl1",
				Amount:    9000,
				Currency:  "USD",
				Frequency: "MONTHLY",
				PaidAt:    asOf.Add(-70 * time.Hour),
				Candidate: referred("alice", "IRL"),
			},
		)
		actAs(t, tx, dbroles.ProjectionWorker)
		// The referred donors are projected while alice has NO projection row,
		// so each ApplyReferralDelta hits a 0-row UPDATE and is dropped. Only the
		// recompute backstop in alice's own projection recovers the totals.
		for _, name := range []string{"link1", "irl1", "alice"} {
			project(t, tx, h, name)
		}
		actAs(t, tx, dbroles.API)
		got, want := dashboardTotals(t, tx, h.donor("alice"), asOf), oldReferralAggregate(t, tx, h.donor("alice"))
		if got != want {
			t.Fatalf("referred-before-referrer did not converge: got %+v want %+v", got, want)
		}
		if want.Link != 1 || want.IRL != 1 || want.LinkMinor != 3000 || want.IRLMinor != 9000 {
			t.Fatalf("unexpected aggregate baseline: %+v", want)
		}
	})

	t.Run("full refund zero-crossing drops a referred donor from the count", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		h := newHistoryFixture(t, tx)
		h.gift(
			recognition.Gift{
				ID:        "l1",
				DonorID:   "link1",
				Amount:    3000,
				Currency:  "USD",
				Frequency: "MONTHLY",
				PaidAt:    asOf.Add(-72 * time.Hour),
				Candidate: referred("alice", "LINK"),
			},
		)
		h.gift(
			recognition.Gift{
				ID:        "l2",
				DonorID:   "link2",
				Amount:    5000,
				Currency:  "USD",
				Frequency: "MONTHLY",
				PaidAt:    asOf.Add(-71 * time.Hour),
				Candidate: referred("alice", "LINK"),
			},
		)
		actAs(t, tx, dbroles.ProjectionWorker)
		for _, name := range []string{"alice", "link1", "link2"} {
			project(t, tx, h, name)
		}
		if got, want := oldReferralAggregate(t, tx, h.donor("alice")), (referralTotals{Link: 2, LinkMinor: 8000}); got != want {
			t.Fatalf("baseline before refund: got %+v want %+v", got, want)
		}
		// Refund link1's whole gift, then reproject it: its net crosses to 0, so
		// it must drop from the LINK count while its minor contribution goes to 0.
		// The refund fixture spans donor_stripe_events (API-written) and the gift
		// tables (StripeWorker-written), so write the fact as the owner — as the
		// gift fixtures above do — then project as the projections worker, which
		// holds only SELECT on the financial tables.
		mustExec(t, tx, "RESET ROLE")
		h.refund(recognition.Refund{GiftID: "l1", Amount: 3000, Status: "SUCCEEDED", UpdatedAt: asOf.Add(-48 * time.Hour)})
		actAs(t, tx, dbroles.ProjectionWorker)
		if _, err = refreshProjection(ctx, tx, h.donor("link1"), asOf); err != nil {
			t.Fatal(err)
		}
		link1, err := queries.New(tx).Projection(ctx, h.donor("link1"))
		if err != nil || link1.NetMinor.Int64 != 0 {
			t.Fatalf("refunded donor net did not reach zero: %+v %v", link1, err)
		}
		actAs(t, tx, dbroles.API)
		got, want := dashboardTotals(t, tx, h.donor("alice"), asOf), oldReferralAggregate(t, tx, h.donor("alice"))
		if got != want {
			t.Fatalf("totals after refund disagree with aggregate: got %+v want %+v", got, want)
		}
		if want.Link != 1 || want.LinkMinor != 5000 {
			t.Fatalf("zero-crossing did not drop the refunded donor: %+v", want)
		}
	})
}

// TestReferralTotalsConvergeAfterRulesBumpRecompute proves the recompute is
// idempotent: re-projecting an already-converged referrer and its referred
// donors (as the projectionRules 3->4 bump makes every donor do once) leaves
// the four denormalized columns equal to the aggregate, never double-counted.
func TestReferralTotalsConvergeAfterRulesBumpRecompute(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	asOf := time.Date(2027, 4, 1, 12, 0, 0, 0, time.UTC)
	window := asOf.AddDate(0, 1, 0)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	h := newHistoryFixture(t, tx)
	ref := func(kind string) *recognition.Candidate {
		return &recognition.Candidate{ReferrerID: "alice", Kind: kind, ExpiresAt: window}
	}
	h.gift(
		recognition.Gift{
			ID:        "l1",
			DonorID:   "link1",
			Amount:    3000,
			Currency:  "USD",
			Frequency: "MONTHLY",
			PaidAt:    asOf.Add(-72 * time.Hour),
			Candidate: ref("LINK"),
		},
	)
	h.gift(
		recognition.Gift{
			ID:        "l2",
			DonorID:   "link2",
			Amount:    5000,
			Currency:  "USD",
			Frequency: "MONTHLY",
			PaidAt:    asOf.Add(-71 * time.Hour),
			Candidate: ref("LINK"),
		},
	)
	h.gift(
		recognition.Gift{
			ID:        "i1",
			DonorID:   "irl1",
			Amount:    9000,
			Currency:  "USD",
			Frequency: "MONTHLY",
			PaidAt:    asOf.Add(-70 * time.Hour),
			Candidate: ref("IRL"),
		},
	)
	names := []string{"alice", "link1", "link2", "irl1"}
	actAs(t, tx, dbroles.ProjectionWorker)
	for _, name := range names {
		if _, err = refreshProjection(ctx, tx, h.donor(name), asOf); err != nil {
			t.Fatal(name, err)
		}
	}
	stored := func() referralTotals {
		p, err := queries.New(tx).Projection(ctx, h.donor("alice"))
		if err != nil {
			t.Fatal(err)
		}
		return referralTotals{
			Link:      p.ReferredLinkCount,
			IRL:       p.ReferredIrlCount,
			LinkMinor: p.ReferredLinkMinor,
			IRLMinor:  p.ReferredIrlMinor,
		}
	}
	first := stored()
	if want := oldReferralAggregate(t, tx, h.donor("alice")); first != want {
		t.Fatalf("stored totals after first projection: got %+v want %+v", first, want)
	}
	// Re-run every donor's projection, as the rules bump does; the recompute SETs
	// (not adds), so an unchanged referred set must leave the columns unchanged.
	for _, name := range names {
		if _, err = refreshProjection(ctx, tx, h.donor(name), asOf.Add(time.Hour)); err != nil {
			t.Fatal(name, err)
		}
	}
	if second := stored(); second != first {
		t.Fatalf("recompute double-counted: first %+v second %+v", first, second)
	}
}

func TestGiftStoresAttributionValidWhenCharged(t *testing.T) {
	pool := database(t)
	referrer, _ := donor(t, pool)
	accountCleanup(t, pool, referrer)
	f := newWorkerFixture(t, "MONTHLY")
	ctx := context.Background()
	visitor := "visitor-" + f.donorID.String()
	f.request.Visitor = &visitor
	if _, err := pool.Exec(ctx, `INSERT INTO donor_referral_touches (id,token_hash,referrer_id,kind,touched_at,expires_at) VALUES ($1,$2,$3,'LINK',$4,$5)`, testID(t), HashToken(visitor), referrer, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, "DELETE FROM donor_referral_touches WHERE referrer_id=$1", referrer); err != nil {
			t.Error(err)
		}
	})
	pay := func(checkout, invoiceID string, start time.Time) {
		f.receive(t, "invoice.payment_succeeded", invoice(checkout, invoiceID, start, true))
		f.process(t)
	}
	firstStart := time.Now().UTC().Truncate(time.Second).Add(-33 * 24 * time.Hour)
	first := f.checkout(t)
	pay(first.CheckoutID, "in_first_"+f.donorID.String(), firstStart)
	var touchReferrer, giftReferrer, checkoutReferrer pgtype.UUID
	var touchKind, giftKind, checkoutKind string
	if err := pool.QueryRow(ctx, `SELECT t.referrer_id,t.kind,c.referrer_id,c.referral_kind,g.referrer_id,g.referral_kind
FROM donor_gifts g JOIN donor_checkouts c ON c.id=g.checkout_id JOIN donor_referral_touches t ON t.token_hash=$2
WHERE g.stripe_object_id=$1`, "in_first_"+f.donorID.String(), HashToken(visitor)).Scan(&touchReferrer, &touchKind, &checkoutReferrer, &checkoutKind, &giftReferrer, &giftKind); err != nil {
		t.Fatal(err)
	}
	if giftReferrer != referrer || checkoutReferrer != referrer || touchReferrer != referrer || giftKind != "LINK" ||
		checkoutKind != touchKind ||
		giftKind != touchKind {
		t.Fatal("payment record lost the touch's attribution")
	}
	// A renewal charged after the attribution window records no referrer: the
	// gift captures the checkout snapshot as it stands when the invoice is paid.
	// The window closes between the first charge and the renewal.
	if _, err := pool.Exec(ctx, "UPDATE donor_checkouts SET referral_expires_at=$2 WHERE id=$1", first.CheckoutID, firstStart.Add(15*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	pay(first.CheckoutID, "in_second_"+f.donorID.String(), firstStart.Add(30*24*time.Hour))
	var attributed bool
	if err := pool.QueryRow(ctx, "SELECT referrer_id IS NOT NULL OR referral_kind IS NOT NULL FROM donor_gifts WHERE stripe_object_id=$1", "in_second_"+f.donorID.String()).Scan(&attributed); err != nil ||
		attributed {
		t.Fatal("expired attribution written to payment", err)
	}
	p, err := queries.New(pool).Projection(ctx, f.donorID)
	if err != nil || p.ReferrerID != referrer || p.ReferralKind.String != "LINK" {
		t.Fatal("projection disagrees with payment attribution", err)
	}
}

func TestEarlierPaymentCorrectsConversionAndPreservesFoundingAward(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	h := newHistoryFixture(t, tx)
	later := time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC)
	earlier := later.AddDate(0, -1, 0)
	window := later.AddDate(0, 1, 0)
	h.gift(
		recognition.Gift{
			ID:        "later",
			DonorID:   "ben",
			Amount:    3000,
			Currency:  "USD",
			Frequency: "MONTHLY",
			PaidAt:    later,
			Candidate: &recognition.Candidate{ReferrerID: "alice", Kind: "LINK", ExpiresAt: window},
		},
	)
	first, err := projectFacts(ctx, queries.New(tx), h.donor("ben"), later)
	if err != nil || first.ReferrerID != h.donor("alice") || !first.FoundingAwardedAt.Time.Equal(later) {
		t.Fatalf("first projection: %+v %v", first, err)
	}
	// Provider collection time, rather than processing order, determines credit.
	h.gift(
		recognition.Gift{
			ID:        "earlier",
			DonorID:   "ben",
			Amount:    3000,
			Currency:  "USD",
			Frequency: "MONTHLY",
			PaidAt:    earlier,
			Candidate: &recognition.Candidate{ReferrerID: "cara", Kind: "IRL", ExpiresAt: window},
		},
	)
	second, err := projectFacts(ctx, queries.New(tx), h.donor("ben"), later.Add(time.Hour))
	if err != nil || second.ReferrerID != h.donor("cara") || second.ReferralKind.String != "IRL" ||
		second.FirstGiftID.String != h.objects["earlier"] ||
		!second.FoundingAwardedAt.Time.Equal(first.FoundingAwardedAt.Time) ||
		second.NetMinor.Int64 != 6000 {
		t.Fatalf("incorrect corrected relationship: %+v %v", second, err)
	}
	var conversions int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM analytics_events WHERE donor_id=$1 AND name='referral_converted' AND source='SERVER'",
		h.donor("ben")).Scan(&conversions); err != nil || conversions != 1 {
		t.Fatalf("recorded %d referral conversions, want 1 across the correction: %v", conversions, err)
	}
}

func TestRefundRecordedByLaterClockUsesEarlierObservation(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	h := newHistoryFixture(t, tx)
	paid := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	asOf := paid.Add(time.Hour)
	h.gift(recognition.Gift{ID: "g", DonorID: "ben", Amount: 3000, Currency: "USD", Frequency: "MONTHLY", PaidAt: paid})
	h.refund(recognition.Refund{GiftID: "g", Amount: 3000, Status: "PENDING", UpdatedAt: asOf.Add(-time.Minute)})
	ahead := asOf.Add(2 * time.Second)
	if _, err = tx.Exec(ctx, "UPDATE donor_refunds SET status='SUCCEEDED',observed_at=$2 WHERE gift_id=(SELECT id FROM donor_gifts WHERE stripe_object_id=$1)", h.objects["g"], ahead); err != nil {
		t.Fatal(err)
	}
	before, err := projectFacts(ctx, queries.New(tx), h.donor("ben"), asOf)
	if err != nil || before.NetMinor.Int64 != 3000 || !before.NextRefreshAt.Time.Equal(ahead) {
		t.Fatalf("as-of refund state: %+v %v", before, err)
	}
	after, err := projectFacts(ctx, queries.New(tx), h.donor("ben"), ahead)
	if err != nil || after.NetMinor.Int64 != 0 {
		t.Fatalf("later refund state: %+v %v", after, err)
	}
}

func TestRefreshNextProjectionDefersUnprojectableDonor(t *testing.T) {
	pool := database(t)
	worker := as(t, dbroles.ProjectionWorker)
	ctx := context.Background()
	// A historical clock keeps other tests' due projections out of this queue.
	now := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	poisoned, healthy := testID(t), testID(t)
	checkout, gift := testID(t), testID(t)
	t.Cleanup(func() {
		for _, sql := range []string{
			"DELETE FROM donor_messages WHERE donor_id=ANY($1)",
			"DELETE FROM donor_refunds WHERE gift_id IN (SELECT id FROM donor_gifts WHERE donor_id=ANY($1))",
			"DELETE FROM donor_gifts WHERE donor_id=ANY($1)",
			"DELETE FROM donor_checkouts WHERE donor_id=ANY($1)",
			"DELETE FROM donor_metric_projections WHERE donor_id=ANY($1)",
			"DELETE FROM donors WHERE id=ANY($1)",
		} {
			if _, err := pool.Exec(context.Background(), sql, []pgtype.UUID{poisoned, healthy}); err != nil {
				t.Error(err)
			}
		}
	})
	for i, id := range []pgtype.UUID{poisoned, healthy} {
		if _, err := pool.Exec(ctx, "INSERT INTO donors(id,email,created_at) VALUES($1,$2,$3)", id, id.String()+"@example.org", now); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, "INSERT INTO donor_metric_projections(id,donor_id,computed_at,next_refresh_at) VALUES($1,$2,$3,$4)", testID(t), id, now, now.Add(time.Duration(i-2)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	// A succeeded refund larger than its gift cannot be projected.
	if _, err := pool.Exec(ctx, "INSERT INTO donor_checkouts(id,donor_id,amount_minor,currency,frequency) VALUES($1,$2,3000,'USD','MONTHLY')", checkout, poisoned); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO donor_gifts(id,donor_id,checkout_id,stripe_object_id,amount_minor,currency,frequency,paid_at) VALUES($1,$2,$3,$4,3000,'USD','MONTHLY',$5)", gift, poisoned, checkout, "pi_"+gift.String(), now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO donor_refunds(id,gift_id,stripe_refund_id,amount_minor,currency,status,observed_at) VALUES($1,$2,$3,9000,'USD','SUCCEEDED',$4)", testID(t), gift, "re_"+gift.String(), now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if worked, err := RefreshNextProjection(ctx, worker, now); err != nil || !worked {
			t.Fatal("projection worker stalled", worked, err)
		}
	}
	if worked, err := RefreshNextProjection(ctx, worker, now); err != nil || worked {
		t.Fatal("deferred donor retried immediately", worked, err)
	}
	q := queries.New(pool)
	bad, err := q.Projection(ctx, poisoned)
	if err != nil || bad.RefreshFailures != 1 || bad.RefreshError.String != "InvalidFacts" ||
		!bad.NextRefreshAt.Time.Equal(now.Add(5*time.Minute)) ||
		bad.NetMinor.Valid {
		t.Fatalf("poisoned projection: %+v %v", bad, err)
	}
	good, err := q.Projection(ctx, healthy)
	if err != nil || !good.NetMinor.Valid || !good.ComputedAt.Time.Equal(now) || good.RefreshError.Valid {
		t.Fatalf("healthy projection blocked: %+v %v", good, err)
	}
	rows, err := queries.New(as(t, dbroles.Monitor)).QueueHealth(ctx, timestamp(now.Add(5*time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	if row := queueHealthRow(t, rows, "projections"); row.Due != 1 || row.Parked != 1 {
		t.Fatal("unprojectable donor not surfaced", row)
	}
	if worked, err := RefreshNextProjection(ctx, worker, now.Add(5*time.Minute)); err != nil || !worked {
		t.Fatal(worked, err)
	}
	if bad, err = q.Projection(ctx, poisoned); err != nil || bad.RefreshFailures != 2 ||
		!bad.NextRefreshAt.Time.Equal(now.Add(15*time.Minute)) {
		t.Fatalf("backoff did not grow: %+v %v", bad, err)
	}
	if _, err = pool.Exec(ctx, "UPDATE donor_refunds SET amount_minor=1000 WHERE gift_id=$1", gift); err != nil {
		t.Fatal(err)
	}
	if worked, err := RefreshNextProjection(ctx, worker, now.Add(15*time.Minute)); err != nil || !worked {
		t.Fatal(worked, err)
	}
	if bad, err = q.Projection(ctx, poisoned); err != nil || bad.RefreshFailures != 0 || bad.RefreshError.Valid ||
		bad.NetMinor.Int64 != 2000 {
		t.Fatalf("corrected donor not recovered: %+v %v", bad, err)
	}
}

func TestRefreshDueProjectionsBatchesDonors(t *testing.T) {
	pool := database(t)
	worker := as(t, dbroles.ProjectionWorker)
	ctx := context.Background()
	// A historical clock keeps other tests' due projections out of this queue.
	now := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	due := []pgtype.UUID{testID(t), testID(t), testID(t)}
	outdated := testID(t)
	ids := append(append([]pgtype.UUID{}, due...), outdated)
	t.Cleanup(func() {
		for _, sql := range []string{
			"DELETE FROM donor_messages WHERE donor_id=ANY($1)",
			"DELETE FROM donor_metric_projections WHERE donor_id=ANY($1)",
			"DELETE FROM donors WHERE id=ANY($1)",
		} {
			if _, err := pool.Exec(context.Background(), sql, ids); err != nil {
				t.Error(err)
			}
		}
	})
	for i, id := range due {
		mustExec(t, pool, "INSERT INTO donors(id,email,created_at) VALUES($1,$2,$3)", id, id.String()+"@example.org", now)
		// Staggered next_refresh_at, all in the past, so all are due at now.
		mustExec(t, pool, "INSERT INTO donor_metric_projections(id,donor_id,computed_at,next_refresh_at) VALUES($1,$2,$3,$4)",
			testID(t), id, now, now.Add(time.Duration(i-len(due))*time.Hour))
	}
	// A donor computed under earlier rules, not yet due, to exercise the fallback.
	mustExec(t, pool, "INSERT INTO donors(id,email,created_at) VALUES($1,$2,$3)", outdated, outdated.String()+"@example.org", now)
	mustExec(t, pool, "INSERT INTO donor_metric_projections(id,donor_id,computed_at,next_refresh_at,rules_version) VALUES($1,$2,$3,$4,$5)",
		testID(t), outdated, now.Add(-24*time.Hour), now.AddDate(0, 6, 0), projectionRules-1)

	// One batch claims and refreshes every due donor.
	processed, err := RefreshDueProjections(ctx, worker, now, 16)
	if err != nil || processed != len(due) {
		t.Fatalf("due batch: processed %d want %d: %v", processed, len(due), err)
	}
	q := queries.New(pool)
	for _, id := range due {
		p, err := q.Projection(ctx, id)
		if err != nil || !p.NetMinor.Valid || !p.ComputedAt.Time.Equal(now) || p.RefreshError.Valid {
			t.Fatalf("due donor not refreshed: %+v %v", p, err)
		}
	}

	// With no due donors left, the batch falls back to the outdated donor.
	processed, err = RefreshDueProjections(ctx, worker, now, 16)
	if err != nil || processed != 1 {
		t.Fatalf("outdated fallback: processed %d want 1: %v", processed, err)
	}
	o, err := q.Projection(ctx, outdated)
	if err != nil || o.RulesVersion != currentVersion(t) || !o.ComputedAt.Time.Equal(now) {
		t.Fatalf("outdated donor not recomputed: %+v %v", o, err)
	}

	// Nothing left to do.
	if processed, err = RefreshDueProjections(ctx, worker, now, 16); err != nil || processed != 0 {
		t.Fatalf("expected an empty batch: processed %d: %v", processed, err)
	}
}

func TestRefreshDueProjectionsDefersPoisonedDonorInBatch(t *testing.T) {
	pool := database(t)
	worker := as(t, dbroles.ProjectionWorker)
	ctx := context.Background()
	// A historical clock keeps other tests' due projections out of this queue.
	now := time.Date(2000, 1, 2, 0, 0, 0, 0, time.UTC)
	poisoned, healthy := testID(t), testID(t)
	checkout, gift := testID(t), testID(t)
	t.Cleanup(func() {
		for _, sql := range []string{
			"DELETE FROM donor_messages WHERE donor_id=ANY($1)",
			"DELETE FROM donor_refunds WHERE gift_id IN (SELECT id FROM donor_gifts WHERE donor_id=ANY($1))",
			"DELETE FROM donor_gifts WHERE donor_id=ANY($1)",
			"DELETE FROM donor_checkouts WHERE donor_id=ANY($1)",
			"DELETE FROM donor_metric_projections WHERE donor_id=ANY($1)",
			"DELETE FROM donors WHERE id=ANY($1)",
		} {
			if _, err := pool.Exec(context.Background(), sql, []pgtype.UUID{poisoned, healthy}); err != nil {
				t.Error(err)
			}
		}
	})
	for i, id := range []pgtype.UUID{poisoned, healthy} {
		mustExec(t, pool, "INSERT INTO donors(id,email,created_at) VALUES($1,$2,$3)", id, id.String()+"@example.org", now)
		mustExec(t, pool, "INSERT INTO donor_metric_projections(id,donor_id,computed_at,next_refresh_at) VALUES($1,$2,$3,$4)",
			testID(t), id, now, now.Add(time.Duration(i-2)*time.Hour))
	}
	// A succeeded refund larger than its gift cannot be projected.
	mustExec(
		t,
		pool,
		"INSERT INTO donor_checkouts(id,donor_id,amount_minor,currency,frequency) VALUES($1,$2,3000,'USD','MONTHLY')",
		checkout,
		poisoned,
	)
	mustExec(
		t,
		pool,
		"INSERT INTO donor_gifts(id,donor_id,checkout_id,stripe_object_id,amount_minor,currency,frequency,paid_at) VALUES($1,$2,$3,$4,3000,'USD','MONTHLY',$5)",
		gift,
		poisoned,
		checkout,
		"pi_"+gift.String(),
		now.Add(-48*time.Hour),
	)
	mustExec(
		t,
		pool,
		"INSERT INTO donor_refunds(id,gift_id,stripe_refund_id,amount_minor,currency,status,observed_at) VALUES($1,$2,$3,9000,'USD','SUCCEEDED',$4)",
		testID(t),
		gift,
		"re_"+gift.String(),
		now.Add(-24*time.Hour),
	)

	// One poisoned donor in the batch is deferred; the healthy one still refreshes.
	processed, err := RefreshDueProjections(ctx, worker, now, 16)
	if err != nil || processed != 2 {
		t.Fatalf("batch: processed %d want 2: %v", processed, err)
	}
	q := queries.New(pool)
	bad, err := q.Projection(ctx, poisoned)
	if err != nil || bad.RefreshFailures != 1 || bad.RefreshError.String != "InvalidFacts" ||
		!bad.NextRefreshAt.Time.Equal(now.Add(5*time.Minute)) ||
		bad.NetMinor.Valid {
		t.Fatalf("poisoned donor not deferred (unknown, not zero): %+v %v", bad, err)
	}
	good, err := q.Projection(ctx, healthy)
	if err != nil || !good.NetMinor.Valid || !good.ComputedAt.Time.Equal(now) || good.RefreshError.Valid {
		t.Fatalf("healthy donor blocked by poisoned batch member: %+v %v", good, err)
	}
}

func TestProjectionWaitsForItsNextBoundaryAndSummarizesAtNewYear(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	h := newHistoryFixture(t, tx)
	donor := h.donor("ana")
	paid := time.Date(2027, 6, 1, 12, 0, 0, 0, time.UTC)
	periodEnd := paid.AddDate(0, 1, 0)
	// A monthly donor who gave once in 2027 then cancelled at period end: the
	// subscription stop makes the missed renewal a clean streak end (not an
	// incomplete billing history), so the only far boundary is the 2027 annual
	// summary at New Year.
	checkoutID, attemptID, periodID, giftID := testID(t), testID(t), testID(t), testID(t)
	subID := "sub_" + attemptID.String()
	for _, step := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO donor_checkouts (id,donor_id,amount_minor,currency,frequency,created_at) VALUES ($1,$2,3000,'USD','MONTHLY',$3)", []any{checkoutID, donor, paid}},
		{"INSERT INTO donor_stripe_checkouts (id,checkout_id,request_hash,request_params,stripe_subscription_id,status) VALUES ($1,$2,'h','{}',$3,'COMPLETE')", []any{attemptID, checkoutID, subID}},
		{"INSERT INTO donor_billing_periods (id,donor_id,checkout_id,stripe_invoice_id,stripe_subscription_id,due_at,ends_at,status) VALUES ($1,$2,$3,'in_june',$4,$5,$6,'PAID')", []any{periodID, donor, checkoutID, subID, paid, periodEnd}},
		{"INSERT INTO donor_gifts (id,donor_id,checkout_id,stripe_object_id,amount_minor,currency,frequency,paid_at,stripe_payment_intent_id,period_id) VALUES ($1,$2,$3,'pi_june',3000,'USD','MONTHLY',$4,'pi_june',$5)", []any{giftID, donor, checkoutID, paid, periodID}},
		{"INSERT INTO donor_subscriptions (id,attempt_id,donor_id,status,period_end,stops_at,observed_at) VALUES ($1,$2,$3,'canceled',$4,$4,$5)", []any{testID(t), attemptID, donor, periodEnd, paid}},
	} {
		if _, err := tx.Exec(ctx, step.sql, step.args...); err != nil {
			t.Fatal(err)
		}
	}
	summer := paid.Add(time.Hour)
	// The next refresh is the nearest of the donor's refresh cadence, this
	// period's end, and the next streak milestone: seven days, reached at
	// midnight in the donor's zone (New York without a ZIP).
	wantNext := summer.Add(refreshCap(donor))
	if periodEnd.Before(wantNext) {
		wantNext = periodEnd
	}
	sevenDays := recognition.Streak{Since: &paid}.Reaches(7, recognition.NewYork)
	if sevenDays.Before(wantNext) {
		wantNext = sevenDays
	}
	if p, err := projectFacts(ctx, h.q, donor, summer); err != nil || !p.NextRefreshAt.Time.Equal(wantNext) ||
		p.RulesVersion != currentVersion(t) {
		t.Fatalf("projection did not schedule its nearest boundary: %+v %v", p, err)
	}
	newYear := time.Date(2028, 1, 1, 0, 0, 0, 0, recognition.NewYork)
	december, err := refreshProjection(ctx, tx, donor, newYear.Add(-10*24*time.Hour))
	if err != nil || !december.NextRefreshAt.Time.Equal(newYear) {
		t.Fatalf("New Year is not a boundary: %+v %v", december, err)
	}
	summaries := func() int {
		var n int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM donor_messages WHERE donor_id=$1 AND dedup_key=$2", donor, "annual:"+donor.String()+":2027").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if summaries() != 0 {
		t.Fatal("2027 summarized before the year ended")
	}
	if _, err = refreshProjection(ctx, tx, donor, newYear); err != nil || summaries() != 1 {
		t.Fatal("2027 not summarized at New Year", err)
	}
}

func TestProjectionsFromEarlierRulesAreRecomputed(t *testing.T) {
	pool := database(t)
	worker := as(t, dbroles.ProjectionWorker)
	ctx := context.Background()
	// A historical clock keeps other tests' projections out of this queue.
	now := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	outdated, recent := testID(t), testID(t)
	t.Cleanup(func() {
		for _, sql := range []string{"DELETE FROM donor_messages WHERE donor_id=ANY($1)", "DELETE FROM donor_metric_projections WHERE donor_id=ANY($1)", "DELETE FROM donors WHERE id=ANY($1)"} {
			if _, err := pool.Exec(context.Background(), sql, []pgtype.UUID{outdated, recent}); err != nil {
				t.Error(err)
			}
		}
	})
	for id, computed := range map[pgtype.UUID]time.Time{outdated: now.Add(-24 * time.Hour), recent: now.Add(time.Hour)} {
		mustExec(t, pool, "INSERT INTO donors(id,email,created_at) VALUES($1,$2,$3)", id, id.String()+"@example.org", now)
		mustExec(
			t,
			pool,
			"INSERT INTO donor_metric_projections(id,donor_id,computed_at,next_refresh_at,rules_version) VALUES($1,$2,$3,$4,$5)",
			testID(t),
			id,
			computed,
			now.AddDate(0, 6, 0),
			projectionRules-1,
		)
	}
	if worked, err := RefreshNextProjection(ctx, worker, now); err != nil || !worked {
		t.Fatal("projection from earlier rules not recomputed", worked, err)
	}
	if worked, err := RefreshNextProjection(ctx, worker, now); err != nil || worked {
		t.Fatal("recomputed twice, or a projection computed after the clock taken", worked, err)
	}
	p, err := queries.New(pool).Projection(ctx, outdated)
	if err != nil || p.RulesVersion != currentVersion(t) || !p.ComputedAt.Time.Equal(now) {
		t.Fatalf("outdated projection: %+v %v", p, err)
	}
}

func TestRefreshCapsSpreadDonorsComputedTogether(t *testing.T) {
	days := map[int]int{}
	for range 10_000 {
		id := testID(t)
		wait := refreshCap(id)
		if wait < minRefreshInterval || wait >= maxRefreshInterval || refreshCap(id) != wait {
			t.Fatal("cap outside its range or not stable for a donor", wait)
		}
		days[int((wait-minRefreshInterval)/(24*time.Hour))]++
	}
	for day := range 10 {
		if days[day] < 800 || days[day] > 1200 {
			t.Fatal("donors computed together fall due unevenly", days)
		}
	}
}
