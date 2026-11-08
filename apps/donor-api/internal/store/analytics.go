package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"dad/donor-api/internal/analytics"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrAnalytics is returned when a batch is malformed: too large, a bad source,
// an unknown event name, or an invalid property payload. The whole batch is
// rejected rather than partially stored, so a client never half-records a page.
var ErrAnalytics = errors.New("invalid analytics batch")

// MaxAnalyticsBatch bounds one ingest call. A client buffers events and flushes
// in batches; this caps the work and payload per request.
const MaxAnalyticsBatch = 100

// AnalyticsService ingests §9 analytics events into the shared append-only
// stream.
type AnalyticsService struct{ DB *pgxpool.Pool }

// Event is one client-submitted analytics event. OccurredAt is the client's
// event time; Properties is the event's typed payload (may be empty).
type Event struct {
	ID         string
	JourneyID  string
	Name       string
	OccurredAt time.Time
	Properties map[string]string
}

// validSource is the closed set matching the ck_analytics_source constraint.
func validSource(s string) bool { return s == "WEB" || s == "IOS" || s == "ANDROID" }

// Record validates and stores a batch of events for the given platform source.
// token is the caller's donor session if any: a valid token attributes the
// events to that donor, and an empty or invalid token records them anonymously
// (an event before sign-in is expected and must not be rejected). The batch is
// validated in full before any insert, so an invalid event rejects the whole
// batch, and stored in one statement, so it is all stored or none of it is.
func (s AnalyticsService) Record(ctx context.Context, source, token string, events []Event) error {
	if !validSource(source) {
		return ErrAnalytics
	}
	if len(events) == 0 || len(events) > MaxAnalyticsBatch {
		return ErrAnalytics
	}
	now := time.Now().UTC()

	// Resolve the optional session leniently: attribute to the donor when the
	// token authenticates, otherwise stay anonymous. An auth failure is never a
	// batch error — analytics must accept pre-sign-in events.
	var donor pgtype.UUID
	if token != "" {
		if user, err := authenticate(ctx, queries.New(s.DB), token, now); err == nil {
			donor = user.ID
		} else if !errors.Is(err, ErrDenied) {
			return err
		}
	}

	batch := queries.InsertObservedEventsParams{
		Ids:        make([]pgtype.UUID, 0, len(events)),
		Names:      make([]string, 0, len(events)),
		DonorID:    donor,
		Source:     source,
		Properties: make([][]byte, 0, len(events)),
		OccurredAt: make([]pgtype.Timestamptz, 0, len(events)),
		Journeys:   make([]pgtype.UUID, 0, len(events)),
	}
	for _, e := range events {
		if !analytics.Known(e.Name) {
			return ErrAnalytics
		}
		if err := analytics.ValidateProperties(e.Name, e.Properties); err != nil {
			return ErrAnalytics
		}
		// A missing time, or one implausibly in the future, is clock abuse;
		// modest skew is allowed.
		if e.OccurredAt.IsZero() || e.OccurredAt.After(now.Add(5*time.Minute)) {
			return ErrAnalytics
		}
		payload := []byte("{}")
		if len(e.Properties) > 0 {
			encoded, err := json.Marshal(e.Properties)
			if err != nil {
				return ErrAnalytics
			}
			payload = encoded
		}
		var id, journey pgtype.UUID
		if e.ID == "" {
			var err error
			if id, err = NewID(); err != nil {
				return err
			}
		} else if id.Scan(e.ID) != nil || !ValidRequestID(id) {
			return ErrAnalytics
		}
		if e.JourneyID != "" && (journey.Scan(e.JourneyID) != nil || !ValidRequestID(journey)) {
			return ErrAnalytics
		}
		batch.Ids = append(batch.Ids, id)
		batch.Names = append(batch.Names, e.Name)
		batch.Properties = append(batch.Properties, payload)
		batch.OccurredAt = append(batch.OccurredAt, timestamp(e.OccurredAt))
		batch.Journeys = append(batch.Journeys, journey)
	}
	return queries.New(s.DB).InsertObservedEvents(ctx, batch)
}

// recordServerEvent lives in the same transaction as its authoritative fact.
func recordServerEvent(ctx context.Context, q *queries.Queries, donor pgtype.UUID, name, key string, now time.Time) error {
	id, err := NewID()
	if err != nil {
		return err
	}
	return q.InsertObservedEvent(
		ctx,
		queries.InsertObservedEventParams{
			ID:         id,
			DonorID:    donor,
			Name:       name,
			Source:     "SERVER",
			Properties: []byte("{}"),
			OccurredAt: timestamp(now),
			DedupKey:   text(key),
		},
	)
}
