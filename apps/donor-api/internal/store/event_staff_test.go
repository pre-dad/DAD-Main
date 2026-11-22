package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/mail"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestStaffEventRosterCheckInAndCancellation(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	ctx := context.Background()
	cleanEvents(t, pool)
	_, adminToken := staffFixture(t, pool, "SUPER_ADMIN")
	_, impact := staffFixture(t, pool, "OP_IMPACT")
	events := EventService{Staff: &StaffService{DB: api}}
	reservations := ReservationService{DB: api}
	reader := EventReader{DB: pool}

	in := eventInput("staff-roster", "San Francisco", "94103", "VOLUNTEER", 30*time.Minute)
	in.Capacity = 1
	eventID, err := events.Create(ctx, adminToken, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Publish(ctx, adminToken, eventID); err != nil {
		t.Fatal(err)
	}
	seated, seatedToken := signedInDonor(t, pool)
	waiting, waitingToken := signedInDonor(t, pool)
	for _, d := range []any{seated, waiting} {
		mustExec(t, pool, "UPDATE donors SET city='San Francisco', zip='94103' WHERE id=$1", d)
	}
	pushCleanupDonor(t, pool, seated)
	pushCleanupDonor(t, pool, waiting)
	t.Cleanup(func() { cleanExec(t, "DELETE FROM donor_badges WHERE donor_id=$1", seated) })
	if _, err := reservations.Reserve(ctx, seatedToken, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := reservations.Reserve(ctx, waitingToken, eventID); err != nil {
		t.Fatal(err)
	}

	if _, err := events.List(ctx, impact, 0); !errors.Is(err, ErrStaffForbidden) {
		t.Fatalf("OP_IMPACT listed events: %v", err)
	}
	page, err := events.List(ctx, adminToken, 0)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(page.Events, func(e StaffEvent) bool { return e.ID == eventID.String() })
	if i < 0 || page.Events[i].Reserved != 1 || page.Events[i].Waitlisted != 1 || page.Events[i].PublishState != "PUBLISHED" {
		t.Fatalf("event list: %+v", page.Events)
	}
	detail, err := events.Detail(ctx, adminToken, eventID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Attendees) != 2 || detail.Attendees[0].DonorID != seated.String() || detail.Attendees[1].Status != reservationWaitlisted {
		t.Fatalf("roster: %+v", detail.Attendees)
	}

	if err := events.CheckInAttendee(ctx, adminToken, eventID, waiting); !errors.Is(err, ErrCheckIn) {
		t.Fatalf("checked in a waitlisted donor: %v", err)
	}
	if err := events.CheckInAttendee(ctx, adminToken, eventID, mustNewID(t)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("checked in an unknown donor: %v", err)
	}
	for range 2 {
		if err := events.CheckInAttendee(ctx, adminToken, eventID, seated); err != nil {
			t.Fatal(err)
		}
	}
	if detail, err = events.Detail(ctx, adminToken, eventID, 0); err != nil || detail.CheckedIn != 1 ||
		!detail.Attendees[0].CheckedInAt.Valid {
		t.Fatalf("after check-in: %+v %v", detail, err)
	}

	for range 2 {
		if err := events.Cancel(ctx, adminToken, eventID); err != nil {
			t.Fatal(err)
		}
	}
	if detail, err = events.Detail(ctx, adminToken, eventID, 0); err != nil || !detail.CancelledAt.Valid || len(detail.Attendees) != 0 {
		t.Fatalf("after cancellation: %+v %v", detail, err)
	}
	for _, d := range []any{seated, waiting} {
		var pushes, emails int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_push_notifications WHERE donor_id=$1 AND category=$2", d, eventCancelledCategory).
			Scan(&pushes); err != nil ||
			pushes != 1 {
			t.Fatalf("cancellation pushes for %v = %d (%v), want 1", d, pushes, err)
		}
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_messages WHERE donor_id=$1 AND kind=$2", d, eventCancelledCategory).
			Scan(&emails); err != nil || emails != 1 {
			t.Fatalf("cancellation emails for %v = %d (%v), want 1", d, emails, err)
		}
	}
	transport := &recordingMail{}
	worker := MessageWorker{
		DB:        as(t, dbroles.MessageWorker),
		Settings:  mail.Settings{Host: "smtp.fixture", Port: 587, Sender: "sender@example.org", EncryptionKeys: emailTestKeys},
		Origin:    "https://donor.example.org",
		Transport: transport,
	}
	for {
		worked, err := worker.DeliverNext(ctx, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			break
		}
	}
	notices := 0
	for _, m := range transport.messages {
		if m.Subject == "Event staff-roster has been cancelled" && strings.Contains(m.Body, "Your place has been released") {
			notices++
		}
	}
	if notices != 2 {
		t.Fatalf("cancellation emails sent = %d, want 2: %+v", notices, transport.messages)
	}
	for _, intent := range pushIntents(t, pool, waiting) {
		if intent.Category != eventCancelledCategory {
			continue
		}
		donorRow, err := queries.New(pool).LockDonor(ctx, waiting)
		if err != nil {
			t.Fatal(err)
		}
		if reason, err := pushSuppression(ctx, queries.New(pool), intent, donorRow, time.Now().UTC()); err != nil || reason != "" {
			t.Fatalf("cancellation push suppressed: %q %v", reason, err)
		}
		if note, err := pushContent(intent); err != nil || note.Title != "An event was cancelled" {
			t.Fatalf("cancellation push: %+v %v", note, err)
		}
	}
	if _, err := reservations.Reserve(ctx, waitingToken, eventID); !errors.Is(err, ErrEventCancelled) {
		t.Fatalf("reserved a cancelled event: %v", err)
	}
	if err := events.Update(ctx, adminToken, eventID, in); !errors.Is(err, ErrEventCancelled) {
		t.Fatalf("edited a cancelled event: %v", err)
	}
	near, err := reader.NearDonor(ctx, waitingToken)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(near, func(v EventView) bool { return v.ID == eventID.String() }) {
		t.Fatal("a cancelled event is still listed to donors")
	}
	view, err := reader.DetailForDonor(ctx, waitingToken, eventID)
	if err != nil || !view.Cancelled || view.ReservationStatus != nil {
		t.Fatalf("cancelled event detail: %+v %v", view, err)
	}
}

func seedLargeEventRoster(t *testing.T, pool *pgxpool.Pool, event pgtype.UUID, size int) {
	t.Helper()
	t.Cleanup(func() {
		cleanExec(
			t,
			`DELETE FROM donor_push_notifications WHERE donor_id IN (SELECT donor_id FROM donor_event_reservations WHERE event_id=$1)`,
			event,
		)
		cleanExec(
			t,
			`DELETE FROM donor_messages WHERE donor_id IN (SELECT donor_id FROM donor_event_reservations WHERE event_id=$1)`,
			event,
		)
		cleanExec(t, `DELETE FROM donors WHERE id IN (SELECT donor_id FROM donor_event_reservations WHERE event_id=$1)`, event)
	})
	mustExec(t, pool, `WITH people AS (
 INSERT INTO donors (id,email) SELECT gen_random_uuid(), 'roster-'||gen_random_uuid()::text||'@example.org' FROM generate_series(1,$2::integer) RETURNING id
 ) INSERT INTO donor_event_reservations (id,event_id,donor_id,status,reserved_at)
 SELECT gen_random_uuid(),$1,id,CASE WHEN row_number() OVER () <= 75 THEN 'RESERVED' ELSE 'WAITLISTED' END, now() FROM people`, event, size)
}

func TestStaffEventRosterIsBoundedAndCountsAreGlobal(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	cleanEvents(t, pool)
	_, token := staffFixture(t, pool, "SUPER_ADMIN")
	events := EventService{Staff: &StaffService{DB: as(t, dbroles.API)}}
	id, err := events.Create(ctx, token, eventInput("large-roster", "Boston", "02108", "VOLUNTEER", time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	seedLargeEventRoster(t, pool, id, 110)
	seen := map[string]bool{}
	offset := int64(0)
	for pageIndex, want := range []int{50, 50, 10} {
		detail, err := events.Detail(ctx, token, id, offset)
		if err != nil {
			t.Fatal(err)
		}
		if len(detail.Attendees) != want || detail.Reserved != 75 || detail.Waitlisted != 35 {
			t.Fatalf("page %d: attendees=%d totals=%+v", pageIndex, len(detail.Attendees), detail.StaffEvent)
		}
		for _, attendee := range detail.Attendees {
			if seen[attendee.DonorID] {
				t.Fatalf("repeated attendee %s", attendee.DonorID)
			}
			seen[attendee.DonorID] = true
		}
		if pageIndex < 2 {
			if detail.NextOffset == nil || *detail.NextOffset != offset+50 {
				t.Fatalf("next page = %v", detail.NextOffset)
			}
			offset = *detail.NextOffset
		} else if detail.NextOffset != nil {
			t.Fatalf("last page has offset %d", *detail.NextOffset)
		}
	}
	if len(seen) != 110 {
		t.Fatalf("saw %d unique attendees", len(seen))
	}
	if _, err := events.Detail(ctx, token, id, -1); !errors.Is(err, ErrStaffInput) {
		t.Fatalf("negative offset: %v", err)
	}
}

type eventQueryCounter struct{ count atomic.Int64 }

func (c *eventQueryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.count.Add(1)
	return ctx
}
func (*eventQueryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestStaffEventCancellationUsesBoundedRoundTrips(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	cleanEvents(t, pool)
	_, token := staffFixture(t, pool, "SUPER_ADMIN")
	counter := &eventQueryCounter{}
	config := as(t, dbroles.API).Config()
	config.ConnConfig.Tracer = counter
	traced, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(traced.Close)
	events := EventService{Staff: &StaffService{DB: traced}}
	id, err := events.Create(ctx, token, eventInput("cancel-large-roster", "Boston", "02108", "VOLUNTEER", time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	seedLargeEventRoster(t, pool, id, 5000)
	counter.count.Store(0)
	if err := events.Cancel(ctx, token, id); err != nil {
		t.Fatal(err)
	}
	if calls := counter.count.Load(); calls < 1 || calls > 15 {
		t.Errorf("cancellation made %d SQL round trips for 5000 donors, want at most 15", calls)
	}
	for range 2 {
		var live, pushes, messages int
		if err := pool.QueryRow(ctx, `SELECT
   (SELECT count(*) FROM donor_event_reservations WHERE event_id=$1 AND status<>'CANCELLED'),
   (SELECT count(*) FROM donor_push_notifications WHERE context->>'event_id'=$1::text AND category='EVENT_CANCELLED'),
   (SELECT count(*) FROM donor_messages WHERE context->>'event_id'=$1::text AND kind='EVENT_CANCELLED')`, id).Scan(&live, &pushes, &messages); err != nil {
			t.Fatal(err)
		}
		if live != 0 || pushes != 5000 || messages != 5000 {
			t.Fatalf("after cancellation live=%d pushes=%d messages=%d", live, pushes, messages)
		}
		if err := events.Cancel(ctx, token, id); err != nil {
			t.Fatal(err)
		}
	}
}
