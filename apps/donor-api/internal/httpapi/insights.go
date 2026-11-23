package httpapi

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"dad/donor-api/internal/store"
)

type InsightSource interface {
	List(ctx context.Context, token, state, after string) (store.InsightSpecPage, error)
	Get(ctx context.Context, token string, id pgtype.UUID) (store.InsightSpec, error)
	Create(ctx context.Context, token string, spec []byte) (store.InsightSpec, error)
	Update(ctx context.Context, token string, id pgtype.UUID, spec []byte) (store.InsightSpec, error)
	Submit(ctx context.Context, token string, id pgtype.UUID) (store.InsightSpec, error)
	Approve(ctx context.Context, token string, id pgtype.UUID) (store.InsightSpec, error)
	Reject(ctx context.Context, token string, id pgtype.UUID, reason string) (store.InsightSpec, error)
	Retire(ctx context.Context, token string, id pgtype.UUID) (store.InsightSpec, error)
}

func (a *api) insights() service {
	return service{a.Insights != nil, "Insight layouts are temporarily unavailable", readTimeout, staffStatus}
}

func (a *api) staffInsights(w http.ResponseWriter, r *http.Request, session string) {
	cursor, ok := cursorParam(w, r)
	if !ok {
		return
	}
	state := r.URL.Query().Get("state")
	a.insights().serve(w, r, func(ctx context.Context) (any, error) { return a.Insights.List(ctx, session, state, cursor) })
}

func (a *api) staffInsight(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "spec_id")
	if !ok {
		return
	}
	a.insights().serve(w, r, func(ctx context.Context) (any, error) { return a.Insights.Get(ctx, session, id) })
}

type insightBody struct {
	Spec json.RawMessage `json:"spec"`
}

func (b insightBody) valid() bool { return len(b.Spec) > 0 && string(b.Spec) != "null" }

func (a *api) createInsight(w http.ResponseWriter, r *http.Request, session string) {
	body, ok := decodeValid[insightBody](w, r)
	if !ok {
		return
	}
	a.insights().serve(w, r, func(ctx context.Context) (any, error) { return a.Insights.Create(ctx, session, body.Spec) })
}

func (a *api) updateInsight(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "spec_id")
	if !ok {
		return
	}
	body, ok := decodeValid[insightBody](w, r)
	if !ok {
		return
	}
	a.insights().serve(w, r, func(ctx context.Context) (any, error) { return a.Insights.Update(ctx, session, id, body.Spec) })
}

func (a *api) submitInsight(w http.ResponseWriter, r *http.Request, session string) {
	a.insightTransition(w, r, session, InsightSource.Submit)
}

func (a *api) approveInsight(w http.ResponseWriter, r *http.Request, session string) {
	a.insightTransition(w, r, session, InsightSource.Approve)
}

func (a *api) retireInsight(w http.ResponseWriter, r *http.Request, session string) {
	a.insightTransition(w, r, session, InsightSource.Retire)
}

func (a *api) insightTransition(
	w http.ResponseWriter,
	r *http.Request,
	session string,
	run func(InsightSource, context.Context, string, pgtype.UUID) (store.InsightSpec, error),
) {
	id, ok := pathID(w, r, "spec_id")
	if !ok {
		return
	}
	a.insights().serve(w, r, func(ctx context.Context) (any, error) { return run(a.Insights, ctx, session, id) })
}

type rejectInsightBody struct {
	Reason string `json:"reason"`
}

func (b rejectInsightBody) valid() bool { return b.Reason != "" }

func (a *api) rejectInsight(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "spec_id")
	if !ok {
		return
	}
	body, ok := decodeValid[rejectInsightBody](w, r)
	if !ok {
		return
	}
	a.insights().serve(w, r, func(ctx context.Context) (any, error) { return a.Insights.Reject(ctx, session, id, body.Reason) })
}
