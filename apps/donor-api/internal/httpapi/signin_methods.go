package httpapi

import (
	"context"
	"errors"
	"net/http"

	"dad/donor-api/internal/oidc"
	"dad/donor-api/internal/store"
)

type PhoneSignInSource interface {
	BeginSignIn(ctx context.Context, phone string) error
	FinishSignIn(ctx context.Context, phone, code string, client store.Client) (store.Tokens, error)
}

type OIDCSignInSource interface {
	SignIn(ctx context.Context, provider oidc.Provider, idToken string, client store.Client) (store.Tokens, error)
}

const signInMethodUnavailable = "This sign-in method is temporarily unavailable"

func (a *api) phoneSignIn() service {
	return service{a.PhoneSignIn != nil, signInMethodUnavailable, readTimeout, signInStatus}
}

func (a *api) oidcSignIn() service {
	return service{a.OIDCSignIn != nil, signInMethodUnavailable, readTimeout, signInStatus}
}

func signInStatus(err error) (int, string) {
	switch {
	case errors.Is(err, store.ErrSignInUnavailable):
		return http.StatusServiceUnavailable, "This sign-in method is not available"
	case errors.Is(err, store.ErrSignInCode):
		return http.StatusUnauthorized, "That code is incorrect or has expired"
	case errors.Is(err, store.ErrDenied):
		return http.StatusUnauthorized, "Sign in required"
	default:
		return http.StatusServiceUnavailable, signInMethodUnavailable
	}
}

// beginPhoneSignIn sends a one-time code. The response is always the same so it
// never reveals whether the phone has an account.
func (a *api) beginPhoneSignIn(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone string `json:"phone"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	a.phoneSignIn().serve(w, r, func(ctx context.Context) (any, error) {
		if err := a.PhoneSignIn.BeginSignIn(ctx, body.Phone); err != nil {
			return nil, err
		}
		return map[string]bool{"sent": true}, nil
	})
}

func (a *api) finishPhoneSignIn(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone    string  `json:"phone"`
		Code     string  `json:"code"`
		Device   *string `json:"device_name"`
		Platform *string `json:"platform"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Phone == "" || body.Code == "" {
		invalidFields(w, r)
		return
	}
	client, ok := signInClient(r, body.Device, body.Platform)
	if !ok {
		invalidFields(w, r)
		return
	}
	a.phoneSignIn().serve(w, r, func(ctx context.Context) (any, error) {
		tokens, err := a.PhoneSignIn.FinishSignIn(ctx, body.Phone, body.Code, client)
		if err != nil {
			return nil, err
		}
		return a.sessionResponse(w, client, tokens), nil
	})
}

func (a *api) oidcSignInHandler(w http.ResponseWriter, r *http.Request) {
	provider, ok := oidc.ParseProvider(r.PathValue("provider"))
	if !ok {
		invalidFields(w, r)
		return
	}
	var body struct {
		IDToken  string  `json:"id_token"`
		Device   *string `json:"device_name"`
		Platform *string `json:"platform"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.IDToken == "" {
		invalidFields(w, r)
		return
	}
	client, ok := signInClient(r, body.Device, body.Platform)
	if !ok {
		invalidFields(w, r)
		return
	}
	a.oidcSignIn().serve(w, r, func(ctx context.Context) (any, error) {
		tokens, err := a.OIDCSignIn.SignIn(ctx, provider, body.IDToken, client)
		if err != nil {
			return nil, err
		}
		return a.sessionResponse(w, client, tokens), nil
	})
}

// signInClient builds the session Client from an optional device/platform,
// honouring them only for a native request, exactly as passkey sign-in does.
func signInClient(r *http.Request, device, platform *string) (store.Client, bool) {
	if !native(r) {
		return store.Client{CompanyToken: cookieValue(r, companyCookie), JourneyID: cookieValue(r, journeyCookie)}, true
	}
	client := store.Client{App: true}
	if device != nil {
		client.Device = *device
	}
	if platform != nil {
		client.Platform = *platform
	}
	ok := len([]rune(client.Device)) <= store.MaxDeviceName &&
		(client.Platform == "" || client.Platform == "IOS" || client.Platform == "ANDROID")
	return client, ok
}

// sessionResponse writes the issued session as a browser cookie or a native
// token payload, matching the other donor sign-in entries.
func (a *api) sessionResponse(w http.ResponseWriter, client store.Client, tokens store.Tokens) any {
	if client.App {
		return newNativeSession(tokens)
	}
	a.setCookie(w, donorCookie, tokens.Access, int(tokens.AccessTTL.Seconds()), http.SameSiteLaxMode)
	return map[string]bool{"signed_in": true}
}
