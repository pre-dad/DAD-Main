package httpapi

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"dad/donor-api/internal/store"
)

// BadgeSource is the donor badge read and the staff grant/revoke (PRD §6.9).
type BadgeSource interface {
	Badges(context.Context, string) ([]store.BadgeView, error)
	Grant(context.Context, string, pgtype.UUID, string) error
	Revoke(context.Context, string, pgtype.UUID, string) error
}

// donorBadges serves the signed-in donor's badge wall. It is a donorRead route
// but records newly-earned badges as a side effect (permanent earn-state).
func (a *api) donorBadges(w http.ResponseWriter, r *http.Request, session string) {
	svc := service{a.Badges != nil, donorDatabaseUnavailable, readTimeout, donorStatus}
	svc.serve(w, r, func(ctx context.Context) (any, error) {
		views, err := a.Badges.Badges(ctx, session)
		if err != nil {
			return nil, err
		}
		return map[string]any{"badges": views}, nil
	})
}

// badgeCodeBody carries the badge code; the donor is in the path.
type badgeCodeBody struct {
	Code string `json:"code"`
}

func (a *api) grantBadge(w http.ResponseWriter, r *http.Request, session string) {
	a.badgeAction(w, r, session, true)
}

func (a *api) revokeBadge(w http.ResponseWriter, r *http.Request, session string) {
	a.badgeAction(w, r, session, false)
}

func (a *api) badgeAction(w http.ResponseWriter, r *http.Request, session string, grant bool) {
	id, ok := pathID(w, r, "donor_id")
	if !ok {
		return
	}
	var body badgeCodeBody
	if !decodeJSON(w, r, &body) {
		return
	}
	svc := service{a.Badges != nil, "Badge administration is temporarily unavailable", staffTimeout, staffStatus}
	svc.serve(w, r, func(ctx context.Context) (any, error) {
		if grant {
			return map[string]bool{"granted": true}, a.Badges.Grant(ctx, session, id, body.Code)
		}
		return map[string]bool{"revoked": true}, a.Badges.Revoke(ctx, session, id, body.Code)
	})
}
