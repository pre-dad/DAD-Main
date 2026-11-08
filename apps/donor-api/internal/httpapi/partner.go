package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"dad/donor-api/internal/sharing"
	"dad/donor-api/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// PartnerApplySource is the public application entry point (P-1).
type PartnerApplySource interface {
	Apply(context.Context, pgtype.UUID, store.PartnerApplicationInput) error
}

// PartnerPortalSource is the authenticated partner portal surface (§7.2/§7.3).
type PartnerPortalSource interface {
	RequestSignIn(ctx context.Context, email string) error
	AcceptInvite(ctx context.Context, invite string, app ...bool) (string, error)
	SignOut(ctx context.Context, token string) error
	Users(ctx context.Context, token string) ([]store.PartnerUser, error)
	Invite(ctx context.Context, token, email, name, role string) error
	RemoveUser(ctx context.Context, token string, userID pgtype.UUID) error
	SaveProfile(ctx context.Context, token, organization, description string) error
	Overview(ctx context.Context, token string) (store.PartnerOverview, error)
	Initiatives(ctx context.Context, token string) ([]store.PartnerInitiative, error)
	Spend(ctx context.Context, token string) ([]store.PartnerSpendRow, error)
	Exports(ctx context.Context, token string) ([]store.PartnerReportRow, error)
	ExportReport(ctx context.Context, token string) ([]byte, error)
	ReportArtifact(context.Context, string, pgtype.UUID) ([]byte, error)
	Logo(context.Context, string) ([]byte, error)
	SaveLogo(context.Context, string, string) error
	CompanyCode(ctx context.Context, token string) (string, error)
}

// StaffPartnerSource is the staff-side partner administration (§8, partner.manage).
type StaffPartnerSource interface {
	PendingApplications(ctx context.Context, token string, offset int64) ([]store.PartnerApplication, error)
	Approve(ctx context.Context, token string, appID pgtype.UUID) (pgtype.UUID, error)
	Reject(ctx context.Context, token string, appID pgtype.UUID, reason string) error
	LinkInitiative(ctx context.Context, token string, partnerID, initiativeID pgtype.UUID, commitment ...int64) error
	SetMatch(ctx context.Context, token string, partnerID pgtype.UUID, workforce, rateBP int32, capMinor int64) error
	Partners(ctx context.Context, token string, offset int64) (store.StaffPartnerPage, error)
	Partner(ctx context.Context, token string, partnerID pgtype.UUID) (store.StaffPartnerDetail, error)
	InviteUser(ctx context.Context, token string, partnerID pgtype.UUID, email, name, role string) error
	RemoveUser(ctx context.Context, token string, partnerID, userID pgtype.UUID) error
	SetLogo(ctx context.Context, token string, partnerID pgtype.UUID, dataURL string) error
	Logo(ctx context.Context, token string, partnerID pgtype.UUID) ([]byte, error)
}

// partnerSessionMaxAge mirrors the 12h store session lifetime.
const partnerSessionMaxAge = 12 * time.Hour

// partnerSession reads the partner session: the bearer token of a native
// request, otherwise the web portal's cookie, as donorSession does.
func partnerSession(r *http.Request) string {
	if !native(r) {
		return sessionCookieValue(r, partnerCookie)
	}
	scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if !strings.EqualFold(scheme, "Bearer") || !validToken(token) {
		return ""
	}
	return token
}

func partnerStatusCode(err error) (int, string) {
	switch {
	case errors.Is(err, store.ErrPushToken):
		return http.StatusUnprocessableEntity, "Check the device token and try again"
	case errors.Is(err, store.ErrPartnerDenied):
		return http.StatusUnauthorized, "Partner sign-in required"
	case errors.Is(err, store.ErrPartnerForbidden):
		return http.StatusForbidden, "You do not have permission for this action"
	case errors.Is(err, store.ErrPartnerInvite):
		return http.StatusUnprocessableEntity, "This invite link is invalid or has expired"
	case errors.Is(err, store.ErrPartnerApplication):
		return http.StatusUnprocessableEntity, "Check the highlighted fields and try again"
	case errors.Is(err, store.ErrPartnerDecision):
		return http.StatusConflict, "This application has already been decided"
	case errors.Is(err, store.EmailRoleError{}):
		return http.StatusConflict, emailRoleDetail(err)
	case errors.Is(err, pgx.ErrNoRows):
		return http.StatusNotFound, "Record not found"
	default:
		return http.StatusServiceUnavailable, "The partner portal is temporarily unavailable"
	}
}

func (a *api) partnerPortal() service {
	return service{a.PartnerPortal != nil, "The partner portal is temporarily unavailable", readTimeout, partnerStatusCode}
}

func staffPartnerStatus(err error) (int, string) {
	switch {
	case errors.Is(err, store.ErrPartnerApplication):
		return http.StatusUnprocessableEntity, "Check the highlighted fields and try again"
	case errors.Is(err, store.ErrPartnerDecision):
		return http.StatusConflict, "This application has already been decided"
	default:
		return staffStatus(err)
	}
}

func (a *api) staffPartner() service {
	return service{a.StaffPartner != nil, "Partner administration is temporarily unavailable", staffTimeout, staffPartnerStatus}
}

func (a *api) partnerApply(w http.ResponseWriter, r *http.Request) {
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var body store.PartnerApplicationInput
	if !decodeJSON(w, r, &body) {
		return
	}
	svc := service{a.PartnerApply != nil, "Partner applications are temporarily unavailable", readTimeout, partnerStatusCode}
	svc.serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]any{"received": true}, a.PartnerApply.Apply(ctx, key, body)
	})
}

func (a *api) partnerSignIn(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email *string `json:"email"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Email == nil || utf8.RuneCountInString(*body.Email) > maxEmailRunes {
		invalidFields(w, r)
		return
	}
	if !a.EmailConfigured {
		emailOff.write(w)
		return
	}
	if !a.partnerPortal().available(w, r) {
		return
	}
	ctx, cancel := withTimeout(r, readTimeout)
	defer cancel()
	if err := a.PartnerPortal.RequestSignIn(ctx, *body.Email); err != nil {
		status, detail := partnerStatusCode(err)
		writeError(w, r, status, detail, err)
		return
	}
	writeJSON(w, http.StatusAccepted,
		map[string]string{"message": "If this email belongs to a partner portal user, a sign-in link will arrive shortly."})
}

// partnerAccept exchanges an emailed link for a session: a bearer token for
// the app, an HttpOnly cookie for a browser, which never sees the token.
func (a *api) partnerAccept(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Invite string `json:"invite"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !validToken(body.Invite) {
		writeError(w, r, http.StatusUnprocessableEntity, "This invite link is invalid or has expired", nil)
		return
	}
	a.partnerPortal().serve(w, r, func(ctx context.Context) (any, error) {
		token, err := a.PartnerPortal.AcceptInvite(ctx, body.Invite, native(r))
		if err != nil {
			return nil, err
		}
		if native(r) {
			return map[string]string{"session": token}, nil
		}
		a.setSessionCookie(w, partnerCookie, token, int(partnerSessionMaxAge.Seconds()))
		return map[string]bool{"signed_in": true}, nil
	})
}

func (a *api) partnerSignOut(w http.ResponseWriter, r *http.Request, session string) {
	a.setSessionCookie(w, partnerCookie, "", -1)
	a.partnerPortal().serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]any{"signed_out": true}, a.PartnerPortal.SignOut(ctx, session)
	})
}

func (a *api) partnerOverview(w http.ResponseWriter, r *http.Request, session string) {
	a.partnerPortal().serve(w, r, func(ctx context.Context) (any, error) { return a.PartnerPortal.Overview(ctx, session) })
}

func (a *api) partnerInitiatives(w http.ResponseWriter, r *http.Request, session string) {
	a.partnerPortal().serve(w, r, func(ctx context.Context) (any, error) {
		rows, err := a.PartnerPortal.Initiatives(ctx, session)
		return map[string]any{"initiatives": rows}, err
	})
}

func (a *api) partnerSpend(w http.ResponseWriter, r *http.Request, session string) {
	a.partnerPortal().serve(w, r, func(ctx context.Context) (any, error) {
		rows, err := a.PartnerPortal.Spend(ctx, session)
		return map[string]any{"categories": rows}, err
	})
}

func (a *api) partnerUsers(w http.ResponseWriter, r *http.Request, session string) {
	a.partnerPortal().serve(w, r, func(ctx context.Context) (any, error) {
		rows, err := a.PartnerPortal.Users(ctx, session)
		return map[string]any{"users": rows}, err
	})
}

func (a *api) partnerInviteUser(w http.ResponseWriter, r *http.Request, session string) {
	var body struct {
		Email string `json:"email"`
		Name  string `json:"name"`
		Role  string `json:"role"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	a.partnerPortal().serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]bool{"invited": true}, a.PartnerPortal.Invite(ctx, session, body.Email, body.Name, body.Role)
	})
}

func (a *api) partnerSaveProfile(w http.ResponseWriter, r *http.Request, session string) {
	var body struct {
		Organization *string `json:"organization"`
		Description  *string `json:"description"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Organization == nil || body.Description == nil {
		invalidFields(w, r)
		return
	}
	a.partnerPortal().serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]bool{"saved": true}, a.PartnerPortal.SaveProfile(ctx, session, *body.Organization, *body.Description)
	})
}

func (a *api) partnerRemoveUser(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "userID")
	if !ok {
		return
	}
	a.partnerPortal().serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]any{"removed": true}, a.PartnerPortal.RemoveUser(ctx, session, id)
	})
}

func (a *api) partnerExports(w http.ResponseWriter, r *http.Request, session string) {
	a.partnerPortal().serve(w, r, func(ctx context.Context) (any, error) {
		rows, err := a.PartnerPortal.Exports(ctx, session)
		return map[string]any{"exports": rows}, err
	})
}

// partnerCompanyCode is the IRL version of the company code (EM-1): a QR code
// of the code's landing link, for a poster or a slide.
func (a *api) partnerCompanyCode(w http.ResponseWriter, r *http.Request, session string) {
	if !a.partnerPortal().available(w, r) {
		return
	}
	ctx, cancel := withTimeout(r, readTimeout)
	defer cancel()
	code, err := a.PartnerPortal.CompanyCode(ctx, session)
	if err == nil && code == "" {
		err = pgx.ErrNoRows
	}
	if err != nil {
		respond(w, r, nil, err, partnerPortalStatus)
		return
	}
	image, err := sharing.SVG(a.Origin + "/c/" + code)
	if err != nil {
		writeError(w, r, http.StatusServiceUnavailable, "The partner portal is temporarily unavailable", err)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	_, _ = w.Write(image)
}

func partnerPortalStatus(err error) (int, string) {
	if errors.Is(err, pgx.ErrNoRows) {
		return http.StatusNotFound, "Your organization has no company code yet"
	}
	return partnerStatusCode(err)
}

// partnerReport streams the branded PDF (P-14). Not JSON — a file download.
func (a *api) partnerReport(w http.ResponseWriter, r *http.Request, session string) {
	if !a.partnerPortal().available(w, r) {
		return
	}
	ctx, cancel := withTimeout(r, staffTimeout)
	defer cancel()
	pdf, err := a.PartnerPortal.ExportReport(ctx, session)
	if err != nil {
		respond(w, r, nil, err, partnerStatusCode)
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `attachment; filename="dad-impact-report.pdf"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pdf)
}

func (a *api) partnerReportArtifact(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "report_id")
	if !ok {
		return
	}
	if !a.partnerPortal().available(w, r) {
		return
	}
	ctx, cancel := withTimeout(r, staffTimeout)
	defer cancel()
	pdf, err := a.PartnerPortal.ReportArtifact(ctx, session, id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, r, http.StatusNotFound, "This report was not retained or is not available", nil)
		return
	}
	if err != nil {
		respond(w, r, nil, err, partnerStatusCode)
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `attachment; filename="dad-impact-report.pdf"`)
	_, _ = w.Write(pdf)
}

func (a *api) partnerLogo(w http.ResponseWriter, r *http.Request, session string) {
	if !a.partnerPortal().available(w, r) {
		return
	}
	ctx, cancel := withTimeout(r, staffTimeout)
	defer cancel()
	logo, err := a.PartnerPortal.Logo(ctx, session)
	if err != nil {
		respond(w, r, nil, err, partnerStatusCode)
		return
	}
	writeLogo(w, r, logo)
}

func (a *api) partnerSaveLogo(w http.ResponseWriter, r *http.Request, session string) {
	dataURL, ok := logoBody(w, r)
	if !ok {
		return
	}
	a.partnerPortal().serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]bool{"saved": true}, a.PartnerPortal.SaveLogo(ctx, session, dataURL)
	})
}

// logoBody reads a logo upload: a data URL, or empty to remove the logo.
func logoBody(w http.ResponseWriter, r *http.Request) (string, bool) {
	var body struct {
		DataURL *string `json:"data_url"`
	}
	if !decodeJSONLimit(w, r, &body, 360000) {
		return "", false
	}
	if body.DataURL == nil {
		invalidFields(w, r)
		return "", false
	}
	return *body.DataURL, true
}

// writeLogo serves a stored logo, which is always re-encoded as PNG.
func writeLogo(w http.ResponseWriter, r *http.Request, logo []byte) {
	if len(logo) == 0 {
		writeError(w, r, http.StatusNotFound, "No organization logo has been uploaded", nil)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(logo)
}

func (a *api) staffPartnerSaveLogo(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "partnerID")
	if !ok {
		return
	}
	dataURL, ok := logoBody(w, r)
	if !ok {
		return
	}
	a.staffPartner().serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]bool{"saved": true}, a.StaffPartner.SetLogo(ctx, session, id, dataURL)
	})
}

func (a *api) staffPartnerLogo(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "partnerID")
	if !ok || !a.staffPartner().available(w, r) {
		return
	}
	ctx, cancel := withTimeout(r, staffTimeout)
	defer cancel()
	logo, err := a.StaffPartner.Logo(ctx, session, id)
	if err != nil {
		respond(w, r, nil, err, staffPartnerStatus)
		return
	}
	writeLogo(w, r, logo)
}

func (a *api) staffPartners(w http.ResponseWriter, r *http.Request, session string) {
	offset, ok := offsetParam(r, "offset")
	if !ok {
		invalidFields(w, r)
		return
	}
	a.staffPartner().serve(w, r, func(ctx context.Context) (any, error) { return a.StaffPartner.Partners(ctx, session, offset) })
}

func (a *api) staffPartnerDetail(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "partnerID")
	if !ok {
		return
	}
	a.staffPartner().serve(w, r, func(ctx context.Context) (any, error) { return a.StaffPartner.Partner(ctx, session, id) })
}

func (a *api) staffPartnerInviteUser(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "partnerID")
	if !ok {
		return
	}
	var body struct {
		Email string `json:"email"`
		Name  string `json:"name"`
		Role  string `json:"role"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	a.staffPartner().serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]bool{"invited": true}, a.StaffPartner.InviteUser(ctx, session, id, body.Email, body.Name, body.Role)
	})
}

func (a *api) staffPartnerRemoveUser(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "partnerID")
	if !ok {
		return
	}
	userID, ok := pathID(w, r, "userID")
	if !ok {
		return
	}
	a.staffPartner().serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]bool{"removed": true}, a.StaffPartner.RemoveUser(ctx, session, id, userID)
	})
}

func (a *api) staffPartnerPending(w http.ResponseWriter, r *http.Request, session string) {
	a.staffPartner().serve(w, r, func(ctx context.Context) (any, error) {
		rows, err := a.StaffPartner.PendingApplications(ctx, session, 0)
		return map[string]any{"applications": rows}, err
	})
}

func (a *api) staffPartnerApprove(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "appID")
	if !ok {
		return
	}
	a.staffPartner().serve(w, r, func(ctx context.Context) (any, error) {
		partnerID, err := a.StaffPartner.Approve(ctx, session, id)
		if err != nil {
			return nil, err
		}
		return map[string]any{"partner_id": partnerID.String()}, nil
	})
}

func (a *api) staffPartnerReject(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "appID")
	if !ok {
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	a.staffPartner().serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]any{"rejected": true}, a.StaffPartner.Reject(ctx, session, id, body.Reason)
	})
}

func (a *api) staffPartnerLink(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "partnerID")
	if !ok {
		return
	}
	var body struct {
		InitiativeID   string `json:"initiative_id"`
		CommittedMinor *int64 `json:"committed_minor"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	var initiativeID pgtype.UUID
	if initiativeID.Scan(body.InitiativeID) != nil || !initiativeID.Valid {
		invalidFields(w, r)
		return
	}
	a.staffPartner().serve(w, r, func(ctx context.Context) (any, error) {
		var amount []int64
		if body.CommittedMinor != nil {
			amount = append(amount, *body.CommittedMinor)
		}
		return map[string]any{"linked": true}, a.StaffPartner.LinkInitiative(ctx, session, id, initiativeID, amount...)
	})
}

func (a *api) staffPartnerMatch(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "partnerID")
	if !ok {
		return
	}
	var body struct {
		RateBP    int32 `json:"match_rate_bp"`
		CapMinor  int64 `json:"match_cap_minor"`
		Workforce int32 `json:"workforce_size"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	a.staffPartner().serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]any{"saved": true}, a.StaffPartner.SetMatch(ctx, session, id, body.Workforce, body.RateBP, body.CapMinor)
	})
}
