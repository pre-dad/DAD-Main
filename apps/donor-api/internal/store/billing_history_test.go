package store

import (
	"context"
	"encoding/csv"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"dad/donor-api/internal/store/queries"
)

func TestBillingHistoryPreservesFailuresRecoveryAndReplay(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		name := "failure_then_recovery"
		if reverse {
			name = "recovery_delivered_first"
		}
		t.Run(name, func(t *testing.T) {
			f := newWorkerFixture(t, "MONTHLY")
			checkout := f.checkout(t)
			ctx := context.Background()
			start := time.Now().UTC().Truncate(time.Second).Add(-20 * 24 * time.Hour)
			invoiceID := "in_history_" + f.donorID.String()
			paidAt := start.Add(15 * 24 * time.Hour)
			paid := invoice(checkout.CheckoutID, invoiceID, start, true)
			paid["status_transitions"] = map[string]any{"paid_at": paidAt.Unix()}
			failed := invoice(checkout.CheckoutID, invoiceID, start, false)
			paidEvent := ""
			failure := func() {
				f.receiveAt(t, "invoice.payment_failed", failed, start.Add(time.Minute))
				f.process(t)
				f.receiveAt(t, "invoice.payment_failed", failed, start.Add(7*24*time.Hour))
				f.process(t)
			}
			success := func() {
				paidEvent = f.receiveAt(t, "invoice.payment_succeeded", paid, paidAt.Add(time.Hour))
				f.process(t)
			}
			if reverse {
				success()
				failure()
			} else {
				failure()
				success()
			}
			// Reprocessing the same verified event cannot mint another observation.
			if _, err := f.db.Exec(ctx, "UPDATE donor_stripe_events SET processed_at=NULL,available_at=now() WHERE stripe_event_id=$1", paidEvent); err != nil {
				t.Fatal(err)
			}
			f.process(t)
			rows, err := f.db.Query(
				ctx,
				`SELECT o.status,o.amount_minor,o.currency,o.billing_reason,o.occurred_at,o.event_created_at,o.recorded_at,p.status
				FROM donor_billing_observations o JOIN donor_billing_periods p ON p.id=o.period_id
				WHERE p.donor_id=$1 ORDER BY o.occurred_at`,
				f.donorID,
			)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			statuses := []string{}
			for rows.Next() {
				var status, currency, reason, current string
				var amount int64
				var occurred, created, recorded time.Time
				if err := rows.Scan(&status, &amount, &currency, &reason, &occurred, &created, &recorded, &current); err != nil {
					t.Fatal(err)
				}
				statuses = append(statuses, status)
				if amount != 3000 || currency != "USD" || reason != "subscription_cycle" || current != "PAID" || !recorded.After(created) {
					t.Fatalf("incorrect observation: %s %d %s %s %s %v", status, amount, currency, reason, current, recorded)
				}
				if status == "PAID" && (!occurred.Equal(paidAt) || !created.Equal(paidAt.Add(time.Hour))) {
					t.Fatal("payment time conflated with event creation")
				}
				if status == "FAILED" && !occurred.Equal(created) {
					t.Fatal("failure time not preserved from verified event")
				}
			}
			if err := rows.Err(); err != nil || !slices.Equal(statuses, []string{"FAILED", "FAILED", "PAID"}) {
				t.Fatalf("history lost or duplicated: %v %v", statuses, err)
			}
			rows.Close()
			churn, err := queries.New(f.service.DB).
				ReportInvoluntaryChurn(ctx, queries.ReportInvoluntaryChurnParams{StartAt: timestamp(start), EndAt: timestamp(start.Add(24 * time.Hour)), Cutoff: timestamp(time.Now().UTC())})
			if err != nil || churn.FailedPeriods != 1 || churn.LostMinor != 3000 || churn.RecoveredPeriods != 1 ||
				churn.RecoveredMinor != 3000 ||
				churn.RecoveredWithinGrace != 0 {
				t.Fatalf("late recovery must retain its failure, once, in either delivery order: %+v %v", churn, err)
			}
		})
	}
}

func TestBillingHistoryExportsVerifiedFactsWithFinancialPermission(t *testing.T) {
	f := newWorkerFixture(t, "MONTHLY")
	checkout := f.checkout(t)
	ctx := context.Background()
	start := time.Now().UTC().Add(-24 * time.Hour)
	f.receiveAt(t, "invoice.payment_failed", invoice(checkout.CheckoutID, "in_export_"+f.donorID.String(), start, false), start)
	f.process(t)
	_, finance := staffFixture(t, f.db, "SUPER_ADMIN")
	_, support := staffFixture(t, f.db, "OP_PARTNERSHIPS")
	s, worker := newExportRig(t)
	if _, err := s.RequestExport(ctx, support, "billing-history"); !errors.Is(err, ErrStaffForbidden) {
		t.Fatalf("support exported billing history: %v", err)
	}
	rows, err := csv.NewReader(strings.NewReader(exportCSV(t, s, worker, finance, "billing-history"))).ReadAll()
	if err != nil || len(rows) < 2 {
		t.Fatalf("missing billing export: %v", err)
	}
	fields := rows[0]
	for _, field := range []string{"period_id", "source_event_id", "status", "amount_minor", "currency", "billing_reason", "occurred_at", "event_created_at", "recorded_at"} {
		if !slices.Contains(fields, field) {
			t.Fatalf("missing field %s", field)
		}
	}
	var period string
	if err := f.db.QueryRow(ctx, "SELECT id::text FROM donor_billing_periods WHERE donor_id=$1", f.donorID).Scan(&period); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows[1:] {
		if row[slices.Index(fields, "period_id")] == period {
			found = true
			if row[slices.Index(fields, "status")] != "FAILED" || row[slices.Index(fields, "amount_minor")] != "3000" {
				t.Fatalf("incorrect exported failure: %v", row)
			}
		}
	}
	if !found {
		t.Fatal("verified failure absent from export")
	}
}
