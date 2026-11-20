package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgtype"

	"dad/donor-api/internal/store"
)

// InitiativeSource is the staff (Impact-Ops) surface for initiatives and their
// headline metrics.
type InitiativeSource interface {
	Create(context.Context, string, store.InitiativeInput) (pgtype.UUID, error)
	Update(context.Context, string, pgtype.UUID, store.InitiativeInput) error
	Publish(context.Context, string, pgtype.UUID) error
	SetMetric(context.Context, string, pgtype.UUID, store.MetricInput) error
	PublishMetric(context.Context, string, pgtype.UUID, int32) error
	DeleteMetric(context.Context, string, pgtype.UUID, int32) error
	SetSpend(context.Context, string, pgtype.UUID, store.SpendInput) error
	PublishSpend(context.Context, string, pgtype.UUID, string) error
	DeleteSpend(context.Context, string, pgtype.UUID, string) error
	List(context.Context, string, int64) (store.StaffInitiativePage, error)
	Detail(context.Context, string, pgtype.UUID) (store.StaffInitiativeDetail, error)
	SetRotation(context.Context, string, pgtype.UUID, bool) error
}

// InitiativeReadSource is the public read of published initiatives and their spend.
type InitiativeReadSource interface {
	Spend(context.Context, pgtype.UUID) (store.InitiativeSpend, error)
	OverallSpend(context.Context) (store.OverallSpend, error)
	Published(context.Context) ([]store.PublicInitiative, error)
	PublishedBySlug(context.Context, string) (store.PublicInitiative, error)
}

// TodayInitiativeSource is the signed-in donor's daily rotation.
type TodayInitiativeSource interface {
	Today(context.Context, string) (store.TodayInitiative, error)
}

func (a *api) initiativesAdmin() service {
	return service{
		a.Initiatives != nil,
		"Initiative administration is temporarily unavailable",
		staffTimeout,
		staffStatus,
	}
}

func (a *api) staffInitiatives(w http.ResponseWriter, r *http.Request, session string) {
	offset, ok := offsetParam(r, "offset")
	if !ok {
		invalidFields(w, r)
		return
	}
	a.initiativesAdmin().serve(w, r, func(ctx context.Context) (any, error) { return a.Initiatives.List(ctx, session, offset) })
}

func (a *api) staffInitiative(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "initiative_id")
	if !ok {
		return
	}
	a.initiativesAdmin().serve(w, r, func(ctx context.Context) (any, error) { return a.Initiatives.Detail(ctx, session, id) })
}

func (a *api) setInitiativeRotation(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "initiative_id")
	if !ok {
		return
	}
	var body struct {
		InRotation bool `json:"in_rotation"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	a.initiativesAdmin().serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]bool{"in_rotation": body.InRotation}, a.Initiatives.SetRotation(ctx, session, id, body.InRotation)
	})
}

func (a *api) createInitiative(w http.ResponseWriter, r *http.Request, session string) {
	var body store.InitiativeInput
	if !decodeJSON(w, r, &body) {
		return
	}
	a.initiativesAdmin().serve(w, r, func(ctx context.Context) (any, error) {
		id, err := a.Initiatives.Create(ctx, session, body)
		if err != nil {
			return nil, err
		}
		return map[string]string{"id": id.String()}, nil
	})
}

func (a *api) updateInitiative(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "initiative_id")
	if !ok {
		return
	}
	var body store.InitiativeInput
	if !decodeJSON(w, r, &body) {
		return
	}
	a.initiativesAdmin().serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]bool{"updated": true}, a.Initiatives.Update(ctx, session, id, body)
	})
}

func (a *api) publishInitiative(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "initiative_id")
	if !ok {
		return
	}
	a.initiativesAdmin().serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]bool{"published": true}, a.Initiatives.Publish(ctx, session, id)
	})
}

func (a *api) setInitiativeMetric(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "initiative_id")
	if !ok {
		return
	}
	var body store.MetricInput
	if !decodeJSON(w, r, &body) {
		return
	}
	a.initiativesAdmin().serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]bool{"saved": true}, a.Initiatives.SetMetric(ctx, session, id, body)
	})
}

func (a *api) publishInitiativeMetric(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "initiative_id")
	if !ok {
		return
	}
	position, ok := metricPosition(w, r)
	if !ok {
		return
	}
	a.initiativesAdmin().serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]bool{"published": true}, a.Initiatives.PublishMetric(ctx, session, id, position)
	})
}

func (a *api) deleteInitiativeMetric(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "initiative_id")
	if !ok {
		return
	}
	position, ok := metricPosition(w, r)
	if !ok {
		return
	}
	a.initiativesAdmin().serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]bool{"deleted": true}, a.Initiatives.DeleteMetric(ctx, session, id, position)
	})
}

// metricPosition reads the {position} path value (a headline-metric slot 0-5).
func metricPosition(w http.ResponseWriter, r *http.Request) (int32, bool) {
	n, err := strconv.Atoi(r.PathValue("position"))
	if err != nil || n < 0 || n >= store.MetricSlots {
		writeError(w, r, http.StatusNotFound, "Record not found", nil)
		return 0, false
	}
	return int32(n), true
}

func (a *api) setInitiativeSpend(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "initiative_id")
	if !ok {
		return
	}
	var body store.SpendInput
	if !decodeJSON(w, r, &body) {
		return
	}
	a.initiativesAdmin().serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]bool{"saved": true}, a.Initiatives.SetSpend(ctx, session, id, body)
	})
}

// spendCategoryBody names the category to remove or publish; a category is free
// text, so it travels in the body rather than the path.
type spendCategoryBody struct {
	Category string `json:"category"`
}

func (a *api) deleteInitiativeSpend(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "initiative_id")
	if !ok {
		return
	}
	var body spendCategoryBody
	if !decodeJSON(w, r, &body) {
		return
	}
	a.initiativesAdmin().serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]bool{"deleted": true}, a.Initiatives.DeleteSpend(ctx, session, id, body.Category)
	})
}

// initiativeSpend is the donor-facing, unauthenticated read of a published
// initiative's spend by category. A draft or unknown id answers 404.
func (a *api) initiativeSpend(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "initiative_id")
	if !ok {
		return
	}
	a.publicInitiativeRead(w, r, func(ctx context.Context) (any, error) { return a.InitiativeRead.Spend(ctx, id) })
}

// overallSpend is the public, unauthenticated read of spend by category
// across every published initiative: category totals only (PRD §5.2).
func (a *api) overallSpend(w http.ResponseWriter, r *http.Request) {
	a.publicInitiativeRead(w, r, func(ctx context.Context) (any, error) { return a.InitiativeRead.OverallSpend(ctx) })
}

// publishedInitiatives lists every published initiative for the public pages.
func (a *api) publishedInitiatives(w http.ResponseWriter, r *http.Request) {
	a.publicInitiativeRead(w, r, func(ctx context.Context) (any, error) { return a.InitiativeRead.Published(ctx) })
}

// publishedInitiative is one published initiative by slug; a draft or unknown
// slug answers 404.
func (a *api) publishedInitiative(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	a.publicInitiativeRead(w, r, func(ctx context.Context) (any, error) { return a.InitiativeRead.PublishedBySlug(ctx, slug) })
}

// publicInitiativeRead answers a published-initiative read, cacheable for a
// minute like the published content.
func (a *api) publicInitiativeRead(w http.ResponseWriter, r *http.Request, read func(context.Context) (any, error)) {
	if a.InitiativeRead == nil {
		writeError(w, r, http.StatusServiceUnavailable, donorDatabaseUnavailable, nil)
		return
	}
	ctx, cancel := withTimeout(r, publicReadTimeout)
	defer cancel()
	body, err := read(ctx)
	switch {
	case errors.Is(err, store.ErrInitiativeNotPublished):
		writeError(w, r, http.StatusNotFound, "Record not found", nil)
	case err != nil:
		writeError(w, r, http.StatusServiceUnavailable, donorDatabaseUnavailable, err)
	default:
		w.Header().Set("Cache-Control", "public, max-age=60")
		writeJSON(w, http.StatusOK, body)
	}
}

const donorDatabaseUnavailable = "Donor database unavailable"

// todayInitiative serves the signed-in donor's initiative of the day. A rotation
// with no published, active initiative answers 404.
func (a *api) todayInitiative(w http.ResponseWriter, r *http.Request, session string) {
	svc := service{a.TodayInitiative != nil, donorDatabaseUnavailable, readTimeout, donorStatus}.
		missing("No initiative to show today")
	svc.serve(w, r, func(ctx context.Context) (any, error) { return a.TodayInitiative.Today(ctx, session) })
}

func (a *api) publishInitiativeSpend(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "initiative_id")
	if !ok {
		return
	}
	var body spendCategoryBody
	if !decodeJSON(w, r, &body) {
		return
	}
	a.initiativesAdmin().serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]bool{"published": true}, a.Initiatives.PublishSpend(ctx, session, id, body.Category)
	})
}
