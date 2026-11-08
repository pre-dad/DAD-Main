package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestAnalyticsRecord(t *testing.T) {
	_, token, donor := donorServiceFixture(t)
	svc := AnalyticsService{DB: as(t, dbroles.API)}
	pool := database(t)
	ctx := context.Background()
	now := time.Now().UTC()
	anonymous, attributed, invalidToken, rejected := testID(t), testID(t), testID(t), testID(t)
	t.Cleanup(func() {
		cleanExec(t, "DELETE FROM analytics_events WHERE id = ANY($1)", []pgtype.UUID{anonymous, attributed, invalidToken, rejected})
	})
	owner := func(id pgtype.UUID) (pgtype.UUID, bool) {
		var got pgtype.UUID
		err := pool.QueryRow(ctx, "SELECT donor_id FROM analytics_events WHERE id=$1", id).Scan(&got)
		return got, err == nil
	}

	if err := svc.Record(ctx, "WEB", "", []Event{
		{ID: anonymous.String(), Name: "attribution_captured", OccurredAt: now, Properties: map[string]string{"source": "company"}},
	}); err != nil {
		t.Fatalf("anonymous record: %v", err)
	}
	if got, ok := owner(anonymous); !ok || got.Valid {
		t.Fatalf("anonymous event stored with donor %v (stored %v)", got, ok)
	}

	if err := svc.Record(ctx, "IOS", token, []Event{{ID: attributed.String(), Name: "gift_completed", OccurredAt: now}}); err != nil {
		t.Fatalf("authenticated record: %v", err)
	}
	if got, ok := owner(attributed); !ok || got != donor {
		t.Fatalf("attributed event stored with donor %v (stored %v), want %v", got, ok, donor)
	}

	if err := svc.Record(ctx, "WEB", "not-a-valid-session", []Event{{ID: invalidToken.String(), Name: "app_opened", OccurredAt: now}}); err != nil {
		t.Fatalf("an invalid session must record anonymously: %v", err)
	}
	if got, ok := owner(invalidToken); !ok || got.Valid {
		t.Fatalf("invalid-session event stored with donor %v (stored %v)", got, ok)
	}

	good := Event{ID: rejected.String(), Name: "app_opened", OccurredAt: now}
	for _, tc := range []struct {
		name   string
		source string
		events []Event
	}{
		{"unknown name", "WEB", []Event{{Name: "nope", OccurredAt: now}}},
		{"bad property", "WEB", []Event{{Name: "attribution_captured", OccurredAt: now, Properties: map[string]string{"source": "bogus"}}}},
		{"bad source", "DESKTOP", []Event{good}},
		{"empty batch", "WEB", []Event{}},
		{"oversize batch", "WEB", make([]Event, MaxAnalyticsBatch+1)},
		{"missing time", "WEB", []Event{{Name: "app_opened"}}},
		{"future time", "WEB", []Event{{Name: "app_opened", OccurredAt: now.Add(time.Hour)}}},
		{"bad event id", "WEB", []Event{{ID: "nope", Name: "app_opened", OccurredAt: now}}},
		{"bad journey id", "WEB", []Event{{JourneyID: "nope", Name: "app_opened", OccurredAt: now}}},
		{"one bad in batch", "WEB", []Event{good, {Name: "nope", OccurredAt: now}}},
	} {
		if err := svc.Record(ctx, tc.source, "", tc.events); !errors.Is(err, ErrAnalytics) {
			t.Errorf("%s: error = %v; want ErrAnalytics", tc.name, err)
		}
	}
	if _, ok := owner(rejected); ok {
		t.Error("a rejected batch stored its valid event")
	}
}

func TestAnalyticsBatchStoresEveryEventAndIgnoresDuplicates(t *testing.T) {
	svc := AnalyticsService{DB: as(t, dbroles.API)}
	ctx := context.Background()
	now := time.Now().UTC()
	first, second, journey := testID(t), testID(t), testID(t)
	t.Cleanup(func() { cleanExec(t, "DELETE FROM analytics_events WHERE id = ANY($1)", []pgtype.UUID{first, second}) })
	events := []Event{
		{ID: first.String(), JourneyID: journey.String(), Name: "app_opened", OccurredAt: now},
		{ID: second.String(), Name: "attribution_captured", OccurredAt: now, Properties: map[string]string{"source": "company"}},
		{ID: first.String(), JourneyID: journey.String(), Name: "app_opened", OccurredAt: now},
	}
	if err := svc.Record(ctx, "ANDROID", "", events); err != nil {
		t.Fatal(err)
	}
	rows, err := database(
		t,
	).Query(ctx, "SELECT id, source, journey_id, properties->>'source' FROM analytics_events WHERE id = ANY($1)", []pgtype.UUID{first, second})
	if err != nil {
		t.Fatal(err)
	}
	type stored struct {
		source, property string
		journey          pgtype.UUID
	}
	got := map[pgtype.UUID]stored{}
	for rows.Next() {
		var id pgtype.UUID
		var row stored
		var property *string
		if err = rows.Scan(&id, &row.source, &row.journey, &property); err != nil {
			t.Fatal(err)
		}
		if property != nil {
			row.property = *property
		}
		got[id] = row
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := map[pgtype.UUID]stored{
		first:  {source: "ANDROID", journey: journey},
		second: {source: "ANDROID", property: "company"},
	}
	if len(got) != len(want) || got[first] != want[first] || got[second] != want[second] {
		t.Fatalf("stored %+v, want %+v", got, want)
	}
}

func TestAnalyticsEventReplayIsIdempotent(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	api := as(t, dbroles.API)
	s := AnalyticsService{DB: api}
	id, journey := testID(t), testID(t)
	t.Cleanup(func() { cleanExec(t, "DELETE FROM analytics_events WHERE id=$1", id) })
	batch := []Event{{ID: id.String(), JourneyID: journey.String(), Name: "app_opened", OccurredAt: time.Now().UTC()}}
	for range 2 {
		if err := s.Record(ctx, "WEB", "", batch); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM analytics_events WHERE id=$1 AND journey_id=$2", id, journey).Scan(&count); err != nil ||
		count != 1 {
		t.Fatalf("duplicate observations: %d %v", count, err)
	}
}
