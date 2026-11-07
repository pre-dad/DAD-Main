package store

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
)

func TestCheckInSerializesWithEventChanges(t *testing.T) {
	for _, manual := range []bool{false, true} {
		name := "QR"
		if manual {
			name = "manual"
		}
		t.Run(name, func(t *testing.T) {
			pool := database(t)
			api := as(t, dbroles.API)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cleanEvents(t, pool)
			_, staff := staffFixture(t, pool, "SUPER_ADMIN")
			events := EventService{Staff: &StaffService{DB: api}}
			eventID, err := events.Create(ctx, staff, eventInput("concurrent-checkin", "SF", "94103", "VOLUNTEER", -time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if err := events.Publish(ctx, staff, eventID); err != nil {
				t.Fatal(err)
			}
			donorID, token := signedInDonor(t, pool)
			t.Cleanup(func() { cleanExec(t, "DELETE FROM donor_badges WHERE donor_id=$1", donorID) })
			mustExec(t, pool, `INSERT INTO donor_event_reservations(id,event_id,donor_id,status,reserved_at,created_at,updated_at)
				VALUES(gen_random_uuid(),$1,$2,'RESERVED',now(),now(),now())`, eventID, donorID)
			lock, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Rollback(ctx)
			mustExec(t, lock, "UPDATE donor_events SET cancelled_at=now() WHERE id=$1", eventID)
			result := make(chan error, 1)
			go func() {
				if manual {
					result <- events.CheckInAttendee(ctx, staff, eventID, donorID)
					return
				}
				_, err := (ReservationService{DB: api, CheckInSecret: "fixture"}).CheckIn(ctx, token, checkInToken(eventID, "fixture"))
				result <- err
			}()
			for {
				select {
				case err := <-result:
					t.Fatalf("check-in bypassed the pending event change: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				default:
				}
				var waiting bool
				if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
					WHERE datname=current_database() AND cardinality(pg_blocking_pids(pid))>0)`).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if err := lock.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			err = <-result
			if !errors.Is(err, ErrCheckIn) && !(manual && errors.Is(err, ErrEventCancelled)) {
				t.Fatalf("cancelled event check-in = %v", err)
			}
			var attended bool
			if err := pool.QueryRow(ctx, "SELECT checked_in_at IS NOT NULL FROM donor_event_reservations WHERE event_id=$1 AND donor_id=$2", eventID, donorID).Scan(&attended); err != nil ||
				attended {
				t.Fatalf("cancelled event recorded attendance=%t: %v", attended, err)
			}
		})
	}
}

func TestJoinTeamDuringConcurrentCreation(t *testing.T) {
	pool := database(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	partner := approvedPartner(t, pool, "Team Race", "TEAM-RACE", 0, 0)
	donor, token := signedInDonor(t, pool)
	mustExec(t, pool, "INSERT INTO donor_partner_employees(donor_id,partner_id,enrolled_at) VALUES($1,$2,now())", donor, partner)
	lock, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(ctx)
	team := mustNewID(t)
	mustExec(t, lock, "INSERT INTO donor_partner_teams(id,partner_id,name,created_at) VALUES($1,$2,'Community',now())", team, partner)
	type outcome struct {
		id  string
		err error
	}
	result := make(chan outcome, 1)
	api := as(t, dbroles.API)
	go func() {
		id, err := (EmployeeService{DB: api}).JoinTeam(ctx, token, "Community")
		result <- outcome{id, err}
	}()
	awaitPartnerWaiters(t, pool, 1)
	if err := lock.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got := <-result
	if got.err != nil || got.id != team.String() {
		t.Fatalf("concurrent team join = %q, %v; want %s", got.id, got.err, team.String())
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_partner_teams WHERE partner_id=$1", partner).Scan(&count); err != nil ||
		count != 1 {
		t.Fatalf("team count=%d: %v", count, err)
	}
}

func TestReservationRechecksStartAfterEventLock(t *testing.T) {
	for _, cancelReservation := range []bool{false, true} {
		name := "reserve"
		if cancelReservation {
			name = "cancel"
		}
		t.Run(name, func(t *testing.T) {
			pool := database(t)
			api := as(t, dbroles.API)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cleanEvents(t, pool)
			_, staff := staffFixture(t, pool, "SUPER_ADMIN")
			events := EventService{Staff: &StaffService{DB: api}}
			eventID, err := events.Create(ctx, staff, eventInput("booking-deadline", "SF", "94103", "VOLUNTEER", time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if err := events.Publish(ctx, staff, eventID); err != nil {
				t.Fatal(err)
			}
			donorID, token := signedInDonor(t, pool)
			reservations := ReservationService{DB: api}
			if cancelReservation {
				if _, err := reservations.Reserve(ctx, token, eventID); err != nil {
					t.Fatal(err)
				}
			}
			lock, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Rollback(ctx)
			mustExec(t, lock, "SELECT id FROM donor_events WHERE id=$1 FOR UPDATE", eventID)
			result := make(chan error, 1)
			go func() {
				var err error
				if cancelReservation {
					_, err = reservations.Cancel(ctx, token, eventID)
				} else {
					_, err = reservations.Reserve(ctx, token, eventID)
				}
				result <- err
			}()
			awaitPartnerWaiters(t, pool, 1)
			mustExec(t, lock, "UPDATE donor_events SET starts_at=clock_timestamp() WHERE id=$1", eventID)
			if err := lock.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-result; !errors.Is(err, ErrReservationClosed) {
				t.Fatalf("%s crossed the event start while blocked: %v", name, err)
			}
			var active int
			if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_event_reservations WHERE event_id=$1 AND donor_id=$2 AND status='RESERVED'", eventID, donorID).Scan(&active); err != nil {
				t.Fatal(err)
			}
			want := 0
			if cancelReservation {
				want = 1
			}
			if active != want {
				t.Fatalf("%s changed a closed reservation: %d seats, want %d", name, active, want)
			}
		})
	}
}

func TestConcurrentReservationsNeverOversellAndCancellationsRefillSeats(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cleanEvents(t, pool)
	_, staff := staffFixture(t, pool, "SUPER_ADMIN")
	events := EventService{Staff: &StaffService{DB: api}}
	in := eventInput("oversell", "SF", "94103", "COMMUNITY", 48*time.Hour)
	const seats, donors = 3, 12
	in.Capacity = seats
	eventID, err := events.Create(ctx, staff, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Publish(ctx, staff, eventID); err != nil {
		t.Fatal(err)
	}
	tokens := make([]string, donors)
	for i := range tokens {
		id, token := signedInDonor(t, pool)
		pushCleanupDonor(t, pool, id)
		tokens[i] = token
	}
	reservations := ReservationService{DB: api}
	run := func(action func(token string) error) {
		t.Helper()
		var wg sync.WaitGroup
		errs := make(chan error, donors)
		for _, token := range tokens {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := action(token); err != nil {
					errs <- err
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
	}
	counts := func() (reserved, waitlisted int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE status='RESERVED'), count(*) FILTER(WHERE status='WAITLISTED')
			FROM donor_event_reservations WHERE event_id=$1`, eventID).Scan(&reserved, &waitlisted); err != nil {
			t.Fatal(err)
		}
		return reserved, waitlisted
	}

	run(func(token string) error {
		_, err := reservations.Reserve(ctx, token, eventID)
		return err
	})
	if reserved, waitlisted := counts(); reserved != seats || waitlisted != donors-seats {
		t.Fatalf(
			"after %d concurrent reservations: %d reserved, %d waitlisted; want %d and %d",
			donors,
			reserved,
			waitlisted,
			seats,
			donors-seats,
		)
	}

	var holders []string
	rows, err := pool.Query(ctx, `SELECT s.token_hash FROM donor_event_reservations r JOIN donor_sessions s ON s.donor_id=r.donor_id
		WHERE r.event_id=$1 AND r.status='RESERVED'`, eventID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			t.Fatal(err)
		}
		holders = append(holders, hash)
	}
	rows.Close()
	if len(holders) != seats {
		t.Fatalf("seat holders with sessions = %d, want %d", len(holders), seats)
	}
	var wg sync.WaitGroup
	errs := make(chan error, donors)
	for _, token := range tokens {
		if !slices.Contains(holders, HashToken(token)) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := reservations.Cancel(ctx, token, eventID); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if reserved, waitlisted := counts(); reserved != seats || waitlisted != donors-2*seats {
		t.Fatalf("after concurrent cancellations: %d reserved, %d waitlisted; want %d and %d", reserved, waitlisted, seats, donors-2*seats)
	}
}
