package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"dad/donor-api/internal/geo"
	"dad/donor-api/internal/identity"
	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// consentVersion names the notification consent wording a donor accepted,
// which preference events record.
const consentVersion = "r2-notifications-v1"

// referralWindow is how long a recorded referral touch holds before it expires.
const referralWindow = 30 * 24 * time.Hour

// pageSize is the length of every offset-paged list. Queries fetch one extra
// row to learn whether another page follows.
const pageSize = 50

func pageOffsetValid(offset int64) bool { return offset >= 0 && offset <= math.MaxInt64-pageSize }

// page trims a query's look-ahead row and returns the next page's offset, if any.
func page[T any](rows []T, offset int64) ([]T, *int64) {
	if len(rows) > pageSize {
		next := offset + pageSize
		return rows[:pageSize], &next
	}
	return rows, nil
}

var ErrPreferenceConflict = errors.New("preferences changed; refresh before saving")

// checkoutSettleMargin allows for the completion event of a checkout paid just
// before it expired to arrive and be processed.
const checkoutSettleMargin = time.Hour

// ErrGivingActive refuses an account deletion while a monthly gift would keep
// charging the card of a donor whose account no longer exists.
var ErrGivingActive = errors.New("cancel monthly giving before deleting the account")
var ErrPublicName = errors.New("choose a public first name or nickname of 1 to 80 characters")

// ErrPublicNameCharacters is an ErrPublicName for a name that looks like an
// email address or contains control characters.
var ErrPublicNameCharacters = fmt.Errorf("%w, without @ or control characters", ErrPublicName)

type DonorService struct {
	DB     *pgxpool.Pool
	Origin string
}

type Profile struct {
	Email            string  `json:"email"`
	PublicName       *string `json:"public_name"`
	AccountRole      string  `json:"account_role"`
	Zip              *string `json:"zip"`
	City             *string `json:"city"`
	Timezone         *string `json:"timezone"`
	TimezoneOverride *string `json:"timezone_override,omitempty"`
	Phone            *string `json:"phone"`
	ConsentVersion   string  `json:"consent_version"`
	Preferences
}

// Preferences are the donor's notification choices. Email is opt-in; app
// notifications start on, since the device's permission prompt is the opt-in.
type Preferences struct {
	Version               int32 `json:"version"`
	ReferralEmails        bool  `json:"referral_emails"`
	MarketingEmails       bool  `json:"marketing_emails"`
	PushReferralConverted bool  `json:"push_referral_converted"`
	PushStreakAtRisk      bool  `json:"push_streak_at_risk"`
	PushStreakMilestone   bool  `json:"push_streak_milestone"`
	PushStreakLapsed      bool  `json:"push_streak_lapsed"`
	PushBadgeEarned       bool  `json:"push_badge_earned"`
	PushGiftProcessed     bool  `json:"push_gift_processed"`
	PushEventNearby       bool  `json:"push_event_nearby"`
	PushEventReminder     bool  `json:"push_event_reminder"`
	PushEventPromoted     bool  `json:"push_event_promoted"`
	RankingsOptedOut      bool  `json:"rankings_opted_out"`
	NetworkVisible        bool  `json:"network_visible"`
	// Volunteer opt-in (PRD E-1): turning this on puts the donor in the
	// volunteer pool and turns on event alerts. VolunteerInterests is the set
	// of event interests they picked; empty when they have chosen none.
	Volunteer          bool     `json:"volunteer"`
	VolunteerInterests []string `json:"volunteer_interests"`
}

// defaultPreferences apply until a donor first saves a choice. It is a function
// (not a shared var) so each caller gets its own empty interests slice rather
// than aliasing one, and so the slice marshals as [] not null.
func defaultPreferences() Preferences {
	return Preferences{
		PushReferralConverted: true, PushStreakAtRisk: true, PushStreakMilestone: true,
		PushStreakLapsed: true, PushBadgeEarned: true, PushGiftProcessed: true, NetworkVisible: true,
		PushEventNearby: true, PushEventReminder: true, PushEventPromoted: true,
		VolunteerInterests: []string{},
	}
}

// readPreferences returns the donor's current choices.
func readPreferences(ctx context.Context, q *queries.Queries, donor pgtype.UUID) (Preferences, error) {
	pref, err := q.DonorPreference(ctx, donor)
	if errors.Is(err, pgx.ErrNoRows) {
		return defaultPreferences(), nil
	}
	interests := pref.VolunteerInterests
	if interests == nil {
		interests = []string{}
	}
	return Preferences{
		Version: pref.Version, ReferralEmails: pref.ReferralEmails, MarketingEmails: pref.MarketingEmails,
		PushReferralConverted: pref.PushReferralConverted, PushStreakAtRisk: pref.PushStreakAtRisk, PushStreakMilestone: pref.PushStreakMilestone,
		PushStreakLapsed: pref.PushStreakLapsed, PushBadgeEarned: pref.PushBadgeEarned, PushGiftProcessed: pref.PushGiftProcessed,
		PushEventNearby: pref.PushEventNearby, PushEventReminder: pref.PushEventReminder, PushEventPromoted: pref.PushEventPromoted,
		RankingsOptedOut: pref.RankingsOptedOut, NetworkVisible: pref.NetworkVisible,
		Volunteer: pref.Volunteer, VolunteerInterests: interests,
	}, err
}

type Sharing struct {
	PublicName *string `json:"public_name"`
	Link       *string `json:"link"`
	IRLLink    *string `json:"irl_link"`
	// ReferralCode is the code inside Link and IRLLink, null exactly when they are.
	ReferralCode *string `json:"referral_code"`
}

type ReferralCapture struct {
	Token      string
	PublicName *string
}

type GiftsPage struct {
	Email      string                      `json:"email"`
	Mode       string                      `json:"mode"`
	Gifts      []queries.DonorGiftsPageRow `json:"gifts"`
	NextOffset *int64                      `json:"next_offset"`
}

func readProfile(ctx context.Context, q *queries.Queries, donor queries.Donor) (Profile, error) {
	pref, err := readPreferences(ctx, q, donor.ID)
	return Profile{
		Email: donor.Email, PublicName: textPointer(donor.PublicName), AccountRole: donor.AccountRole,
		Zip: textPointer(donor.Zip), City: textPointer(donor.City), Timezone: textPointer(donor.Timezone),
		TimezoneOverride: textPointer(
			donor.TimezoneOverride,
		), Phone: textPointer(donor.Phone), ConsentVersion: consentVersion, Preferences: pref,
	}, err
}

func (s DonorService) Profile(ctx context.Context, token string) (Profile, error) {
	return donorRead(
		ctx,
		s.DB,
		token,
		func(q *queries.Queries, d queries.Donor, _ time.Time) (Profile, error) { return readProfile(ctx, q, d) },
	)
}

// PreferenceChange is a save of the donor's choices. A nil field keeps its
// saved value, so a client that predates a choice cannot clear it.
type PreferenceChange struct {
	Version               int32
	ReferralEmails        bool
	MarketingEmails       bool
	PushReferralConverted bool
	PushStreakAtRisk      bool
	PushStreakMilestone   bool
	PushStreakLapsed      *bool
	PushBadgeEarned       *bool
	PushGiftProcessed     *bool
	PushEventNearby       *bool
	PushEventReminder     *bool
	PushEventPromoted     *bool
	RankingsOptedOut      *bool
	NetworkVisible        *bool
	Volunteer             *bool
	VolunteerInterests    *[]string
}

// Replace is a change that sets every choice to pref.
func (pref Preferences) Replace() PreferenceChange {
	interests := pref.VolunteerInterests
	return PreferenceChange{
		Version: pref.Version, ReferralEmails: pref.ReferralEmails, MarketingEmails: pref.MarketingEmails,
		PushReferralConverted: pref.PushReferralConverted, PushStreakAtRisk: pref.PushStreakAtRisk, PushStreakMilestone: pref.PushStreakMilestone,
		PushStreakLapsed: &pref.PushStreakLapsed, PushBadgeEarned: &pref.PushBadgeEarned, PushGiftProcessed: &pref.PushGiftProcessed,
		PushEventNearby: &pref.PushEventNearby, PushEventReminder: &pref.PushEventReminder, PushEventPromoted: &pref.PushEventPromoted,
		RankingsOptedOut: &pref.RankingsOptedOut, NetworkVisible: &pref.NetworkVisible,
		Volunteer: &pref.Volunteer, VolunteerInterests: &interests,
	}
}

func (c PreferenceChange) apply(saved Preferences) Preferences {
	pref := Preferences{
		Version: c.Version, ReferralEmails: c.ReferralEmails, MarketingEmails: c.MarketingEmails,
		PushReferralConverted: c.PushReferralConverted, PushStreakAtRisk: c.PushStreakAtRisk, PushStreakMilestone: c.PushStreakMilestone,
		PushStreakLapsed: saved.PushStreakLapsed, PushBadgeEarned: saved.PushBadgeEarned, PushGiftProcessed: saved.PushGiftProcessed,
		PushEventNearby: saved.PushEventNearby, PushEventReminder: saved.PushEventReminder, PushEventPromoted: saved.PushEventPromoted,
		RankingsOptedOut: saved.RankingsOptedOut, NetworkVisible: saved.NetworkVisible,
		Volunteer: saved.Volunteer, VolunteerInterests: saved.VolunteerInterests,
	}
	for _, field := range []struct {
		value *bool
		into  *bool
	}{
		{c.PushGiftProcessed, &pref.PushGiftProcessed}, {c.PushStreakLapsed, &pref.PushStreakLapsed}, {c.PushBadgeEarned, &pref.PushBadgeEarned},
		{c.PushEventNearby, &pref.PushEventNearby}, {c.PushEventReminder, &pref.PushEventReminder},
		{c.PushEventPromoted, &pref.PushEventPromoted},
		{c.RankingsOptedOut, &pref.RankingsOptedOut}, {c.NetworkVisible, &pref.NetworkVisible}, {c.Volunteer, &pref.Volunteer},
	} {
		if field.value != nil {
			*field.into = *field.value
		}
	}
	if c.VolunteerInterests != nil {
		pref.VolunteerInterests = *c.VolunteerInterests
	}
	return pref
}

// SavePreferences applies change if its Version is still current, and records
// the consent change.
func (s DonorService) SavePreferences(ctx context.Context, token string, change PreferenceChange) (Profile, error) {
	return donorWrite(ctx, s.DB, donorOwner(token), func(tx pgx.Tx, d queries.Donor, now time.Time) (Profile, error) {
		q := queries.New(tx)
		current, err := readProfile(ctx, q, d)
		if err != nil {
			return Profile{}, err
		}
		if change.Version < 0 || change.Version != current.Version || change.Version == math.MaxInt32 {
			return Profile{}, ErrPreferenceConflict
		}
		pref := change.apply(current.Preferences)
		id, err := NewID()
		if err != nil {
			return Profile{}, err
		}
		version := current.Version + 1
		interests := pref.VolunteerInterests
		if interests == nil {
			interests = []string{}
		}
		if err = q.SavePreference(ctx, queries.SavePreferenceParams{
			ID: id, DonorID: d.ID, Version: version, ReferralEmails: pref.ReferralEmails, MarketingEmails: pref.MarketingEmails,
			PushReferralConverted: pref.PushReferralConverted, PushStreakAtRisk: pref.PushStreakAtRisk, PushStreakMilestone: pref.PushStreakMilestone,
			PushStreakLapsed: pref.PushStreakLapsed, PushBadgeEarned: pref.PushBadgeEarned, PushGiftProcessed: pref.PushGiftProcessed,
			PushEventNearby: pref.PushEventNearby, PushEventReminder: pref.PushEventReminder, PushEventPromoted: pref.PushEventPromoted,
			RankingsOptedOut: pref.RankingsOptedOut, NetworkVisible: pref.NetworkVisible,
			Volunteer: pref.Volunteer, VolunteerInterests: interests,
		}); err != nil {
			return Profile{}, err
		}
		event, err := NewID()
		if err != nil {
			return Profile{}, err
		}
		if err = q.RecordPreferenceConsent(ctx, queries.RecordPreferenceConsentParams{
			ID: event, DonorID: d.ID, Version: version, ReferralEmails: pref.ReferralEmails, MarketingEmails: pref.MarketingEmails,
			PushReferralConverted: pgtype.Bool{Bool: pref.PushReferralConverted, Valid: true}, PushStreakAtRisk: pgtype.Bool{Bool: pref.PushStreakAtRisk, Valid: true},
			PushStreakMilestone: pgtype.Bool{Bool: pref.PushStreakMilestone, Valid: true}, ConsentVersion: consentVersion,
			PushStreakLapsed: pgtype.Bool{Bool: pref.PushStreakLapsed, Valid: true}, PushBadgeEarned: pgtype.Bool{Bool: pref.PushBadgeEarned, Valid: true},
			PushGiftProcessed: pgtype.Bool{Bool: pref.PushGiftProcessed, Valid: true},
			PushEventNearby:   pgtype.Bool{Bool: pref.PushEventNearby, Valid: true},
			PushEventReminder: pgtype.Bool{Bool: pref.PushEventReminder, Valid: true},
			PushEventPromoted: pgtype.Bool{Bool: pref.PushEventPromoted, Valid: true},
			RankingsOptedOut:  pgtype.Bool{Bool: pref.RankingsOptedOut, Valid: true}, NetworkVisible: pgtype.Bool{Bool: pref.NetworkVisible, Valid: true},
			Volunteer: pgtype.Bool{Bool: pref.Volunteer, Valid: true}, VolunteerInterests: interests,
		}); err != nil {
			return Profile{}, err
		}
		if pref.Volunteer && !current.Volunteer {
			if err = awardBadges(ctx, q, d, now); err != nil {
				return Profile{}, err
			}
		}
		return readProfile(ctx, q, d)
	})
}

var (
	ErrZip   = fmt.Errorf("%w", geo.ErrZip)
	ErrPhone = errors.New("enter a valid phone number in international format, e.g. +14155550123")
	ErrCity  = errors.New("enter a city of 1 to 80 characters")
)

// phonePattern mirrors the ck_donor_phone database constraint (E.164).
var phonePattern = regexp.MustCompile(`^\+[1-9][0-9]{1,14}$`)

// An omitted correction preserves the donor's choice; empty explicitly restores ZIP suggestions.
type ProfileUpdate struct {
	Zip              string
	City             string
	Phone            string
	TimezoneOverride *string
}

var ErrTimezone = errors.New("choose a supported US time zone")

func (s DonorService) SaveProfile(ctx context.Context, token string, in ProfileUpdate) (Profile, error) {
	zip, city, phone := strings.TrimSpace(in.Zip), strings.TrimSpace(in.City), strings.TrimSpace(in.Phone)

	if in.TimezoneOverride != nil && *in.TimezoneOverride != "" && !geo.ValidTimezoneOverride(*in.TimezoneOverride) {
		return Profile{}, ErrTimezone
	}
	var zipCol, tzCol pgtype.Text
	if zip != "" {
		normalized, err := geo.NormalizeZip(zip)
		if err != nil {
			return Profile{}, ErrZip
		}
		zone, err := geo.Timezone(normalized)
		if err != nil {
			return Profile{}, ErrZip
		}
		zipCol, tzCol = text(normalized), text(zone)
	}

	var cityCol pgtype.Text
	if city != "" {
		if utf8.RuneCountInString(city) > 80 || !plainText(city, false) {
			return Profile{}, ErrCity
		}
		cityCol = text(city)
	}

	var phoneCol pgtype.Text
	if phone != "" {
		if !phonePattern.MatchString(phone) {
			return Profile{}, ErrPhone
		}
		phoneCol = text(phone)
	}

	return donorWrite(ctx, s.DB, donorOwner(token), func(tx pgx.Tx, d queries.Donor, now time.Time) (Profile, error) {
		q := queries.New(tx)
		override := d.TimezoneOverride
		if in.TimezoneOverride != nil {
			override = text(*in.TimezoneOverride)
			if *in.TimezoneOverride == "" {
				override = pgtype.Text{}
			}
		}
		if override.Valid {
			tzCol = override
		}
		if d.Phone.Valid && d.Phone != phoneCol {
			if err := q.DeletePhoneCode(ctx, d.Phone.String); err != nil {
				return Profile{}, err
			}
		}
		updated, err := q.UpdateDonorProfile(ctx, queries.UpdateDonorProfileParams{
			ID: d.ID, Zip: zipCol, City: cityCol, Timezone: tzCol, Phone: phoneCol, TimezoneOverride: override,
		})
		if err != nil {
			if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == uniqueViolation {
				return Profile{}, ErrPhone
			}
			return Profile{}, err
		}
		if d.Timezone != updated.Timezone {
			if err := q.InvalidateDonorProjection(ctx, queries.InvalidateDonorProjectionParams{DonorID: d.ID, AsOf: timestamp(now)}); err != nil {
				return Profile{}, err
			}
		}
		return readProfile(ctx, q, updated)
	})
}

// DeleteAccount erases the signed-in donor's personal data (PRD §6.12). It
// anonymises the donor row and removes personal satellites: passkeys, OIDC
// identities, preferences, login links, the addresses of email changes, the
// device names of sessions, which are all revoked, and the employer link. It
// gives up the donor's places at upcoming events, so the waitlist moves up. The
// financial ledger (gifts, checkouts, billing periods, refunds) is retained for
// legal and tax records and carries no personal data of its own. The erasure
// is recorded in donor_deletions, which outlives the scrub. It is refused
// while a monthly gift is still set to renew.
func (s DonorService) DeleteAccount(ctx context.Context, token string) error {
	_, err := donorWrite(ctx, s.DB, donorOwner(token), func(tx pgx.Tx, d queries.Donor, now time.Time) (struct{}, error) {
		q := queries.New(tx)
		if renewing, err := givingRenews(ctx, q, d.ID, now); err != nil || renewing {
			if err == nil {
				err = ErrGivingActive
			}
			return struct{}{}, err
		}
		if d.Phone.Valid {
			if err := q.DeletePhoneCode(ctx, d.Phone.String); err != nil {
				return struct{}{}, err
			}
		}
		if err := q.RecordDonorDeletion(ctx, queries.RecordDonorDeletionParams{DonorID: d.ID, DeletedAt: timestamp(now)}); err != nil {
			return struct{}{}, err
		}
		if err := releaseEventPlaces(ctx, q, d.ID, now); err != nil {
			return struct{}{}, err
		}
		if _, err := q.DeleteAllDonorPasskeys(ctx, d.ID); err != nil {
			return struct{}{}, err
		}
		for _, scrub := range []func(context.Context, pgtype.UUID) error{
			q.DeleteDonorAnalytics, q.DeleteDonorPassword, q.DeleteDonorOIDCIdentities, q.DeleteDonorPreferences, q.CancelEmailChanges,
			q.ScrubDonorEmailChanges, q.ScrubDonorMessages, q.DeleteEmployeeLink,
		} {
			if err := scrub(ctx, d.ID); err != nil {
				return struct{}{}, err
			}
		}
		if err := q.ConsumeDonorLinks(ctx, queries.ConsumeDonorLinksParams{DonorID: d.ID, ConsumedAt: timestamp(now)}); err != nil {
			return struct{}{}, err
		}
		if err := q.RevokeAllSessions(ctx, queries.RevokeAllSessionsParams{DonorID: d.ID, RevokedAt: timestamp(now)}); err != nil {
			return struct{}{}, err
		}
		if err := q.ScrubDonorSessionDevices(ctx, d.ID); err != nil {
			return struct{}{}, err
		}
		// Scrub the donor row last, so a failure above leaves the account intact
		// and retryable rather than half-erased.
		return struct{}{}, q.AnonymizeDonor(ctx, d.ID)
	})
	return err
}

// givingRenews reports whether the donor's latest monthly gift will charge
// again: its subscription is live and not scheduled to stop, checkout has
// finished but the subscription has not been synced yet, or a checkout is
// still open and could yet complete.
func givingRenews(ctx context.Context, q *queries.Queries, donor pgtype.UUID, now time.Time) (bool, error) {
	attempt, row, err := syncedSubscription(ctx, q, donor)
	if err != nil {
		return false, err
	}
	if row == nil {
		open := (attempt.Status == "PENDING" || attempt.Status == "OPEN") &&
			now.Before(attempt.CreatedAt.Time.Add(checkoutLifetime+checkoutSettleMargin))
		return attempt.StripeSubscriptionID.Valid || attempt.Status == "COMPLETE" || open, nil
	}
	return !terminalSubscription(row.Status) && !row.StopsAt.Valid, nil
}

// releaseEventPlaces cancels a donor's reservations at upcoming events and
// fills each seat it frees from the waitlist. Events are locked in id order,
// so concurrent releases cannot deadlock.
func releaseEventPlaces(ctx context.Context, q *queries.Queries, donor pgtype.UUID, now time.Time) error {
	places, err := q.DonorUpcomingReservations(ctx, queries.DonorUpcomingReservationsParams{DonorID: donor, AsOf: timestamp(now)})
	if err != nil {
		return err
	}
	for _, place := range places {
		event, err := q.LockEvent(ctx, place.EventID)
		if err != nil {
			return err
		}
		if err = q.SetReservationStatus(ctx, queries.SetReservationStatusParams{
			EventID: place.EventID, DonorID: donor, Status: reservationCancelled, UpdatedAt: timestamp(now),
		}); err != nil {
			return err
		}
		if place.Status == reservationReserved {
			if err = fillSeats(ctx, q, event, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// sharing offers referral links only once the donor has chosen a public name.
func sharing(d queries.Donor, site publicSite) Sharing {
	out := Sharing{PublicName: textPointer(d.PublicName)}
	if d.ReferralCode.String != "" && d.PublicName.String != "" {
		link, irl := site.referral(d.ReferralCode.String), site.irlReferral(d.ReferralCode.String)
		code := d.ReferralCode.String
		out.Link, out.IRLLink, out.ReferralCode = &link, &irl, &code
	}
	return out
}

func (s DonorService) Sharing(ctx context.Context, token string) (Sharing, error) {
	site, err := site(s.Origin)
	if err != nil {
		return Sharing{}, err
	}
	return donorRead(
		ctx,
		s.DB,
		token,
		func(_ *queries.Queries, d queries.Donor, _ time.Time) (Sharing, error) { return sharing(d, site), nil },
	)
}

// SaveSharing sets the donor's public name, issuing a referral code on first
// use or a replacement when replace is set; a replaced code stops working.
func (s DonorService) SaveSharing(ctx context.Context, token, name string, replace bool) (Sharing, error) {
	site, err := site(s.Origin)
	if err != nil {
		return Sharing{}, err
	}
	name = identity.TrimSpace(name)
	if !plainText(name, false) || strings.Contains(name, "@") {
		return Sharing{}, ErrPublicNameCharacters
	}
	if name == "" || utf8.RuneCountInString(name) > 80 {
		return Sharing{}, ErrPublicName
	}
	return donorWrite(ctx, s.DB, donorOwner(token), func(tx pgx.Tx, d queries.Donor, _ time.Time) (Sharing, error) {
		code := d.ReferralCode
		if !code.Valid || replace {
			value, err := randomToken(24)
			if err != nil {
				return Sharing{}, err
			}
			code = text(value)
		}
		d, err := queries.New(tx).SaveSharing(ctx, queries.SaveSharingParams{ID: d.ID, PublicName: text(name), ReferralCode: code})
		if err != nil {
			return Sharing{}, err
		}
		return sharing(d, site), nil
	})
}

// referralCode matches the codes SaveSharing issues; anything else cannot
// name a referrer.
var referralCode = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ReferrerName resolves a shareable code to its owner's chosen public name.
func (s DonorService) ReferrerName(ctx context.Context, code string) (string, error) {
	if !referralCode.MatchString(code) {
		return "", pgx.ErrNoRows
	}
	referrer, err := queries.New(s.DB).ReferrerByCode(ctx, text(code))
	if err != nil {
		return "", err
	}
	return referrer.PublicName.String, nil
}

// CaptureReferral records a visit through a referral code. A later valid code
// overwrites an earlier capture for the same visitor: the most recent touch
// wins (last-touch, PRD R-4). It returns the visitor token to keep, which is
// empty for an unknown code, with the captured referrer's public name.
func (s DonorService) CaptureReferral(ctx context.Context, code, kind, visitor string, now time.Time) (ReferralCapture, error) {
	if kind != "LINK" && kind != "IRL" {
		return ReferralCapture{}, payments.ErrFacts
	}
	return transact(ctx, s.DB, pgx.TxOptions{}, func(tx pgx.Tx) (ReferralCapture, error) {
		q := queries.New(tx)
		token, err := captureTouch(ctx, q, code, kind, visitor, now)
		if err != nil || token == "" {
			return ReferralCapture{}, err
		}
		name, err := q.CapturedReferrerName(ctx, HashToken(token))
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return ReferralCapture{}, err
		}
		return ReferralCapture{Token: token, PublicName: textPointer(name)}, nil
	})
}

func captureTouch(ctx context.Context, q *queries.Queries, code, kind, visitor string, now time.Time) (string, error) {
	// Resolve a well-formed code to its referrer. A malformed or unknown code is
	// not a new touch: fall back to the visitor's existing unexpired touch.
	var referrer queries.Donor
	if referralCode.MatchString(code) {
		found, err := q.ReferrerByCode(ctx, text(code))
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", err
		}
		if err == nil {
			referrer = found
		}
	}
	if referrer.ID == (pgtype.UUID{}) {
		if visitor != "" {
			touch, terr := q.ReferralTouchByToken(ctx, HashToken(visitor))
			if terr == nil && touch.ExpiresAt.Time.After(now) {
				return visitor, nil
			}
			if terr != nil && !errors.Is(terr, pgx.ErrNoRows) {
				return "", terr
			}
		}
		return "", nil
	}
	// A new valid code wins (last-touch, PRD R-4). Reuse the visitor's token
	// only while its touch is still unexpired; an expired or unknown token gets
	// a fresh identity rather than being resurrected.
	token := ""
	if visitor != "" {
		touch, terr := q.ReferralTouchByToken(ctx, HashToken(visitor))
		if terr != nil && !errors.Is(terr, pgx.ErrNoRows) {
			return "", terr
		}
		if terr == nil && touch.ExpiresAt.Time.After(now) {
			token = visitor
		}
	}
	if token == "" {
		minted, err := randomToken(32)
		if err != nil {
			return "", err
		}
		token = minted
	}
	id, err := NewID()
	if err != nil {
		return "", err
	}
	return token, q.UpsertReferralTouch(
		ctx,
		queries.UpsertReferralTouchParams{
			ID:         id,
			TokenHash:  HashToken(token),
			ReferrerID: referrer.ID,
			Kind:       kind,
			TouchedAt:  timestamp(now),
			ExpiresAt:  timestamp(now.Add(referralWindow)),
		},
	)
}

// companyCode is the accepted employer-code form; it mirrors the ck_donor_company_code
// database constraint (uppercase alphanumeric, 2-64 chars, internal hyphens).
var companyCode = regexp.MustCompile(`^[A-Z0-9][A-Z0-9-]{1,63}$`)

// CompanyCapture is the visitor token to keep after a company-code touch, empty
// for a malformed code.
type CompanyCapture struct {
	Token string `json:"token"`
}

// CaptureCompanyCode records a pre-signup visit through an employer's company
// code against an anonymous visitor token, so the attribution survives the app
// install gap and binds when the donor proves account ownership in that browser.
// Last touch wins. A malformed code keeps the visitor's existing unexpired touch
// if any, else records nothing. The code is normalised to upper case.
func (s DonorService) CaptureCompanyCode(ctx context.Context, code, visitor string, now time.Time) (CompanyCapture, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	return transact(ctx, s.DB, pgx.TxOptions{}, func(tx pgx.Tx) (CompanyCapture, error) {
		q := queries.New(tx)
		if !companyCode.MatchString(code) {
			// Unknown/malformed: keep an existing unexpired touch, else nothing.
			if visitor != "" {
				if touch, err := q.CompanyTouchByToken(ctx, HashToken(visitor)); err == nil && touch.ExpiresAt.Time.After(now) {
					return CompanyCapture{Token: visitor}, nil
				} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return CompanyCapture{}, err
				}
			}
			return CompanyCapture{}, nil
		}
		// Reuse the visitor's token while its touch is unexpired, else mint one.
		token := visitor
		if token != "" {
			if touch, err := q.CompanyTouchByToken(ctx, HashToken(visitor)); err != nil {
				if !errors.Is(err, pgx.ErrNoRows) {
					return CompanyCapture{}, err
				}
				token = ""
			} else if !touch.ExpiresAt.Time.After(now) {
				token = ""
			}
		}
		if token == "" {
			fresh, err := randomToken(32)
			if err != nil {
				return CompanyCapture{}, err
			}
			token = fresh
		}
		id, err := NewID()
		if err != nil {
			return CompanyCapture{}, err
		}
		if err := q.UpsertCompanyTouch(ctx, queries.UpsertCompanyTouchParams{
			ID: id, TokenHash: HashToken(token), CompanyCode: code,
			TouchedAt: timestamp(now), ExpiresAt: timestamp(now.Add(referralWindow)),
		}); err != nil {
			return CompanyCapture{}, err
		}
		return CompanyCapture{Token: token}, nil
	})
}

// BindCompanyTouch binds the company code the app captured on visitor before
// the donor behind token signed in, as signing in binds a browser's touch. It
// reports whether an unexpired touch was found.
func (s DonorService) BindCompanyTouch(ctx context.Context, token, visitor string) (bool, error) {
	return donorWrite(ctx, s.DB, donorOwner(token), func(tx pgx.Tx, d queries.Donor, now time.Time) (bool, error) {
		code, err := bindCompanyCode(ctx, queries.New(tx), d.ID, visitor, now)
		return code != "", err
	})
}

// bindCompanyCode binds the company code captured for visitor to donor when
// the donor proves account ownership (D-3, D-4), and links them to the
// partner it resolves to, so their giving rolls up to the employer (EM-2). It
// does nothing without an unexpired touch, and never replaces a donor's code
// or employer. It returns the code it found, or empty.
func bindCompanyCode(ctx context.Context, q *queries.Queries, donor pgtype.UUID, visitor string, now time.Time) (string, error) {
	if visitor == "" {
		return "", nil
	}
	touch, err := q.CompanyTouchByToken(ctx, HashToken(visitor))
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !touch.ExpiresAt.Time.After(now) {
		return "", nil
	}
	if err := q.BindDonorCompanyCode(ctx, queries.BindDonorCompanyCodeParams{ID: donor, CompanyCode: text(touch.CompanyCode)}); err != nil {
		return "", err
	}
	partnerID, err := q.PartnerByCompanyCode(ctx, text(touch.CompanyCode))
	if errors.Is(err, pgx.ErrNoRows) {
		return touch.CompanyCode, nil
	}
	if err != nil {
		return "", err
	}
	return touch.CompanyCode, q.UpsertEmployeeLink(ctx, queries.UpsertEmployeeLinkParams{
		DonorID: donor, PartnerID: partnerID, EnrolledAt: timestamp(now),
	})
}

func (s DonorService) Gifts(ctx context.Context, token string, offset int64) (GiftsPage, error) {
	if !pageOffsetValid(offset) {
		return GiftsPage{}, payments.ErrFacts
	}
	return donorRead(ctx, s.DB, token, func(q *queries.Queries, d queries.Donor, _ time.Time) (GiftsPage, error) {
		gifts, err := q.DonorGiftsPage(ctx, queries.DonorGiftsPageParams{DonorID: d.ID, PageOffset: offset})
		if err != nil {
			return GiftsPage{}, err
		}
		out := GiftsPage{Email: d.Email, Mode: processorMode}
		out.Gifts, out.NextOffset = page(gifts, offset)
		return out, nil
	})
}

// conversionPage bounds a conversions response, matching ReferralConversions.
const conversionPage = 20

// Conversions are the donor's latest referral conversions, newest first. More
// says older ones after since were left out.
type Conversions struct {
	Conversions []queries.ReferralConversionsRow `json:"conversions"`
	More        bool                             `json:"more"`
}

// Conversions lists the referrals that converted after since, if set: when
// each referred donor's first gift was paid, and its channel. Nothing about
// a referred donor is shown, as on the dashboard.
func (s DonorService) Conversions(ctx context.Context, token string, since *time.Time) (Conversions, error) {
	after := pgtype.Timestamptz{}
	if since != nil {
		after = timestamp(*since)
	}
	return donorRead(ctx, s.DB, token, func(q *queries.Queries, d queries.Donor, _ time.Time) (Conversions, error) {
		rows, err := q.ReferralConversions(ctx, queries.ReferralConversionsParams{ReferrerID: d.ID, Since: after})
		if err != nil {
			return Conversions{}, err
		}
		if len(rows) > conversionPage {
			return Conversions{Conversions: rows[:conversionPage], More: true}, nil
		}
		return Conversions{Conversions: rows}, nil
	})
}

func (s DonorService) Receipt(ctx context.Context, token string, gift pgtype.UUID) (Receipt, error) {
	return donorRead(ctx, s.DB, token, func(q *queries.Queries, d queries.Donor, now time.Time) (Receipt, error) {
		return readReceipt(ctx, q, d.ID, gift, now)
	})
}

func (s DonorService) AnnualSummary(ctx context.Context, token string, year int) (AnnualSummary, error) {
	return donorRead(ctx, s.DB, token, func(q *queries.Queries, d queries.Donor, now time.Time) (AnnualSummary, error) {
		return readAnnualSummary(ctx, q, d.ID, year, now)
	})
}
