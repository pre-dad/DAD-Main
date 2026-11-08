package store

import (
	"context"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/recognition"
	"dad/donor-api/internal/store/queries"
)

func TestHistoricalRetentionSurvivesLaterLapseAndDoesNotCountRestarts(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "continuous", true: "restarted"}[restart], func(t *testing.T) {
			pool := database(t)
			donor, _ := donor(t, pool)
			first := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
			h := history{}
			for month := 0; month < 13; month++ {
				start, end := first.AddDate(0, month, 0), first.AddDate(0, month+1, 0)
				id := mustNewID(t).String()
				h.Periods = append(
					h.Periods,
					recognition.Period{ID: id, DonorID: donor.String(), DueAt: start, EndsAt: end, Status: "EXPECTED"},
				)
				if !restart || month != 1 {
					h.Gifts = append(
						h.Gifts,
						recognition.Gift{
							ID:        "gift-" + id,
							DonorID:   donor.String(),
							Amount:    3000,
							Currency:  "USD",
							Frequency: "MONTHLY",
							PaidAt:    start,
							PeriodID:  id,
						},
					)
				}
			}
			if err := saveRetention(context.Background(), queries.New(as(t, dbroles.API)), donor, h, nil, nil, time.UTC, first.AddDate(1, 1, 0)); err != nil {
				t.Fatal(err)
			}
			rows, err := pool.Query(
				context.Background(),
				"SELECT days,retained FROM donor_retention_milestones WHERE donor_id=$1 ORDER BY days",
				donor,
			)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			count := 0
			for rows.Next() {
				var days int
				var retained bool
				if err := rows.Scan(&days, &retained); err != nil {
					t.Fatal(err)
				}
				want := !restart || days == 30
				if retained != want {
					t.Errorf("day %d retained=%t, want %t", days, retained, want)
				}
				count++
			}
			if rows.Err() != nil || count != 4 {
				t.Fatalf("milestones=%d: %v", count, rows.Err())
			}
		})
	}
}

func TestHistoricalRetentionKeepsUnknownHistoryExplicit(t *testing.T) {
	pool := database(t)
	id, _ := donor(t, pool)
	first := time.Date(2031, 1, 1, 12, 0, 0, 0, time.UTC)
	period := mustNewID(t).String()
	h := history{
		Gifts: []recognition.Gift{
			{ID: "first", DonorID: id.String(), Amount: 3000, Currency: "USD", Frequency: "MONTHLY", PaidAt: first, PeriodID: period},
		},
		Periods: []recognition.Period{{ID: period, DonorID: id.String(), DueAt: first, EndsAt: first.AddDate(0, 1, 0), Status: "EXPECTED"}},
	}
	if err := saveRetention(context.Background(), queries.New(as(t, dbroles.API)), id, h, nil, nil, time.UTC, first.AddDate(1, 1, 0)); err != nil {
		t.Fatal(err)
	}
	var known, unknown int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FILTER(WHERE retained IS NOT NULL),count(*) FILTER(WHERE retained IS NULL) FROM donor_retention_milestones WHERE donor_id=$1`, id).Scan(&known, &unknown); err != nil ||
		known != 1 ||
		unknown != 3 {
		t.Fatalf("known=%d unknown=%d: %v", known, unknown, err)
	}
}

func TestHistoricalRetentionRecordsWhereALaterLapseBegan(t *testing.T) {
	pool := database(t)
	id, _ := donor(t, pool)
	first := time.Date(2032, 1, 1, 12, 0, 0, 0, time.UTC)
	h := history{}
	for month := range 4 {
		start := first.AddDate(0, month, 0)
		period := mustNewID(t).String()
		h.Periods = append(
			h.Periods,
			recognition.Period{ID: period, DonorID: id.String(), DueAt: start, EndsAt: first.AddDate(0, month+1, 0), Status: "EXPECTED"},
		)
		h.Gifts = append(h.Gifts, recognition.Gift{
			ID: "gift-" + period, DonorID: id.String(), Amount: 3000, Currency: "USD", Frequency: "MONTHLY", PaidAt: start, PeriodID: period,
		})
	}
	for month := 4; month < 13; month++ {
		h.Periods = append(h.Periods, recognition.Period{
			ID: mustNewID(
				t,
			).String(),
			DonorID: id.String(), DueAt: first.AddDate(0, month, 0), EndsAt: first.AddDate(0, month+1, 0), Status: "EXPECTED",
		})
	}
	if err := saveRetention(context.Background(), queries.New(as(t, dbroles.API)), id, h, nil, nil, time.UTC, first.AddDate(1, 1, 0)); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(
		context.Background(),
		"SELECT days,retained FROM donor_retention_milestones WHERE donor_id=$1 ORDER BY days",
		id,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[int]bool{}
	for rows.Next() {
		var days int
		var retained bool
		if err := rows.Scan(&days, &retained); err != nil {
			t.Fatal(err)
		}
		got[days] = retained
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	want := map[int]bool{30: true, 90: true, 180: false, 365: false}
	for days, retained := range want {
		if value, ok := got[days]; !ok || value != retained {
			t.Errorf("day %d retained=%v (recorded %v), want %v", days, value, ok, retained)
		}
	}
}
