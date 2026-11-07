package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"dad/donor-api/internal/store"
	"github.com/jackc/pgx/v5/pgtype"
)

type AnalyticsSource interface {
	Record(ctx context.Context, source, token string, events []store.Event) error
}

const analyticsUnavailable = "Analytics is temporarily unavailable"

func (a *api) analytics() service {
	return service{a.Analytics != nil, analyticsUnavailable, readTimeout, analyticsStatus}
}

func analyticsStatus(err error) (int, string) {
	switch {
	case errors.Is(err, store.ErrAnalytics):
		return http.StatusUnprocessableEntity, "The analytics batch is invalid"
	default:
		return http.StatusServiceUnavailable, analyticsUnavailable
	}
}

// ingestAnalytics accepts a batch of §9 events. It is authenticated-or-anon: a
// valid donor session attributes the events, otherwise they are anonymous. The
// platform source is taken from the X-DAD-Client header (native) or defaults to
// web for a browser request.
func (a *api) ingestAnalytics(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Events []struct {
			ID         string            `json:"id"`
			JourneyID  string            `json:"journey_id"`
			Name       string            `json:"name"`
			OccurredAt time.Time         `json:"occurred_at"`
			Properties map[string]string `json:"properties"`
		} `json:"events"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	events := make([]store.Event, 0, len(body.Events))
	for _, e := range body.Events {
		events = append(
			events,
			store.Event{ID: e.ID, JourneyID: e.JourneyID, Name: e.Name, OccurredAt: e.OccurredAt, Properties: e.Properties},
		)
	}
	source := "WEB"
	if native(r) {
		source = nativePlatform(r)
	}
	a.analytics().serve(w, r, func(ctx context.Context) (any, error) {
		if err := a.Analytics.Record(ctx, source, donorSession(r), events); err != nil {
			return nil, err
		}
		if !native(r) && len(events) > 0 {
			var journey pgtype.UUID
			if journey.Scan(events[0].JourneyID) == nil && store.ValidRequestID(journey) {
				a.setCookie(w, journeyCookie, journey.String(), 86400, http.SameSiteLaxMode)
			}
		}
		return map[string]int{"accepted": len(events)}, nil
	})
}

// nativePlatform maps the app's declared platform to an analytics source,
// defaulting to IOS/ANDROID from the X-DAD-Platform header; an unrecognised or
// missing platform falls back to WEB so a malformed header cannot fail ingest.
func nativePlatform(r *http.Request) string {
	switch r.Header.Get("X-DAD-Platform") {
	case "IOS":
		return "IOS"
	case "ANDROID":
		return "ANDROID"
	default:
		return "WEB"
	}
}
