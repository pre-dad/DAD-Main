package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"dad/donor-api/internal/sharing"
	"dad/donor-api/internal/store"
)

// EventSource is the Growth staff surface for events (E-3).
type EventSource interface {
	Create(context.Context, string, store.EventInput) (pgtype.UUID, error)
	Update(context.Context, string, pgtype.UUID, store.EventInput) error
	Publish(context.Context, string, pgtype.UUID) error
	AddGuest(context.Context, string, pgtype.UUID, pgtype.UUID) error
	RemoveGuest(context.Context, string, pgtype.UUID, pgtype.UUID) error
	CheckInToken(context.Context, string, pgtype.UUID, string) (string, error)
	List(context.Context, string, int64) (store.StaffEventPage, error)
	Detail(context.Context, string, pgtype.UUID) (store.StaffEventDetail, error)
	CheckInAttendee(context.Context, string, pgtype.UUID, pgtype.UUID) error
	Cancel(context.Context, string, pgtype.UUID) error
}

// EventReadSource is the donor-facing read of published events (E-2/E-3/E-5).
type EventReadSource interface {
	NearDonor(context.Context, string) ([]store.EventView, error)
	InviteOnlyEvents(context.Context, string) ([]store.EventView, error)
	Detail(context.Context, pgtype.UUID) (store.EventView, error)
	DetailForDonor(context.Context, string, pgtype.UUID) (store.EventView, error)
	Calendar(context.Context, pgtype.UUID) (string, error)
	CalendarForDonor(context.Context, string, pgtype.UUID) (string, error)
}

// ReservationSource books, waitlists, and cancels event reservations (E-4/E-9),
// and records QR check-ins (E-8).
type ReservationSource interface {
	Reserve(context.Context, string, pgtype.UUID) (store.ReservationResult, error)
	Cancel(context.Context, string, pgtype.UUID) (store.ReservationResult, error)
	CheckIn(context.Context, string, string) (store.CheckInResult, error)
}

func (a *api) eventsAdmin() service {
	return service{
		a.Events != nil,
		"Event administration is temporarily unavailable",
		staffTimeout,
		staffEventStatus,
	}
}

func staffEventStatus(err error) (int, string) {
	switch {
	case errors.Is(err, store.ErrEventCancelled):
		return http.StatusConflict, "This event has been cancelled"
	case errors.Is(err, store.ErrReservationClosed):
		return http.StatusConflict, "This event has already ended"
	case errors.Is(err, store.ErrCheckIn):
		return http.StatusUnprocessableEntity, "Only a donor holding a seat can be checked in, from an hour before the start until the end"
	default:
		return staffStatus(err)
	}
}

func (a *api) staffEvents(w http.ResponseWriter, r *http.Request, session string) {
	offset, ok := offsetParam(r, "offset")
	if !ok {
		invalidFields(w, r)
		return
	}
	a.eventsAdmin().serve(w, r, func(ctx context.Context) (any, error) { return a.Events.List(ctx, session, offset) })
}

func (a *api) staffEvent(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "event_id")
	if !ok {
		return
	}
	a.eventsAdmin().serve(w, r, func(ctx context.Context) (any, error) { return a.Events.Detail(ctx, session, id) })
}

func (a *api) staffEventCheckIn(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "event_id")
	if !ok {
		return
	}
	donor, ok := pathID(w, r, "donor_id")
	if !ok {
		return
	}
	a.eventsAdmin().serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]bool{"checked_in": true}, a.Events.CheckInAttendee(ctx, session, id, donor)
	})
}

// eventCheckInQR renders an event's check-in token as the QR code staff print
// or display at the venue (E-8, §8 Growth).
func (a *api) eventCheckInQR(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "event_id")
	if !ok {
		return
	}
	if !a.eventsAdmin().available(w, r) {
		return
	}
	ctx, cancel := withTimeout(r, staffTimeout)
	defer cancel()
	token, err := a.Events.CheckInToken(ctx, session, id, a.EventCheckInSecret)
	if err != nil {
		respond(w, r, nil, err, staffEventStatus)
		return
	}
	image, err := sharing.SVG(token)
	if err != nil {
		writeError(w, r, http.StatusServiceUnavailable, "Event administration is temporarily unavailable", err)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	_, _ = w.Write(image)
}

func (a *api) cancelStaffEvent(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "event_id")
	if !ok {
		return
	}
	a.eventsAdmin().serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]bool{"cancelled": true}, a.Events.Cancel(ctx, session, id)
	})
}

func (a *api) createEvent(w http.ResponseWriter, r *http.Request, session string) {
	var body store.EventInput
	if !decodeJSON(w, r, &body) {
		return
	}
	a.eventsAdmin().serve(w, r, func(ctx context.Context) (any, error) {
		id, err := a.Events.Create(ctx, session, body)
		if err != nil {
			return nil, err
		}
		return map[string]string{"id": id.String()}, nil
	})
}

func (a *api) updateEvent(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "event_id")
	if !ok {
		return
	}
	var body store.EventInput
	if !decodeJSON(w, r, &body) {
		return
	}
	a.eventsAdmin().serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]bool{"updated": true}, a.Events.Update(ctx, session, id, body)
	})
}

func (a *api) publishEvent(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "event_id")
	if !ok {
		return
	}
	a.eventsAdmin().serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]bool{"published": true}, a.Events.Publish(ctx, session, id)
	})
}

// events lists the signed-in donor's upcoming events in and near their city
// (E-2), soonest first.
func (a *api) events(w http.ResponseWriter, r *http.Request, session string) {
	svc := service{a.EventRead != nil, donorUnavailable, readTimeout, donorStatus}
	svc.serve(w, r, func(ctx context.Context) (any, error) {
		views, err := a.EventRead.NearDonor(ctx, session)
		if err != nil {
			return nil, err
		}
		return map[string]any{"events": views}, nil
	})
}

// eventDetail is the donor-facing read of one published event (E-3). A draft or
// unknown id answers 404.
func (a *api) eventDetail(w http.ResponseWriter, r *http.Request) {
	if a.EventRead == nil {
		writeError(w, r, http.StatusServiceUnavailable, donorUnavailable, nil)
		return
	}
	id, ok := pathID(w, r, "event_id")
	if !ok {
		return
	}
	ctx, cancel := withTimeout(r, 3*time.Second)
	defer cancel()
	body, err := a.EventRead.Detail(ctx, id)
	switch {
	case errors.Is(err, store.ErrEventNotFound):
		writeError(w, r, http.StatusNotFound, "Record not found", nil)
	case err != nil:
		writeError(w, r, http.StatusServiceUnavailable, donorUnavailable, err)
	default:
		w.Header().Set("Cache-Control", "public, max-age=60")
		writeJSON(w, http.StatusOK, body)
	}
}

// eventCalendar serves a published event as an .ics download (E-6). INVITE_ONLY
// and draft events read as not found, like the anonymous detail.
func (a *api) eventCalendar(w http.ResponseWriter, r *http.Request) {
	if a.EventRead == nil {
		writeError(w, r, http.StatusServiceUnavailable, donorUnavailable, nil)
		return
	}
	id, ok := pathID(w, r, "event_id")
	if !ok {
		return
	}
	ctx, cancel := withTimeout(r, 3*time.Second)
	defer cancel()
	ics, err := a.EventRead.Calendar(ctx, id)
	switch {
	case errors.Is(err, store.ErrEventNotFound):
		writeError(w, r, http.StatusNotFound, "Record not found", nil)
	case err != nil:
		writeError(w, r, http.StatusServiceUnavailable, donorUnavailable, err)
	default:
		w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=\"event.ics\"")
		w.Header().Set("Cache-Control", "public, max-age=60")
		_, _ = w.Write([]byte(ics))
	}
}

// inviteOnlyEvents lists the INVITE_ONLY events the signed-in donor can see
// (E-5), via a qualifying badge perk or the admin guest list.
func (a *api) inviteOnlyEvents(w http.ResponseWriter, r *http.Request, session string) {
	svc := service{a.EventRead != nil, donorUnavailable, readTimeout, donorStatus}
	svc.serve(w, r, func(ctx context.Context) (any, error) {
		views, err := a.EventRead.InviteOnlyEvents(ctx, session)
		if err != nil {
			return nil, err
		}
		return map[string]any{"events": views}, nil
	})
}

// donorEventDetail is the signed-in donor's view of one event, enforcing the
// E-5 gate for INVITE_ONLY events (a donor who cannot see it gets 404).
func (a *api) donorEventDetail(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "event_id")
	if !ok {
		return
	}
	svc := service{a.EventRead != nil, donorUnavailable, readTimeout, donorStatus}
	svc.serve(w, r, func(ctx context.Context) (any, error) {
		return a.EventRead.DetailForDonor(ctx, session, id)
	})
}

func (a *api) donorEventCalendar(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "event_id")
	if !ok {
		return
	}
	svc := service{a.EventRead != nil, donorUnavailable, readTimeout, donorStatus}
	if !svc.available(w, r) {
		return
	}
	ctx, cancel := withTimeout(r, readTimeout)
	defer cancel()
	ics, err := a.EventRead.CalendarForDonor(ctx, session, id)
	if err != nil {
		respond(w, r, nil, err, donorStatus)
		return
	}
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=\"event.ics\"")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(ics))
}

// addEventGuest puts a donor on an event's guest list (E-5, admin).
func (a *api) addEventGuest(w http.ResponseWriter, r *http.Request, session string) {
	eventID, ok := pathID(w, r, "event_id")
	if !ok {
		return
	}
	donorID, ok := pathID(w, r, "donor_id")
	if !ok {
		return
	}
	a.eventsAdmin().serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]bool{"added": true}, a.Events.AddGuest(ctx, session, eventID, donorID)
	})
}

// removeEventGuest removes a donor from an event's guest list (E-5, admin).
func (a *api) removeEventGuest(w http.ResponseWriter, r *http.Request, session string) {
	eventID, ok := pathID(w, r, "event_id")
	if !ok {
		return
	}
	donorID, ok := pathID(w, r, "donor_id")
	if !ok {
		return
	}
	a.eventsAdmin().serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]bool{"removed": true}, a.Events.RemoveGuest(ctx, session, eventID, donorID)
	})
}

// reserveEvent books (or waitlists) the signed-in donor at an event (E-4).
func (a *api) reserveEvent(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "event_id")
	if !ok {
		return
	}
	svc := service{a.Reservations != nil, donorUnavailable, providerTimeout, donorStatus}
	svc.serve(w, r, func(ctx context.Context) (any, error) {
		return a.Reservations.Reserve(ctx, session, id)
	})
}

// cancelEvent cancels the signed-in donor's reservation, promoting the waitlist
// head if a seat freed (E-4/E-9).
func (a *api) cancelEvent(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "event_id")
	if !ok {
		return
	}
	svc := service{a.Reservations != nil, donorUnavailable, providerTimeout, donorStatus}
	svc.serve(w, r, func(ctx context.Context) (any, error) {
		return a.Reservations.Cancel(ctx, session, id)
	})
}

// checkInBody is the QR token the app scanned at the venue.
type checkInBody struct {
	Token string `json:"token"`
}

// checkInEvent records the signed-in donor's attendance from a scanned QR token
// (E-8). The token names the event and is HMAC-verified server-side.
func (a *api) checkInEvent(w http.ResponseWriter, r *http.Request, session string) {
	var body checkInBody
	if !decodeJSON(w, r, &body) {
		return
	}
	svc := service{a.Reservations != nil, donorUnavailable, providerTimeout, donorStatus}
	svc.serve(w, r, func(ctx context.Context) (any, error) {
		return a.Reservations.CheckIn(ctx, session, body.Token)
	})
}

// eventCheckInToken returns an event's signed QR token for staff to render the
// venue QR code (E-8). Admin-only.
func (a *api) eventCheckInToken(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "event_id")
	if !ok {
		return
	}
	a.eventsAdmin().serve(w, r, func(ctx context.Context) (any, error) {
		token, err := a.Events.CheckInToken(ctx, session, id, a.EventCheckInSecret)
		if err != nil {
			return nil, err
		}
		return map[string]string{"token": token}, nil
	})
}
