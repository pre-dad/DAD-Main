package httpapi

import (
	"context"
	"errors"
	"net/http"
	"unicode/utf8"

	"dad/donor-api/internal/identity"
	"dad/donor-api/internal/store"
)

type EmailChangeSource interface {
	Request(context.Context, string, string) error
	Confirm(context.Context, string) (string, error)
}

func (a *api) emailChange() service {
	return service{a.EmailChange != nil, "Email changes are temporarily unavailable", readTimeout, emailChangeStatus}
}

func (a *api) requestEmailChange(w http.ResponseWriter, r *http.Request, session string) {
	var body struct {
		Email string `json:"email"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if utf8.RuneCountInString(body.Email) > identity.MaxEmailRunes {
		writeError(w, r, http.StatusUnprocessableEntity, "Enter a valid email address", nil)
		return
	}
	if !a.EmailConfigured {
		emailOff.write(w)
		return
	}
	a.emailChange().serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]string{"stage": "CURRENT"}, a.EmailChange.Request(ctx, session, body.Email)
	})
}

// confirmEmailChange needs only the emailed proof. Completing a change revokes
// every session, so it never issues one.
func (a *api) confirmEmailChange(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !validToken(body.Token) {
		writeError(w, r, http.StatusUnprocessableEntity, "Invalid confirmation link", nil)
		return
	}
	if !a.EmailConfigured {
		emailOff.write(w)
		return
	}
	a.emailChange().serve(w, r, func(ctx context.Context) (any, error) {
		stage, err := a.EmailChange.Confirm(ctx, body.Token)
		return map[string]string{"stage": stage}, err
	})
}

func emailChangeStatus(err error) (int, string) {
	switch {
	case errors.Is(err, store.ErrDenied):
		return http.StatusUnauthorized, "Sign in again or request a fresh confirmation link."
	case errors.Is(err, identity.ErrEmail):
		return http.StatusUnprocessableEntity, "Enter a valid email address"
	case errors.Is(err, store.ErrEmailChangeConflict):
		return http.StatusConflict, "This address cannot replace the account email. Existing accounts are never merged automatically."
	default:
		return http.StatusServiceUnavailable, "The change was not confirmed. Please retry."
	}
}
