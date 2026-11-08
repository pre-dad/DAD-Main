package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"unicode/utf8"

	"dad/donor-api/internal/identity"
	"dad/donor-api/internal/store"
)

type AccountSource interface {
	ClaimAccount(context.Context, string, string, store.Client) (store.Tokens, error)
	Refresh(context.Context, string) (store.Tokens, error)
	SignOut(context.Context, string) error
	QueueLogin(ctx context.Context, email, next string) error
}

const maxEmailRunes = 320

// nativeSession hands the donor app its tokens. Browsers never receive one:
// their session is only an HttpOnly cookie.
type nativeSession struct {
	AccessToken      string `json:"access_token"`
	ExpiresIn        int    `json:"expires_in"`
	RefreshToken     string `json:"refresh_token"`
	RefreshExpiresIn int    `json:"refresh_expires_in"`
}

func newNativeSession(tokens store.Tokens) nativeSession {
	return nativeSession{tokens.Access, int(tokens.AccessTTL.Seconds()), tokens.Refresh, int(tokens.RefreshTTL.Seconds())}
}

func (a *api) account() service {
	return service{a.Account != nil, "Account sign-in is temporarily unavailable", readTimeout, accountStatus}
}

func accountStatus(err error) (int, string) {
	switch {
	case errors.Is(err, identity.ErrEmail):
		return http.StatusUnprocessableEntity, "Enter a valid email address"
	case errors.Is(err, store.ErrDenied):
		return http.StatusUnauthorized, "This link is invalid or expired. Request a new link."
	default:
		return http.StatusServiceUnavailable, "Account sign-in is temporarily unavailable"
	}
}

func (a *api) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email *string         `json:"email"`
		Next  json.RawMessage `json:"next"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	next, ok := optional(body.Next, "")
	if !ok || (next != "" && !store.ValidReturnPath(next)) || body.Email == nil || utf8.RuneCountInString(*body.Email) > maxEmailRunes {
		invalidFields(w, r)
		return
	}
	if !a.EmailConfigured {
		emailOff.write(w)
		return
	}
	if !a.account().available(w, r) {
		return
	}
	ctx, cancel := withTimeout(r, readTimeout)
	defer cancel()
	if err := a.Account.QueueLogin(ctx, *body.Email, next); err != nil {
		status, detail := accountStatus(err)
		writeError(w, r, status, detail, err)
		return
	}
	writeJSON(
		w,
		http.StatusAccepted,
		map[string]string{"message": "If this email matches a donor account, a sign-in link will arrive shortly."},
	)
}

// claim exchanges an emailed proof for a session, revoking the session the
// caller presented, if any. The app may describe its device; a browser's
// description is ignored.
func (a *api) claim(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token    *string         `json:"token"`
		Device   json.RawMessage `json:"device_name"`
		Platform json.RawMessage `json:"platform"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	device, deviceOK := optional(body.Device, "")
	platform, platformOK := optional(body.Platform, "")
	if body.Token == nil || !validToken(*body.Token) || !deviceOK || utf8.RuneCountInString(device) > store.MaxDeviceName ||
		!platformOK || (platform != "" && platform != "IOS" && platform != "ANDROID") {
		invalidFields(w, r)
		return
	}
	if !a.account().available(w, r) {
		return
	}
	client := store.Client{CompanyToken: cookieValue(r, companyCookie), JourneyID: cookieValue(r, journeyCookie)}
	if native(r) {
		client = store.Client{App: true, Device: device, Platform: platform}
	}
	ctx, cancel := withTimeout(r, readTimeout)
	defer cancel()
	tokens, err := a.Account.ClaimAccount(ctx, *body.Token, donorSession(r), client)
	if err != nil {
		status, detail := accountStatus(err)
		writeError(w, r, status, detail, err)
		return
	}
	if client.App {
		writeJSON(w, http.StatusOK, newNativeSession(tokens))
		return
	}
	a.setSessionCookie(w, donorCookie, tokens.Access, int(tokens.AccessTTL.Seconds()))
	writeJSON(w, http.StatusOK, map[string]bool{"claimed": true})
}

// refresh exchanges the app's refresh token for new tokens.
func (a *api) refresh(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RefreshToken *string `json:"refresh_token"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.RefreshToken == nil || !validToken(*body.RefreshToken) {
		invalidFields(w, r)
		return
	}
	refreshes := a.account()
	refreshes.classify = refreshStatus
	refreshes.serve(w, r, func(ctx context.Context) (any, error) {
		tokens, err := a.Account.Refresh(ctx, *body.RefreshToken)
		return newNativeSession(tokens), err
	})
}

// refreshStatus answers a refused refresh as an ended session, not a bad link.
func refreshStatus(err error) (int, string) {
	if errors.Is(err, store.ErrDenied) {
		return http.StatusUnauthorized, "Sign in required"
	}
	return accountStatus(err)
}

// logout succeeds even without a valid session, so a client can always
// discard its credential.
func (a *api) logout(w http.ResponseWriter, r *http.Request) {
	if !a.account().available(w, r) {
		return
	}
	ctx, cancel := withTimeout(r, readTimeout)
	defer cancel()
	if err := a.Account.SignOut(ctx, donorSession(r)); err != nil {
		writeError(w, r, http.StatusServiceUnavailable, "Sign-out is temporarily unavailable", err)
		return
	}
	a.clearDonorCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]bool{"signed_out": true})
}

func (a *api) clearDonorCookie(w http.ResponseWriter, r *http.Request) {
	if !native(r) {
		a.setSessionCookie(w, donorCookie, "", -1)
	}
}
