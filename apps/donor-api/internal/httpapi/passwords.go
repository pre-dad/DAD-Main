package httpapi

import (
	"context"
	"dad/donor-api/internal/geo"
	"dad/donor-api/internal/store"
	"errors"
	"net/http"
)

// codeCurrentPassword marks a 422 whose only fault is the current password given to change a password.
const codeCurrentPassword = "CURRENT_PASSWORD_INCORRECT"

type PasswordSource interface {
	Signup(context.Context, store.SignupInput) error
	Set(context.Context, string, string, string) error
	SignIn(context.Context, string, string, store.Client) (store.Tokens, error)
}

func (a *api) passwords() service {
	return service{a.Passwords != nil, "Account access is temporarily unavailable", readTimeout, passwordStatus}
}
func passwordStatus(err error) (int, string) {
	if errors.Is(err, store.ErrPassword) {
		return http.StatusUnprocessableEntity, err.Error()
	}
	if errors.Is(err, store.ErrStaffInput) || errors.Is(err, geo.ErrZip) {
		return http.StatusUnprocessableEntity, "Enter your name, city and a valid ZIP code"
	}
	if errors.Is(err, store.ErrSignInCode) || errors.Is(err, store.ErrDenied) {
		return http.StatusUnauthorized, "The email or password is incorrect"
	}
	return accountStatus(err)
}
func (a *api) signup(w http.ResponseWriter, r *http.Request) {
	var body store.SignupInput
	if !decodeJSON(w, r, &body) {
		return
	}
	if !a.EmailConfigured {
		emailOff.write(w)
		return
	}
	a.passwords().serve(w, r, func(ctx context.Context) (any, error) {
		err := a.Passwords.Signup(ctx, body)
		if errors.Is(err, store.EmailRoleError{}) || errors.Is(err, store.ErrDenied) {
			// Account creation must not become an alternative email-enumeration endpoint.
			err = nil
		}
		return map[string]bool{"sent": true}, err
	})
}
func (a *api) passwordSignIn(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string  `json:"email"`
		Password string  `json:"password"`
		Device   *string `json:"device_name"`
		Platform *string `json:"platform"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	client, ok := signInClient(r, body.Device, body.Platform)
	if !ok {
		invalidFields(w, r)
		return
	}
	a.passwords().serve(w, r, func(ctx context.Context) (any, error) {
		tokens, err := a.Passwords.SignIn(ctx, body.Email, body.Password, client)
		if err != nil {
			return nil, err
		}
		return a.sessionResponse(w, client, tokens), nil
	})
}
func (a *api) setPassword(w http.ResponseWriter, r *http.Request, token string) {
	var body struct {
		Current  string `json:"current_password"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	svc := a.passwords()
	if !svc.available(w, r) {
		return
	}
	ctx, cancel := withTimeout(r, svc.timeout)
	defer cancel()
	err := a.Passwords.Set(ctx, token, body.Current, body.Password)
	if errors.Is(err, store.ErrCurrentPassword) {
		// The session is valid; only the supplied current password is wrong. A 401 here would read as signed out.
		writeJSON(w, http.StatusUnprocessableEntity, problem{Detail: "The email or password is incorrect", Code: codeCurrentPassword})
		return
	}
	respond(w, r, map[string]bool{"saved": true}, err, svc.classify)
}
