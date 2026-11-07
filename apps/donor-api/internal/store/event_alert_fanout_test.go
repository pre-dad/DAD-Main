package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestEventAlertsResumeInBoundedBatchesAndStopAfterCancellation(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	cleanEvents(t, pool)
	_, token := staffFixture(t, pool, "SUPER_ADMIN")
	events := EventService{Staff: &StaffService{DB: as(t, dbroles.API)}}
	id, err := events.Create(ctx, token, eventInput("bounded-alerts", "Fanout City", "", "COMMUNITY", 48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for range EventAlertBatch + 3 {
		d, _ := donor(t, pool)
		t.Cleanup(func() {
			cleanExec(t, "DELETE FROM donor_push_notifications WHERE donor_id=$1", d)
			cleanExec(t, "DELETE FROM donor_preferences WHERE donor_id=$1", d)
		})
		mustExec(t, pool, "UPDATE donors SET city='Fanout City' WHERE id=$1", d)
		mustExec(t, pool, `INSERT INTO donor_preferences(id,donor_id,version,referral_emails,marketing_emails,volunteer,push_event_nearby)
			VALUES($1,$2,1,false,false,true,true)`, testID(t), d)
	}
	if err := events.Publish(ctx, token, id); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	step := func() bool {
		t.Helper()
		worked, err := (EventAlertService{DB: as(t, dbroles.ProjectionWorker)}).QueueNext(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		return worked
	}
	assertProgress := func(want int, complete bool) {
		t.Helper()
		var count int
		var done bool
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM donor_push_notifications WHERE context->>'event_id'=$1`, id.String()).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SELECT completed_at IS NOT NULL FROM donor_event_alert_jobs WHERE event_id=$1`, id).Scan(&done); err != nil {
			t.Fatal(err)
		}
		if count != want || done != complete {
			t.Fatalf("alerts=%d complete=%t, want %d %t", count, done, want, complete)
		}
	}
	if !step() {
		t.Fatal("first batch was not claimed")
	}
	assertProgress(EventAlertBatch, false)
	if !step() {
		t.Fatal("restarted worker did not resume")
	}
	assertProgress(EventAlertBatch+3, true)
	if err := events.Publish(ctx, token, id); err != nil {
		t.Fatal(err)
	}
	if step() {
		t.Fatal("republishing restarted a completed fanout")
	}
	assertProgress(EventAlertBatch+3, true)

	// A cancelled event must end a partially completed job without queueing more.
	mustExec(t, pool, "UPDATE donor_event_alert_jobs SET last_donor_id=NULL,completed_at=NULL WHERE event_id=$1", id)
	mustExec(t, pool, "UPDATE donor_events SET cancelled_at=now() WHERE id=$1", id)
	if !step() {
		t.Fatal("cancelled job was not retired")
	}
	assertProgress(EventAlertBatch+3, true)
}

func TestEventAlertBatchesPageEachAudienceInDonorOrder(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	cleanEvents(t, pool)
	_, token := staffFixture(t, pool, "SUPER_ADMIN")
	events := EventService{Staff: &StaffService{DB: as(t, dbroles.API)}}
	// Neither the city nor the ZIP prefix may match a donor from another test.
	city := "Paging " + testID(t).String()
	id, err := events.Create(ctx, token, eventInput("audience-paging", city, "99950", "COMMUNITY", 48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Publish(ctx, token, id); err != nil {
		t.Fatal(err)
	}
	audience := func(city, zip string, volunteer, alerts bool) pgtype.UUID {
		d, _ := donor(t, pool)
		t.Cleanup(func() {
			cleanExec(t, "DELETE FROM donor_push_notifications WHERE donor_id=$1", d)
			cleanExec(t, "DELETE FROM donor_preferences WHERE donor_id=$1", d)
		})
		mustExec(t, pool, "UPDATE donors SET city=$2,zip=$3 WHERE id=$1", d, city, zip)
		mustExec(t, pool, `INSERT INTO donor_preferences(id,donor_id,version,referral_emails,marketing_emails,volunteer,push_event_nearby)
			VALUES($1,$2,1,false,false,$3,$4)`, testID(t), d, volunteer, alerts)
		return d
	}
	want := map[pgtype.UUID]bool{
		audience(strings.ToLower(city), "10001", true, true): true,
		audience("Elsewhere", "99951", true, true):           true,
		audience(strings.ToUpper(city), "99903", true, true): true,
		audience(city, "99950", true, true):                  true,
		audience(city, "99950", true, true):                  true,
	}
	audience(city, "99950", false, true)
	audience(city, "99950", true, false)
	audience("Elsewhere", "10001", true, true)
	disabled := audience(city, "99950", true, true)
	mustExec(t, pool, "UPDATE donors SET disabled=true WHERE id=$1", disabled)

	q := queries.New(pool)
	var after pgtype.UUID
	var last, got []pgtype.UUID
	for range 10 {
		batch, err := q.QueueEventAlertBatch(ctx, queries.QueueEventAlertBatchParams{
			EventID: id, AfterDonor: after, AsOf: timestamp(time.Now()), BatchSize: 2,
		})
		if err != nil {
			t.Fatal(err)
		}
		if batch.Queued != batch.Considered {
			t.Fatalf("queued %d of %d considered donors", batch.Queued, batch.Considered)
		}
		if batch.Considered == 0 {
			break
		}
		last = append(last, batch.LastDonor)
		after = batch.LastDonor
		rows, err := pool.Query(ctx, `SELECT donor_id FROM donor_push_notifications WHERE context->>'event_id'=$1`, id.String())
		if err != nil {
			t.Fatal(err)
		}
		if got, err = pgx.CollectRows(rows, pgx.RowTo[pgtype.UUID]); err != nil {
			t.Fatal(err)
		}
		if batch.Considered < 2 {
			break
		}
	}
	if len(last) != 3 {
		t.Fatalf("%d batches of two covered five donors, want 3", len(last))
	}
	if len(got) != len(want) {
		t.Fatalf("queued %d alerts, want %d", len(got), len(want))
	}
	for _, d := range got {
		if !want[d] {
			t.Fatalf("alert queued for donor %s outside the audience", d)
		}
	}
}

func TestEventAlertsAreQueuedOnlyForOpenPublishedEvents(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	cleanEvents(t, pool)
	_, token := staffFixture(t, pool, "SUPER_ADMIN")
	events := EventService{Staff: &StaffService{DB: as(t, dbroles.API)}}
	city := "Gated " + testID(t).String()
	d, _ := donor(t, pool)
	t.Cleanup(func() {
		cleanExec(t, "DELETE FROM donor_push_notifications WHERE donor_id=$1", d)
		cleanExec(t, "DELETE FROM donor_preferences WHERE donor_id=$1", d)
	})
	mustExec(t, pool, "UPDATE donors SET city=$2 WHERE id=$1", d, city)
	mustExec(t, pool, `INSERT INTO donor_preferences(id,donor_id,version,referral_emails,marketing_emails,volunteer,push_event_nearby)
		VALUES($1,$2,1,false,false,true,true)`, testID(t), d)
	jobs := func() (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_event_alert_jobs").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, err := events.Create(ctx, token, eventInput("gated-draft", city, "", "COMMUNITY", 48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	invite, err := events.Create(ctx, token, eventInput("gated-invite", city, "", "INVITE_ONLY", 48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err = events.Publish(ctx, token, invite); err != nil {
		t.Fatal(err)
	}
	if jobs() != 0 {
		t.Fatalf("%d alert jobs for a draft and an invite-only event", jobs())
	}
	worker := EventAlertService{DB: as(t, dbroles.ProjectionWorker)}
	if worked, err := worker.QueueNext(ctx, time.Now().UTC()); err != nil || worked {
		t.Fatalf("a worker found alert work with no eligible event: %v %v", worked, err)
	}

	open, err := events.Create(ctx, token, eventInput("gated-open", city, "", "COMMUNITY", 48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err = events.Publish(ctx, token, open); err != nil {
		t.Fatal(err)
	}
	// The event starts before the worker reaches it: nobody is told of a past event.
	if worked, err := worker.QueueNext(ctx, time.Now().UTC().Add(72*time.Hour)); err != nil || !worked {
		t.Fatalf("the lapsed job was not retired: %v %v", worked, err)
	}
	var queued int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM donor_push_notifications WHERE donor_id=$1", d).Scan(&queued); err != nil ||
		queued != 0 {
		t.Fatalf("%d alerts queued for an event already started: %v", queued, err)
	}
	var done bool
	if err = pool.QueryRow(ctx, "SELECT completed_at IS NOT NULL FROM donor_event_alert_jobs WHERE event_id=$1", open).Scan(&done); err != nil ||
		!done {
		t.Fatal("lapsed job left to be picked up again", err)
	}
}
