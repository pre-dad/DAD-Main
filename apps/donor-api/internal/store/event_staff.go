package store

import (
	"context"
	"errors"
	"time"

	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// eventCancelledCategory is the push category, and the email kind, telling a
// donor an event they reserved or waitlisted for was cancelled (§8 Growth).
const eventCancelledCategory = "EVENT_CANCELLED"

// eventNearbyCategory is the push telling a volunteer an event was published
// in or near their city (E-1, §6.11).
const eventNearbyCategory = "EVENT_NEARBY"

// ErrEventCancelled is returned when a donor or staff member acts on a
// cancelled event.
var ErrEventCancelled = errors.New("event has been cancelled")

// StaffEvent is an event as Growth edits it, drafts and cancellations included.
type StaffEvent struct {
	EventInput
	ID           string             `json:"id"`
	PublishState string             `json:"publish_state"`
	CancelledAt  pgtype.Timestamptz `json:"cancelled_at"`
	Reserved     int64              `json:"reserved"`
	Waitlisted   int64              `json:"waitlisted"`
	CheckedIn    int64              `json:"checked_in"`
}

type StaffEventPage struct {
	Events     []StaffEvent `json:"events"`
	NextOffset *int64       `json:"next_offset"`
}

type EventAttendee struct {
	DonorID     string             `json:"donor_id"`
	Email       string             `json:"email"`
	PublicName  string             `json:"public_name"`
	Status      string             `json:"status"`
	ReservedAt  pgtype.Timestamptz `json:"reserved_at"`
	CheckedInAt pgtype.Timestamptz `json:"checked_in_at"`
}

// StaffEventDetail is the §8 RSVPs screen: the event, its seats in booking
// order, then its waitlist in promotion order.
type StaffEventDetail struct {
	StaffEvent
	Attendees  []EventAttendee `json:"attendees"`
	NextOffset *int64          `json:"next_offset"`
}

func staffEvent(e queries.DonorEvent) StaffEvent {
	return StaffEvent{
		EventInput: EventInput{
			Slug: e.Slug, Title: e.Title, Type: e.Type, Description: e.Description, Venue: e.Venue, Address: e.Address,
			City: e.City, Zip: e.Zip, StartsAt: e.StartsAt.Time.UTC().Format(time.RFC3339), EndsAt: e.EndsAt.Time.UTC().Format(time.RFC3339),
			Capacity: e.Capacity, QualifyingPerk: e.QualifyingPerk, BookingOpensAt: rfc3339(e.BookingOpensAt), TimeZone: e.TimeZone,
		},
		ID: e.ID.String(), PublishState: e.PublishState, CancelledAt: e.CancelledAt,
	}
}

func (s EventService) List(ctx context.Context, token string, offset int64) (StaffEventPage, error) {
	if !pageOffsetValid(offset) {
		return StaffEventPage{}, ErrStaffInput
	}
	return staffRead(ctx, s.Staff, token, eventPermission,
		func(tx pgx.Tx, _ queries.DonorStaff, _ time.Time) (StaffEventPage, error) {
			q := queries.New(tx)
			rows, err := q.StaffEventsPage(ctx, offset)
			if err != nil {
				return StaffEventPage{}, err
			}
			rows, next := page(rows, offset)
			ids := make([]pgtype.UUID, 0, len(rows))
			for _, e := range rows {
				ids = append(ids, e.ID)
			}
			counts, err := q.EventReservationCounts(ctx, ids)
			if err != nil {
				return StaffEventPage{}, err
			}
			byEvent := make(map[pgtype.UUID]queries.EventReservationCountsRow, len(counts))
			for _, c := range counts {
				byEvent[c.EventID] = c
			}
			out := StaffEventPage{Events: make([]StaffEvent, 0, len(rows)), NextOffset: next}
			for _, e := range rows {
				v := staffEvent(e)
				c := byEvent[e.ID]
				v.Reserved, v.Waitlisted, v.CheckedIn = c.Reserved, c.Waitlisted, c.CheckedIn
				out.Events = append(out.Events, v)
			}
			return out, nil
		})
}

func (s EventService) Detail(ctx context.Context, token string, id pgtype.UUID, offset int64) (StaffEventDetail, error) {
	if !pageOffsetValid(offset) {
		return StaffEventDetail{}, ErrStaffInput
	}
	return staffRead(ctx, s.Staff, token, eventPermission,
		func(tx pgx.Tx, _ queries.DonorStaff, _ time.Time) (StaffEventDetail, error) {
			q := queries.New(tx)
			e, err := q.EventByID(ctx, id)
			if err != nil {
				return StaffEventDetail{}, err
			}
			roster, err := q.EventRoster(ctx, queries.EventRosterParams{EventID: id, PageOffset: offset})
			if err != nil {
				return StaffEventDetail{}, err
			}
			roster, next := page(roster, offset)
			counts, err := q.EventReservationCounts(ctx, []pgtype.UUID{id})
			if err != nil {
				return StaffEventDetail{}, err
			}
			out := StaffEventDetail{StaffEvent: staffEvent(e), Attendees: make([]EventAttendee, 0, len(roster)), NextOffset: next}
			if len(counts) > 0 {
				out.Reserved, out.Waitlisted, out.CheckedIn = counts[0].Reserved, counts[0].Waitlisted, counts[0].CheckedIn
			}
			for _, r := range roster {
				out.Attendees = append(out.Attendees, EventAttendee{
					DonorID: r.DonorID.String(), Email: r.Email, PublicName: r.PublicName.String, Status: r.Status,
					ReservedAt: r.ReservedAt, CheckedInAt: r.CheckedInAt,
				})
			}
			return out, nil
		})
}

// CheckInAttendee is the manual fallback to QR check-in (§8 Growth): staff
// mark a donor holding a seat as attended, from an hour before the start until
// the end. Checking in an attendee twice is a no-op. Audited.
func (s EventService) CheckInAttendee(ctx context.Context, token string, eventID, donorID pgtype.UUID) error {
	_, err := staffWrite(ctx, s.Staff, token, eventPermission,
		func(tx pgx.Tx, actor queries.DonorStaff, _ time.Time) (struct{}, error) {
			q := queries.New(tx)
			donor, err := q.LockDonor(ctx, donorID)
			if err != nil {
				return struct{}{}, err
			}
			e, err := q.LockEvent(ctx, eventID)
			if err != nil {
				return struct{}{}, err
			}
			now := time.Now().UTC()
			if e.CancelledAt.Valid {
				return struct{}{}, ErrEventCancelled
			}
			if e.PublishState != "PUBLISHED" || now.Before(e.StartsAt.Time.Add(-time.Hour)) || now.After(e.EndsAt.Time) {
				return struct{}{}, ErrCheckIn
			}
			reservation, err := q.ReservationFor(ctx, queries.ReservationForParams{EventID: eventID, DonorID: donorID})
			if err != nil {
				return struct{}{}, err
			}
			if reservation.Status != reservationReserved {
				return struct{}{}, ErrCheckIn
			}
			if reservation.CheckedInAt.Valid {
				return struct{}{}, nil
			}
			if err = q.CheckInReservation(ctx, queries.CheckInReservationParams{
				EventID: eventID, DonorID: donorID, CheckedInAt: timestamp(now),
			}); err != nil {
				return struct{}{}, err
			}
			if err = auditStaff(ctx, q, actor.ID, "event.checked_in", eventID.String(),
				map[string]string{"donor_id": donorID.String()}, now); err != nil {
				return struct{}{}, err
			}
			return struct{}{}, awardBadges(ctx, q, donor, now)
		})
	return err
}

// Cancel cancels an event that has not ended, releases every seat and waitlist
// place, and emails and notifies each donor who held one (§8 Growth). Repeating
// it is a no-op. Audited.
func (s EventService) Cancel(ctx context.Context, token string, eventID pgtype.UUID) error {
	_, err := staffWrite(ctx, s.Staff, token, eventPermission,
		func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (struct{}, error) {
			q := queries.New(tx)
			e, err := q.LockEvent(ctx, eventID)
			if err != nil {
				return struct{}{}, err
			}
			if e.CancelledAt.Valid {
				return struct{}{}, nil
			}
			if now.After(e.EndsAt.Time) {
				return struct{}{}, ErrReservationClosed
			}
			if _, err = q.CancelEvent(ctx, queries.CancelEventParams{ID: eventID, CancelledAt: timestamp(now)}); err != nil {
				return struct{}{}, err
			}
			notified, err := q.CancelEventReservationsAndNotify(ctx, queries.CancelEventReservationsAndNotifyParams{
				EventID: eventID, AsOf: timestamp(now), ExpiresAt: e.EndsAt,
			})
			if err != nil {
				return struct{}{}, err
			}
			return struct{}{}, auditStaff(ctx, q, actor.ID, "event.cancelled", eventID.String(),
				map[string]int64{"notified": notified}, now)
		})
	return err
}
