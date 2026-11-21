package store

import (
	"context"
	"maps"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/recognition"
	"dad/donor-api/internal/store/queries"

	"github.com/jackc/pgx/v5/pgtype"
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
		period := mustNewID(t).String()
		h.Periods = append(h.Periods, recognition.Period{
			ID: period, DonorID: id.String(), DueAt: first.AddDate(0, month, 0), EndsAt: first.AddDate(0, month+1, 0), Status: "EXPECTED",
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

// paidMonths builds a donor history with one paid monthly period per month from
// first, for the given number of months.
func paidMonths(t *testing.T, id pgtype.UUID, first time.Time, months int) history {
	t.Helper()
	h := history{}
	for month := range months {
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
	return h
}

// retainedByDays reads the donor's recorded retention verdicts keyed by checkpoint days.
func retainedByDays(t *testing.T, id pgtype.UUID) map[int]bool {
	t.Helper()
	rows, err := database(t).Query(context.Background(), "SELECT days,retained FROM donor_retention_milestones WHERE donor_id=$1", id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[int]bool{}
	for rows.Next() {
		var days int
		var retained pgtype.Bool
		if err := rows.Scan(&days, &retained); err != nil {
			t.Fatal(err)
		}
		if !retained.Valid {
			t.Fatalf("day %d retention unknown, want a verdict", days)
		}
		got[days] = retained.Bool
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	return got
}

func TestHistoricalRetentionIgnoresRefundsObservedAfterTheCheckpoint(t *testing.T) {
	id, _ := donor(t, database(t))
	first := time.Date(2033, 1, 1, 12, 0, 0, 0, time.UTC)
	h := paidMonths(t, id, first, 13)
	h.Refunds = []recognition.Refund{{
		ID: "re_" + mustNewID(t).String(), GiftID: h.Gifts[0].ID, Amount: 3000, Status: "SUCCEEDED", Currency: "USD",
		UpdatedAt: first.AddDate(0, 0, 45),
	}}
	if err := saveRetention(context.Background(), queries.New(as(t, dbroles.API)), id, h, nil, nil, time.UTC, first.AddDate(1, 1, 0)); err != nil {
		t.Fatal(err)
	}
	want := map[int]bool{30: true, 90: false, 180: false, 365: false}
	if got := retainedByDays(t, id); !maps.Equal(got, want) {
		t.Fatalf("retained by checkpoint = %v, want %v (the refund postdates only the 30-day checkpoint)", got, want)
	}
}

func TestHistoricalRetentionStartsANewRunAfterAStop(t *testing.T) {
	id, _ := donor(t, database(t))
	first := time.Date(2034, 1, 1, 12, 0, 0, 0, time.UTC)
	h := paidMonths(t, id, first, 13)
	stops := []time.Time{first.AddDate(0, 1, 0)}
	if err := saveRetention(context.Background(), queries.New(as(t, dbroles.API)), id, h, nil, stops, time.UTC, first.AddDate(1, 1, 0)); err != nil {
		t.Fatal(err)
	}
	want := map[int]bool{30: true, 90: false, 180: false, 365: false}
	if got := retainedByDays(t, id); !maps.Equal(got, want) {
		t.Fatalf("retained by checkpoint = %v, want %v (renewals stopped after the first month)", got, want)
	}
}

func TestHistoricalRetentionCheckpointsFollowTheZoneAcrossDST(t *testing.T) {
	id, _ := donor(t, database(t))
	// 07:00 New York in January; 90 days later the clocks have moved forward.
	first := time.Date(2035, 1, 1, 12, 0, 0, 0, time.UTC)
	h := paidMonths(t, id, first, 1)
	if err := saveRetention(context.Background(), queries.New(as(t, dbroles.API)), id, h, nil, nil, recognition.NewYork, first.AddDate(0, 0, 10)); err != nil {
		t.Fatal(err)
	}
	checkpoints := map[int]time.Time{}
	rows, err := database(t).Query(context.Background(), "SELECT days,checkpoint_at FROM donor_retention_milestones WHERE donor_id=$1", id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var days int
		var at time.Time
		if err := rows.Scan(&days, &at); err != nil {
			t.Fatal(err)
		}
		checkpoints[days] = at.UTC()
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	want := map[int]time.Time{
		30:  time.Date(2035, 1, 31, 12, 0, 0, 0, time.UTC),
		90:  time.Date(2035, 4, 1, 11, 0, 0, 0, time.UTC),
		180: time.Date(2035, 6, 30, 11, 0, 0, 0, time.UTC),
		365: time.Date(2036, 1, 1, 12, 0, 0, 0, time.UTC),
	}
	if !maps.EqualFunc(checkpoints, want, time.Time.Equal) {
		t.Fatalf("checkpoints = %v, want %v", checkpoints, want)
	}
}
