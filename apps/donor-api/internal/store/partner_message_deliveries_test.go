package store

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/mail"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5/pgtype"
)

// drainPartnerMessages runs worker steps until none finds work and returns how
// many did. The bound turns a step that never finishes into a failure.
func drainPartnerMessages(t *testing.T, w PartnerMessageWorker, now time.Time) int {
	t.Helper()
	steps := 0
	for {
		worked, err := w.DeliverNext(context.Background(), now)
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			return steps
		}
		if steps++; steps > 10000 {
			t.Fatal("partner mail did not drain")
		}
	}
}

// partnerDeliveryState reads the only delivery of the message queued under key.
func partnerDeliveryState(t *testing.T, key string) (int32, string, bool) {
	t.Helper()
	var attempts int32
	var lastError string
	var completed bool
	if err := database(t).QueryRow(context.Background(),
		`SELECT d.attempts,coalesce(d.last_error,''),d.completed_at IS NOT NULL
		 FROM donor_partner_message_deliveries d JOIN donor_partner_messages m ON m.id=d.message_id
		 WHERE m.dedup_key=$1`, key).Scan(&attempts, &lastError, &completed); err != nil {
		t.Fatal(err)
	}
	return attempts, lastError, completed
}

// partnerOrg returns an approved partner with n accepted users and a queued
// METRICS_VERIFIED intent, after clearing the shared outbox.
func partnerOrg(t *testing.T, n int) (pgtype.UUID, string) {
	t.Helper()
	mustExec(t, database(t), "DELETE FROM donor_partner_messages")
	partnerID := approvedPartner(t, as(t, dbroles.API), "Outbox Org", "OUTBOX-"+mustNewID(t).String()[:8], 0, 0)
	mustExec(t, database(t),
		`INSERT INTO donor_partner_users (id, partner_id, email, name, role, accepted_at, created_at)
		 SELECT gen_random_uuid(), $1, 'user-' || g || '-' || $2::text || '@x.test', 'U', 'VIEWER', now(), now()
		 FROM generate_series(1, $3::int) g`, partnerID, partnerID.String(), n)
	t.Cleanup(func() { cleanExec(t, "DELETE FROM donor_partner_users WHERE partner_id=$1", partnerID) })
	key := "outbox:METRICS_VERIFIED:" + partnerID.String()
	queuePartnerIntent(t, partnerID, "METRICS_VERIFIED", key)
	return partnerID, key
}

type selectiveMail struct {
	mu     sync.Mutex
	failed map[string]bool
	sent   map[string]int
	ids    map[string]string
}

func newSelectiveMail() *selectiveMail {
	return &selectiveMail{failed: map[string]bool{}, sent: map[string]int{}, ids: map[string]string{}}
}

func (s *selectiveMail) Send(_ context.Context, m mail.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed[m.To] {
		return errors.New("rejected")
	}
	s.sent[m.To]++
	s.ids[m.To] = m.ID
	return nil
}

func TestPartnerMessageFansOutOneDeliveryPerRecipient(t *testing.T) {
	_, key := partnerOrg(t, 3)
	w := partnerMessageWorker(t, &recordingMail{})
	now := time.Now().UTC().Add(time.Second)
	if worked, err := w.fanOut(context.Background(), "https://donor.example.org", now); err != nil || !worked {
		t.Fatalf("fan-out: worked=%v err=%v", worked, err)
	}
	var deliveries, ordinals int
	var fannedOut, completed bool
	if err := database(t).QueryRow(context.Background(),
		`SELECT count(d.id), count(DISTINCT d.ordinal), bool_and(m.fanned_out_at IS NOT NULL), bool_and(m.completed_at IS NOT NULL)
		 FROM donor_partner_messages m JOIN donor_partner_message_deliveries d ON d.message_id=m.id WHERE m.dedup_key=$1`,
		key).Scan(&deliveries, &ordinals, &fannedOut, &completed); err != nil {
		t.Fatal(err)
	}
	if deliveries != 3 || ordinals != 3 || !fannedOut || !completed {
		t.Fatalf("deliveries=%d ordinals=%d fannedOut=%v completed=%v", deliveries, ordinals, fannedOut, completed)
	}
}

func TestPartnerMessageFailureForOneRecipientDoesNotResendToOthers(t *testing.T) {
	_, key := partnerOrg(t, 3)
	var emails []string
	rows, err := database(t).Query(context.Background(),
		"SELECT u.email FROM donor_partner_messages m JOIN donor_partner_users u ON u.partner_id=m.partner_id WHERE m.dedup_key=$1 ORDER BY u.email", key)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var e string
		if err = rows.Scan(&e); err != nil {
			t.Fatal(err)
		}
		emails = append(emails, e)
	}
	rows.Close()
	transport := newSelectiveMail()
	transport.failed[emails[1]] = true
	w := partnerMessageWorker(t, nil)
	w.Transport = transport
	now := time.Now().UTC().Add(time.Second)
	drainPartnerMessages(t, w, now)
	transport.failed[emails[1]] = false
	drainPartnerMessages(t, w, now.Add(3*time.Minute))
	for _, e := range emails {
		if transport.sent[e] != 1 {
			t.Fatalf("%s received %d copies, want exactly 1", e, transport.sent[e])
		}
	}
	if transport.ids[emails[0]] == transport.ids[emails[1]] {
		t.Fatal("recipients share a Message-ID")
	}
}

func TestPartnerMessageCountsAttemptAtClaimAndParksPoison(t *testing.T) {
	_, key := partnerOrg(t, 1)
	w := partnerMessageWorker(t, &recordingMail{})
	now := time.Now().UTC().Add(time.Second)
	if worked, err := w.fanOut(context.Background(), "https://donor.example.org", now); err != nil || !worked {
		t.Fatalf("fan-out: worked=%v err=%v", worked, err)
	}
	q := queries.New(w.DB)
	// A worker that dies after every claim never records a failure: the claim
	// alone must count the attempt and park the delivery after the cap.
	for i := range maxDeliveryAttempts {
		at := now.Add(time.Duration(i) * (deliveryLease + time.Minute))
		if _, err := q.NextPartnerDelivery(context.Background(), queries.NextPartnerDeliveryParams{
			AsOf: timestamp(at), LeaseUntil: timestamp(at.Add(deliveryLease)),
		}); err != nil {
			t.Fatalf("claim %d: %v", i+1, err)
		}
	}
	transport := &recordingMail{}
	w.Transport = transport
	if steps := drainPartnerMessages(t, w, now.Add(24*time.Hour)); steps != 0 || len(transport.messages) != 0 {
		t.Fatalf("a parked delivery was sent: steps=%d sent=%d", steps, len(transport.messages))
	}
	if attempts, _, completed := partnerDeliveryState(t, key); attempts != maxDeliveryAttempts || completed {
		t.Fatalf("attempts=%d completed=%v, want parked at %d", attempts, completed, maxDeliveryAttempts)
	}
}

func TestPartnerMessageRetriesAfterLeaseExpiry(t *testing.T) {
	_, key := partnerOrg(t, 1)
	w := partnerMessageWorker(t, &recordingMail{})
	now := time.Now().UTC().Add(time.Second)
	if worked, err := w.fanOut(context.Background(), "https://donor.example.org", now); err != nil || !worked {
		t.Fatalf("fan-out: worked=%v err=%v", worked, err)
	}
	if _, err := queries.New(w.DB).NextPartnerDelivery(context.Background(), queries.NextPartnerDeliveryParams{
		AsOf: timestamp(now), LeaseUntil: timestamp(now.Add(deliveryLease)),
	}); err != nil {
		t.Fatal(err)
	}
	if steps := drainPartnerMessages(t, w, now.Add(deliveryLease-time.Second)); steps != 0 {
		t.Fatal("a delivery was resent while its lease was held")
	}
	if steps := drainPartnerMessages(t, w, now.Add(deliveryLease+time.Second)); steps != 1 {
		t.Fatalf("expired lease took %d steps, want 1", steps)
	}
	if attempts, _, completed := partnerDeliveryState(t, key); attempts != 2 || !completed {
		t.Fatalf("attempts=%d completed=%v", attempts, completed)
	}
}

func TestPartnerMessageConcurrentWorkersDeliverEachRecipientOnce(t *testing.T) {
	_, _ = partnerOrg(t, 40)
	transport := newSelectiveMail()
	now := time.Now().UTC().Add(time.Second)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		w := partnerMessageWorker(t, nil)
		w.Transport = transport
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				worked, err := w.DeliverNext(context.Background(), now)
				if err != nil {
					errs <- err
					return
				}
				if !worked {
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if len(transport.sent) != 40 {
		t.Fatalf("delivered to %d recipients, want 40", len(transport.sent))
	}
	for e, n := range transport.sent {
		if n != 1 {
			t.Fatalf("%s received %d copies", e, n)
		}
	}
}

func TestPartnerMessageLargeOrganisationDrainsInBoundedSteps(t *testing.T) {
	const recipients = 2000
	_, _ = partnerOrg(t, recipients)
	transport := newSelectiveMail()
	w := partnerMessageWorker(t, nil)
	w.Transport = transport
	started := time.Now()
	steps := drainPartnerMessages(t, w, time.Now().UTC().Add(time.Second))
	if steps != recipients+1 {
		t.Fatalf("steps=%d, want one fan-out plus one send per recipient (%d)", steps, recipients+1)
	}
	if len(transport.sent) != recipients {
		t.Fatalf("delivered to %d recipients, want %d", len(transport.sent), recipients)
	}
	// Each step is its own short transaction, so no single step holds a lease
	// for the whole organisation however large it is.
	if elapsed := time.Since(started); elapsed > deliveryLease {
		t.Fatalf("draining %d recipients took %s, longer than one lease", recipients, elapsed)
	}
}

// Intents queued before deliveries existed carry a rendered body and attempts
// but no delivery rows; the worker expands and sends them once, and expanding
// again adds nothing.
func TestPartnerMessageDrainsIntentsQueuedUnderTheOldShape(t *testing.T) {
	partnerID, key := partnerOrg(t, 2)
	mustExec(t, database(t),
		"UPDATE donor_partner_messages SET subject='Old', body='Old body', attempts=2 WHERE dedup_key=$1", key)
	transport := newSelectiveMail()
	w := partnerMessageWorker(t, nil)
	w.Transport = transport
	now := time.Now().UTC().Add(time.Second)
	if steps := drainPartnerMessages(t, w, now); steps != 3 {
		t.Fatalf("steps=%d, want a fan-out and two sends", steps)
	}
	if len(transport.sent) != 2 {
		t.Fatalf("delivered to %d recipients, want 2", len(transport.sent))
	}
	var id pgtype.UUID
	if err := database(t).QueryRow(context.Background(), "SELECT id FROM donor_partner_messages WHERE dedup_key=$1", key).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if again, err := queries.New(database(t)).FanOutPartnerMessage(context.Background(), queries.FanOutPartnerMessageParams{
		MessageID: id, PartnerID: partnerID, AvailableAt: timestamp(now),
	}); err != nil || again != 0 {
		t.Fatalf("repeat expansion added %d deliveries (err=%v)", again, err)
	}
}

func TestPartnerMessageSkipsRecipientDisabledAfterFanOut(t *testing.T) {
	partnerID, key := partnerOrg(t, 2)
	w := partnerMessageWorker(t, nil)
	transport := newSelectiveMail()
	w.Transport = transport
	now := time.Now().UTC().Add(time.Second)
	if worked, err := w.fanOut(context.Background(), "https://donor.example.org", now); err != nil || !worked {
		t.Fatalf("fan-out: worked=%v err=%v", worked, err)
	}
	mustExec(
		t,
		database(t),
		"UPDATE donor_partner_users SET disabled=true WHERE id=(SELECT id FROM donor_partner_users WHERE partner_id=$1 ORDER BY email LIMIT 1)",
		partnerID,
	)
	drainPartnerMessages(t, w, now)
	var suppressed int
	if err := database(t).QueryRow(context.Background(),
		`SELECT count(*) FROM donor_partner_message_deliveries d JOIN donor_partner_messages m ON m.id=d.message_id
		 WHERE m.dedup_key=$1 AND d.suppressed_reason='recipient_inactive'`, key).Scan(&suppressed); err != nil {
		t.Fatal(err)
	}
	if len(transport.sent) != 1 || suppressed != 1 {
		t.Fatalf("sent=%d suppressed=%d, want 1 and 1", len(transport.sent), suppressed)
	}
}

func TestQueueHealthCountsPartnerDeliveries(t *testing.T) {
	ctx := context.Background()
	tx, err := database(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// An isolated historical cutoff excludes other tests' committed queue rows.
	now := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	partner, message := testID(t), testID(t)
	mustExec(
		t,
		tx,
		`INSERT INTO donor_partner_signups (id, organization, name, email, website, message, request_hash, consent_version, approved_at, company_code)
		 VALUES ($1,'Monitor Org','C','c@x.test','https://x.test','',$2,'v1',$3,$4)`,
		partner,
		"h-"+partner.String(),
		now,
		"MON-"+partner.String()[:8],
	)
	mustExec(t, tx,
		`INSERT INTO donor_partner_messages (id, partner_id, kind, dedup_key, context, available_at, completed_at)
		 VALUES ($1,$2,'METRICS_VERIFIED',$3,'{}',$4,$4)`, message, partner, message.String(), now)
	for i, delivery := range []struct {
		due      time.Duration
		attempts int
		done     bool
	}{{-time.Hour, 0, false}, {-time.Minute, 5, false}, {time.Hour, 0, false}, {-time.Hour, 1, true}} {
		user := testID(t)
		mustExec(t, tx,
			`INSERT INTO donor_partner_users (id, partner_id, email, name, role, accepted_at, created_at)
			 VALUES ($1,$2,$3,'U','VIEWER',$4,$4)`, user, partner, "monitor-"+strconv.Itoa(i)+"-"+user.String()+"@x.test", now)
		mustExec(t, tx,
			`INSERT INTO donor_partner_message_deliveries (id, message_id, partner_user_id, ordinal, available_at, attempts, completed_at)
			 VALUES ($1,$2,$3,$4,$5,$6,CASE WHEN $7 THEN $5::timestamptz ELSE NULL END)`,
			testID(t), message, user, i, now.Add(delivery.due), delivery.attempts, delivery.done)
	}
	actAs(t, tx, dbroles.Monitor)
	rows, err := queries.New(tx).QueueHealth(ctx, timestamp(now))
	if err != nil {
		t.Fatal(err)
	}
	if row := queueHealthRow(t, rows, "messages"); row.Due != 2 || row.Parked != 1 || row.OldestSeconds != 3600 {
		t.Fatalf("messages: %+v, want 2 due, 1 parked, oldest 3600s", row)
	}
}
