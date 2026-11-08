package store

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"dad/donor-api/internal/badges"
	"dad/donor-api/internal/geo"
	"dad/donor-api/internal/identity"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EventService is the Growth staff surface for events (PRD §8). An event only
// becomes donor-visible once published, and stays editable until it is cancelled.
type EventService struct{ Staff *StaffService }

const eventPermission = "event.manage"

// eventTypes are the allowed event kinds (E-3).
var eventTypes = map[string]bool{"VOLUNTEER": true, "COMMUNITY": true, "INVITE_ONLY": true}

// EventInput is a staff create/update of an event's editable fields. Times are
// RFC3339; StartsAt must precede EndsAt (the DB enforces it too). Capacity 0
// means uncapped.
type EventInput struct {
	Slug        string `json:"slug"`
	Title       string `json:"title"`
	Type        string `json:"type"`
	Description string `json:"description"`
	Venue       string `json:"venue"`
	Address     string `json:"address"`
	City        string `json:"city"`
	Zip         string `json:"zip"`
	StartsAt    string `json:"starts_at"`
	EndsAt      string `json:"ends_at"`
	Capacity    int32  `json:"capacity"`
	// QualifyingPerk gates an INVITE_ONLY event (E-5): the badge perk that
	// unlocks it (e.g. "FOUNDING_EVENTS"), or empty for guest-list-only.
	QualifyingPerk string `json:"qualifying_perk"`
	// BookingOpensAt is when reservations open to everyone (RFC3339), or empty
	// for when the event is published. Team Captains may book 48 hours earlier.
	BookingOpensAt string `json:"booking_opens_at"`
	// TimeZone is the IANA zone of the venue, such as "America/New_York": the
	// zone clients show starts_at and ends_at in. Empty takes the zone of Zip,
	// else defaultEventTimeZone.
	TimeZone string `json:"time_zone"`
}

// defaultEventTimeZone is the zone of an event staff gave neither a zone nor a
// resolvable ZIP; it is also the column default existing events took.
const defaultEventTimeZone = "America/New_York"

// ianaZone matches the shape of an IANA zone name; it is also the database
// check on the column.
var ianaZone = regexp.MustCompile(`^[A-Za-z0-9+_/-]{1,64}$`)

// eventTimeZone returns the zone to store for an event: the one staff entered
// if it is a real IANA name, else the zone of its ZIP, else the default.
func eventTimeZone(zone, zip string) (string, bool) {
	if zone == "" {
		if name, err := geo.Timezone(zip); err == nil {
			return name, true
		}
		return defaultEventTimeZone, true
	}
	if !ianaZone.MatchString(zone) || zone == "Local" {
		return "", false
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return "", false
	}
	return zone, true
}

// validate checks an event input, returning ErrStaffInput on any problem. The
// slug and plain-text rules mirror InitiativeInput.validate so the two staff
// surfaces agree on what valid authored content looks like.
func (in EventInput) validate(now time.Time) (queries.InsertEventParams, error) {
	var params queries.InsertEventParams
	if len(in.Slug) > 100 || !initiativeSlug.MatchString(in.Slug) {
		return params, ErrStaffInput
	}
	if identity.TrimSpace(in.Title) == "" || utf8.RuneCountInString(in.Title) > 150 || !plainText(in.Title, false) {
		return params, ErrStaffInput
	}
	if !eventTypes[in.Type] {
		return params, ErrStaffInput
	}
	// Optional long/free-text fields: bounded and plain, but may be empty.
	for _, f := range []struct {
		value   string
		maximum int
		multi   bool
	}{{in.Description, 4000, true}, {in.Venue, 200, false}, {in.Address, 300, false}, {in.City, 80, false}} {
		if utf8.RuneCountInString(f.value) > f.maximum || !plainText(f.value, f.multi) {
			return params, ErrStaffInput
		}
	}
	if in.Zip != "" {
		normalized, err := geo.NormalizeZip(in.Zip)
		if err != nil {
			return params, ErrStaffInput
		}
		in.Zip = normalized
	}
	if in.Capacity < 0 {
		return params, ErrStaffInput
	}
	zone, ok := eventTimeZone(in.TimeZone, in.Zip)
	if !ok {
		return params, ErrStaffInput
	}
	starts, err := time.Parse(time.RFC3339, in.StartsAt)
	if err != nil {
		return params, ErrStaffInput
	}
	ends, err := time.Parse(time.RFC3339, in.EndsAt)
	if err != nil {
		return params, ErrStaffInput
	}
	if !ends.After(starts) {
		return params, ErrStaffInput
	}
	var opens pgtype.Timestamptz
	if in.BookingOpensAt != "" {
		at, err := time.Parse(time.RFC3339, in.BookingOpensAt)
		if err != nil || !at.Before(starts) {
			return params, ErrStaffInput
		}
		opens = timestamp(at.UTC())
	}
	// qualifying_perk, when set, must be a known badge perk and only makes
	// sense on an INVITE_ONLY event.
	if in.QualifyingPerk != "" {
		if !knownPerks[in.QualifyingPerk] || in.Type != "INVITE_ONLY" {
			return params, ErrStaffInput
		}
	}
	params = queries.InsertEventParams{
		Slug: in.Slug, Title: in.Title, Type: in.Type, Description: in.Description,
		Venue: in.Venue, Address: in.Address, City: in.City, Zip: in.Zip,
		StartsAt: timestamp(starts.UTC()), EndsAt: timestamp(ends.UTC()),
		Capacity: in.Capacity, QualifyingPerk: in.QualifyingPerk, CreatedAt: timestamp(now), BookingOpensAt: opens,
		TimeZone: zone,
	}
	return params, nil
}

// knownPerks is the set of badge perks an invite-only event may require (E-5).
var knownPerks = map[string]bool{
	string(badges.PerkFoundingEvents):  true,
	string(badges.PerkMilestoneEvents): true,
	string(badges.PerkTeamCaptain):     true,
}

// Create inserts a new DRAFT event and returns its id.
func (s EventService) Create(ctx context.Context, token string, in EventInput) (pgtype.UUID, error) {
	return staffWrite(ctx, s.Staff, token, eventPermission,
		func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (pgtype.UUID, error) {
			params, err := in.validate(now)
			if err != nil {
				return pgtype.UUID{}, err
			}
			id, err := NewID()
			if err != nil {
				return pgtype.UUID{}, err
			}
			params.ID = id
			q := queries.New(tx)
			if err = q.InsertEvent(ctx, params); err != nil {
				return pgtype.UUID{}, err
			}
			return id, auditStaff(ctx, q, actor.ID, "event.created", id.String(), map[string]string{"slug": in.Slug}, now)
		})
}

// Update edits an existing event's fields. Publishing is separate. Raising
// the capacity of an upcoming event seats waitlisted donors at once (E-4);
// lowering it never removes a seat already held.
func (s EventService) Update(ctx context.Context, token string, id pgtype.UUID, in EventInput) error {
	_, err := staffWrite(ctx, s.Staff, token, eventPermission,
		func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (struct{}, error) {
			params, err := in.validate(now)
			if err != nil {
				return struct{}{}, err
			}
			q := queries.New(tx)
			event, err := q.LockEvent(ctx, id)
			if err != nil {
				return struct{}{}, err
			}
			if event.CancelledAt.Valid {
				return struct{}{}, ErrEventCancelled
			}
			if err = q.UpdateEvent(ctx, queries.UpdateEventParams{
				ID: id, Title: params.Title, Type: params.Type, Description: params.Description,
				Venue: params.Venue, Address: params.Address, City: params.City, Zip: params.Zip,
				StartsAt: params.StartsAt, EndsAt: params.EndsAt, Capacity: params.Capacity,
				QualifyingPerk: params.QualifyingPerk, UpdatedAt: timestamp(now), BookingOpensAt: params.BookingOpensAt,
				TimeZone: params.TimeZone,
			}); err != nil {
				return struct{}{}, err
			}
			event.Capacity, event.StartsAt = params.Capacity, params.StartsAt
			if now.Before(event.StartsAt.Time) {
				if err = fillSeats(ctx, q, event, now); err != nil {
					return struct{}{}, err
				}
			}
			return struct{}{}, auditStaff(ctx, q, actor.ID, "event.updated", id.String(), nil, now)
		})
	return err
}

// Publish makes a DRAFT event donor-visible. Publishing a published event is a
// no-op, so its published time and alert fan-out are not repeated.
func (s EventService) Publish(ctx context.Context, token string, id pgtype.UUID) error {
	_, err := staffWrite(ctx, s.Staff, token, eventPermission,
		func(tx pgx.Tx, actor queries.DonorStaff, _ time.Time) (struct{}, error) {
			q := queries.New(tx)
			event, err := q.LockEvent(ctx, id)
			if err != nil {
				return struct{}{}, err
			}
			if event.CancelledAt.Valid {
				return struct{}{}, ErrEventCancelled
			}
			if event.PublishState == "PUBLISHED" {
				return struct{}{}, nil
			}
			now := time.Now().UTC()
			if err := q.PublishEvent(ctx, queries.PublishEventParams{ID: id, PublishedAt: timestamp(now)}); err != nil {
				return struct{}{}, err
			}
			queued, err := q.QueueEventAlertJob(ctx, queries.QueueEventAlertJobParams{EventID: id, AsOf: timestamp(now)})
			if err != nil {
				return struct{}{}, err
			}
			return struct{}{}, auditStaff(ctx, q, actor.ID, "event.published", id.String(), map[string]int64{"alert_jobs": queued}, now)
		})
	return err
}

// AddGuest puts a donor on an INVITE_ONLY event's admin guest list (E-5), so
// they can see and reserve it regardless of badges. Idempotent.
func (s EventService) AddGuest(ctx context.Context, token string, eventID, donorID pgtype.UUID) error {
	_, err := staffWrite(ctx, s.Staff, token, eventPermission,
		func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (struct{}, error) {
			q := queries.New(tx)
			if _, err := q.EventByID(ctx, eventID); err != nil {
				return struct{}{}, err
			}
			id, err := NewID()
			if err != nil {
				return struct{}{}, err
			}
			if err = q.AddEventGuest(ctx, queries.AddEventGuestParams{
				ID: id, EventID: eventID, DonorID: donorID, CreatedAt: timestamp(now),
			}); err != nil {
				return struct{}{}, err
			}
			return struct{}{}, auditStaff(
				ctx,
				q,
				actor.ID,
				"event.guest_added",
				eventID.String(),
				map[string]string{"donor_id": donorID.String()},
				now,
			)
		})
	return err
}

// RemoveGuest removes a donor from an event's guest list. Their existing
// reservation, if any, is left intact.
func (s EventService) RemoveGuest(ctx context.Context, token string, eventID, donorID pgtype.UUID) error {
	_, err := staffWrite(ctx, s.Staff, token, eventPermission,
		func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (struct{}, error) {
			q := queries.New(tx)
			if err := q.RemoveEventGuest(ctx, queries.RemoveEventGuestParams{EventID: eventID, DonorID: donorID}); err != nil {
				return struct{}{}, err
			}
			return struct{}{}, auditStaff(
				ctx,
				q,
				actor.ID,
				"event.guest_removed",
				eventID.String(),
				map[string]string{"donor_id": donorID.String()},
				now,
			)
		})
	return err
}

// EventReader is the donor-facing read of published events. It only
// ever returns PUBLISHED events; a draft or unknown id reads as "not found", so
// the draft side is never observable to donors.
type EventReader struct {
	DB *pgxpool.Pool
	// SupportEmail, when set, is the address the report-issue mailto targets
	// (E-10); empty leaves report_issue_url nil.
	SupportEmail string
}

// reportIssueURL builds the E-10 mailto for an event, or nil when no support
// address is configured.
func (r EventReader) reportIssueURL(e queries.DonorEvent) *string {
	if r.SupportEmail == "" {
		return nil
	}
	u := "mailto:" + r.SupportEmail +
		"?subject=" + url.QueryEscape("Issue with event: "+e.Title) +
		"&body=" + url.QueryEscape("Event ID: "+e.ID.String()+"\n\n")
	return &u
}

// ErrEventNotFound is returned when an event does not exist or is not published;
// both map to 404 so the draft state is not observable.
var ErrEventNotFound = errors.New("event not found")

// EventView is one event as a donor sees it (E-3). SpotsLeft is nil when the
// event is uncapped. ReservationStatus is the signed-in donor's own status for
// this event (RESERVED/WAITLISTED), or nil on an anonymous read or when they
// hold no active reservation — it drives the Reserve / Join waitlist / Cancel
// button (E-4).
type EventView struct {
	ID          string    `json:"id"`
	Slug        string    `json:"slug"`
	Title       string    `json:"title"`
	Type        string    `json:"type"`
	Description string    `json:"description"`
	Venue       string    `json:"venue"`
	Address     string    `json:"address"`
	City        string    `json:"city"`
	StartsAt    time.Time `json:"starts_at"`
	EndsAt      time.Time `json:"ends_at"`
	// TimeZone is the venue's IANA zone; show starts_at and ends_at in it.
	TimeZone          string  `json:"time_zone"`
	Capacity          int32   `json:"capacity"`
	SpotsLeft         *int32  `json:"spots_left"`
	ReservationStatus *string `json:"reservation_status"`
	// CheckedInAt is when the signed-in donor checked in, nil until they do and
	// on an anonymous read.
	CheckedInAt *time.Time `json:"checked_in_at"`
	// InvitationNote is "Invitation earned at ..." for an INVITE_ONLY event the
	// donor can see via a badge perk (E-5); nil otherwise.
	InvitationNote *string `json:"invitation_note"`
	// ReportIssueURL is a mailto link to report an issue about this event to DAD
	// (E-10); nil when no support email is configured.
	ReportIssueURL *string `json:"report_issue_url"`
	Cancelled      bool    `json:"cancelled"`
	// BookingOpensAt is when reservations open to everyone; nil once the event
	// is published without a later opening.
	BookingOpensAt *time.Time `json:"booking_opens_at"`
	// EarlyBooking is true while the signed-in donor, a Team Captain, may
	// reserve before booking opens to everyone (B-3).
	EarlyBooking bool `json:"early_booking"`
}

func eventView(e queries.DonorEvent) EventView {
	return EventView{
		ID: e.ID.String(), Slug: e.Slug, Title: e.Title, Type: e.Type, Description: e.Description,
		Venue: e.Venue, Address: e.Address, City: e.City,
		StartsAt: e.StartsAt.Time, EndsAt: e.EndsAt.Time, TimeZone: e.TimeZone, Capacity: e.Capacity, Cancelled: e.CancelledAt.Valid,
		BookingOpensAt: timePointer(e.BookingOpensAt),
	}
}

// earlyBookingLead is how long before booking opens a Team Captain may
// reserve (PRD B-3).
const earlyBookingLead = 48 * time.Hour

// ErrBookingNotOpen is returned when a donor reserves before booking opens to
// them.
var ErrBookingNotOpen = errors.New("booking for this event has not opened yet")

// bookingOpen reports whether donor may reserve e at now, and whether that is
// only because they hold the Team Captain perk.
func bookingOpen(ctx context.Context, q *queries.Queries, donor pgtype.UUID, e queries.DonorEvent, now time.Time) (bool, bool, error) {
	if !e.BookingOpensAt.Valid || !now.Before(e.BookingOpensAt.Time) {
		return true, false, nil
	}
	if now.Before(e.BookingOpensAt.Time.Add(-earlyBookingLead)) {
		return false, false, nil
	}
	codes, err := q.DonorEarnedBadgeCodes(ctx, donor)
	if err != nil {
		return false, false, err
	}
	earned := make(map[string]bool, len(codes))
	for _, c := range codes {
		earned[c] = true
	}
	captain := badges.HoldsPerk(earned, badges.PerkTeamCaptain)
	return captain, captain, nil
}

// spotsLeft is capacity minus RESERVED count, floored at 0; nil when uncapped.
func spotsLeft(capacity int32, reserved int64) *int32 {
	if capacity <= 0 {
		return nil
	}
	left := capacity - int32(reserved)
	if left < 0 {
		left = 0
	}
	return &left
}

// NearDonor lists published, upcoming events in and near the signed-in donor's
// city (E-2): the same city label or the same three-digit ZIP region, soonest
// first. INVITE_ONLY events are never in this list. A donor with neither a city
// nor a ZIP gets an empty list rather than everything.
func (r EventReader) NearDonor(ctx context.Context, token string) ([]EventView, error) {
	return donorRead(ctx, r.DB, token, func(q *queries.Queries, d queries.Donor, now time.Time) ([]EventView, error) {
		zip3 := ""
		if len(d.Zip.String) >= 3 {
			zip3 = d.Zip.String[:3]
		}
		rows, err := q.UpcomingEventsNearDonor(ctx, queries.UpcomingEventsNearDonorParams{
			Now: timestamp(now), City: d.City.String, Zip3: zip3,
		})
		if err != nil {
			return nil, err
		}
		return r.donorViews(ctx, q, d.ID, rows)
	})
}

// donorViews builds a donor's views of a list of events with two queries
// however long the list: the seats taken on each and the donor's own status.
func (r EventReader) donorViews(
	ctx context.Context,
	q *queries.Queries,
	donor pgtype.UUID,
	rows []queries.DonorEvent,
) ([]EventView, error) {
	out := make([]EventView, 0, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	ids := make([]pgtype.UUID, 0, len(rows))
	for _, e := range rows {
		ids = append(ids, e.ID)
	}
	reservedCounts, err := q.ReservedCountsForEvents(ctx, ids)
	if err != nil {
		return nil, err
	}
	reserved := make(map[pgtype.UUID]int64, len(reservedCounts))
	for _, rc := range reservedCounts {
		reserved[rc.EventID] = rc.Reserved
	}
	mine, err := q.DonorReservationsForEvents(ctx, queries.DonorReservationsForEventsParams{DonorID: donor, EventIds: ids})
	if err != nil {
		return nil, err
	}
	status := make(map[pgtype.UUID]queries.DonorReservationsForEventsRow, len(mine))
	for _, m := range mine {
		status[m.EventID] = m
	}
	for _, e := range rows {
		v := eventView(e)
		v.ReportIssueURL = r.reportIssueURL(e)
		v.SpotsLeft = spotsLeft(e.Capacity, reserved[e.ID])
		if m, ok := status[e.ID]; ok {
			s := m.Status
			v.ReservationStatus = &s
			v.CheckedInAt = timePointer(m.CheckedInAt)
		}
		out = append(out, v)
	}
	return out, nil
}

// Detail returns one published event by id for the detail page (E-3). A draft
// or unknown id is ErrEventNotFound. INVITE_ONLY events are never exposed on
// this anonymous path — they are only visible through DetailForDonor to a donor
// who passes the E-5 gate — so an INVITE_ONLY id reads as not found here.
func (r EventReader) Detail(ctx context.Context, id pgtype.UUID) (EventView, error) {
	q := queries.New(r.DB)
	e, err := q.EventByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return EventView{}, ErrEventNotFound
	}
	if err != nil {
		return EventView{}, err
	}
	if e.PublishState != "PUBLISHED" || e.Type == "INVITE_ONLY" {
		return EventView{}, ErrEventNotFound
	}
	return r.withSpotsLeft(ctx, q, e)
}

// withSpotsLeft builds an EventView and fills SpotsLeft for a capped event.
func (r EventReader) withSpotsLeft(ctx context.Context, q *queries.Queries, e queries.DonorEvent) (EventView, error) {
	v := eventView(e)
	v.ReportIssueURL = r.reportIssueURL(e)
	if e.Capacity > 0 {
		reserved, err := q.ReservedCount(ctx, e.ID)
		if err != nil {
			return EventView{}, err
		}
		v.SpotsLeft = spotsLeft(e.Capacity, reserved)
	}
	return v, nil
}

// Calendar returns an RFC 5545 iCalendar (.ics) document for a published,
// non-invite event (E-6). A draft, unknown, or INVITE_ONLY id is
// ErrEventNotFound, matching the anonymous Detail gate.
func (r EventReader) Calendar(ctx context.Context, id pgtype.UUID) (string, error) {
	q := queries.New(r.DB)
	e, err := q.EventByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrEventNotFound
	}
	if err != nil {
		return "", err
	}
	if e.PublishState != "PUBLISHED" || e.Type == "INVITE_ONLY" || e.CancelledAt.Valid {
		return "", ErrEventNotFound
	}
	return renderICS(e), nil
}

func (r EventReader) CalendarForDonor(ctx context.Context, token string, id pgtype.UUID) (string, error) {
	return donorRead(ctx, r.DB, token, func(q *queries.Queries, d queries.Donor, _ time.Time) (string, error) {
		e, err := q.EventByID(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrEventNotFound
		}
		if err != nil {
			return "", err
		}
		if e.PublishState != "PUBLISHED" || e.CancelledAt.Valid {
			return "", ErrEventNotFound
		}
		if e.Type == "INVITE_ONLY" {
			allowed, _, err := canSeeInviteOnly(ctx, q, d.ID, e)
			if err != nil {
				return "", err
			}
			if !allowed {
				return "", ErrEventNotFound
			}
		}
		return renderICS(e), nil
	})
}

// renderICS builds a single-VEVENT VCALENDAR. Times are UTC (the trailing Z
// form), which every calendar client reads correctly. Text fields are escaped
// and long lines folded per RFC 5545.
func renderICS(e queries.DonorEvent) string {
	stamp := func(t time.Time) string { return t.UTC().Format("20060102T150405Z") }
	location := e.Venue
	if e.Address != "" {
		if location != "" {
			location += ", "
		}
		location += e.Address
	}
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\n")
	b.WriteString("VERSION:2.0\r\n")
	b.WriteString("PRODID:-//DAD//Events//EN\r\n")
	b.WriteString("CALSCALE:GREGORIAN\r\n")
	b.WriteString("METHOD:PUBLISH\r\n")
	b.WriteString("BEGIN:VEVENT\r\n")
	b.WriteString("UID:" + e.ID.String() + "@dad\r\n")
	b.WriteString("DTSTAMP:" + stamp(time.Now()) + "\r\n")
	b.WriteString("DTSTART:" + stamp(e.StartsAt.Time) + "\r\n")
	b.WriteString("DTEND:" + stamp(e.EndsAt.Time) + "\r\n")
	b.WriteString(foldICS("SUMMARY:" + escapeICS(e.Title)))
	if e.Description != "" {
		b.WriteString(foldICS("DESCRIPTION:" + escapeICS(e.Description)))
	}
	if location != "" {
		b.WriteString(foldICS("LOCATION:" + escapeICS(location)))
	}
	b.WriteString("END:VEVENT\r\n")
	b.WriteString("END:VCALENDAR\r\n")
	return b.String()
}

// escapeICS escapes a text value for an iCalendar property (RFC 5545 §3.3.11).
func escapeICS(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, ";", "\\;")
	s = strings.ReplaceAll(s, ",", "\\,")
	s = strings.ReplaceAll(s, "\r\n", "\\n")
	s = strings.ReplaceAll(s, "\n", "\\n")
	return s
}

// foldICS ends a content line, folding it so no physical line exceeds 75
// octets (RFC 5545 §3.1). A fold never splits a UTF-8 sequence.
func foldICS(line string) string {
	var b strings.Builder
	limit := 75
	for len(line) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		b.WriteString(line[:cut] + "\r\n ")
		line = line[cut:]
		limit = 74
	}
	b.WriteString(line + "\r\n")
	return b.String()
}

// invitationNote is the "Invitation earned at ..." label for an INVITE_ONLY
// event unlocked by a badge perk (E-5).
func invitationNote(perk string) *string {
	title := "an invitation"
	switch perk {
	case string(badges.PerkFoundingEvents):
		title = "the Founding Donor badge"
	case string(badges.PerkMilestoneEvents):
		title = "a giving-milestone badge"
	case string(badges.PerkTeamCaptain):
		title = "the Team Captain badge"
	}
	note := "Invitation earned at " + title
	return &note
}

// canSeeInviteOnly reports whether a donor passes the E-5 gate for an
// INVITE_ONLY event: they hold a badge carrying its qualifying perk, OR they
// are on the admin guest list. Returns the reason (perk/guest) for the note.
func canSeeInviteOnly(ctx context.Context, q *queries.Queries, donor pgtype.UUID, e queries.DonorEvent) (bool, bool, error) {
	// Perk side.
	byPerk := false
	if e.QualifyingPerk != "" {
		codes, err := q.DonorEarnedBadgeCodes(ctx, donor)
		if err != nil {
			return false, false, err
		}
		earned := make(map[string]bool, len(codes))
		for _, c := range codes {
			earned[c] = true
		}
		byPerk = badges.HoldsPerk(earned, badges.Perk(e.QualifyingPerk))
	}
	if byPerk {
		return true, true, nil
	}
	// Guest-list side.
	guest, err := q.IsEventGuest(ctx, queries.IsEventGuestParams{EventID: e.ID, DonorID: donor})
	if err != nil {
		return false, false, err
	}
	return guest, false, nil
}

// InviteOnlyEvents lists the published, upcoming INVITE_ONLY events the
// signed-in donor can see (E-5): those unlocked by a badge perk they hold or on
// which they are an admin guest, soonest first. Perk-unlocked events carry the
// "Invitation earned at ..." note.
func (r EventReader) InviteOnlyEvents(ctx context.Context, token string) ([]EventView, error) {
	return donorRead(ctx, r.DB, token, func(q *queries.Queries, d queries.Donor, now time.Time) ([]EventView, error) {
		rows, err := q.UpcomingInviteOnlyEvents(ctx, timestamp(now))
		if err != nil || len(rows) == 0 {
			return []EventView{}, err
		}
		codes, err := q.DonorEarnedBadgeCodes(ctx, d.ID)
		if err != nil {
			return nil, err
		}
		earned := make(map[string]bool, len(codes))
		for _, c := range codes {
			earned[c] = true
		}
		ids := make([]pgtype.UUID, 0, len(rows))
		for _, e := range rows {
			ids = append(ids, e.ID)
		}
		guestOf, err := q.DonorGuestEvents(ctx, queries.DonorGuestEventsParams{DonorID: d.ID, EventIds: ids})
		if err != nil {
			return nil, err
		}
		guest := make(map[pgtype.UUID]bool, len(guestOf))
		for _, id := range guestOf {
			guest[id] = true
		}
		var visible []queries.DonorEvent
		var byPerk []bool
		for _, e := range rows {
			perk := badges.HoldsPerk(earned, badges.Perk(e.QualifyingPerk))
			if perk || guest[e.ID] {
				visible = append(visible, e)
				byPerk = append(byPerk, perk)
			}
		}
		views, err := r.donorViews(ctx, q, d.ID, visible)
		if err != nil {
			return nil, err
		}
		for i, e := range visible {
			if byPerk[i] {
				views[i].InvitationNote = invitationNote(e.QualifyingPerk)
			}
		}
		return views, nil
	})
}

// DetailForDonor returns one published event for the signed-in donor, applying
// the E-5 gate for INVITE_ONLY events: a donor who cannot see it gets
// ErrEventNotFound, so the event's existence is not leaked.
func (r EventReader) DetailForDonor(ctx context.Context, token string, id pgtype.UUID) (EventView, error) {
	return donorRead(ctx, r.DB, token, func(q *queries.Queries, d queries.Donor, _ time.Time) (EventView, error) {
		e, err := q.EventByID(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return EventView{}, ErrEventNotFound
		}
		if err != nil {
			return EventView{}, err
		}
		if e.PublishState != "PUBLISHED" {
			return EventView{}, ErrEventNotFound
		}
		byPerk := false
		if e.Type == "INVITE_ONLY" {
			ok, perkGrant, err := canSeeInviteOnly(ctx, q, d.ID, e)
			if err != nil {
				return EventView{}, err
			}
			if !ok {
				return EventView{}, ErrEventNotFound
			}
			byPerk = perkGrant
		}
		v, err := r.withSpotsLeft(ctx, q, e)
		if err != nil {
			return EventView{}, err
		}
		if byPerk {
			v.InvitationNote = invitationNote(e.QualifyingPerk)
		}
		if _, v.EarlyBooking, err = bookingOpen(ctx, q, d.ID, e, time.Now().UTC()); err != nil {
			return EventView{}, err
		}
		if res, err := q.ReservationFor(ctx, queries.ReservationForParams{EventID: id, DonorID: d.ID}); err == nil &&
			res.Status != reservationCancelled {
			s := res.Status
			v.ReservationStatus = &s
			v.CheckedInAt = timePointer(res.CheckedInAt)
		} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return EventView{}, err
		}
		return v, nil
	})
}

// Reservation statuses (also the DB check constraint).
const (
	reservationReserved   = "RESERVED"
	reservationWaitlisted = "WAITLISTED"
	reservationCancelled  = "CANCELLED"
)

// eventPromotedCategory is the push category, and the email kind, of a waitlist
// promotion (E-4, §6.11).
const eventPromotedCategory = "EVENT_PROMOTED"

// ErrReservationClosed is returned when a donor tries to reserve or cancel
// after the event has started (E-9: cancel is allowed only up to start).
var ErrReservationClosed = errors.New("event has already started")

// ReservationResult is the outcome of a reserve/cancel, so the caller can tell
// the donor whether they got a seat or a waitlist place.
type ReservationResult struct {
	Status string `json:"status"`
}

// ReservationService books, waitlists, and cancels event reservations under
// the event row lock so capacity is enforced race-free, and promotes the
// waitlist head synchronously on cancel (PRD §6.10 E-4, E-9). CheckInSecret is
// the HMAC key the QR check-in token is signed with (E-8); when empty, check-in
// is disabled.
type ReservationService struct {
	DB            *pgxpool.Pool
	CheckInSecret string
}

// Reserve books the signed-in donor a spot at an event, or waitlists them when
// it is full (E-4). Reserving is refused once the event has started. A donor
// who already holds an active reservation keeps it (idempotent).
func (s ReservationService) Reserve(ctx context.Context, token string, eventID pgtype.UUID) (ReservationResult, error) {
	return donorWrite(ctx, s.DB, donorOwner(token), func(tx pgx.Tx, d queries.Donor, _ time.Time) (ReservationResult, error) {
		q := queries.New(tx)
		event, err := q.LockEvent(ctx, eventID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ReservationResult{}, ErrEventNotFound
		}
		if err != nil {
			return ReservationResult{}, err
		}
		now := time.Now().UTC()
		if event.PublishState != "PUBLISHED" {
			return ReservationResult{}, ErrEventNotFound
		}
		if event.CancelledAt.Valid {
			return ReservationResult{}, ErrEventCancelled
		}
		if !now.Before(event.StartsAt.Time) {
			return ReservationResult{}, ErrReservationClosed
		}
		// An INVITE_ONLY event can only be reserved by a donor who passes the
		// E-5 gate; otherwise it reads as not found, like the detail path.
		if event.Type == "INVITE_ONLY" {
			ok, _, err := canSeeInviteOnly(ctx, q, d.ID, event)
			if err != nil {
				return ReservationResult{}, err
			}
			if !ok {
				return ReservationResult{}, ErrEventNotFound
			}
		}
		if open, _, err := bookingOpen(ctx, q, d.ID, event, now); err != nil {
			return ReservationResult{}, err
		} else if !open {
			return ReservationResult{}, ErrBookingNotOpen
		}
		// An existing active reservation is returned unchanged (idempotent).
		existing, err := q.ReservationFor(ctx, queries.ReservationForParams{EventID: eventID, DonorID: d.ID})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return ReservationResult{}, err
		}
		if err == nil && existing.Status != reservationCancelled {
			return ReservationResult{Status: existing.Status}, nil
		}
		// Capacity 0 means uncapped. Otherwise a seat is free when RESERVED
		// count is below capacity.
		status := reservationReserved
		if event.Capacity > 0 {
			reserved, err := q.ReservedCount(ctx, eventID)
			if err != nil {
				return ReservationResult{}, err
			}
			if reserved >= int64(event.Capacity) {
				status = reservationWaitlisted
			}
		}
		id, err := NewID()
		if err != nil {
			return ReservationResult{}, err
		}
		if err = q.UpsertReservation(ctx, queries.UpsertReservationParams{
			ID: id, EventID: eventID, DonorID: d.ID, Status: status,
			ReservedAt: timestamp(now), CreatedAt: timestamp(now),
		}); err != nil {
			return ReservationResult{}, err
		}
		return ReservationResult{Status: status}, nil
	})
}

// Cancel releases the signed-in donor's reservation (E-9, up to event start)
// and, if that freed a seat, promotes the head of the waitlist to RESERVED and
// queues a push to them (E-4) — all in the same transaction under the event
// lock, so the promotion is atomic with the cancellation.
func (s ReservationService) Cancel(ctx context.Context, token string, eventID pgtype.UUID) (ReservationResult, error) {
	return donorWrite(ctx, s.DB, donorOwner(token), func(tx pgx.Tx, d queries.Donor, _ time.Time) (ReservationResult, error) {
		q := queries.New(tx)
		event, err := q.LockEvent(ctx, eventID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ReservationResult{}, ErrEventNotFound
		}
		if err != nil {
			return ReservationResult{}, err
		}
		now := time.Now().UTC()
		if !now.Before(event.StartsAt.Time) {
			return ReservationResult{}, ErrReservationClosed
		}
		existing, err := q.ReservationFor(ctx, queries.ReservationForParams{EventID: eventID, DonorID: d.ID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ReservationResult{}, ErrEventNotFound
		}
		if err != nil {
			return ReservationResult{}, err
		}
		if existing.Status == reservationCancelled {
			return ReservationResult{Status: reservationCancelled}, nil
		}
		wasReserved := existing.Status == reservationReserved
		if err = q.SetReservationStatus(ctx, queries.SetReservationStatusParams{
			EventID: eventID, DonorID: d.ID, Status: reservationCancelled, UpdatedAt: timestamp(now),
		}); err != nil {
			return ReservationResult{}, err
		}
		if wasReserved {
			if err = fillSeats(ctx, q, event, now); err != nil {
				return ReservationResult{}, err
			}
		}
		return ReservationResult{Status: reservationCancelled}, nil
	})
}

// fillSeats promotes waitlisted donors, earliest first, into every free seat
// of an event whose row the caller has locked.
func fillSeats(ctx context.Context, q *queries.Queries, event queries.DonorEvent, now time.Time) error {
	for {
		if event.Capacity > 0 {
			reserved, err := q.ReservedCount(ctx, event.ID)
			if err != nil {
				return err
			}
			if reserved >= int64(event.Capacity) {
				return nil
			}
		}
		promoted, err := promoteWaitlistHead(ctx, q, event, now)
		if err != nil || !promoted {
			return err
		}
	}
}

// promoteWaitlistHead promotes the earliest-queued waitlisted donor, if any, to
// RESERVED and queues the promotion push and email (§6.11).
func promoteWaitlistHead(ctx context.Context, q *queries.Queries, event queries.DonorEvent, now time.Time) (bool, error) {
	head, err := q.WaitlistHead(ctx, event.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err = q.PromoteReservation(ctx, queries.PromoteReservationParams{ID: head.ID, UpdatedAt: timestamp(now)}); err != nil {
		return false, err
	}
	pushID, err := NewID()
	if err != nil {
		return false, err
	}
	payload, err := json.Marshal(map[string]string{"event_id": event.ID.String()})
	if err != nil {
		return false, err
	}
	// Keyed by the waitlist place, so a later re-promotion is a new notice.
	promotion := event.ID.String() + ":" + head.DonorID.String() + ":" + head.ReservedAt.Time.UTC().Format(time.RFC3339Nano)
	if err = q.QueueEventPush(ctx, queries.QueueEventPushParams{
		ID: pushID, DonorID: head.DonorID, Category: eventPromotedCategory, DedupKey: "push:event-promoted:" + promotion,
		Context: payload, AvailableAt: timestamp(now), ExpiresAt: event.StartsAt,
	}); err != nil {
		return false, err
	}
	messageID, err := NewID()
	if err != nil {
		return false, err
	}
	return true, q.InsertFinancialMessage(ctx, queries.InsertFinancialMessageParams{
		ID: messageID, DonorID: head.DonorID, Kind: eventPromotedCategory, DedupKey: "message:event-promoted:" + promotion,
		Context: payload, AvailableAt: timestamp(now),
	})
}

// ErrCheckIn is returned when a check-in token is invalid, the event is outside
// its check-in window, or the donor holds no reservation to check in.
var ErrCheckIn = errors.New("check-in is not valid for this event right now")

// ErrCheckInDisabled is returned when no check-in secret is configured.
var ErrCheckInDisabled = errors.New("event check-in is not configured")

// checkInToken is the opaque, event-scoped QR payload (E-8):
// base64url(event_id) "." base64url(HMAC-SHA256(event_id, secret)). It is not
// guessable or forgeable without the secret, and a screenshotted token still
// only works during the event window (enforced on check-in), so sharing it
// buys nothing. It carries no per-use state, so the same QR works for every
// attendee.
func checkInToken(eventID pgtype.UUID, secret string) string {
	raw := eventID.Bytes[:]
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(raw)
	return base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// checkInTokenLength is the exact length of a check-in token: 22 characters for
// the 16-byte event id, ".", and 43 for the 32-byte HMAC-SHA256.
const checkInTokenLength = 22 + 1 + 43

// parseCheckInToken verifies a token and returns the event id it names.
func parseCheckInToken(token, secret string) (pgtype.UUID, bool) {
	var id pgtype.UUID
	if len(token) != checkInTokenLength {
		return id, false
	}
	idPart, sigPart, ok := strings.Cut(token, ".")
	if !ok {
		return id, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(idPart)
	if err != nil || len(raw) != 16 {
		return id, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigPart)
	if err != nil {
		return id, false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(raw)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return id, false
	}
	copy(id.Bytes[:], raw)
	id.Valid = true
	return id, true
}

// CheckInToken returns the signed QR token for an event, for staff to render
// the venue QR code (E-8). Admin-only.
func (s EventService) CheckInToken(ctx context.Context, token string, eventID pgtype.UUID, secret string) (string, error) {
	if secret == "" {
		return "", ErrCheckInDisabled
	}
	return staffWrite(ctx, s.Staff, token, eventPermission,
		func(tx pgx.Tx, _ queries.DonorStaff, _ time.Time) (string, error) {
			if _, err := queries.New(tx).EventByID(ctx, eventID); err != nil {
				return "", err
			}
			return checkInToken(eventID, secret), nil
		})
}

// CheckInResult is the outcome of a successful check-in.
type CheckInResult struct {
	EventID   string `json:"event_id"`
	CheckedIn bool   `json:"checked_in"`
	AlreadyIn bool   `json:"already_checked_in"`
}

// CheckIn records the signed-in donor's attendance from a scanned QR token
// (E-8). It verifies the token, requires the event to be within its window
// (starts_at..ends_at) and the donor to hold a seat, not a waitlist place, then
// stamps checked_in_at. A second scan is a no-op success.
func (s ReservationService) CheckIn(ctx context.Context, sessionToken, qrToken string) (CheckInResult, error) {
	if s.CheckInSecret == "" {
		return CheckInResult{}, ErrCheckInDisabled
	}
	eventID, ok := parseCheckInToken(qrToken, s.CheckInSecret)
	if !ok {
		return CheckInResult{}, ErrCheckIn
	}
	return donorWrite(ctx, s.DB, donorOwner(sessionToken), func(tx pgx.Tx, d queries.Donor, _ time.Time) (CheckInResult, error) {
		q := queries.New(tx)
		event, err := q.LockEvent(ctx, eventID)
		if errors.Is(err, pgx.ErrNoRows) {
			return CheckInResult{}, ErrCheckIn
		}
		if err != nil {
			return CheckInResult{}, err
		}
		now := time.Now().UTC()
		if event.PublishState != "PUBLISHED" || event.CancelledAt.Valid ||
			now.Before(event.StartsAt.Time) || now.After(event.EndsAt.Time) {
			return CheckInResult{}, ErrCheckIn
		}
		existing, err := q.ReservationFor(ctx, queries.ReservationForParams{EventID: eventID, DonorID: d.ID})
		if errors.Is(err, pgx.ErrNoRows) {
			return CheckInResult{}, ErrCheckIn
		}
		if err != nil {
			return CheckInResult{}, err
		}
		if existing.Status != reservationReserved {
			return CheckInResult{}, ErrCheckIn
		}
		if existing.CheckedInAt.Valid {
			return CheckInResult{EventID: eventID.String(), CheckedIn: true, AlreadyIn: true}, nil
		}
		if err = q.CheckInReservation(ctx, queries.CheckInReservationParams{
			EventID: eventID, DonorID: d.ID, CheckedInAt: timestamp(now),
		}); err != nil {
			return CheckInResult{}, err
		}
		return CheckInResult{EventID: eventID.String(), CheckedIn: true}, awardBadges(ctx, q, d, now)
	})
}

// EventReminderService queues the 24-hour and 2-hour reminder pushes before an
// event (PRD §6.10 E-7). It is run periodically by the projections worker; each
// window is deduped per (event, donor, window), so running it often is cheap
// and idempotent — a window that has already fired is a no-op.
type EventReminderService struct{ DB *pgxpool.Pool }

// reminderWindows are the lead times before an event at which a reminder fires.
var reminderWindows = []struct {
	label string
	lead  time.Duration
}{{"24h", 24 * time.Hour}, {"2h", 2 * time.Hour}}

// QueueDueReminders queues reminders for every published event whose start is
// within each window and that a donor holds a reservation for. Returns the
// number of reminders newly queued across both windows.
func (s EventReminderService) QueueDueReminders(ctx context.Context, now time.Time) (int64, error) {
	q := queries.New(s.DB)
	var queued int64
	for _, w := range reminderWindows {
		n, err := q.QueueEventReminders(ctx, queries.QueueEventRemindersParams{
			WindowLabel: w.label,
			AsOf:        timestamp(now),
			Until:       timestamp(now.Add(w.lead)),
		})
		if err != nil {
			return queued, err
		}
		queued += n
	}
	return queued, nil
}
