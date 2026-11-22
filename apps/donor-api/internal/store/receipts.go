package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"dad/donor-api/internal/recognition"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5/pgtype"
)

var ErrReceipt = errors.New("gift record unavailable")

type RefundRecord struct {
	ID         string `json:"id"`
	Amount     int64  `json:"amount_minor"`
	Status     string `json:"status"`
	ObservedAt string `json:"observed_at"`
}

type GiftRecord struct {
	ID        string         `json:"id"`
	Amount    int64          `json:"amount_minor"`
	Currency  string         `json:"currency"`
	PaidAt    string         `json:"paid_at"`
	Frequency string         `json:"frequency"`
	Refunded  int64          `json:"refunded_minor"`
	Net       int64          `json:"net_minor"`
	Refunds   []RefundRecord `json:"refunds"`
}

type Receipt struct {
	Mode       string     `json:"mode"`
	TaxReceipt bool       `json:"tax_receipt"`
	AsOf       string     `json:"as_of"`
	ID         string     `json:"receipt_id"`
	Gift       GiftRecord `json:"gift"`
}

type AnnualSummary struct {
	Mode       string       `json:"mode"`
	TaxReceipt bool         `json:"tax_receipt"`
	Year       int          `json:"year"`
	Timezone   string       `json:"timezone"`
	AsOf       string       `json:"as_of"`
	Gifts      []GiftRecord `json:"gifts"`
	Amount     int64        `json:"amount_minor"`
	Refunded   int64        `json:"refunded_minor"`
	Net        int64        `json:"net_minor"`
	Currency   string       `json:"currency"`
}

func iso(t time.Time) string {
	t = t.UTC().Truncate(time.Microsecond)
	if t.Nanosecond() == 0 {
		return t.Format("2006-01-02T15:04:05+00:00")
	}
	return t.Format("2006-01-02T15:04:05.000000+00:00")
}

func readGiftRecord(ctx context.Context, q *queries.Queries, donor, gift pgtype.UUID, now time.Time) (GiftRecord, error) {
	out := GiftRecord{Refunds: []RefundRecord{}}
	row, err := q.ReceiptGift(ctx, queries.ReceiptGiftParams{ID: gift, DonorID: donor, PaidAt: timestamp(now)})
	if err != nil {
		return out, err
	}
	refunds, err := q.ReceiptRefunds(ctx, gift)
	if err != nil {
		return out, err
	}
	return giftRecord(row, refunds)
}

func giftRecord(row queries.DonorGift, refunds []queries.DonorRefund) (GiftRecord, error) {
	out := GiftRecord{Refunds: []RefundRecord{}}
	if row.AmountMinor <= 0 || row.Currency != "USD" {
		return out, ErrReceipt
	}
	out.ID = row.ID.String()
	out.Amount = row.AmountMinor
	out.Currency = row.Currency
	out.PaidAt = iso(row.PaidAt.Time)
	out.Frequency = row.Frequency
	for _, r := range refunds {
		if r.Currency != "USD" || r.AmountMinor <= 0 {
			return out, ErrReceipt
		}
		if r.Status == "SUCCEEDED" {
			if r.AmountMinor > out.Amount-out.Refunded {
				return out, ErrReceipt
			}
			out.Refunded += r.AmountMinor
		}
		out.Refunds = append(
			out.Refunds,
			RefundRecord{ID: r.ID.String(), Amount: r.AmountMinor, Status: r.Status, ObservedAt: iso(r.ObservedAt.Time)},
		)
	}
	out.Net = out.Amount - out.Refunded
	return out, nil
}

func readReceipt(ctx context.Context, q *queries.Queries, donor, gift pgtype.UUID, now time.Time) (Receipt, error) {
	record, err := readGiftRecord(ctx, q, donor, gift, now)
	return Receipt{Mode: processorMode, AsOf: iso(now), ID: "DAD-TEST-" + gift.String(), Gift: record}, err
}

// sandboxStartYear is the first year with any gifts; earlier summaries are refused.
const sandboxStartYear = 2026

func readAnnualSummary(ctx context.Context, q *queries.Queries, donor pgtype.UUID, year int, now time.Time) (AnnualSummary, error) {
	zone := recognition.NewYork
	out := AnnualSummary{Mode: processorMode, Year: year, Timezone: zone.String(), AsOf: iso(now), Currency: "USD", Gifts: []GiftRecord{}}
	if year < sandboxStartYear || year > now.In(zone).Year() {
		return out, ErrReceipt
	}
	rangeParams := queries.AnnualGiftsParams{
		DonorID:  donor,
		StartsAt: timestamp(time.Date(year, 1, 1, 0, 0, 0, 0, zone)),
		EndsAt:   timestamp(time.Date(year+1, 1, 1, 0, 0, 0, 0, zone)),
		AsOf:     timestamp(now),
	}
	gifts, err := q.AnnualGifts(ctx, rangeParams)
	if err != nil {
		return out, err
	}
	refunds, err := q.AnnualRefunds(ctx, queries.AnnualRefundsParams(rangeParams))
	if err != nil {
		return out, err
	}
	byGift := make(map[pgtype.UUID][]queries.DonorRefund)
	for _, refund := range refunds {
		byGift[refund.GiftID] = append(byGift[refund.GiftID], refund)
	}
	for _, row := range gifts {
		gift, err := giftRecord(row, byGift[row.ID])
		if err != nil {
			return out, err
		}
		if gift.Amount > math.MaxInt64-out.Amount {
			return out, ErrReceipt
		}
		out.Amount += gift.Amount
		out.Refunded += gift.Refunded
		out.Net += gift.Net
		out.Gifts = append(out.Gifts, gift)
	}
	return out, nil
}

func money(value int64) string {
	dollars := strconv.FormatInt(value/100, 10)
	for pos := len(dollars) - 3; pos > 0; pos -= 3 {
		dollars = dollars[:pos] + "," + dollars[pos:]
	}
	return fmt.Sprintf("$%s.%02d", dollars, value%100)
}

func renderReceipt(r Receipt) string {
	g := r.Gift
	frequency := "One-time"
	if g.Frequency == "MONTHLY" {
		frequency = "Monthly"
	}
	return "DAD sandbox gift record " + r.ID + "\nTEST PAYMENT — NOT A TAX RECEIPT. No real donation was collected.\n\n" +
		frequency + " test gift: " + money(g.Amount) + " USD\nCompleted: " + g.PaidAt + "\nSuccessful refunds: " + money(g.Refunded) + " USD\nNet test amount: " + money(g.Net) + " USD\nAs of: " + r.AsOf + "\n\n" +
		"DAD launches in April 2027. A first gift completed before 1 January 2027 (New York time) earns permanent Founding Donor recognition; recognition in this sandbox is simulated.\n"
}

func renderSummary(s AnnualSummary) string {
	rows := make([]string, 0, len(s.Gifts))
	for _, g := range s.Gifts {
		rows = append(rows, g.PaidAt+" | "+money(g.Amount)+" | refunds "+money(g.Refunded)+" | "+g.ID)
	}
	detail := strings.Join(rows, "\n")
	if detail == "" {
		detail = "No confirmed gifts in this year."
	}
	return fmt.Sprintf(
		"DAD sandbox annual summary — %d\nTEST PAYMENTS — NOT A TAX RECEIPT. No real donations were collected.\n\nCalendar year uses %s. As of %s.\nRefund adjustments reflect the latest verified status for gifts in this year, including adjustments observed in a later year. This is not a tax deduction calculation.\n\n%s\n\nCollected: %s USD\nSuccessful refunds: %s USD\nNet: %s USD\n",
		s.Year,
		s.Timezone,
		s.AsOf,
		detail,
		money(s.Amount),
		money(s.Refunded),
		money(s.Net),
	)
}
