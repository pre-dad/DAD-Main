// Package reconcile compares processor charges and refunds with stored financial facts.
package reconcile

import (
	"errors"
	"slices"
	"strings"
	"time"
	_ "time/tzdata" // America/New_York resolves inside the stripped container.

	"dad/donor-api/internal/payments"
)

// ErrWindow reports an unusable reconciliation window.
var ErrWindow = errors.New("invalid reconciliation window")

// NewYork is the system's civil-time zone: finance reasons about a day as a
// full New York calendar day, as the Founding cutoff and projection
// boundaries do.
var NewYork = mustLoad("America/New_York")

func mustLoad(name string) *time.Location {
	zone, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return zone
}

// Window is the UTC half-open interval [Start, End) a run reconciles, the New
// York calendar day it represents, and the processing delay required after End.
type Window struct {
	Start  time.Time     `json:"start"`
	End    time.Time     `json:"end"`
	MaxLag time.Duration `json:"max_lag"`
	NYDate string        `json:"ny_date"`
}

// YesterdayWindow returns the previous New York calendar day.
func YesterdayWindow(now time.Time, maxLag time.Duration) (Window, error) {
	ny := now.In(NewYork)
	midnight := time.Date(ny.Year(), ny.Month(), ny.Day(), 0, 0, 0, 0, NewYork)
	return DayWindow(midnight.AddDate(0, 0, -1), maxLag)
}

// DayWindow interprets day in New York, including daylight saving transitions.
func DayWindow(day time.Time, maxLag time.Duration) (Window, error) {
	if maxLag < 0 {
		return Window{}, ErrWindow
	}
	ny := day.In(NewYork)
	start := time.Date(ny.Year(), ny.Month(), ny.Day(), 0, 0, 0, 0, NewYork)
	end := start.AddDate(0, 0, 1)
	return RangeWindow(start, end, maxLag)
}

// RangeWindow keeps explicit half-open bounds in UTC.
func RangeWindow(start, end time.Time, maxLag time.Duration) (Window, error) {
	if start.IsZero() || end.IsZero() || !end.After(start) || maxLag < 0 {
		return Window{}, ErrWindow
	}
	return Window{Start: start.UTC(), End: end.UTC(), MaxLag: maxLag, NYDate: start.In(NewYork).Format("2006-01-02")}, nil
}

// StripeCreatedRange is the inclusive Unix-second range for Stripe's created
// filter. End is inclusive of the window's last second.
func (w Window) StripeCreatedRange() (start, end int64) {
	ceil := func(t time.Time) int64 {
		seconds := t.Unix()
		if t.Nanosecond() != 0 {
			seconds++
		}
		return seconds
	}
	return ceil(w.Start), ceil(w.End) - 1
}

// Settled reports whether the processing delay after the window has passed.
func (w Window) Settled(now time.Time) bool {
	return !now.Before(w.End.Add(w.MaxLag))
}

// Gifts retain PaymentIntent or invoice IDs; charge IDs are a legacy match key.
type Gift struct {
	StripeObjectID        string
	StripePaymentIntentID string
	AmountMinor           int64
	Currency              string
	PaidAt                time.Time
}

type InvoicePayment struct {
	InvoiceID, PaymentIntentID, Currency string
	AmountMinor                          int64
}

// InvoicePaymentFacts validates a Stripe invoice payment and returns the
// invoice and payment intent it links.
func InvoicePaymentFacts(o payments.Object) (InvoicePayment, error) {
	payment, err := o.Object("payment")
	amount, amountErr := o.Int("amount_paid")
	_, createdErr := o.Unix("created")
	if err != nil || amountErr != nil || createdErr != nil || amount <= 0 || !o.BoolIs("livemode", false) ||
		o.Text("status") != "paid" || payment.Text("type") != "payment_intent" ||
		!processorID(o.Text("id"), "inpay_") || !processorID(o.Text("invoice"), "in_") ||
		!processorID(payment.Text("payment_intent"), "pi_") || o.Text("currency") != "usd" {
		return InvoicePayment{}, payments.ErrFacts
	}
	return InvoicePayment{
		InvoiceID:       o.Text("invoice"),
		PaymentIntentID: payment.Text("payment_intent"),
		Currency:        o.Text("currency"),
		AmountMinor:     amount,
	}, nil
}

// Refund is the subset of a donor_refunds row the diff needs.
type Refund struct {
	StripeRefundID string
	AmountMinor    int64
	Currency       string
	Status         string
	ObservedAt     time.Time
}

// Report is the aggregate verdict. Counts drive the alarm; the id lists name
// the offending objects for an operator. IDS ONLY — never donor PII or
// payloads, per the Financial consistency rule in docs/architecture.md.
type Report struct {
	PendingRefundsChecked int      `json:"pending_refunds_checked"`
	PendingRefundBatches  int      `json:"pending_refund_batches"`
	Window                Window   `json:"window"`
	MissingGifts          int      `json:"missing_gifts"`
	MissingRefunds        int      `json:"missing_refunds"`
	AmountMismatches      int      `json:"amount_mismatches"`
	StatusMismatches      int      `json:"status_mismatches"`
	DBWithoutStripe       int      `json:"db_without_stripe"`
	CurrencyMismatches    int      `json:"currency_mismatches"`
	InvalidObjects        int      `json:"invalid_objects"`
	MissingGiftIDs        []string `json:"missing_gift_ids"`
	MissingRefundIDs      []string `json:"missing_refund_ids"`
	AmountMismatchIDs     []string `json:"amount_mismatch_ids"`
	StatusMismatchIDs     []string `json:"status_mismatch_ids"`
	DBWithoutStripeIDs    []string `json:"db_without_stripe_ids"`
	CurrencyMismatchIDs   []string `json:"currency_mismatch_ids"`
	InvalidObjectIDs      []string `json:"invalid_object_ids"`
}

// Add retains full discrepancy counts while bounding accumulated identifiers.
func (r *Report) Add(other Report) {
	r.MissingGifts += other.MissingGifts
	r.MissingRefunds += other.MissingRefunds
	r.AmountMismatches += other.AmountMismatches
	r.StatusMismatches += other.StatusMismatches
	r.DBWithoutStripe += other.DBWithoutStripe
	r.CurrencyMismatches += other.CurrencyMismatches
	r.InvalidObjects += other.InvalidObjects
	for _, pair := range []struct {
		into *[]string
		from []string
	}{
		{&r.MissingGiftIDs, other.MissingGiftIDs}, {&r.MissingRefundIDs, other.MissingRefundIDs},
		{&r.AmountMismatchIDs, other.AmountMismatchIDs}, {&r.StatusMismatchIDs, other.StatusMismatchIDs},
		{&r.DBWithoutStripeIDs, other.DBWithoutStripeIDs}, {&r.CurrencyMismatchIDs, other.CurrencyMismatchIDs},
		{&r.InvalidObjectIDs, other.InvalidObjectIDs},
	} {
		*pair.into = append(*pair.into, pair.from...)
		slices.Sort(*pair.into)
		if len(*pair.into) > maxIDs {
			*pair.into = (*pair.into)[:maxIDs]
		}
	}
}

// maxIDs bounds each id list so a bad day cannot print an unbounded report.
const maxIDs = 100

// HasDiscrepancies reports whether any class has a non-zero count, which the
// CLI turns into a non-zero exit so an alarm fires.
func (r Report) HasDiscrepancies() bool {
	return r.MissingGifts > 0 || r.MissingRefunds > 0 || r.AmountMismatches > 0 ||
		r.StatusMismatches > 0 || r.DBWithoutStripe > 0 || r.CurrencyMismatches > 0 || r.InvalidObjects > 0
}

func processorID(id, prefix string) bool {
	if !strings.HasPrefix(id, prefix) || len(id) <= len(prefix) || len(id) > 255 {
		return false
	}
	for _, r := range id {
		if r != '_' && r != '-' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func financialObject(o payments.Object, prefix string) (string, int64, error) {
	id := o.Text("id")
	amount, err := o.Int("amount")
	_, createdErr := o.Unix("created")
	if !processorID(id, prefix) || amount <= 0 || err != nil || createdErr != nil || !o.BoolIs("livemode", false) ||
		len(o.Text("currency")) != 3 {
		return "", 0, payments.ErrFacts
	}
	for _, r := range strings.ToLower(o.Text("currency")) {
		if r < 'a' || r > 'z' {
			return "", 0, payments.ErrFacts
		}
	}
	return id, amount, nil
}

// CollectedCharge returns the captured amount of a succeeded, paid charge, and
// false for a failed or pending one.
func CollectedCharge(o payments.Object) (int64, bool, error) {
	_, amount, err := financialObject(o, "ch_")
	captured, captureErr := o.Int("amount_captured")
	if err != nil || captureErr != nil || captured < 0 || captured > amount ||
		(!o.BoolIs("paid", true) && !o.BoolIs("paid", false)) ||
		(!o.BoolIs("captured", true) && !o.BoolIs("captured", false)) {
		return 0, false, payments.ErrFacts
	}
	switch o.Text("status") {
	case "failed", "pending":
		if o.BoolIs("paid", true) || captured > 0 {
			return 0, false, payments.ErrFacts
		}
		return 0, false, nil
	case "succeeded":
		if o.BoolIs("paid", true) && o.BoolIs("captured", true) && captured > 0 {
			return captured, true, nil
		}
		return 0, false, nil
	default:
		return 0, false, payments.ErrFacts
	}
}

// refundStatus maps a Stripe lowercase refund status to the uppercase DB enum.
func refundStatus(stripe string) string {
	switch stripe {
	case "pending":
		return "PENDING"
	case "requires_action":
		return "REQUIRES_ACTION"
	case "succeeded":
		return "SUCCEEDED"
	case "failed":
		return "FAILED"
	case "canceled":
		return "CANCELED"
	default:
		return ""
	}
}

// Diff classifies every discrepancy between the Stripe window and the
// database rows. An unattributed-but-present gift is NOT a discrepancy:
// attribution is a downstream projection concern, not money reconciliation.
func Diff(window Window, stripeCharges, stripeRefunds []payments.Object, dbGifts []Gift, dbRefunds []Refund) Report {
	report := Report{
		Window:              window,
		MissingGiftIDs:      []string{},
		MissingRefundIDs:    []string{},
		AmountMismatchIDs:   []string{},
		StatusMismatchIDs:   []string{},
		DBWithoutStripeIDs:  []string{},
		CurrencyMismatchIDs: []string{},
		InvalidObjectIDs:    []string{},
	}

	// Index the database rows by every Stripe key they can match on.
	giftByCharge := map[string]Gift{}
	giftByIntent := map[string]Gift{}
	for _, g := range dbGifts {
		if (!processorID(g.StripeObjectID, "pi_") && !processorID(g.StripeObjectID, "in_") && !processorID(g.StripeObjectID, "ch_")) ||
			g.AmountMinor <= 0 ||
			g.Currency != "USD" {
			report.InvalidObjects++
		}
		if g.StripeObjectID != "" {
			giftByCharge[g.StripeObjectID] = g
		}
		if g.StripePaymentIntentID != "" {
			giftByIntent[g.StripePaymentIntentID] = g
		}
		if strings.HasPrefix(g.StripeObjectID, "pi_") {
			giftByIntent[g.StripeObjectID] = g
		}
	}
	refundByID := map[string]Refund{}
	for _, r := range dbRefunds {
		if !processorID(r.StripeRefundID, "re_") || r.AmountMinor <= 0 || r.Currency != "USD" ||
			refundStatus(strings.ToLower(r.Status)) == "" {
			report.InvalidObjects++
		}
		if r.StripeRefundID != "" {
			refundByID[r.StripeRefundID] = r
		}
	}

	seenGift := map[string]bool{}
	for _, charge := range stripeCharges {
		id := charge.Text("id")
		amount, collected, err := CollectedCharge(charge)
		if err != nil {
			report.InvalidObjects++
			if processorID(id, "ch_") {
				appendID(&report.InvalidObjectIDs, id)
			}
			continue
		}
		if !collected {
			continue
		}
		gift, ok := giftByCharge[id]
		if !ok {
			gift, ok = giftByIntent[charge.Text("payment_intent")]
		}
		if !ok {
			report.MissingGifts++
			appendID(&report.MissingGiftIDs, id)
			continue
		}
		seenGift[gift.StripeObjectID] = true
		if amount != gift.AmountMinor {
			report.AmountMismatches++
			appendID(&report.AmountMismatchIDs, id)
		}
		if !strings.EqualFold(charge.Text("currency"), gift.Currency) {
			report.CurrencyMismatches++
			appendID(&report.CurrencyMismatchIDs, id)
		}
	}

	seenRefund := map[string]bool{}
	for _, refund := range stripeRefunds {
		id, amount, err := financialObject(refund, "re_")
		status := refundStatus(refund.Text("status"))
		if err != nil || status == "" {
			report.InvalidObjects++
			if processorID(refund.Text("id"), "re_") {
				appendID(&report.InvalidObjectIDs, refund.Text("id"))
			}
			continue
		}
		row, ok := refundByID[id]
		if !ok {
			report.MissingRefunds++
			appendID(&report.MissingRefundIDs, id)
			continue
		}
		seenRefund[id] = true
		if amount != row.AmountMinor {
			report.AmountMismatches++
			appendID(&report.AmountMismatchIDs, id)
		}
		if status != row.Status {
			report.StatusMismatches++
			appendID(&report.StatusMismatchIDs, id)
		}
		if !strings.EqualFold(refund.Text("currency"), row.Currency) {
			report.CurrencyMismatches++
			appendID(&report.CurrencyMismatchIDs, id)
		}
	}

	// DB rows with no Stripe counterpart: a recorded fact Stripe does not
	// confirm. Expected to be empty; a hit is reported, never repaired.
	for _, g := range dbGifts {
		if g.StripeObjectID != "" && !seenGift[g.StripeObjectID] {
			report.DBWithoutStripe++
			appendID(&report.DBWithoutStripeIDs, g.StripeObjectID)
		}
	}
	for _, r := range dbRefunds {
		if r.StripeRefundID != "" && !seenRefund[r.StripeRefundID] {
			report.DBWithoutStripe++
			appendID(&report.DBWithoutStripeIDs, r.StripeRefundID)
		}
	}

	sortIDs(&report)
	return report
}

func appendID(list *[]string, id string) {
	valid := processorID(id, "ch_") || processorID(id, "pi_") || processorID(id, "in_") || processorID(id, "re_")
	if valid && len(*list) < maxIDs {
		*list = append(*list, id)
	}
}

// sortIDs makes the report deterministic for a given Stripe/DB state so a
// re-run produces the same output.
func sortIDs(r *Report) {
	for _, list := range [][]string{r.MissingGiftIDs, r.MissingRefundIDs, r.AmountMismatchIDs, r.StatusMismatchIDs, r.DBWithoutStripeIDs, r.CurrencyMismatchIDs, r.InvalidObjectIDs} {
		slices.Sort(list)
	}
}
