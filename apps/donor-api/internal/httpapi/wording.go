package httpapi

import (
	"context"
	"net/http"

	"dad/donor-api/internal/store"
)

type WordingSource interface {
	Rules(context.Context, string) (store.RecognitionRules, error)
	SetRules(context.Context, string, store.RecognitionRules) error
	Wording(context.Context, string) (store.RecognitionWording, error)
	SetWording(context.Context, string, store.WordingChange) error
}

func (a *api) wording() service {
	return service{a.Wording != nil, "Content administration is temporarily unavailable", readTimeout, staffStatus}
}

func (a *api) recognitionWording(w http.ResponseWriter, r *http.Request, session string) {
	a.wording().serve(w, r, func(ctx context.Context) (any, error) { return a.Wording.Wording(ctx, session) })
}

type wordingBody struct {
	Kind  string `json:"kind"`
	Key   string `json:"key"`
	Title string `json:"title"`
	Text  string `json:"text"`
}

func (b wordingBody) valid() bool {
	return (b.Kind == store.WordingBadge || b.Kind == store.WordingMilestone) && b.Key != ""
}

func (a *api) setRecognitionWording(w http.ResponseWriter, r *http.Request, session string) {
	body, ok := decodeValid[wordingBody](w, r)
	if !ok {
		return
	}
	change := store.WordingChange(body)
	a.wording().serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]bool{"saved": true}, a.Wording.SetWording(ctx, session, change)
	})
}

func (a *api) recognitionRules(w http.ResponseWriter, r *http.Request, token string) {
	a.wording().serve(w, r, func(ctx context.Context) (any, error) { return a.Wording.Rules(ctx, token) })
}
func (a *api) setRecognitionRules(w http.ResponseWriter, r *http.Request, token string) {
	var body store.RecognitionRules
	if !decodeJSON(w, r, &body) {
		return
	}
	a.wording().serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]bool{"saved": true}, a.Wording.SetRules(ctx, token, body)
	})
}
