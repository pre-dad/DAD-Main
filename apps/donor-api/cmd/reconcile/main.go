// Command reconcile is the daily Stripe reconciliation job. It lists the
// charges and refunds Stripe created in a past day and diffs them against the
// gifts and refunds the database recorded, prints an aggregate JSON report to
// stdout, and exits non-zero when any discrepancy is found so a CloudWatch
// alarm fires. It is strictly read-only against both Stripe and the database:
// it never repairs, deletes or writes any financial fact. Remediation stays
// manual — re-send the missing event in Stripe, then
// `worker --kind stripe --retry-event <evt>`.
//
// By default it reconciles yesterday as a full calendar day in
// America/New_York (intended to run ~00:30 NY once the day has closed). A
// -date, or an explicit -since/-until pair, makes a re-run deterministic. A
// -max-lag (default 30m) requires the entire window to have settled before it runs.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/migrations"
	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/reconcile"
	"dad/donor-api/internal/store"
	"dad/donor-api/internal/store/queries"
)

// reconcileRowBound caps the database rows, and the pending refunds, one run
// holds in memory.
const reconcileRowBound = 100000

// window resolves the flags into the reconciliation window. -since/-until win
// when both are set; otherwise -date (or, empty, yesterday in New York).
func window(date, since, until string, maxLag time.Duration, now time.Time) (reconcile.Window, error) {
	switch {
	case since != "" || until != "":
		if since == "" || until == "" {
			return reconcile.Window{}, errors.New("-since and -until must be given together")
		}
		start, err := time.Parse(time.RFC3339, since)
		if err != nil {
			return reconcile.Window{}, errors.New("invalid -since; use RFC3339")
		}
		end, err := time.Parse(time.RFC3339, until)
		if err != nil {
			return reconcile.Window{}, errors.New("invalid -until; use RFC3339")
		}
		return reconcile.RangeWindow(start, end, maxLag)
	case date != "":
		day, err := time.ParseInLocation("2006-01-02", date, reconcile.NewYork)
		if err != nil {
			return reconcile.Window{}, errors.New("invalid -date; use YYYY-MM-DD")
		}
		return reconcile.DayWindow(day, maxLag)
	default:
		return reconcile.YesterdayWindow(now, maxLag)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	flags := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	date := flags.String("date", "", "reconcile this New York calendar day (YYYY-MM-DD); default yesterday in New York")
	since := flags.String("since", "", "window start (RFC3339); use with -until for an explicit range")
	until := flags.String("until", "", "window end, exclusive (RFC3339); use with -since")
	maxLag := flags.Duration("max-lag", 30*time.Minute, "required processing delay after the window closes")
	budget := flags.Duration("timeout", time.Minute, "total read-only job deadline (1s to 1h); raise for a pending-refund backlog")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("invalid reconcile arguments")
	}
	if *budget < time.Second || *budget > time.Hour {
		return errors.New("-timeout must be between 1s and 1h")
	}
	win, err := window(*date, *since, *until, *maxLag, time.Now())
	if err != nil {
		return err
	}
	if !win.Settled(time.Now()) {
		return errors.New("reconciliation window has not settled; wait until its end plus max-lag")
	}

	ceiling, err := payments.RequestsPerSecond(getenv)
	if err != nil {
		return err
	}
	dsn := dbroles.ReconcileJob.URL(getenv)
	if dsn == "" {
		return errors.New(dbroles.ReconcileJob.Variable() + " is required")
	}
	ctx, cancel := context.WithTimeout(ctx, *budget)
	defer cancel()
	pool, err := store.Open(ctx, dsn, dbroles.ReconcileJob.Pool)
	if err != nil {
		return errors.New("reconcile database unavailable")
	}
	defer pool.Close()
	if err = migrations.Verify(ctx, pool); err != nil {
		return errors.New("reconcile schema is not current")
	}
	if err = dbroles.ReconcileJob.Check(ctx, pool, getenv(dbroles.AllowOwnerVariable) == "true"); err != nil {
		return err
	}

	gateway, err := payments.NewStripeGateway(getenv("STRIPE_SECRET_KEY"))
	if err != nil {
		return errors.New("stripe gateway unavailable")
	}
	// The daily job is a single process, so it paces itself within its share.
	requests := payments.NewRateBudget(ceiling)
	gateway.UseBudget(requests)
	wait := func(ctx context.Context) error { return requests.Acquire(ctx, payments.BucketReconcile) }

	var report reconcile.Report
	err = pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		report, err = reconcileWindowPaced(ctx, win, gateway, queries.New(tx), wait)
		return err
	})
	if err != nil {
		return err
	}
	if err = json.NewEncoder(out).Encode(report); err != nil {
		return err
	}
	if report.HasDiscrepancies() {
		return errors.New("stripe reconciliation found discrepancies")
	}
	return nil
}

// reconcileWindowPaced lists the window's Stripe charges and refunds and the
// matching database rows, then diffs them; wait paces each provider read.
// Both sides are read-only.
func reconcileWindowPaced(
	ctx context.Context,
	win reconcile.Window,
	gateway payments.ReconcileGateway,
	q *queries.Queries,
	wait func(context.Context) error,
) (reconcile.Report, error) {
	gte, lte := win.StripeCreatedRange()
	start := time.Unix(gte, 0).UTC()
	end := time.Unix(lte, 0).UTC()
	charges, refunds := []payments.Object{}, []payments.Object{}
	if gte <= lte {
		var err error
		charges, err = listSplitting(ctx, start, end, gateway.ListChargesByCreated)
		if err != nil {
			return reconcile.Report{}, errors.New("stripe charge listing failed")
		}
		refunds, err = listSplitting(ctx, start, end, gateway.ListRefundsByCreated)
		if err != nil {
			return reconcile.Report{}, errors.New("stripe refund listing failed")
		}
	}

	chargeIDs, paymentIDs, refundIDs := []string{}, []string{}, []string{}
	seenCharges, seenRefunds := map[string]bool{}, map[string]bool{}
	for _, charge := range charges {
		_, collected, err := reconcile.CollectedCharge(charge)
		if err != nil || !collected {
			continue
		}
		id, payment := charge.Text("id"), charge.Text("payment_intent")
		chargeIDs, paymentIDs = append(chargeIDs, id), append(paymentIDs, payment)
		seenCharges[id], seenCharges[payment] = true, true
	}
	for _, refund := range refunds {
		id := refund.Text("id")
		refundIDs = append(refundIDs, id)
		seenRefunds[id] = true
	}
	bound := func(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }
	giftParams := queries.ReconciliationGiftsParams{
		Start:          bound(win.Start),
		EndAt:          bound(win.End),
		StripeObjects:  append(chargeIDs, paymentIDs...),
		PaymentIntents: paymentIDs,
	}
	giftRows, err := q.ReconciliationGifts(ctx, giftParams)
	if err != nil {
		return reconcile.Report{}, errors.New("gift window query failed")
	}
	knownPayments := map[string]bool{}
	for _, gift := range giftRows {
		knownPayments[gift.StripePaymentIntentID.String] = true
		if strings.HasPrefix(gift.StripeObjectID, "pi_") {
			knownPayments[gift.StripeObjectID] = true
		}
	}
	invoiceLinks := map[string]reconcile.InvoicePayment{}
	for _, charge := range charges {
		amount, collected, err := reconcile.CollectedCharge(charge)
		payment := charge.Text("payment_intent")
		if err != nil || !collected || !strings.HasPrefix(payment, "pi_") || knownPayments[payment] {
			continue
		}
		link, found, err := invoiceLink(ctx, gateway, "", payment)
		if err != nil {
			return reconcile.Report{}, err
		}
		if !found {
			continue
		}
		if link.AmountMinor != amount || link.Currency != charge.Text("currency") {
			return reconcile.Report{}, errors.New("invoice payment allocation is not a complete charge")
		}
		if previous, ok := invoiceLinks[link.InvoiceID]; ok && previous.PaymentIntentID != payment {
			return reconcile.Report{}, errors.New("invoice has multiple collected payments; operator review required")
		}
		invoiceLinks[link.InvoiceID] = link
		giftParams.StripeObjects = append(giftParams.StripeObjects, link.InvoiceID)
	}
	if len(invoiceLinks) > 0 {
		giftRows, err = q.ReconciliationGifts(ctx, giftParams)
		if err != nil {
			return reconcile.Report{}, errors.New("legacy gift lookup failed")
		}
	}
	refundRows, err := q.ReconciliationRefunds(
		ctx,
		queries.ReconciliationRefundsParams{Start: bound(win.Start), EndAt: bound(win.End), StripeRefunds: refundIDs},
	)
	if err != nil {
		return reconcile.Report{}, errors.New("refund window query failed")
	}
	// The diff needs the complete DB set for the window in memory to compare it
	// against the complete Stripe set, so this bounds memory rather than the
	// window length — Stripe listing itself is no longer day-capped (it splits
	// sub-day; see listSplitting). A window whose DB side exceeds this bound is
	// rejected with guidance to narrow it; streaming the DB-side diff to lift
	// this bound entirely is a follow-up for horizon-scale daily volumes.
	if len(giftRows) > reconcileRowBound || len(refundRows) > reconcileRowBound {
		return reconcile.Report{}, errors.New("reconciliation window exceeds the in-memory row bound; use a smaller -since/-until range")
	}

	gifts := make([]reconcile.Gift, 0, len(giftRows))
	for _, g := range giftRows {
		var allocation *reconcile.InvoicePayment
		payment := g.StripePaymentIntentID.String
		if payment == "" && strings.HasPrefix(g.StripeObjectID, "pi_") {
			payment = g.StripeObjectID
		}
		if payment == "" && strings.HasPrefix(g.StripeObjectID, "in_") {
			link, found := invoiceLinks[g.StripeObjectID]
			if !found {
				var err error
				link, found, err = invoiceLink(ctx, gateway, g.StripeObjectID, "")
				if err != nil {
					return reconcile.Report{}, err
				}
			}
			if found {
				payment = link.PaymentIntentID
				allocation = &link
			}
		}
		gifts = append(gifts, reconcile.Gift{
			StripeObjectID:        g.StripeObjectID,
			StripePaymentIntentID: payment,
			AmountMinor:           g.AmountMinor,
			Currency:              g.Currency,
			PaidAt:                g.PaidAt.Time,
		})
		if seenCharges[g.StripeObjectID] || (payment != "" && seenCharges[payment]) {
			continue
		}
		if payment == "" {
			continue
		}
		if err := wait(ctx); err != nil {
			return reconcile.Report{}, err
		}
		intent, err := gateway.RetrievePaymentIntent(ctx, payment)
		if err != nil {
			return reconcile.Report{}, errors.New("stripe payment verification failed")
		}
		if intent.Text("id") != payment || !intent.BoolIs("livemode", false) {
			return reconcile.Report{}, errors.New("stripe payment verification returned invalid facts")
		}
		if intent.Text("status") != "succeeded" {
			continue
		}
		charge, err := intent.Object("latest_charge")
		if err != nil || charge.Text("payment_intent") != payment {
			return reconcile.Report{}, errors.New("stripe payment charge verification returned invalid facts")
		}
		if allocation != nil {
			amount, collected, err := reconcile.CollectedCharge(charge)
			if err != nil || !collected || allocation.AmountMinor != amount || allocation.Currency != charge.Text("currency") {
				return reconcile.Report{}, errors.New("invoice payment allocation is not a complete charge")
			}
		}
		charges = append(charges, payments.ReconciliationFacts(charge))
		seenCharges[payment] = true
	}
	dbRefunds := make([]reconcile.Refund, 0, len(refundRows))
	for _, r := range refundRows {
		dbRefunds = append(dbRefunds, reconcile.Refund{
			StripeRefundID: r.StripeRefundID,
			AmountMinor:    r.AmountMinor,
			Currency:       r.Currency,
			Status:         r.Status,
			ObservedAt:     r.ObservedAt.Time,
		})
		if !seenRefunds[r.StripeRefundID] {
			if err := wait(ctx); err != nil {
				return reconcile.Report{}, err
			}
			refund, err := gateway.RetrieveRefund(ctx, r.StripeRefundID)
			if err != nil {
				return reconcile.Report{}, errors.New("stripe refund verification failed")
			}
			if refund.Text("id") != r.StripeRefundID {
				return reconcile.Report{}, errors.New("stripe refund verification returned invalid facts")
			}
			refunds = append(refunds, payments.ReconciliationFacts(refund))
		}
	}
	report := reconcile.Diff(win, charges, refunds, gifts, dbRefunds)
	excluded := make([]string, 0, len(dbRefunds))
	for _, refund := range dbRefunds {
		excluded = append(excluded, refund.StripeRefundID)
	}
	var after pgtype.UUID
	for {
		rows, err := q.PendingReconciliationRefunds(
			ctx,
			queries.PendingReconciliationRefundsParams{AfterID: after, ExcludedRefunds: excluded},
		)
		if err != nil {
			return reconcile.Report{}, errors.New("pending refund batch query failed")
		}
		if len(rows) == 0 {
			return report, nil
		}
		if report.PendingRefundsChecked+len(rows) > reconcileRowBound {
			return reconcile.Report{}, errors.New("pending refund backlog exceeds the in-memory row bound")
		}
		remote := make([]payments.Object, 0, len(rows))
		local := make([]reconcile.Refund, 0, len(rows))
		for _, row := range rows {
			if err := wait(ctx); err != nil {
				return reconcile.Report{}, err
			}
			refund, err := gateway.RetrieveRefund(ctx, row.StripeRefundID)
			if err != nil || refund.Text("id") != row.StripeRefundID {
				return reconcile.Report{}, errors.New("stripe pending refund verification failed")
			}
			remote = append(remote, payments.ReconciliationFacts(refund))
			local = append(
				local,
				reconcile.Refund{
					StripeRefundID: row.StripeRefundID,
					AmountMinor:    row.AmountMinor,
					Currency:       row.Currency,
					Status:         row.Status,
					ObservedAt:     row.ObservedAt.Time,
				},
			)
		}
		report.Add(reconcile.Diff(win, nil, remote, nil, local))
		report.PendingRefundsChecked += len(rows)
		report.PendingRefundBatches++
		after = rows[len(rows)-1].ID
	}
}

// listSplitting lists [start, end] through list, and when a range holds more
// objects than one listing call may return (ErrListPageCap), halves the range
// and lists each half, so an arbitrarily busy day is covered without raising
// the per-call ceiling. Objects are returned in range order; the day's created
// filter is inclusive on both ends, so the split point is kept out of the left
// half (left ends one second before mid) to avoid returning a boundary object
// twice. A range that cannot be split further (a single second over the cap)
// surfaces ErrListPageCap rather than looping.
func listSplitting(
	ctx context.Context,
	start, end time.Time,
	list func(context.Context, time.Time, time.Time) ([]payments.Object, error),
) ([]payments.Object, error) {
	objects, err := list(ctx, start, end)
	if err == nil {
		return objects, nil
	}
	if !errors.Is(err, payments.ErrListPageCap) {
		return nil, err
	}
	if !end.After(start) {
		return nil, err
	}
	mid := start.Add(end.Sub(start) / 2)
	left, err := listSplitting(ctx, start, mid.Add(-time.Second), list)
	if err != nil {
		return nil, err
	}
	right, err := listSplitting(ctx, mid, end, list)
	if err != nil {
		return nil, err
	}
	return append(left, right...), nil
}

func invoiceLink(ctx context.Context, gateway payments.ReconcileGateway, invoice, payment string) (reconcile.InvoicePayment, bool, error) {
	rows, err := gateway.ListInvoicePayments(ctx, invoice, payment)
	if err != nil {
		return reconcile.InvoicePayment{}, false, errors.New("stripe invoice payment lookup failed")
	}
	if len(rows) == 0 {
		return reconcile.InvoicePayment{}, false, nil
	}
	if len(rows) != 1 {
		return reconcile.InvoicePayment{}, false, errors.New("invoice payment mapping is ambiguous; operator review required")
	}
	link, err := reconcile.InvoicePaymentFacts(rows[0])
	if err != nil || (invoice != "" && link.InvoiceID != invoice) || (payment != "" && link.PaymentIntentID != payment) {
		return reconcile.InvoicePayment{}, false, errors.New("stripe invoice payment lookup returned invalid facts")
	}
	return link, true, nil
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
