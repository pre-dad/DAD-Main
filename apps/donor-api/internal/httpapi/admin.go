package httpapi

import (
	"context"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"

	"dad/donor-api/internal/store"
)

// maxCursorLength bounds a keyset cursor before it is decoded.
const maxCursorLength = 600

type AdminSource interface {
	ResetStreak(context.Context, string, pgtype.UUID, string) error
	RecomputeRecognition(context.Context, string, pgtype.UUID, string) error
	SearchDonors(context.Context, string, string, string) (store.DonorSearch, error)
	DonorDetail(context.Context, string, pgtype.UUID, int64, int64) (store.DonorDetail, error)
	AuditEvents(context.Context, string, string) (store.AuditPage, error)
	FailedPayments(context.Context, string, string, string) (store.FailedPaymentPage, error)
	AccountDeletions(context.Context, string, string) (store.AccountDeletionPage, error)
	DonorMessages(context.Context, string, pgtype.UUID, string) (store.MessagePage, error)
	DonorActions(context.Context, string, pgtype.UUID, string) (store.ActionPage, error)
	PendingAction(context.Context, string, pgtype.UUID, pgtype.UUID, string) (store.PendingAction, error)
	RequestClaim(context.Context, string, pgtype.UUID) error
	ResendMessage(context.Context, string, pgtype.UUID, pgtype.UUID) (store.QueuedMessage, error)
}

func (a *api) admin() service {
	return service{a.Admin != nil, staffUnavailable, readTimeout, staffStatus}
}

// cursorParam reads the keyset cursor of a paged staff list, answering the
// request itself when it is longer than any cursor the API issues.
func cursorParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	cursor := r.URL.Query().Get("cursor")
	if len(cursor) > maxCursorLength {
		invalidFields(w, r)
		return "", false
	}
	return cursor, true
}

func (a *api) searchDonors(w http.ResponseWriter, r *http.Request, session string) {
	cursor, ok := cursorParam(w, r)
	if !ok {
		return
	}
	term := r.URL.Query().Get("q")
	if utf8.RuneCountInString(term) > maxEmailRunes || strings.ContainsRune(term, 0) {
		writeError(w, r, http.StatusUnprocessableEntity, "Search for at most 320 characters", nil)
		return
	}
	a.admin().serve(w, r, func(ctx context.Context) (any, error) {
		return a.Admin.SearchDonors(ctx, session, term, cursor)
	})
}

func (a *api) donorDetail(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "donor_id")
	if !ok {
		return
	}
	gifts, okGifts := offsetParam(r, "gifts_offset")
	referrals, okReferrals := offsetParam(r, "referrals_offset")
	if !okGifts || !okReferrals {
		invalidFields(w, r)
		return
	}
	a.admin().serve(w, r, func(ctx context.Context) (any, error) {
		return a.Admin.DonorDetail(ctx, session, id, gifts, referrals)
	})
}

func (a *api) donorMessages(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "donor_id")
	if !ok {
		return
	}
	cursor, ok := cursorParam(w, r)
	if !ok {
		return
	}
	a.admin().serve(w, r, func(ctx context.Context) (any, error) {
		return a.Admin.DonorMessages(ctx, session, id, cursor)
	})
}

func (a *api) donorActions(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "donor_id")
	if !ok {
		return
	}
	cursor, ok := cursorParam(w, r)
	if !ok {
		return
	}
	a.admin().serve(w, r, func(ctx context.Context) (any, error) {
		return a.Admin.DonorActions(ctx, session, id, cursor)
	})
}

func (a *api) pendingAction(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "donor_id")
	if !ok {
		return
	}
	var target pgtype.UUID
	kind := r.URL.Query().Get("kind")
	if target.Scan(r.URL.Query().Get("target_id")) != nil || !target.Valid || (kind != "REFUND" && kind != "CANCEL") {
		invalidFields(w, r)
		return
	}
	a.admin().serve(w, r, func(ctx context.Context) (any, error) {
		return a.Admin.PendingAction(ctx, session, id, target, kind)
	})
}

func (a *api) auditEvents(w http.ResponseWriter, r *http.Request, session string) {
	cursor, ok := cursorParam(w, r)
	if !ok {
		return
	}
	a.admin().serve(w, r, func(ctx context.Context) (any, error) { return a.Admin.AuditEvents(ctx, session, cursor) })
}

func (a *api) failedPayments(w http.ResponseWriter, r *http.Request, session string) {
	cursor, ok := cursorParam(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	a.admin().serve(w, r, func(ctx context.Context) (any, error) {
		return a.Admin.FailedPayments(ctx, session, query.Get("state"), cursor)
	})
}

func (a *api) accountDeletions(w http.ResponseWriter, r *http.Request, session string) {
	cursor, ok := cursorParam(w, r)
	if !ok {
		return
	}
	a.admin().serve(w, r, func(ctx context.Context) (any, error) { return a.Admin.AccountDeletions(ctx, session, cursor) })
}

func (a *api) requestClaim(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "donor_id")
	if !ok {
		return
	}
	a.admin().serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]bool{"queued": true}, a.Admin.RequestClaim(ctx, session, id)
	})
}

func (a *api) resendMessage(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "message_id")
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	a.admin().serve(w, r, func(ctx context.Context) (any, error) { return a.Admin.ResendMessage(ctx, session, id, key) })
}

type recomputeRecognitionBody struct {
	Reason string `json:"reason"`
}

func (a *api) recomputeRecognition(w http.ResponseWriter, r *http.Request, token string) {
	id, ok := pathID(w, r, "donor_id")
	if !ok {
		return
	}
	var body recomputeRecognitionBody
	if !decodeJSON(w, r, &body) {
		return
	}
	svc := service{a.Admin != nil, staffUnavailable, staffTimeout, staffStatus}
	svc.serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]bool{"recomputed": true}, a.Admin.RecomputeRecognition(ctx, token, id, body.Reason)
	})
}

func (a *api) resetStreak(w http.ResponseWriter, r *http.Request, token string) {
	id, ok := pathID(w, r, "donor_id")
	if !ok {
		return
	}
	var body recomputeRecognitionBody
	if !decodeJSON(w, r, &body) {
		return
	}
	a.admin().serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]bool{"reset": true}, a.Admin.ResetStreak(ctx, token, id, body.Reason)
	})
}
