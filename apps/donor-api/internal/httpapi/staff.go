package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"dad/donor-api/internal/identity"
	"dad/donor-api/internal/store"
)

type StaffSource interface {
	BeginSignIn(context.Context) (store.Ceremony, error)
	SignIn(context.Context, string, []byte) (string, error)
	BeginActivation(context.Context, string) (store.Invitation, error)
	Activate(context.Context, string, []byte) (string, error)
	SignOut(context.Context, string) error
	Me(context.Context, string) (store.StaffView, error)
	Accounts(context.Context, string) ([]store.StaffView, error)
	Invite(context.Context, string, string, string, string) (string, error)
	ChangeAccess(context.Context, string, pgtype.UUID, store.StaffAccessAction, string) (store.AccessChange, error)
}

const (
	staffSessionAge  = 8 * 60 * 60
	staffUnavailable = "Staff service is temporarily unavailable"
	// A passkey response is a few kilobytes; this leaves room for large
	// attestation certificate chains.
	maxCredentialBytes = 64 << 10
)

// emailRoleDetail names the account that already holds an address, for staff
// and partner admins adding a login (PRD §3: one email, one role).
func emailRoleDetail(err error) string {
	var held store.EmailRoleError
	errors.As(err, &held)
	switch held.Role {
	case "DONOR":
		return "This email already has a donor account. Use a different email for this login."
	case "STAFF":
		return "This email is a DAD staff login. Use a different email for this login."
	default:
		return "This email already signs in to a partner organization. Use a different email for this login."
	}
}

func staffStatus(err error) (int, string) {
	switch {
	case errors.Is(err, store.ErrStaffDenied):
		return http.StatusUnauthorized, "Passkey, invitation, or session invalid or expired"
	case errors.Is(err, store.ErrStaffForbidden):
		return http.StatusForbidden, "Permission denied"
	case errors.Is(err, store.ErrStaffInput):
		return http.StatusUnprocessableEntity, "Invalid request fields"
	case errors.Is(err, store.ErrStaffExists):
		return http.StatusConflict, "A staff account with this email already exists"
	case errors.Is(err, store.ErrBoardApprovalRequired):
		return http.StatusConflict, "Record the initiative's board approval before publishing it"
	case errors.Is(err, store.ErrStaffConflict):
		return http.StatusConflict, "Staff access changed or conflicts with this request"
	case errors.Is(err, store.EmailRoleError{}):
		return http.StatusConflict, emailRoleDetail(err)
	case errors.Is(err, pgx.ErrNoRows):
		return http.StatusNotFound, "Record not found"
	default:
		return http.StatusServiceUnavailable, staffUnavailable
	}
}

func (a *api) staff() service {
	return service{a.Staff != nil, staffUnavailable, readTimeout, staffStatus}
}

// validRole gates the role a staff invite or role-change may set (PRD §8). The
// bootstrap super admin is created out-of-band (StaffService.Bootstrap), so a
// super admin can be assigned here too when promoting an operator.
func validRole(role string) bool {
	switch role {
	case "SUPER_ADMIN", "OP_PARTNERSHIPS", "OP_GROWTH", "OP_IMPACT":
		return true
	default:
		return false
	}
}

func (a *api) invitationURL(token string) string {
	return a.StaffOrigin + "/staff/activate#token=" + token
}

// staffCommand runs a validated staff request body.
func staffCommand[T requestBody](a *api, w http.ResponseWriter, r *http.Request, run func(context.Context, T) (any, error)) {
	body, ok := decodeValid[T](w, r)
	if !ok {
		return
	}
	a.staff().serve(w, r, func(ctx context.Context) (any, error) { return run(ctx, body) })
}

// setStaffCookie keeps the staff session on the staff host: it never has a
// Domain, and over HTTPS the __Host- prefix makes browsers refuse one.
func (a *api) setStaffCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(
		w,
		&http.Cookie{
			Name:     a.staffCookie,
			Value:    value,
			MaxAge:   maxAge,
			Path:     "/",
			HttpOnly: true,
			Secure:   a.staffSecure,
			SameSite: http.SameSiteStrictMode,
		},
	)
}

func (a *api) staffSession(r *http.Request) string { return cookieValue(r, a.staffCookie) }

// passkeyBody answers a ceremony with the browser's PublicKeyCredential JSON,
// which the store parses and verifies.
type passkeyBody struct {
	Ceremony   string          `json:"ceremony"`
	Credential json.RawMessage `json:"credential"`
}

func (b passkeyBody) valid() bool {
	return validToken(b.Ceremony) && len(b.Credential) > 0 && len(b.Credential) <= maxCredentialBytes
}

func (a *api) beginStaffSignIn(w http.ResponseWriter, r *http.Request) {
	a.staff().serve(w, r, func(ctx context.Context) (any, error) { return a.Staff.BeginSignIn(ctx) })
}

func (a *api) staffSignIn(w http.ResponseWriter, r *http.Request) {
	staffCommand(a, w, r, func(ctx context.Context, b passkeyBody) (any, error) {
		token, err := a.Staff.SignIn(ctx, b.Ceremony, b.Credential)
		if err == nil {
			a.setStaffCookie(w, token, staffSessionAge)
		}
		return map[string]bool{"signed_in": true}, err
	})
}

type invitationTokenBody struct {
	Token string `json:"token"`
}

func (b invitationTokenBody) valid() bool { return validToken(b.Token) }

func (a *api) beginStaffActivation(w http.ResponseWriter, r *http.Request) {
	staffCommand(a, w, r, func(ctx context.Context, b invitationTokenBody) (any, error) {
		return a.Staff.BeginActivation(ctx, b.Token)
	})
}

// staffActivate signs the new staff member in with the passkey they created.
func (a *api) staffActivate(w http.ResponseWriter, r *http.Request) {
	staffCommand(a, w, r, func(ctx context.Context, b passkeyBody) (any, error) {
		token, err := a.Staff.Activate(ctx, b.Ceremony, b.Credential)
		if err == nil {
			a.setStaffCookie(w, token, staffSessionAge)
		}
		return map[string]bool{"activated": true}, err
	})
}

type invitationBody struct {
	Email string `json:"email"`
	Name  string `json:"name"`
	Role  string `json:"role"`
}

func (b invitationBody) valid() bool {
	n := utf8.RuneCountInString(b.Name)
	return utf8.RuneCountInString(b.Email) <= identity.MaxEmailRunes && n > 0 && n <= 100 && validRole(b.Role)
}

func (a *api) inviteStaff(w http.ResponseWriter, r *http.Request, session string) {
	staffCommand(a, w, r, func(ctx context.Context, b invitationBody) (any, error) {
		token, err := a.Staff.Invite(ctx, session, b.Email, b.Name, b.Role)
		if err != nil {
			return nil, err
		}
		return map[string]string{"invitation_url": a.invitationURL(token)}, nil
	})
}

func (a *api) staffMe(w http.ResponseWriter, r *http.Request, session string) {
	a.staff().serve(w, r, func(ctx context.Context) (any, error) { return a.Staff.Me(ctx, session) })
}

func (a *api) staffAccounts(w http.ResponseWriter, r *http.Request, session string) {
	a.staff().serve(w, r, func(ctx context.Context) (any, error) { return a.Staff.Accounts(ctx, session) })
}

// staffSignOut succeeds even without a valid session, so the browser can
// always discard its cookie.
func (a *api) staffSignOut(w http.ResponseWriter, r *http.Request) {
	a.staff().serve(w, r, func(ctx context.Context) (any, error) {
		err := a.Staff.SignOut(ctx, a.staffSession(r))
		if err != nil && !errors.Is(err, store.ErrStaffDenied) {
			return nil, err
		}
		a.setStaffCookie(w, "", -1)
		return map[string]bool{"signed_out": true}, nil
	})
}

type roleBody struct {
	Role string `json:"role"`
}

func (b roleBody) valid() bool { return validRole(b.Role) }

func (a *api) changeStaffAccess(action store.StaffAccessAction) sessionHandler {
	return func(w http.ResponseWriter, r *http.Request, session string) {
		id, ok := pathID(w, r, "staff_id")
		if !ok {
			return
		}
		var body roleBody
		if action == store.AccessRole {
			if body, ok = decodeValid[roleBody](w, r); !ok {
				return
			}
		}
		a.staff().serve(w, r, func(ctx context.Context) (any, error) {
			change, err := a.Staff.ChangeAccess(ctx, session, id, action, body.Role)
			switch {
			case err != nil:
				return nil, err
			case action == store.AccessRecovery:
				return map[string]string{"invitation_url": a.invitationURL(change.Token)}, nil
			case action == store.AccessRole:
				return change.Staff, nil
			default:
				return change, nil
			}
		})
	}
}
