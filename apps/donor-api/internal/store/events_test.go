package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/mail"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func cleanEvents(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	t.Cleanup(func() {
		cleanExec(t, "DELETE FROM donor_events")
	})
}

// eventInput builds a valid staff event input starting at +days days, in the
// given city/zip, of the given type.
func eventInput(slug, city, zip, kind string, startIn time.Duration) EventInput {
	start := time.Now().UTC().Add(startIn)
	return EventInput{
		Slug: slug, Title: "Event " + slug, Type: kind,
		Description: "A gathering.", Venue: "Community Hall", Address: "1 Main St",
		City: city, Zip: zip,
		StartsAt: start.Format(time.RFC3339),
		EndsAt:   start.Add(2 * time.Hour).Format(time.RFC3339),
		Capacity: 20,
	}
}

func TestEventDraftPublishAndValidation(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	ctx := context.Background()
	cleanEvents(t, pool)
	_, adminToken := staffFixture(t, pool, "SUPER_ADMIN")
	_, support := staffFixture(t, pool, "OP_PARTNERSHIPS")
	s := EventService{Staff: &StaffService{DB: api}}

	// A non-publisher role cannot create events.
	if _, err := s.Create(ctx, support, eventInput("water-cleanup", "SF", "94103", "VOLUNTEER", 48*time.Hour)); !errors.Is(
		err,
		ErrStaffForbidden,
	) {
		t.Fatalf("SUPPORT created an event: %v", err)
	}

	// Bad slug, bad type, and ends-before-starts are each refused.
	for name, in := range map[string]EventInput{
		"bad slug": eventInput("Bad Slug!", "SF", "94103", "VOLUNTEER", 48*time.Hour),
		"bad type": eventInput("x", "SF", "94103", "PARTY", 48*time.Hour),
	} {
		if _, err := s.Create(ctx, adminToken, in); !errors.Is(err, ErrStaffInput) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	backwards := eventInput("backwards", "SF", "94103", "VOLUNTEER", 48*time.Hour)
	backwards.EndsAt = backwards.StartsAt
	if _, err := s.Create(ctx, adminToken, backwards); !errors.Is(err, ErrStaffInput) {
		t.Fatalf("ends==starts accepted: %v", err)
	}

	// Create a DRAFT, confirm it is not donor-visible, then publish it.
	id, err := s.Create(ctx, adminToken, eventInput("water-cleanup", "SF", "94103", "VOLUNTEER", 48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	reader := EventReader{DB: pool}
	if _, err := reader.Detail(ctx, id); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("draft event was donor-visible: %v", err)
	}
	if err := s.Publish(ctx, adminToken, id); err != nil {
		t.Fatal(err)
	}
	view, err := reader.Detail(ctx, id)
	if err != nil || view.Title != "Event water-cleanup" || view.Capacity != 20 {
		t.Fatalf("published detail: %+v %v", view, err)
	}

	var first, again time.Time
	var audited int
	if err = pool.QueryRow(ctx, "SELECT published_at FROM donor_events WHERE id=$1", id).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if err = s.Publish(ctx, adminToken, id); err != nil {
		t.Fatalf("republish: %v", err)
	}
	if err = pool.QueryRow(ctx, "SELECT published_at FROM donor_events WHERE id=$1", id).Scan(&again); err != nil || !again.Equal(first) {
		t.Fatalf("republishing moved published_at from %v to %v (%v)", first, again, err)
	}
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM donor_staff_audit WHERE action='event.published' AND target=$1", id.String()).Scan(&audited); err != nil ||
		audited != 1 {
		t.Fatalf("event.published audited %d times (%v), want once", audited, err)
	}
}

func TestUpcomingEventsNearDonor(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	ctx := context.Background()
	cleanEvents(t, pool)
	_, adminToken := staffFixture(t, pool, "SUPER_ADMIN")
	s := EventService{Staff: &StaffService{DB: api}}
	reader := EventReader{DB: pool}

	// A donor in San Francisco, ZIP 94103 (SCF 941).
	donorID, donorToken := signedInDonor(t, pool)
	if _, err := pool.Exec(ctx, "UPDATE donors SET city=$2, zip=$3 WHERE id=$1", donorID, "San Francisco", "94103"); err != nil {
		t.Fatal(err)
	}

	publish := func(in EventInput) pgtype.UUID {
		id, err := s.Create(ctx, adminToken, in)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Publish(ctx, adminToken, id); err != nil {
			t.Fatal(err)
		}
		return id
	}

	// Matches by city label (different ZIP region).
	cityMatch := publish(eventInput("city-match", "San Francisco", "90001", "VOLUNTEER", 24*time.Hour))
	// Matches by ZIP-3 region 941 (different/empty city).
	zipMatch := publish(eventInput("zip-match", "Daly City", "94105", "COMMUNITY", 72*time.Hour))
	// Far away: different city AND different ZIP region -> excluded.
	publish(eventInput("far", "Chicago", "60601", "VOLUNTEER", 24*time.Hour))
	// INVITE_ONLY -> never in the open near-city list even when local.
	publish(eventInput("invite", "San Francisco", "94103", "INVITE_ONLY", 24*time.Hour))
	// In the past -> excluded (upcoming only).
	publish(eventInput("past", "San Francisco", "94103", "VOLUNTEER", -24*time.Hour))
	// A DRAFT local event (created, not published) -> excluded.
	if _, err := s.Create(ctx, adminToken, eventInput("draft-local", "San Francisco", "94103", "VOLUNTEER", 24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	views, err := reader.NearDonor(ctx, donorToken)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, v := range views {
		got[v.ID] = true
	}
	if len(views) != 2 || !got[cityMatch.String()] || !got[zipMatch.String()] {
		t.Fatalf("near-donor list = %d events %+v; want city-match + zip-match only", len(views), views)
	}
	// Soonest first: city-match (+24h) before zip-match (+72h).
	if views[0].ID != cityMatch.String() {
		t.Fatalf("not soonest-first: %+v", views)
	}

	// A donor with neither city nor ZIP matches nothing.
	otherID, otherToken := signedInDonor(t, pool)
	if _, err := pool.Exec(ctx, "UPDATE donors SET city=NULL, zip=NULL WHERE id=$1", otherID); err != nil {
		t.Fatal(err)
	}
	empty, err := reader.NearDonor(ctx, otherToken)
	if err != nil || len(empty) != 0 {
		t.Fatalf("cityless/zipless donor saw %d events: %v", len(empty), err)
	}
}

// TestReservationCapacityAndWaitlistPromotion covers E-4/E-9: reserving within
// capacity books a seat, reserving a full event waitlists, cancelling a seat
// promotes the waitlist head (FIFO) and queues a push, and cancelling a
// waitlisted spot promotes no one. Re-reserving is idempotent.
func TestReservationCapacityAndWaitlistPromotion(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	ctx := context.Background()
	cleanEvents(t, pool)
	_, adminToken := staffFixture(t, pool, "SUPER_ADMIN")
	events := EventService{Staff: &StaffService{DB: api}}
	res := ReservationService{DB: api}

	// A published event with capacity 1 starting in the future.
	in := eventInput("gala", "San Francisco", "94103", "COMMUNITY", 48*time.Hour)
	in.Capacity = 1
	eventID, err := events.Create(ctx, adminToken, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Publish(ctx, adminToken, eventID); err != nil {
		t.Fatal(err)
	}

	d1, t1 := signedInDonor(t, pool)
	d2, t2 := signedInDonor(t, pool)
	d3, t3 := signedInDonor(t, pool)
	for _, id := range []pgtype.UUID{d1, d2, d3} {
		pushCleanupDonor(t, pool, id)
	}

	// d1 reserves the only seat; re-reserving is idempotent.
	if r, err := res.Reserve(ctx, t1, eventID); err != nil || r.Status != reservationReserved {
		t.Fatalf("d1 reserve: %+v %v", r, err)
	}
	if r, err := res.Reserve(ctx, t1, eventID); err != nil || r.Status != reservationReserved {
		t.Fatalf("d1 re-reserve not idempotent: %+v %v", r, err)
	}
	// d2 then d3 are waitlisted (full), d2 ahead of d3.
	if r, err := res.Reserve(ctx, t2, eventID); err != nil || r.Status != reservationWaitlisted {
		t.Fatalf("d2 reserve: %+v %v", r, err)
	}
	if r, err := res.Reserve(ctx, t3, eventID); err != nil || r.Status != reservationWaitlisted {
		t.Fatalf("d3 reserve: %+v %v", r, err)
	}

	// d1 cancels -> the head of the waitlist (d2) is promoted and gets a push.
	if r, err := res.Cancel(ctx, t1, eventID); err != nil || r.Status != reservationCancelled {
		t.Fatalf("d1 cancel: %+v %v", r, err)
	}
	var d2status string
	if err := pool.QueryRow(ctx, "SELECT status FROM donor_event_reservations WHERE event_id=$1 AND donor_id=$2", eventID, d2).Scan(&d2status); err != nil ||
		d2status != reservationReserved {
		t.Fatalf("d2 not promoted: %q %v", d2status, err)
	}
	var pushCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_push_notifications WHERE donor_id=$1 AND category=$2", d2, eventPromotedCategory).Scan(&pushCount); err != nil ||
		pushCount != 1 {
		t.Fatalf("d2 promotion push: count=%d %v", pushCount, err)
	}

	// The promotion is also emailed (§6.11), naming the event and linking to it.
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
	var promoted *mail.Message
	for i, m := range transport.messages {
		if strings.HasPrefix(m.Subject, "You're off the waitlist") {
			promoted = &transport.messages[i]
		}
	}
	if promoted == nil || promoted.Subject != "You're off the waitlist for Event gala" ||
		!strings.Contains(promoted.Body, "https://donor.example.org/events/"+eventID.String()) {
		t.Fatalf("promotion email: %+v", promoted)
	}

	// The push is about d2's own seat, so it goes out though d2 never opted in
	// as a volunteer, and stops once the seat is released.
	var job queries.DonorPushNotification
	for _, intent := range pushIntents(t, pool, d2) {
		if intent.Category == eventPromotedCategory {
			job = intent
		}
	}
	donorRow, err := queries.New(pool).LockDonor(ctx, d2)
	if err != nil {
		t.Fatal(err)
	}
	if reason, err := pushSuppression(ctx, queries.New(pool), job, donorRow, time.Now().UTC()); err != nil || reason != "" {
		t.Fatalf("promotion push suppressed for a non-volunteer: %q %v", reason, err)
	}

	// d2 (now RESERVED) cancels -> d3 (last on the waitlist) is promoted, so
	// exactly one RESERVED remains.
	if _, err := res.Cancel(ctx, t2, eventID); err != nil {
		t.Fatal(err)
	}
	if reason, err := pushSuppression(ctx, queries.New(pool), job, donorRow, time.Now().UTC()); err != nil ||
		reason != "reservation_released" {
		t.Fatalf("promotion push for a released seat: %q %v", reason, err)
	}
	var reservedNow int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_event_reservations WHERE event_id=$1 AND status='RESERVED'", eventID).Scan(&reservedNow); err != nil ||
		reservedNow != 1 {
		t.Fatalf("expected exactly one RESERVED after the chain: %d %v", reservedNow, err)
	}
}

// Raising an upcoming event's capacity seats waitlisted donors in queue order
// at once, rather than leaving them waiting for someone to cancel.
func TestRaisingCapacitySeatsTheWaitlist(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	ctx := context.Background()
	cleanEvents(t, pool)
	_, adminToken := staffFixture(t, pool, "SUPER_ADMIN")
	events := EventService{Staff: &StaffService{DB: api}}
	res := ReservationService{DB: api}

	in := eventInput("garden", "San Francisco", "94103", "VOLUNTEER", 48*time.Hour)
	in.Capacity = 1
	eventID, err := events.Create(ctx, adminToken, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Publish(ctx, adminToken, eventID); err != nil {
		t.Fatal(err)
	}
	var order []pgtype.UUID
	for range 4 {
		id, token := signedInDonor(t, pool)
		pushCleanupDonor(t, pool, id)
		if _, err := res.Reserve(ctx, token, eventID); err != nil {
			t.Fatal(err)
		}
		order = append(order, id)
	}

	in.Capacity = 3
	if err := events.Update(ctx, adminToken, eventID, in); err != nil {
		t.Fatal(err)
	}
	for i, id := range order {
		var status string
		if err := pool.QueryRow(ctx, "SELECT status FROM donor_event_reservations WHERE event_id=$1 AND donor_id=$2", eventID, id).
			Scan(&status); err != nil {
			t.Fatal(err)
		}
		want := reservationReserved
		if i == 3 {
			want = reservationWaitlisted
		}
		if status != want {
			t.Fatalf("donor %d of 4 is %s after capacity 1 -> 3, want %s", i+1, status, want)
		}
	}
	var pushes int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_push_notifications WHERE category=$1 AND donor_id = ANY($2)",
		eventPromotedCategory, order).Scan(&pushes); err != nil || pushes != 2 {
		t.Fatalf("promotion pushes = %d (%v), want 2", pushes, err)
	}

	in.Capacity = 1
	if err := events.Update(ctx, adminToken, eventID, in); err != nil {
		t.Fatal(err)
	}
	var reserved int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_event_reservations WHERE event_id=$1 AND status='RESERVED'", eventID).
		Scan(&reserved); err != nil || reserved != 3 {
		t.Fatalf("lowering capacity changed held seats: %d %v", reserved, err)
	}
}

// pushCleanupDonor removes a donor's queued push notifications after the test
// (they reference the donor, not the cascaded event).
func pushCleanupDonor(t *testing.T, pool *pgxpool.Pool, donor pgtype.UUID) {
	t.Helper()
	t.Cleanup(func() {
		cleanExec(t, "DELETE FROM donor_push_notifications WHERE donor_id=$1", donor)
		cleanExec(t, "DELETE FROM donor_messages WHERE donor_id=$1", donor)
	})
}

// TestReservationClosedAfterStart covers E-9: no reserve once the event has
// started.
func TestReservationClosedAfterStart(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	ctx := context.Background()
	cleanEvents(t, pool)
	_, adminToken := staffFixture(t, pool, "SUPER_ADMIN")
	events := EventService{Staff: &StaffService{DB: api}}
	res := ReservationService{DB: pool}

	in := eventInput("ongoing", "San Francisco", "94103", "VOLUNTEER", -1*time.Hour)
	in.EndsAt = time.Now().UTC().Add(1 * time.Hour).Format(time.RFC3339)
	eventID, err := events.Create(ctx, adminToken, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Publish(ctx, adminToken, eventID); err != nil {
		t.Fatal(err)
	}
	_, tok := signedInDonor(t, pool)
	if _, err := res.Reserve(ctx, tok, eventID); !errors.Is(err, ErrReservationClosed) {
		t.Fatalf("reserve after start accepted: %v", err)
	}
}

// TestInviteOnlyGating covers E-5: an INVITE_ONLY event is visible only to a
// donor who holds the qualifying badge perk or is on the admin guest list; it
// never appears in the open near-city list or the anonymous detail; a
// perk-unlocked view carries the "Invitation earned at ..." note.
func TestInviteOnlyGating(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	ctx := context.Background()
	cleanEvents(t, pool)
	_, adminToken := staffFixture(t, pool, "SUPER_ADMIN")
	events := EventService{Staff: &StaffService{DB: api}}
	reader := EventReader{DB: pool}

	// A Founding Donor Dinner: INVITE_ONLY, gated on the FOUNDING_EVENTS perk.
	in := eventInput("founding-dinner", "San Francisco", "94103", "INVITE_ONLY", 72*time.Hour)
	in.QualifyingPerk = "FOUNDING_EVENTS"
	eventID, err := events.Create(ctx, adminToken, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Publish(ctx, adminToken, eventID); err != nil {
		t.Fatal(err)
	}

	// A perk-qualified event must not require a city match, so set up a donor
	// with the Founding Donor badge ("F" carries FOUNDING_EVENTS).
	founder, founderTok := signedInDonor(t, pool)
	if _, err := pool.Exec(ctx, "UPDATE donors SET city=$2, zip=$3 WHERE id=$1", founder, "San Francisco", "94103"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO donor_badges (donor_id, code, source, earned_at) VALUES ($1,'F','AUTO',now())", founder); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanExec(t, "DELETE FROM donor_badges WHERE donor_id=$1", founder) })

	// A plain donor (no badge, not a guest).
	plain, plainTok := signedInDonor(t, pool)
	if _, err := pool.Exec(ctx, "UPDATE donors SET city=$2, zip=$3 WHERE id=$1", plain, "San Francisco", "94103"); err != nil {
		t.Fatal(err)
	}

	// The open near-city list never contains the invite-only event, for either.
	for _, tok := range []string{founderTok, plainTok} {
		near, err := reader.NearDonor(ctx, tok)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range near {
			if v.ID == eventID.String() {
				t.Fatalf("invite-only event leaked into the open near-city list")
			}
		}
	}
	// The anonymous detail reads as not found.
	if _, err := reader.Detail(ctx, eventID); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("invite-only detail visible anonymously: %v", err)
	}

	// The founder sees it via the perk, with the invitation note.
	fList, err := reader.InviteOnlyEvents(ctx, founderTok)
	if err != nil || len(fList) != 1 || fList[0].ID != eventID.String() || fList[0].InvitationNote == nil {
		t.Fatalf("founder invite-only list: %+v %v", fList, err)
	}
	// The plain donor sees nothing.
	pList, err := reader.InviteOnlyEvents(ctx, plainTok)
	if err != nil || len(pList) != 0 {
		t.Fatalf("plain donor saw %d invite-only events: %v", len(pList), err)
	}
	// The plain donor's gated detail is not found.
	if _, err := reader.DetailForDonor(ctx, plainTok, eventID); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("plain donor saw invite-only detail: %v", err)
	}

	// Add the plain donor to the guest list -> they can now see it (no note,
	// since it is a guest-list grant, not a perk).
	if err := events.AddGuest(ctx, adminToken, eventID, plain); err != nil {
		t.Fatal(err)
	}
	gList, err := reader.InviteOnlyEvents(ctx, plainTok)
	if err != nil || len(gList) != 1 || gList[0].ID != eventID.String() || gList[0].InvitationNote != nil {
		t.Fatalf("guest invite-only list: %+v %v", gList, err)
	}

	// Removing the guest revokes visibility again.
	if err := events.RemoveGuest(ctx, adminToken, eventID, plain); err != nil {
		t.Fatal(err)
	}
	after, err := reader.InviteOnlyEvents(ctx, plainTok)
	if err != nil || len(after) != 0 {
		t.Fatalf("removed guest still saw %d events: %v", len(after), err)
	}
}

// TestEventCheckIn covers E-8: a valid QR token during the event window checks
// in a reserved donor and lights up the volunteer badges; a forged token, a
// closed window, and a missing reservation are each refused; a second scan is a
// no-op success.
func TestEventCheckIn(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	ctx := context.Background()
	cleanEvents(t, pool)
	_, adminToken := staffFixture(t, pool, "SUPER_ADMIN")
	events := EventService{Staff: &StaffService{DB: api}}
	const secret = "test-checkin-secret"
	res := ReservationService{DB: api, CheckInSecret: secret}

	// An event happening right now (started an hour ago, ends in an hour).
	in := eventInput("beach-cleanup", "San Francisco", "94103", "VOLUNTEER", -1*time.Hour)
	in.EndsAt = time.Now().UTC().Add(1 * time.Hour).Format(time.RFC3339)
	eventID, err := events.Create(ctx, adminToken, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Publish(ctx, adminToken, eventID); err != nil {
		t.Fatal(err)
	}
	token := checkInToken(eventID, secret)

	donorID, donorTok := signedInDonor(t, pool)
	t.Cleanup(func() { cleanExec(t, "DELETE FROM donor_badges WHERE donor_id=$1", donorID) })

	// No reservation yet -> refused.
	if _, err := res.CheckIn(ctx, donorTok, token); !errors.Is(err, ErrCheckIn) {
		t.Fatalf("check-in without reservation accepted: %v", err)
	}
	// A waitlist place is not a seat -> refused.
	waitlisted, waitlistedTok := signedInDonor(t, pool)
	mustExec(
		t,
		pool,
		"INSERT INTO donor_event_reservations (id, event_id, donor_id, status, reserved_at, created_at, updated_at) VALUES (gen_random_uuid(),$1,$2,'WAITLISTED',now(),now(),now())",
		eventID,
		waitlisted,
	)
	if _, err := res.CheckIn(ctx, waitlistedTok, token); !errors.Is(err, ErrCheckIn) {
		t.Fatalf("waitlisted donor checked in: %v", err)
	}
	// Seed a reservation directly: the donor reserved before the event started
	// (Reserve itself is correctly closed once an event has begun, E-9).
	if _, err := pool.Exec(ctx,
		"INSERT INTO donor_event_reservations (id, event_id, donor_id, status, reserved_at, created_at, updated_at) VALUES (gen_random_uuid(),$1,$2,'RESERVED',now(),now(),now())",
		eventID, donorID,
	); err != nil {
		t.Fatal(err)
	}
	// A forged token is refused.
	if _, err := res.CheckIn(ctx, donorTok, token+"x"); !errors.Is(err, ErrCheckIn) {
		t.Fatalf("forged token accepted: %v", err)
	}
	// A token of the wrong length is refused even with a valid prefix.
	if _, err := res.CheckIn(ctx, donorTok, token[:len(token)-1]); !errors.Is(err, ErrCheckIn) {
		t.Fatalf("truncated token accepted: %v", err)
	}
	if len(token) != checkInTokenLength {
		t.Fatalf("token is %d characters, documented as %d", len(token), checkInTokenLength)
	}
	reader := EventReader{DB: pool}
	if v, err := reader.DetailForDonor(ctx, donorTok, eventID); err != nil || v.CheckedInAt != nil ||
		v.ReservationStatus == nil || *v.ReservationStatus != reservationReserved {
		t.Fatalf("before check-in: %+v %v", v, err)
	}
	// Valid check-in.
	r, err := res.CheckIn(ctx, donorTok, token)
	if err != nil || !r.CheckedIn || r.AlreadyIn {
		t.Fatalf("check-in: %+v %v", r, err)
	}
	// A later read shows the check-in, on the detail and on the list rows.
	v, err := reader.DetailForDonor(ctx, donorTok, eventID)
	if err != nil || v.CheckedInAt == nil || v.ReservationStatus == nil || *v.ReservationStatus != reservationReserved {
		t.Fatalf("after check-in: %+v %v", v, err)
	}
	if views, err := reader.donorViews(ctx, queries.New(pool), donorID, []queries.DonorEvent{mustEvent(t, pool, eventID)}); err != nil ||
		len(views) != 1 || views[0].CheckedInAt == nil || !views[0].CheckedInAt.Equal(*v.CheckedInAt) {
		t.Fatalf("list row after check-in: %+v %v", views, err)
	}
	// Second scan is an idempotent no-op success.
	r, err = res.CheckIn(ctx, donorTok, token)
	if err != nil || !r.CheckedIn || !r.AlreadyIn {
		t.Fatalf("second check-in: %+v %v", r, err)
	}

	// Volunteer stats reflect the check-in (one event, 120 minutes).
	var evCount, minutes int64
	if err := pool.QueryRow(ctx,
		"SELECT count(*), coalesce(sum(EXTRACT(EPOCH FROM (e.ends_at-e.starts_at))/60),0)::bigint FROM donor_event_reservations r JOIN donor_events e ON e.id=r.event_id WHERE r.donor_id=$1 AND r.checked_in_at IS NOT NULL",
		donorID,
	).Scan(&evCount, &minutes); err != nil || evCount != 1 || minutes != 120 {
		t.Fatalf("volunteer stats: events=%d minutes=%d %v", evCount, minutes, err)
	}

	// A check-in token for a FUTURE event (outside its window) is refused.
	future := eventInput("future", "San Francisco", "94103", "VOLUNTEER", 48*time.Hour)
	futureID, err := events.Create(ctx, adminToken, future)
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Publish(ctx, adminToken, futureID); err != nil {
		t.Fatal(err)
	}
	if _, err := res.Reserve(ctx, donorTok, futureID); err != nil {
		t.Fatal(err)
	}
	if _, err := res.CheckIn(ctx, donorTok, checkInToken(futureID, secret)); !errors.Is(err, ErrCheckIn) {
		t.Fatalf("check-in before window accepted: %v", err)
	}
}

// TestCheckInDisabledWithoutSecret covers the unconfigured case.
func TestCheckInDisabledWithoutSecret(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	res := ReservationService{DB: pool} // no secret
	_, tok := signedInDonor(t, pool)
	if _, err := res.CheckIn(ctx, tok, "anything"); !errors.Is(err, ErrCheckInDisabled) {
		t.Fatalf("check-in without a configured secret: %v", err)
	}
}

// TestEventReminderSweep covers E-7: the 24h and 2h sweeps queue one reminder
// per reserved donor per window, deduped so repeated sweeps do not re-queue,
// and they do not touch events outside the window or cancelled reservations.
func TestEventReminderSweep(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	ctx := context.Background()
	cleanEvents(t, pool)
	_, adminToken := staffFixture(t, pool, "SUPER_ADMIN")
	events := EventService{Staff: &StaffService{DB: api}}
	res := ReservationService{DB: pool}
	rem := EventReminderService{DB: pool}

	// Event in ~90 minutes (inside the 2h window, inside the 24h window).
	soon := eventInput("soon", "San Francisco", "94103", "VOLUNTEER", 90*time.Minute)
	soonID, err := events.Create(ctx, adminToken, soon)
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Publish(ctx, adminToken, soonID); err != nil {
		t.Fatal(err)
	}
	// Event in ~10 days (outside both windows).
	far := eventInput("far", "San Francisco", "94103", "VOLUNTEER", 240*time.Hour)
	farID, err := events.Create(ctx, adminToken, far)
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Publish(ctx, adminToken, farID); err != nil {
		t.Fatal(err)
	}

	reserver, reserverTok := signedInDonor(t, pool)
	pushCleanupDonor(t, pool, reserver)
	if _, err := res.Reserve(ctx, reserverTok, soonID); err != nil {
		t.Fatal(err)
	}
	if _, err := res.Reserve(ctx, reserverTok, farID); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	n, err := rem.QueueDueReminders(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	// The soon event is in both the 24h and the 2h window, so two reminders.
	if n != 2 {
		t.Fatalf("first sweep queued %d reminders, want 2 (24h + 2h for the soon event)", n)
	}
	// A second sweep is idempotent — the dedup keys already exist.
	if again, err := rem.QueueDueReminders(ctx, now); err != nil || again != 0 {
		t.Fatalf("second sweep queued %d (want 0): %v", again, err)
	}
	// Only EVENT_REMINDER rows for the soon event, none for the far one.
	var soonCount, farCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_push_notifications WHERE category='EVENT_REMINDER' AND context->>'event_id'=$1", soonID.String()).Scan(&soonCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_push_notifications WHERE category='EVENT_REMINDER' AND context->>'event_id'=$1", farID.String()).Scan(&farCount); err != nil {
		t.Fatal(err)
	}
	if soonCount != 2 || farCount != 0 {
		t.Fatalf("reminder rows: soon=%d far=%d, want 2 and 0", soonCount, farCount)
	}
}

// TestEventICSAndReportIssue covers E-6/E-10: a published event renders a valid
// VCALENDAR with escaped fields, invite-only/draft events are not downloadable
// anonymously, and the report-issue mailto is present when a support email is
// configured.
func TestEventICSAndReportIssue(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	ctx := context.Background()
	cleanEvents(t, pool)
	_, adminToken := staffFixture(t, pool, "SUPER_ADMIN")
	events := EventService{Staff: &StaffService{DB: api}}
	reader := EventReader{DB: pool, SupportEmail: "help@dad.example"}

	in := eventInput("gala", "San Francisco", "94103", "COMMUNITY", 48*time.Hour)
	in.Description = "Food, drinks; and fun. " + strings.Repeat("Bring a friend — café tables outside. ", 6)
	eventID, err := events.Create(ctx, adminToken, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Publish(ctx, adminToken, eventID); err != nil {
		t.Fatal(err)
	}

	ics, err := reader.Calendar(ctx, eventID)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSuffix(ics, "\r\n"), "\r\n") {
		if len(line) > 75 || !utf8.ValidString(line) {
			t.Fatalf("ics line is %d octets or splits a character: %q", len(line), line)
		}
	}
	unfolded := strings.ReplaceAll(ics, "\r\n ", "")
	for _, want := range []string{"BEGIN:VCALENDAR", "BEGIN:VEVENT", "UID:" + eventID.String() + "@dad", "SUMMARY:Event gala", "DTSTART:", "DTEND:",
		"DESCRIPTION:Food\\, drinks\\; and fun. Bring a friend — café tables outside.", "END:VCALENDAR"} {
		if !strings.Contains(unfolded, want) {
			t.Fatalf("ics missing %q:\n%s", want, ics)
		}
	}

	// report_issue_url present on the detail when a support email is set.
	v, err := reader.Detail(ctx, eventID)
	if err != nil || v.ReportIssueURL == nil || !strings.HasPrefix(*v.ReportIssueURL, "mailto:help@dad.example") {
		t.Fatalf("report issue url: %+v %v", v.ReportIssueURL, err)
	}

	// An invite-only event is not downloadable anonymously.
	inv := eventInput("secret", "San Francisco", "94103", "INVITE_ONLY", 48*time.Hour)
	invID, err := events.Create(ctx, adminToken, inv)
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Publish(ctx, adminToken, invID); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Calendar(ctx, invID); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("invite-only ics was downloadable: %v", err)
	}
	donor, token := signedInDonor(t, pool)
	if _, err := reader.CalendarForDonor(ctx, "invalid", invID); !errors.Is(err, ErrDenied) {
		t.Fatalf("calendar accepted invalid session: %v", err)
	}
	if _, err := reader.CalendarForDonor(ctx, token, invID); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("calendar leaked private event: %v", err)
	}
	if err := events.AddGuest(ctx, adminToken, invID, donor); err != nil {
		t.Fatal(err)
	}
	if ics, err := reader.CalendarForDonor(ctx, token, invID); err != nil || !strings.Contains(ics, "SUMMARY:Event secret") {
		t.Fatalf("guest calendar: %q %v", ics, err)
	}
	if err := events.RemoveGuest(ctx, adminToken, invID, donor); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.CalendarForDonor(ctx, token, invID); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("removed guest downloaded calendar: %v", err)
	}
	if err := events.AddGuest(ctx, adminToken, invID, donor); err != nil {
		t.Fatal(err)
	}
	if err := events.Cancel(ctx, adminToken, invID); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.CalendarForDonor(ctx, token, invID); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("cancelled calendar downloaded: %v", err)
	}

	// With no support email, report_issue_url is nil.
	plain := EventReader{DB: pool}
	if v, err := plain.Detail(ctx, eventID); err != nil || v.ReportIssueURL != nil {
		t.Fatalf("report url set without support email: %+v %v", v.ReportIssueURL, err)
	}
}

// TestPublishAlertsNearbyVolunteers covers E-1: publishing tells volunteers in
// or near the event's city once, and the alert stops if they opt out or the
// event is withdrawn before it is sent.
func TestPublishAlertsNearbyVolunteers(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	cleanEvents(t, pool)
	_, adminToken := staffFixture(t, pool, "SUPER_ADMIN")
	s := EventService{Staff: &StaffService{DB: as(t, dbroles.API)}}

	resident := func(city, zip string, volunteer, nearby bool) pgtype.UUID {
		id, _ := donor(t, pool)
		t.Cleanup(func() {
			cleanExec(t, "DELETE FROM donor_push_notifications WHERE donor_id=$1", id)
			cleanExec(t, "DELETE FROM donor_preferences WHERE donor_id=$1", id)
		})
		mustExec(t, pool, "UPDATE donors SET city=$2, zip=$3 WHERE id=$1", id, city, zip)
		mustExec(
			t,
			pool,
			"INSERT INTO donor_preferences(id,donor_id,version,referral_emails,marketing_emails,volunteer,push_event_nearby) VALUES($1,$2,1,false,false,$3,$4)",
			testID(t),
			id,
			volunteer,
			nearby,
		)
		return id
	}
	sameCity := resident("San Francisco", "90001", true, true)
	sameRegion := resident("Daly City", "94105", true, true)
	optedOut := resident("San Francisco", "94103", true, false)
	notVolunteer := resident("San Francisco", "94103", false, true)
	farAway := resident("Chicago", "60601", true, true)

	alerts := func(donor pgtype.UUID) []queries.DonorPushNotification {
		var found []queries.DonorPushNotification
		for _, intent := range pushIntents(t, pool, donor) {
			if intent.Category == eventNearbyCategory {
				found = append(found, intent)
			}
		}
		return found
	}

	id, err := s.Create(ctx, adminToken, eventInput("park-cleanup", "San Francisco", "94103", "VOLUNTEER", 48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Publish(ctx, adminToken, id); err != nil {
		t.Fatal(err)
	}
	if err := s.Publish(ctx, adminToken, id); err != nil {
		t.Fatal(err)
	}
	if got := len(alerts(sameCity)); got != 0 {
		t.Fatalf("publishing performed inline fanout: %d", got)
	}
	if worked, err := (EventAlertService{DB: as(t, dbroles.ProjectionWorker)}).QueueNext(ctx, time.Now().UTC()); err != nil || !worked {
		t.Fatalf("alert fanout: worked=%t %v", worked, err)
	}
	for _, c := range []struct {
		name  string
		donor pgtype.UUID
		want  int
	}{
		{"same city", sameCity, 1}, {"same ZIP region", sameRegion, 1}, {"opted out", optedOut, 0},
		{"not a volunteer", notVolunteer, 0}, {"far away", farAway, 0},
	} {
		if got := len(alerts(c.donor)); got != c.want {
			t.Errorf("%s: %d alerts; want %d", c.name, got, c.want)
		}
	}

	invite, err := s.Create(ctx, adminToken, eventInput("donor-dinner", "San Francisco", "94103", "INVITE_ONLY", 48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Publish(ctx, adminToken, invite); err != nil {
		t.Fatal(err)
	}
	if got := len(alerts(sameCity)); got != 1 {
		t.Fatalf("an invite-only event alerted volunteers: %d alerts", got)
	}

	job := alerts(sameCity)[0]
	reason := func() string {
		t.Helper()
		donorRow, err := queries.New(pool).LockDonor(ctx, sameCity)
		if err != nil {
			t.Fatal(err)
		}
		reason, err := pushSuppression(ctx, queries.New(pool), job, donorRow, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		return reason
	}
	if got := reason(); got != "" {
		t.Fatalf("a fresh alert was suppressed as %q", got)
	}
	mustExec(t, pool, "UPDATE donor_events SET cancelled_at=now() WHERE id=$1", id)
	if got := reason(); got != "event_withdrawn" {
		t.Fatalf("an alert for a cancelled event: %q; want event_withdrawn", got)
	}
	mustExec(t, pool, "UPDATE donor_preferences SET push_event_nearby=false WHERE donor_id=$1", sameCity)
	if got := reason(); got != "opted_out" {
		t.Fatalf("an alert after opting out: %q; want opted_out", got)
	}
}

// TestTeamCaptainsBookEarly covers B-3: before booking opens, only a Team
// Captain can reserve, and only from 48 hours before it.
func TestTeamCaptainsBookEarly(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	ctx := context.Background()
	cleanEvents(t, pool)
	_, adminToken := staffFixture(t, pool, "SUPER_ADMIN")
	events := EventService{Staff: &StaffService{DB: api}}
	reader := EventReader{DB: pool}
	res := ReservationService{DB: api}
	captain, captainToken := signedInDonor(t, pool)
	mustExec(t, pool, "INSERT INTO donor_badges (donor_id, code, source, earned_at) VALUES ($1,'TC','AUTO',now())", captain)
	t.Cleanup(func() { cleanExec(t, "DELETE FROM donor_badges WHERE donor_id=$1", captain) })
	_, plainToken := signedInDonor(t, pool)

	publish := func(slug string, opensIn time.Duration) pgtype.UUID {
		t.Helper()
		in := eventInput(slug, "Reno", "89501", "VOLUNTEER", 10*24*time.Hour)
		in.BookingOpensAt = time.Now().UTC().Add(opensIn).Format(time.RFC3339)
		id, err := events.Create(ctx, adminToken, in)
		if err != nil {
			t.Fatal(err)
		}
		if err := events.Publish(ctx, adminToken, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	soon := publish("captains-first", 24*time.Hour)
	later := publish("opens-next-week", 5*24*time.Hour)
	open := publish("already-open", -time.Hour)

	if _, err := res.Reserve(ctx, plainToken, soon); !errors.Is(err, ErrBookingNotOpen) {
		t.Fatalf("a donor reserved before booking opened: %v", err)
	}
	if _, err := res.Reserve(ctx, captainToken, later); !errors.Is(err, ErrBookingNotOpen) {
		t.Fatalf("a captain reserved more than 48 hours early: %v", err)
	}
	view, err := reader.DetailForDonor(ctx, captainToken, soon)
	if err != nil || !view.EarlyBooking || view.BookingOpensAt == nil {
		t.Fatalf("captain's view inside the window: early=%v opens=%v err=%v", view.EarlyBooking, view.BookingOpensAt, err)
	}
	if got, err := res.Reserve(ctx, captainToken, soon); err != nil || got.Status != reservationReserved {
		t.Fatalf("captain inside the 48-hour window: %+v %v", got, err)
	}
	if view, err = reader.DetailForDonor(ctx, plainToken, soon); err != nil || view.EarlyBooking {
		t.Fatalf("a non-captain was offered early booking: %v %v", view.EarlyBooking, err)
	}
	if got, err := res.Reserve(ctx, plainToken, open); err != nil || got.Status != reservationReserved {
		t.Fatalf("anyone after booking opened: %+v %v", got, err)
	}

	backwards := eventInput("opens-after-start", "Reno", "89501", "VOLUNTEER", 24*time.Hour)
	backwards.BookingOpensAt = time.Now().UTC().Add(48 * time.Hour).Format(time.RFC3339)
	if _, err := events.Create(ctx, adminToken, backwards); !errors.Is(err, ErrStaffInput) {
		t.Fatalf("booking opening after the start: %v; want ErrStaffInput", err)
	}
}

func mustEvent(t *testing.T, pool *pgxpool.Pool, id pgtype.UUID) queries.DonorEvent {
	t.Helper()
	e, err := queries.New(pool).EventByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestEventTimeZone(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	ctx := context.Background()
	cleanEvents(t, pool)
	_, adminToken := staffFixture(t, pool, "SUPER_ADMIN")
	s := EventService{Staff: &StaffService{DB: api}}
	reader := EventReader{DB: pool}

	for name, in := range map[string]EventInput{
		"unknown zone": func() EventInput {
			i := eventInput("tz-a", "SF", "94103", "VOLUNTEER", 48*time.Hour)
			i.TimeZone = "Mars/Olympus"
			return i
		}(),
		"local": func() EventInput {
			i := eventInput("tz-b", "SF", "94103", "VOLUNTEER", 48*time.Hour)
			i.TimeZone = "Local"
			return i
		}(),
		"injection": func() EventInput {
			i := eventInput("tz-c", "SF", "94103", "VOLUNTEER", 48*time.Hour)
			i.TimeZone = "../etc/passwd"
			return i
		}(),
		"offset string": func() EventInput {
			i := eventInput("tz-d", "SF", "94103", "VOLUNTEER", 48*time.Hour)
			i.TimeZone = "UTC-5 "
			return i
		}(),
	} {
		if _, err := s.Create(ctx, adminToken, in); !errors.Is(err, ErrStaffInput) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}

	want := map[string]string{"explicit": "Pacific/Honolulu", "from-zip": "America/Los_Angeles", "fallback": defaultEventTimeZone}
	for slug, in := range map[string]EventInput{
		"explicit": func() EventInput {
			i := eventInput("explicit", "Honolulu", "96813", "VOLUNTEER", 48*time.Hour)
			i.TimeZone = "Pacific/Honolulu"
			return i
		}(),
		"from-zip": eventInput("from-zip", "SF", "94103", "VOLUNTEER", 48*time.Hour),
		"fallback": eventInput("fallback", "Nowhere", "", "VOLUNTEER", 48*time.Hour),
	} {
		id, err := s.Create(ctx, adminToken, in)
		if err != nil {
			t.Fatalf("%s: %v", slug, err)
		}
		if err = s.Publish(ctx, adminToken, id); err != nil {
			t.Fatal(err)
		}
		view, err := reader.Detail(ctx, id)
		if err != nil || view.TimeZone != want[slug] {
			t.Fatalf("%s: zone %q, want %q (%v)", slug, view.TimeZone, want[slug], err)
		}
		staff, err := s.Detail(ctx, adminToken, id)
		if err != nil || staff.TimeZone != want[slug] {
			t.Fatalf("%s staff detail: zone %q (%v)", slug, staff.TimeZone, err)
		}
		// Updating with the zone the staff screen echoed back keeps it.
		in2 := staff.EventInput
		if err = s.Update(ctx, adminToken, id, in2); err != nil {
			t.Fatalf("%s update: %v", slug, err)
		}
		in2.TimeZone = "America/Chicago"
		if err = s.Update(ctx, adminToken, id, in2); err != nil {
			t.Fatalf("%s update zone: %v", slug, err)
		}
		if view, err = reader.Detail(ctx, id); err != nil || view.TimeZone != "America/Chicago" {
			t.Fatalf("%s after update: %q %v", slug, view.TimeZone, err)
		}
	}
}
