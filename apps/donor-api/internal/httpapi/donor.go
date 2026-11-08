package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/sharing"
	"dad/donor-api/internal/store"
)

type DashboardSource interface {
	DonorDashboard(context.Context, string) (store.DonorDashboard, error)
}

type DonorSource interface {
	Profile(context.Context, string) (store.Profile, error)
	SaveProfile(context.Context, string, store.ProfileUpdate) (store.Profile, error)
	DeleteAccount(context.Context, string) error
	SavePreferences(context.Context, string, store.PreferenceChange) (store.Profile, error)
	Sessions(context.Context, string) (store.Sessions, error)
	SignOutSession(context.Context, string, pgtype.UUID) (store.Sessions, error)
	SignOutOthers(context.Context, string) (store.Sessions, error)
	SignOutEverywhere(context.Context, string) error
	Sharing(context.Context, string) (store.Sharing, error)
	SaveSharing(context.Context, string, string, bool) (store.Sharing, error)
	ReferrerName(context.Context, string) (string, error)
	CaptureReferral(context.Context, string, string, string, time.Time) (store.ReferralCapture, error)
	CaptureCompanyCode(context.Context, string, string, time.Time) (store.CompanyCapture, error)
	BindCompanyTouch(context.Context, string, string) (bool, error)
	Gifts(context.Context, string, int64) (store.GiftsPage, error)
	Receipt(context.Context, string, pgtype.UUID) (store.Receipt, error)
	AnnualSummary(context.Context, string, int) (store.AnnualSummary, error)
	Conversions(context.Context, string, *time.Time) (store.Conversions, error)
}

const (
	referralCookieAge = 30 * 24 * 60 * 60
	maxPublicName     = 80
	donorUnavailable  = "Donor account is temporarily unavailable"
)

func (a *api) donor() service {
	return service{a.Donor != nil, donorUnavailable, readTimeout, donorStatus}
}

func donorStatus(err error) (int, string) {
	switch {
	case errors.Is(err, store.ErrDenied):
		return http.StatusUnauthorized, "Sign in required"
	case errors.Is(err, store.ErrCurrentSession):
		return http.StatusConflict, "This is the session making the request. Sign out instead"
	case errors.Is(err, store.ErrPreferenceConflict):
		return http.StatusConflict, "Preferences changed. Refresh before saving again"
	case errors.Is(err, store.ErrGivingActive):
		return http.StatusConflict, "Cancel your monthly giving, or let a checkout in progress finish, before deleting your account"
	case errors.Is(err, store.ErrPushToken):
		return http.StatusUnprocessableEntity, "The push token is not valid for this platform"
	case errors.Is(err, store.ErrPublicName):
		return http.StatusUnprocessableEntity, "Choose a public first name or nickname of 1 to 80 characters"
	case errors.Is(err, store.ErrZip):
		return http.StatusUnprocessableEntity, "Enter a valid US ZIP code"
	case errors.Is(err, store.ErrCity):
		return http.StatusUnprocessableEntity, "Enter a city of 1 to 80 characters"
	case errors.Is(err, store.ErrPhone):
		return http.StatusUnprocessableEntity, "Enter a valid phone number in international format"
	case errors.Is(err, store.ErrEventNotFound):
		return http.StatusNotFound, "Record not found"
	case errors.Is(err, store.ErrEventCancelled):
		return http.StatusConflict, "This event has been cancelled"
	case errors.Is(err, store.ErrReservationClosed):
		return http.StatusConflict, "This event has already started"
	case errors.Is(err, store.ErrBookingNotOpen):
		return http.StatusConflict, "Booking for this event has not opened yet"
	case errors.Is(err, store.ErrCheckIn):
		return http.StatusUnprocessableEntity, "Check-in is not valid for this event right now"
	case errors.Is(err, store.ErrCheckInDisabled):
		return http.StatusServiceUnavailable, "Event check-in is not configured"
	case errors.Is(err, pgx.ErrNoRows):
		return http.StatusNotFound, "Record not found"
	case errors.Is(err, store.ErrReceipt):
		return http.StatusConflict, "Gift records are unavailable for this request"
	case errors.Is(err, payments.ErrFacts):
		return http.StatusUnprocessableEntity, "Invalid request fields"
	default:
		return http.StatusServiceUnavailable, donorUnavailable
	}
}

func (a *api) dashboard(w http.ResponseWriter, r *http.Request, session string) {
	dashboards := service{a.Dashboard != nil, donorUnavailable, readTimeout, donorStatus}
	dashboards.serve(w, r, func(ctx context.Context) (any, error) { return a.Dashboard.DonorDashboard(ctx, session) })
}

func (a *api) profile(w http.ResponseWriter, r *http.Request, session string) {
	a.donor().serve(w, r, func(ctx context.Context) (any, error) { return a.Donor.Profile(ctx, session) })
}

func (a *api) saveProfile(w http.ResponseWriter, r *http.Request, session string) {
	var body struct {
		Zip   *string `json:"zip"`
		City  *string `json:"city"`
		Phone *string `json:"phone"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	update := store.ProfileUpdate{}
	if body.Zip != nil {
		update.Zip = *body.Zip
	}
	if body.City != nil {
		update.City = *body.City
	}
	if body.Phone != nil {
		update.Phone = *body.Phone
	}
	a.donor().serve(w, r, func(ctx context.Context) (any, error) { return a.Donor.SaveProfile(ctx, session, update) })
}

// deleteAccount erases the donor's personal data and signs them out. The donor
// session cookie is cleared since the deletion revokes every session.
func (a *api) deleteAccount(w http.ResponseWriter, r *http.Request, session string) {
	a.donor().serve(w, r, func(ctx context.Context) (any, error) {
		if err := a.Donor.DeleteAccount(ctx, session); err != nil {
			return nil, err
		}
		a.setSessionCookie(w, donorCookie, "", -1)
		return map[string]bool{"deleted": true}, nil
	})
}

// maxVolunteerInterests and maxInterestLength bound the E-1 interest set.
const (
	maxVolunteerInterests = 20
	maxInterestLength     = 60
)

// savePreferences replaces the donor's choices. The original six fields are
// required; older clients may omit the later ones, which keep their saved
// values.
func (a *api) savePreferences(w http.ResponseWriter, r *http.Request, session string) {
	var body struct {
		Version            *int32    `json:"version"`
		Referral           *bool     `json:"referral_emails"`
		Marketing          *bool     `json:"marketing_emails"`
		ReferralConverted  *bool     `json:"push_referral_converted"`
		StreakAtRisk       *bool     `json:"push_streak_at_risk"`
		StreakMilestone    *bool     `json:"push_streak_milestone"`
		StreakLapsed       *bool     `json:"push_streak_lapsed"`
		BadgeEarned        *bool     `json:"push_badge_earned"`
		GiftProcessed      *bool     `json:"push_gift_processed"`
		EventNearby        *bool     `json:"push_event_nearby"`
		EventReminder      *bool     `json:"push_event_reminder"`
		EventPromoted      *bool     `json:"push_event_promoted"`
		RankingsOptedOut   *bool     `json:"rankings_opted_out"`
		NetworkVisible     *bool     `json:"network_visible"`
		Volunteer          *bool     `json:"volunteer"`
		VolunteerInterests *[]string `json:"volunteer_interests"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Version == nil || *body.Version < 0 || body.Referral == nil || body.Marketing == nil ||
		body.ReferralConverted == nil || body.StreakAtRisk == nil || body.StreakMilestone == nil ||
		(body.VolunteerInterests != nil && !validInterests(*body.VolunteerInterests)) {
		invalidFields(w, r)
		return
	}
	change := store.PreferenceChange{
		Version: *body.Version, ReferralEmails: *body.Referral, MarketingEmails: *body.Marketing,
		PushReferralConverted: *body.ReferralConverted, PushStreakAtRisk: *body.StreakAtRisk, PushStreakMilestone: *body.StreakMilestone,
		PushStreakLapsed: body.StreakLapsed, PushBadgeEarned: body.BadgeEarned, PushGiftProcessed: body.GiftProcessed,
		PushEventNearby: body.EventNearby, PushEventReminder: body.EventReminder, PushEventPromoted: body.EventPromoted,
		RankingsOptedOut: body.RankingsOptedOut, NetworkVisible: body.NetworkVisible,
		Volunteer: body.Volunteer, VolunteerInterests: body.VolunteerInterests,
	}
	a.donor().serve(w, r, func(ctx context.Context) (any, error) { return a.Donor.SavePreferences(ctx, session, change) })
}

func validInterests(interests []string) bool {
	if len(interests) > maxVolunteerInterests {
		return false
	}
	seen := make(map[string]bool, len(interests))
	for _, interest := range interests {
		if interest == "" || strings.TrimSpace(interest) != interest || utf8.RuneCountInString(interest) > maxInterestLength ||
			seen[interest] {
			return false
		}
		seen[interest] = true
	}
	return true
}

func (a *api) logoutAll(w http.ResponseWriter, r *http.Request, session string) {
	a.donor().serve(w, r, func(ctx context.Context) (any, error) {
		err := a.Donor.SignOutEverywhere(ctx, session)
		if err == nil || errors.Is(err, store.ErrDenied) {
			a.clearDonorCookie(w, r)
		}
		return map[string]bool{"signed_out": true}, err
	})
}

func (a *api) sharing(w http.ResponseWriter, r *http.Request, session string) {
	a.donor().serve(w, r, func(ctx context.Context) (any, error) { return a.Donor.Sharing(ctx, session) })
}

func (a *api) saveSharing(w http.ResponseWriter, r *http.Request, session string) {
	var body struct {
		Name    *string         `json:"public_name"`
		Replace json.RawMessage `json:"replace_code"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	replace, ok := optional(body.Replace, false)
	if !ok || body.Name == nil || utf8.RuneCountInString(*body.Name) < 1 || utf8.RuneCountInString(*body.Name) > maxPublicName {
		invalidFields(w, r)
		return
	}
	a.donor().serve(w, r, func(ctx context.Context) (any, error) { return a.Donor.SaveSharing(ctx, session, *body.Name, replace) })
}

// irlLink returns the donor's in-person referral URL, answering the request
// itself when the donor cannot have one yet.
func (a *api) irlLink(w http.ResponseWriter, r *http.Request, session, missing string) (string, bool) {
	if !a.donor().available(w, r) {
		return "", false
	}
	ctx, cancel := withTimeout(r, readTimeout)
	defer cancel()
	details, err := a.Donor.Sharing(ctx, session)
	if err != nil {
		respond(w, r, nil, err, donorStatus)
		return "", false
	}
	if details.IRLLink == nil {
		writeError(w, r, http.StatusConflict, missing, nil)
		return "", false
	}
	return *details.IRLLink, true
}

func (a *api) referralCode(w http.ResponseWriter, r *http.Request, session string) {
	link, ok := a.irlLink(w, r, session, "Choose a public nickname before showing your referral code")
	if !ok {
		return
	}
	image, err := sharing.SVG(link)
	if err != nil {
		writeError(w, r, http.StatusServiceUnavailable, donorUnavailable, err)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	_, _ = w.Write(image)
}

const referralGone = "This referral link is no longer available"

// referralCode reads a code from the path. Issued codes are URL-safe base64,
// so anything else names no referrer.
func referralCode(w http.ResponseWriter, r *http.Request) (string, bool) {
	code := r.PathValue("code")
	valid := len(code) <= 64 && strings.Trim(code, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_") == ""
	if !valid {
		writeError(w, r, http.StatusNotFound, referralGone, nil)
	}
	return code, valid
}

func (a *api) referrer(w http.ResponseWriter, r *http.Request) {
	code, ok := referralCode(w, r)
	if !ok {
		return
	}
	a.donor().missing(referralGone).serve(w, r, func(ctx context.Context) (any, error) {
		name, err := a.Donor.ReferrerName(ctx, code)
		return map[string]string{"public_name": name}, err
	})
}

func (a *api) captureReferral(w http.ResponseWriter, r *http.Request) {
	code, ok := referralCode(w, r)
	if !ok {
		return
	}
	var body struct {
		Kind  json.RawMessage `json:"kind"`
		Token json.RawMessage `json:"token"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	kind, ok := optional(body.Kind, "LINK")
	visitor, visitorOK := visitorToken(r, referralCookie, body.Token)
	if !ok || (kind != "LINK" && kind != "IRL") || !visitorOK {
		invalidFields(w, r)
		return
	}
	if !a.donor().available(w, r) {
		return
	}
	ctx, cancel := withTimeout(r, readTimeout)
	defer cancel()
	result, err := a.Donor.CaptureReferral(ctx, code, kind, visitor, time.Now().UTC())
	if err != nil {
		respond(w, r, nil, err, donorStatus)
		return
	}
	response := map[string]any{"captured": result.Token != "", "public_name": result.PublicName}
	a.keepVisitor(w, r, referralCookie, result.Token, response)
	writeJSON(w, http.StatusOK, response)
}

// captureCompany records a pre-signup company-code visit on an anonymous
// visitor token, mirroring referral capture. The response never reveals whether
// the code resolves to a partner. Account ownership is proved before linking it.
func (a *api) captureCompany(w http.ResponseWriter, r *http.Request) {
	code, ok := referralCode(w, r)
	if !ok {
		return
	}
	var body struct {
		Token json.RawMessage `json:"token"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	visitor, ok := visitorToken(r, companyCookie, body.Token)
	if !ok {
		invalidFields(w, r)
		return
	}
	if !a.donor().available(w, r) {
		return
	}
	ctx, cancel := withTimeout(r, readTimeout)
	defer cancel()
	result, err := a.Donor.CaptureCompanyCode(ctx, code, visitor, time.Now().UTC())
	if err != nil {
		respond(w, r, nil, err, donorStatus)
		return
	}
	response := map[string]any{"captured": result.Token != ""}
	a.keepVisitor(w, r, companyCookie, result.Token, response)
	writeJSON(w, http.StatusOK, response)
}

// visitorToken is the caller's earlier capture token: the cookie of a
// browser, or the token the app kept from its last capture response. Only the
// app may name one in the body; a browser's stays HttpOnly.
func visitorToken(r *http.Request, cookie string, raw json.RawMessage) (string, bool) {
	if !native(r) {
		return cookieValue(r, cookie), raw == nil
	}
	token, ok := optional(raw, "")
	return token, ok && (token == "" || validToken(token))
}

// keepVisitor hands a capture's token back where visitorToken reads it: a
// cookie for a browser, cleared when nothing was captured, or the response
// body for the app, as null when nothing was captured.
func (a *api) keepVisitor(w http.ResponseWriter, r *http.Request, cookie, token string, response map[string]any) {
	if native(r) {
		response["token"] = nil
		if token != "" {
			response["token"] = token
		}
		return
	}
	age := referralCookieAge
	if token == "" {
		age = -1
	}
	a.setCookie(w, cookie, token, age, http.SameSiteLaxMode)
}

// bindCompanyTouch binds a company code the app captured before sign-in, as a
// browser's company cookie binds when it signs in.
func (a *api) bindCompanyTouch(w http.ResponseWriter, r *http.Request, session string) {
	body, ok := decodeValid[companyTouchBody](w, r)
	if !ok {
		return
	}
	a.donor().serve(w, r, func(ctx context.Context) (any, error) {
		bound, err := a.Donor.BindCompanyTouch(ctx, session, body.Token)
		return map[string]bool{"bound": bound}, err
	})
}

type companyTouchBody struct {
	Token string `json:"token"`
}

func (b companyTouchBody) valid() bool { return validToken(b.Token) }

func (a *api) gifts(w http.ResponseWriter, r *http.Request, session string) {
	offset, ok := offsetParam(r, "offset")
	if !ok {
		invalidFields(w, r)
		return
	}
	a.donor().serve(w, r, func(ctx context.Context) (any, error) { return a.Donor.Gifts(ctx, session, offset) })
}

func (a *api) receipt(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "gift_id")
	if !ok {
		return
	}
	a.donor().missing("Gift not found").serve(w, r, func(ctx context.Context) (any, error) { return a.Donor.Receipt(ctx, session, id) })
}

func (a *api) annualSummary(w http.ResponseWriter, r *http.Request, session string) {
	year, err := strconv.Atoi(r.PathValue("year"))
	if err != nil || year < 2000 || year > 9999 {
		invalidFields(w, r)
		return
	}
	a.donor().serve(w, r, func(ctx context.Context) (any, error) { return a.Donor.AnnualSummary(ctx, session, year) })
}
