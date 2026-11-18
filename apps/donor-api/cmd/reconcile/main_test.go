package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/dbroles/dbrolestest"
	"dad/donor-api/internal/migrations"
	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/reconcile"
	"dad/donor-api/internal/store"
	"dad/donor-api/internal/store/queries"
)

// fakeGateway serves canned charge/refund objects to reconcileWindow, so the
// Stripe side is exercised without any live or SDK call. The real StripeGateway
// is tested against an injected httptest server in internal/payments.
type fakeGateway struct {
	charges, refunds          []payments.Object
	paymentsByID, refundsByID map[string]payments.Object
	refundReads               map[string]int
	invoicePayments           map[string][]payments.Object
}

func (f fakeGateway) ListInvoicePayments(_ context.Context, invoice, payment string) ([]payments.Object, error) {
	if rows, ok := f.invoicePayments[invoice+payment]; ok {
		return rows, nil
	}
	return nil, nil
}

func (f fakeGateway) ListChargesByCreated(context.Context, time.Time, time.Time) ([]payments.Object, error) {
	return f.charges, nil
}

func (f fakeGateway) ListRefundsByCreated(context.Context, time.Time, time.Time) ([]payments.Object, error) {
	return f.refunds, nil
}

func (f fakeGateway) RetrievePaymentIntent(_ context.Context, id string) (payments.Object, error) {
	if o, ok := f.paymentsByID[id]; ok {
		return o, nil
	}
	return nil, errors.New("unexpected payment lookup")
}

func (f fakeGateway) RetrieveRefund(_ context.Context, id string) (payments.Object, error) {
	if f.refundReads != nil {
		f.refundReads[id]++
	}
	if o, ok := f.refundsByID[id]; ok {
		return o, nil
	}
	return nil, errors.New("unexpected refund lookup")
}

// reconcileWindow reconciles without pacing, which only the provider needs.
func reconcileWindow(
	ctx context.Context,
	win reconcile.Window,
	gateway payments.ReconcileGateway,
	q *queries.Queries,
) (reconcile.Report, error) {
	return reconcileWindowPaced(ctx, win, gateway, q, func(context.Context) error { return nil })
}

func objects(t *testing.T, raws ...string) []payments.Object {
	t.Helper()
	out := make([]payments.Object, 0, len(raws))
	for _, raw := range raws {
		o, err := payments.DecodeObject([]byte(raw))
		if err != nil {
			t.Fatalf("bad fixture %q: %v", raw, err)
		}
		out = append(out, o)
	}
	return out
}

func TestWindowResolution(t *testing.T) {
	now := time.Date(2027, 3, 2, 5, 30, 0, 0, time.UTC) // 00:30 New York (EST)
	// Default: yesterday in New York.
	def, err := window("", "", "", 30*time.Minute, now)
	if err != nil || def.NYDate != "2027-03-01" {
		t.Fatalf("default window %+v err=%v", def, err)
	}
	// -date fixes the day deterministically.
	byDate, err := window("2027-02-14", "", "", 30*time.Minute, now)
	if err != nil || byDate.NYDate != "2027-02-14" {
		t.Fatalf("dated window %+v err=%v", byDate, err)
	}
	// -since/-until must be given together and parse as RFC3339.
	if _, err = window("", "2027-03-01T00:00:00Z", "", 0, now); err == nil {
		t.Error("lone -since accepted")
	}
	rng, err := window("", "2027-03-01T00:00:00Z", "2027-03-02T00:00:00Z", 0, now)
	if err != nil || !rng.Start.Equal(time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("range window %+v err=%v", rng, err)
	}
	if _, err = window("not-a-date", "", "", 0, now); err == nil {
		t.Error("invalid -date accepted")
	}
}

func TestReconcileWindowAgainstSyntheticDB(t *testing.T) {
	_, reader := reconciliationDatabase(t)
	q := queries.New(reader)
	win, err := reconcile.DayWindow(time.Date(2027, 3, 1, 12, 0, 0, 0, reconcile.NewYork), 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	noon := time.Date(2027, 3, 1, 12, 0, 0, 0, reconcile.NewYork).Unix()

	t.Run("clean day exits 0", func(t *testing.T) {
		// No seeded gifts in the window and no Stripe objects: a clean day.
		report, err := reconcileWindow(context.Background(), win, fakeGateway{}, q)
		if err != nil {
			t.Fatal(err)
		}
		if report.HasDiscrepancies() {
			t.Fatalf("clean day flagged discrepancies: %+v", report)
		}
	})

	t.Run("missing webhook is one missing gift", func(t *testing.T) {
		gw := fakeGateway{
			charges: objects(
				t,
				`{"id":"ch_missing","amount":3000,"amount_captured":3000,"currency":"usd","status":"succeeded","paid":true,"captured":true,"livemode":false,"created":`+strconv.FormatInt(
					noon,
					10,
				)+`}`,
			),
		}
		report, err := reconcileWindow(context.Background(), win, gw, q)
		if err != nil {
			t.Fatal(err)
		}
		if report.MissingGifts != 1 || !report.HasDiscrepancies() {
			t.Fatalf("want one missing gift, got %+v", report)
		}
	})
}

func TestReconciliationMatchesAcrossCreationAndObservationDays(t *testing.T) {
	ctx := context.Background()
	owner, reader := reconciliationDatabase(t)
	id := func() string {
		uuid, err := store.NewID()
		if err != nil {
			t.Fatal(err)
		}
		return uuid.String()
	}
	donor, checkout, giftA, giftB, giftC, refundA, refundB, refundC := id(), id(), id(), id(), id(), id(), id(), id()
	piA, piB, reA, reB, reC := "pi_"+giftA, "pi_"+giftB, "re_"+refundA, "re_"+refundB, "re_"+refundC
	win, err := reconcile.DayWindow(time.Date(2035, 3, 1, 0, 0, 0, 0, reconcile.NewYork), 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := owner.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("INSERT INTO donors(id,email) VALUES($1,$2)", donor, donor+"@reconciliation.invalid")
	t.Cleanup(func() {
		for _, sql := range []string{
			"DELETE FROM donor_refunds WHERE gift_id IN (SELECT id FROM donor_gifts WHERE donor_id=$1)",
			"DELETE FROM donor_gifts WHERE donor_id=$1", "DELETE FROM donor_checkouts WHERE donor_id=$1", "DELETE FROM donors WHERE id=$1",
		} {
			if _, err := owner.Exec(context.Background(), sql, donor); err != nil {
				t.Error(err)
			}
		}
	})
	exec("INSERT INTO donor_checkouts(id,donor_id,amount_minor,frequency) VALUES($1,$2,3000,'MONTHLY')", checkout, donor)
	for _, gift := range []struct {
		id, pi string
		paid   time.Time
	}{{giftA, piA, win.End.Add(time.Hour)}, {giftB, piB, win.Start.Add(time.Hour)}, {giftC, "pi_" + giftC, win.Start.Add(-48 * time.Hour)}} {
		exec(
			"INSERT INTO donor_gifts(id,donor_id,checkout_id,stripe_object_id,stripe_payment_intent_id,amount_minor,currency,frequency,paid_at) VALUES($1,$2,$3,$4,$5,3000,'USD','MONTHLY',$6)",
			gift.id,
			donor,
			checkout,
			"in_"+gift.id,
			gift.pi,
			gift.paid,
		)
	}
	for _, refund := range []struct {
		id, gift, stripe string
		observed         time.Time
	}{{refundA, giftB, reA, win.End.Add(2 * time.Hour)}, {refundB, giftC, reB, win.Start.Add(2 * time.Hour)}} {
		exec(
			"INSERT INTO donor_refunds(id,gift_id,stripe_refund_id,amount_minor,currency,status,observed_at) VALUES($1,$2,$3,1000,'USD','SUCCEEDED',$4)",
			refund.id,
			refund.gift,
			refund.stripe,
			refund.observed,
		)
	}
	exec(
		"INSERT INTO donor_refunds(id,gift_id,stripe_refund_id,amount_minor,currency,status,observed_at) VALUES($1,$2,$3,1000,'USD','PENDING',$4)",
		refundC,
		giftC,
		reC,
		win.Start.Add(-24*time.Hour),
	)
	charge := func(pi string, created time.Time) payments.Object {
		return objects(t, fmt.Sprintf(`{"id":"ch_%s","payment_intent":%q,"amount":3000,"amount_captured":3000,"currency":"usd","status":"succeeded","paid":true,"captured":true,"livemode":false,"created":%d}`, pi, pi, created.Unix()))[0]
	}
	refund := func(id string, created time.Time) payments.Object {
		return objects(t, fmt.Sprintf(`{"id":%q,"amount":1000,"currency":"usd","status":"succeeded","livemode":false,"created":%d}`, id, created.Unix()))[0]
	}
	oldCharge := charge(piB, win.Start.Add(-time.Hour))
	intent := objects(t, fmt.Sprintf(`{"id":%q,"livemode":false,"status":"succeeded"}`, piB))[0]
	intent["latest_charge"], err = json.Marshal(oldCharge)
	if err != nil {
		t.Fatal(err)
	}
	gateway := fakeGateway{
		charges:      []payments.Object{charge(piA, win.Start.Add(time.Hour))},
		refunds:      []payments.Object{refund(reA, win.Start.Add(2*time.Hour))},
		paymentsByID: map[string]payments.Object{piB: intent},
		refundsByID: map[string]payments.Object{
			reB: refund(reB, win.Start.Add(-time.Hour)),
			reC: refund(reC, win.Start.Add(-24*time.Hour)),
		},
	}
	gateway.refundsByID[reC]["status"] = []byte(`"pending"`)
	q := queries.New(reader)
	got, err := reconcileWindow(ctx, win, gateway, q)
	if err != nil || got.HasDiscrepancies() {
		t.Fatal("processing delay or earlier creation caused a false discrepancy", got, err)
	}
	gateway.refundsByID[reC]["status"] = []byte(`"succeeded"`)
	got, err = reconcileWindow(ctx, win, gateway, q)
	if err != nil || got.StatusMismatches != 1 || got.StatusMismatchIDs[0] != reC {
		t.Fatal("missed terminal update of an older refund was not detected", got, err)
	}
	gateway.refundsByID[reC]["status"] = []byte(`"pending"`)
	exec("DELETE FROM donor_refunds WHERE id=$1", refundA)
	got, err = reconcileWindow(ctx, win, gateway, q)
	if err != nil || got.MissingRefunds != 1 || got.MissingRefundIDs[0] != reA {
		t.Fatal("missing delayed refund was not detected", got, err)
	}
	delete(gateway.paymentsByID, piB)
	if _, err := reconcileWindow(ctx, win, gateway, q); err == nil {
		t.Fatal("provider lookup failure passed reconciliation")
	}
}

// Whole-table reconciliation needs a database isolated from concurrent test packages.
func reconciliationDatabase(t *testing.T) (*pgxpool.Pool, *pgxpool.Pool) {
	t.Helper()
	_ = dbrolestest.URL(t, dbroles.ReconcileJob)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	config, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	id, err := store.NewID()
	if err != nil {
		t.Fatal(err)
	}
	name := "donor_go_reconcile_" + strings.ReplaceAll(id.String(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
			t.Error(err)
		}
	})
	config = config.Copy()
	config.ConnConfig.Database = name
	owner, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	if err := migrations.Apply(ctx, owner, false); err != nil {
		t.Fatal(err)
	}
	config = config.Copy()
	config.ConnConfig.RuntimeParams["role"] = dbroles.ReconcileJob.Name
	config.MaxConns = dbroles.ReconcileJob.Pool
	reader, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reader.Close)
	return owner, reader
}

func TestRunRejectsAnUnsettledWindowBeforeAccessingProviders(t *testing.T) {
	err := run(context.Background(), []string{"-date", "9999-01-01"}, func(string) string { return "" }, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "has not settled") {
		t.Fatal("unsettled window was accepted", err)
	}
}

func TestPendingRefundBacklogIsFullyCheckedInBoundedBatches(t *testing.T) {
	owner, reader := reconciliationDatabase(t)
	ctx := context.Background()
	win, err := reconcile.DayWindow(time.Date(2035, 3, 1, 0, 0, 0, 0, reconcile.NewYork), 0)
	if err != nil {
		t.Fatal(err)
	}
	const donor = "00000000-0000-4000-8000-000000000001"
	for _, statement := range []string{
		"INSERT INTO donors(id,email) VALUES('" + donor + "','batch@reconciliation.invalid')",
		"INSERT INTO donor_checkouts(id,donor_id,amount_minor,frequency) VALUES('" + donor + "','" + donor + "',3000,'MONTHLY')",
	} {
		if _, err := owner.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := owner.Exec(ctx, "INSERT INTO donor_gifts(id,donor_id,checkout_id,stripe_object_id,amount_minor,currency,frequency,paid_at) VALUES($1,$1,$1,'pi_batch',3000,'USD','MONTHLY',$2)", donor, win.Start.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	const count = 10105
	rows := make([][]any, 0, count+1)
	gw := fakeGateway{refundsByID: map[string]payments.Object{}, refundReads: map[string]int{}}
	for i := 0; i <= count; i++ {
		id, stripe := fmt.Sprintf("00000000-0000-4000-8000-%012x", i), fmt.Sprintf("re_batch_%05d", i)
		observed := win.Start.Add(-24 * time.Hour)
		if i == 0 {
			observed = win.Start.Add(time.Hour)
		}
		rows = append(rows, []any{id, donor, stripe, int64(1), "USD", "PENDING", observed})
		gw.refundsByID[stripe] = objects(t, fmt.Sprintf(`{"id":%q,"amount":1,"currency":"usd","status":"pending","livemode":false,"created":%d}`, stripe, observed.Unix()))[0]
	}
	if _, err := owner.CopyFrom(ctx, pgx.Identifier{"donor_refunds"}, []string{"id", "gift_id", "stripe_refund_id", "amount_minor", "currency", "status", "observed_at"}, pgx.CopyFromRows(rows)); err != nil {
		t.Fatal(err)
	}
	gw.refunds = []payments.Object{gw.refundsByID["re_batch_00000"]}
	gw.refundsByID[fmt.Sprintf("re_batch_%05d", count)]["status"] = []byte(`"requires_action"`)
	wait := func(context.Context) error { return nil }
	report, err := reconcileWindowPaced(ctx, win, gw, queries.New(reader), wait)
	if err != nil || report.PendingRefundsChecked != count || report.PendingRefundBatches != 102 || report.StatusMismatches != 1 ||
		report.StatusMismatchIDs[0] != fmt.Sprintf("re_batch_%05d", count) {
		t.Fatal("last batch was not fully checked", report, err)
	}
	if gw.refundReads["re_batch_00000"] != 0 {
		t.Fatal("window refund was checked twice")
	}
	for id, reads := range gw.refundReads {
		if reads != 1 {
			t.Fatalf("%s read %d times", id, reads)
		}
	}
	for _, refund := range gw.refundsByID {
		refund["status"] = []byte(`"requires_action"`)
	}
	report, err = reconcileWindowPaced(ctx, win, gw, queries.New(reader), wait)
	if err != nil || report.StatusMismatches != count+1 || len(report.StatusMismatchIDs) != 100 {
		t.Fatal("batch accumulation lost counts or exceeded the ID bound", report, err)
	}
	calls := 0
	wait = func(context.Context) error {
		calls++
		if calls == 130 {
			return context.Canceled
		}
		return nil
	}
	if report, err = reconcileWindowPaced(ctx, win, gw, queries.New(reader), wait); !errors.Is(err, context.Canceled) ||
		report.PendingRefundsChecked != 0 {
		t.Fatal("interrupted scan returned a partial verdict", report, err)
	}
}

func TestRunRejectsUnboundedJobDeadlines(t *testing.T) {
	for _, value := range []string{"0", "500ms", "1h1s"} {
		if err := run(context.Background(), []string{"-timeout", value}, func(string) string { return "" }, io.Discard); err == nil ||
			!strings.Contains(err.Error(), "-timeout") {
			t.Fatalf("deadline %s accepted: %v", value, err)
		}
	}
}

func TestLegacyInvoiceGiftsUseVerifiedPaymentLinksAcrossDays(t *testing.T) {
	owner, reader := reconciliationDatabase(t)
	ctx := context.Background()
	win, err := reconcile.DayWindow(time.Date(2035, 3, 1, 0, 0, 0, 0, reconcile.NewYork), 0)
	if err != nil {
		t.Fatal(err)
	}
	id := func() string {
		x, err := store.NewID()
		if err != nil {
			t.Fatal(err)
		}
		return x.String()
	}
	donor, checkout, a, b := id(), id(), id(), id()
	for _, statement := range []string{
		fmt.Sprintf("INSERT INTO donors(id,email) VALUES('%s','legacy@reconciliation.invalid')", donor),
		fmt.Sprintf("INSERT INTO donor_checkouts(id,donor_id,amount_minor,frequency) VALUES('%s','%s',3000,'MONTHLY')", checkout, donor),
	} {
		if _, err := owner.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, gift := range []struct {
		id   string
		paid time.Time
	}{{a, win.Start.Add(time.Hour)}, {b, win.End.Add(time.Hour)}} {
		if _, err := owner.Exec(ctx, "INSERT INTO donor_gifts(id,donor_id,checkout_id,stripe_object_id,amount_minor,currency,frequency,paid_at) VALUES($1,$2,$3,$4,3000,'USD','MONTHLY',$5)", gift.id, donor, checkout, "in_"+gift.id, gift.paid); err != nil {
			t.Fatal(err)
		}
	}
	charge := func(gift string, created time.Time) payments.Object {
		return objects(t, fmt.Sprintf(`{"id":"ch_%s","payment_intent":"pi_%s","amount":3000,"amount_captured":3000,"currency":"usd","status":"succeeded","paid":true,"captured":true,"livemode":false,"created":%d}`, gift, gift, created.Unix()))[0]
	}
	link := func(gift string) payments.Object {
		return objects(t, fmt.Sprintf(`{"id":"inpay_%s","invoice":"in_%s","amount_paid":3000,"currency":"usd","status":"paid","livemode":false,"created":%d,"payment":{"type":"payment_intent","payment_intent":"pi_%s"}}`, gift, gift, win.Start.Unix(), gift))[0]
	}
	oldCharge := charge(a, win.Start.Add(-time.Hour))
	intent := objects(t, fmt.Sprintf(`{"id":"pi_%s","livemode":false,"status":"succeeded"}`, a))[0]
	intent["latest_charge"], err = json.Marshal(oldCharge)
	if err != nil {
		t.Fatal(err)
	}
	gw := fakeGateway{
		charges:         []payments.Object{charge(b, win.Start.Add(time.Hour))},
		paymentsByID:    map[string]payments.Object{"pi_" + a: intent},
		invoicePayments: map[string][]payments.Object{"in_" + a: {link(a)}, "pi_" + b: {link(b)}},
	}
	wait := func(context.Context) error { return nil }
	q := queries.New(reader)
	report, err := reconcileWindowPaced(ctx, win, gw, q, wait)
	if err != nil || report.HasDiscrepancies() {
		t.Fatal("legacy charge proof or cross-day lookup failed", report, err)
	}
	var stored int
	if err := owner.QueryRow(ctx, "SELECT count(*) FROM donor_gifts WHERE donor_id=$1 AND stripe_payment_intent_id IS NULL", donor).Scan(&stored); err != nil ||
		stored != 2 {
		t.Fatal("read-only verification changed financial facts", stored, err)
	}
	proof := gw.invoicePayments["in_"+a][0]
	proof["amount_paid"] = []byte(`1500`)
	if _, err := reconcileWindowPaced(ctx, win, gw, q, wait); err == nil {
		t.Fatal("partial allocation accepted as an entire charge")
	}
	proof["amount_paid"] = []byte(`3000`)
	gw.invoicePayments["in_"+a] = append(gw.invoicePayments["in_"+a], link(a))
	if _, err := reconcileWindowPaced(ctx, win, gw, q, wait); err == nil {
		t.Fatal("ambiguous invoice payments produced a verdict")
	}
	gw.invoicePayments["in_"+a] = nil
	report, err = reconcileWindowPaced(ctx, win, gw, q, wait)
	if err != nil || report.DBWithoutStripe != 1 || report.DBWithoutStripeIDs[0] != "in_"+a {
		t.Fatal("missing invoice proof was not flagged", report, err)
	}
}

func TestInvoiceLinkRejectsUnrelatedAndInvalidProof(t *testing.T) {
	for _, edit := range []func(payments.Object){
		func(o payments.Object) { o["invoice"] = []byte(`"in_other"`) },
		func(o payments.Object) { o["livemode"] = []byte(`true`) },
		func(o payments.Object) { o["status"] = []byte(`"open"`) },
		func(o payments.Object) { o["amount_paid"] = []byte(`null`) },
		func(o payments.Object) {
			o["payment"] = []byte(`{"type":"payment_record","payment_record":"payrec_test"}`)
		},
	} {
		proof := objects(t, `{"id":"inpay_test","invoice":"in_test","amount_paid":3000,"currency":"usd","status":"paid","livemode":false,"created":1000,"payment":{"type":"payment_intent","payment_intent":"pi_test"}}`)[0]
		edit(proof)
		gateway := fakeGateway{invoicePayments: map[string][]payments.Object{"in_test": {proof}}}
		if _, found, err := invoiceLink(context.Background(), gateway, "in_test", ""); err == nil || found {
			t.Fatal("unrelated or invalid invoice proof accepted", err)
		}
	}
}

// TestListSplittingHalvesOnPageCap proves the adaptive sub-day windowing: a
// lister that returns ErrListPageCap for any range wider than a threshold, but
// succeeds on narrow enough ranges, is split until each slice lists cleanly,
// and the slices' objects are concatenated in range order with no gap or
// overlap across the inclusive split boundary.
func TestListSplittingHalvesOnPageCap(t *testing.T) {
	start := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24*time.Hour - time.Second) // inclusive day, like StripeCreatedRange

	// The lister "holds" one object per second in [start,end] and can only
	// return a slice spanning at most 6 hours; a wider slice trips the cap. This
	// forces at least two splits of a full day.
	const maxSpan = 6 * time.Hour
	var calls int
	seen := map[int64]bool{}
	lister := func(_ context.Context, s, e time.Time) ([]payments.Object, error) {
		calls++
		if e.Sub(s) > maxSpan {
			return nil, payments.ErrListPageCap
		}
		out := []payments.Object{}
		for sec := s.Unix(); sec <= e.Unix(); sec++ {
			if seen[sec] {
				t.Fatalf("second %d listed by more than one slice (overlap)", sec)
			}
			seen[sec] = true
			out = append(out, payments.Object{"id": []byte(`"obj_` + strconv.FormatInt(sec, 10) + `"`)})
		}
		return out, nil
	}

	got, err := listSplitting(context.Background(), start, end, lister)
	if err != nil {
		t.Fatalf("listSplitting: %v", err)
	}
	want := int(end.Unix()-start.Unix()) + 1 // one object per inclusive second
	if len(got) != want {
		t.Fatalf("got %d objects, want %d (every second exactly once)", len(got), want)
	}
	if calls < 3 {
		t.Fatalf("expected the day to split into multiple slices, saw %d calls", calls)
	}
	// Every second in the inclusive range must have been covered exactly once.
	for sec := start.Unix(); sec <= end.Unix(); sec++ {
		if !seen[sec] {
			t.Fatalf("second %d was never listed (gap)", sec)
		}
	}
}

// TestListSplittingSurfacesCapForAnUnsplittableSecond proves the recursion
// terminates: a single-second range that still trips the cap returns the error
// rather than looping forever.
func TestListSplittingSurfacesCapForAnUnsplittableSecond(t *testing.T) {
	at := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	lister := func(_ context.Context, _, _ time.Time) ([]payments.Object, error) {
		return nil, payments.ErrListPageCap
	}
	_, err := listSplitting(context.Background(), at, at, lister)
	if !errors.Is(err, payments.ErrListPageCap) {
		t.Fatalf("got %v, want ErrListPageCap for an unsplittable range", err)
	}
}

// TestListSplittingPropagatesNonCapErrors proves a real listing failure is
// returned immediately, not retried by splitting.
func TestListSplittingPropagatesNonCapErrors(t *testing.T) {
	start := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	sentinel := errors.New("boom")
	var calls int
	lister := func(_ context.Context, _, _ time.Time) ([]payments.Object, error) {
		calls++
		return nil, sentinel
	}
	if _, err := listSplitting(context.Background(), start, end, lister); !errors.Is(err, sentinel) {
		t.Fatalf("got %v, want the sentinel error", err)
	}
	if calls != 1 {
		t.Fatalf("a non-cap error must not trigger a split: %d calls", calls)
	}
}

func TestListSplittingStopsAtStripeSecondResolution(t *testing.T) {
	start := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	calls := 0
	sentinel := errors.New("split did not terminate")
	list := func(_ context.Context, s, e time.Time) ([]payments.Object, error) {
		calls++
		if calls > 16 {
			return nil, sentinel
		}
		if s.Unix() > e.Unix() {
			t.Errorf("invalid Stripe interval: %v .. %v", s, e)
		}
		if e.Unix() == start.Add(time.Second).Unix() {
			return nil, payments.ErrListPageCap
		}
		return nil, nil
	}
	_, err := listSplitting(context.Background(), start, start.Add(time.Second), list)
	if !errors.Is(err, payments.ErrListPageCap) {
		t.Fatalf("busy second: %v after %d calls", err, calls)
	}
}
