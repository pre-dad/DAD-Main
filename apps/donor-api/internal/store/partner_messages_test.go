package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/mail"
	"github.com/jackc/pgx/v5/pgxpool"
)

func partnerMessageWorker(t *testing.T, transport *recordingMail) PartnerMessageWorker {
	t.Helper()
	return PartnerMessageWorker{
		DB:        as(t, dbroles.MessageWorker),
		Settings:  mail.Settings{Host: "smtp.fixture", Port: 587, Sender: "sender@example.org", EncryptionKeys: emailTestKeys},
		Origin:    "https://donor.example.org",
		Transport: transport,
	}
}

// queuePartnerIntent enqueues a partner message intent directly, as the API role
// would from an approval / metric-publish / completion path.
func queuePartnerIntent(t *testing.T, partnerID interface{ String() string }, kind, key string) {
	t.Helper()
	pool := as(t, dbroles.API)
	mustExec(t, pool,
		`INSERT INTO donor_partner_messages (id, partner_id, kind, dedup_key, context, available_at)
		 VALUES ($1,$2,$3,$4,'{}',now()) ON CONFLICT (dedup_key) DO NOTHING`,
		mustNewID(t), partnerID, kind, key)
	t.Cleanup(func() {
		cleanExec(t, "DELETE FROM donor_partner_messages WHERE dedup_key=$1", key)
	})
}

// TestPartnerMessageMatrixDeliversEachTriggerOnce walks the partner side of the
// §6.11 matrix: each status trigger renders once, fans out to every accepted
// org user, and the intent's dedup key makes it idempotent.
func TestPartnerMessageMatrixDeliversEachTriggerOnce(t *testing.T) {
	ctx := context.Background()
	pool := as(t, dbroles.API)
	// The delivery worker drains the globally-oldest due intent, so clear any
	// left by other tests first — this test asserts on its own partner's fan-out.
	mustExec(t, database(t), "DELETE FROM donor_partner_messages")
	partnerID := approvedPartner(t, pool, "Matrix Org", "MATRIX-"+mustNewID(t).String()[:8], 0, 0)
	// Two accepted admins so we can assert fan-out, plus one disabled and one
	// pending (unaccepted) who must NOT receive the notice.
	approvePartnerAdmin(t, pool, partnerID)
	approvePartnerAdmin(t, pool, partnerID)
	mustExec(t, pool,
		`INSERT INTO donor_partner_users (id, partner_id, email, name, role, created_at)
		 VALUES ($1,$2,'pending@x.test','P','VIEWER',now())`, mustNewID(t), partnerID)
	mustExec(t, pool,
		`INSERT INTO donor_partner_users (id, partner_id, email, name, role, accepted_at, disabled, created_at)
		 VALUES ($1,$2,'off@x.test','D','VIEWER',now(),true,now())`, mustNewID(t), partnerID)

	for _, kind := range []string{"METRICS_VERIFIED", "INITIATIVE_COMPLETE", "MONTHLY_REPORT"} {
		key := "matrix:" + kind + ":" + partnerID.String()
		queuePartnerIntent(t, partnerID, kind, key)
		transport := &recordingMail{}
		w := partnerMessageWorker(t, transport)
		now := time.Now().UTC().Add(time.Second)
		if steps := drainPartnerMessages(t, w, now); steps != 3 {
			t.Fatalf("%s: expected one fan-out and two sends, got %d steps", kind, steps)
		}
		// Fan-out: exactly the two accepted admins, never the pending or disabled.
		if got := len(transport.messages); got != 2 {
			t.Fatalf("%s: expected 2 recipients, got %d", kind, got)
		}
		for _, m := range transport.messages {
			if m.To == "pending@x.test" || m.To == "off@x.test" {
				t.Fatalf("%s: delivered to non-recipient %s", kind, m.To)
			}
			if m.Subject == "" || m.Body == "" {
				t.Fatalf("%s: empty subject/body", kind)
			}
		}
		// Idempotent: the intent is completed, so a second drain finds nothing
		// and re-queuing the same key is a no-op.
		if steps := drainPartnerMessages(t, w, now.Add(time.Minute)); steps != 0 {
			t.Fatalf("%s: redelivered a completed intent in %d steps", kind, steps)
		}
		queuePartnerIntent(t, partnerID, kind, key)
		var pending int
		if err := pool.QueryRow(ctx,
			"SELECT count(*) FROM donor_partner_messages WHERE dedup_key=$1 AND completed_at IS NULL", key).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if pending != 0 {
			t.Fatalf("%s: dedup key re-queued a live intent", kind)
		}
	}
}

// A failed send is retried after the same backoff as donor mail, and the retry
// carries the message it failed with: the body is rendered once and the
// Message-ID is stable, so a lost acknowledgement cannot send two different
// emails.
func TestPartnerMessageRetriesWithBackoffAfterDeliveryFailure(t *testing.T) {
	pool := as(t, dbroles.API)
	mustExec(t, database(t), "DELETE FROM donor_partner_messages")
	partnerID := approvedPartner(t, pool, "Retry Org", "RETRY-"+mustNewID(t).String()[:8], 0, 0)
	approvePartnerAdmin(t, pool, partnerID)
	key := "retry:METRICS_VERIFIED:" + partnerID.String()
	queuePartnerIntent(t, partnerID, "METRICS_VERIFIED", key)
	transport := &recordingMail{failure: errors.New("connection reset")}
	w := partnerMessageWorker(t, transport)
	now := time.Now().UTC().Add(time.Second)
	if steps := drainPartnerMessages(t, w, now); steps != 2 {
		t.Fatalf("first attempt took %d steps, want a fan-out and a send", steps)
	}
	attempts, lastError, completed := partnerDeliveryState(t, key)
	if attempts != 1 || lastError != "DeliveryError" || completed {
		t.Fatalf("after a failed send: attempts=%d error=%q completed=%v", attempts, lastError, completed)
	}
	transport.failure = nil
	if steps := drainPartnerMessages(t, w, now.Add(time.Minute)); steps != 0 {
		t.Fatalf("retried before its backoff in %d steps", steps)
	}
	if steps := drainPartnerMessages(t, w, now.Add(3*time.Minute)); steps != 1 {
		t.Fatalf("retry took %d steps", steps)
	}
	if _, _, completed = partnerDeliveryState(t, key); !completed {
		t.Fatal("a successful retry did not complete the delivery")
	}
	if len(transport.messages) != 2 || transport.messages[0] != transport.messages[1] {
		t.Fatalf("retry changed the message: %+v", transport.messages)
	}
}

// TestPartnerMessageSuppressedWithoutRecipients completes an intent as suppressed
// when the org has no accepted user to mail, rather than retrying forever.
func TestPartnerMessageSuppressedWithoutRecipients(t *testing.T) {
	ctx := context.Background()
	pool := as(t, dbroles.API)
	mustExec(t, database(t), "DELETE FROM donor_partner_messages")
	partnerID := approvedPartner(t, pool, "Empty Org", "EMPTY-"+mustNewID(t).String()[:8], 0, 0)
	key := "matrix:MONTHLY_REPORT:empty:" + partnerID.String()
	queuePartnerIntent(t, partnerID, "MONTHLY_REPORT", key)
	transport := &recordingMail{}
	w := partnerMessageWorker(t, transport)
	if steps := drainPartnerMessages(t, w, time.Now().UTC()); steps != 1 {
		t.Fatalf("expected one fan-out step, got %d", steps)
	}
	if len(transport.messages) != 0 {
		t.Fatalf("sent %d messages with no recipients", len(transport.messages))
	}
	var reason string
	if err := pool.QueryRow(ctx,
		"SELECT coalesce(suppressed_reason,'') FROM donor_partner_messages WHERE dedup_key=$1", key).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != "no_recipients" {
		t.Fatalf("expected no_recipients suppression, got %q", reason)
	}
}

// TestPartnerMonthlySweepQueuesOncePerMonth checks the monthly sweep enqueues a
// report for an approved partner with a published initiative, exactly once per
// calendar month (the dedup key carries the period).
func TestPartnerMonthlySweepQueuesOncePerMonth(t *testing.T) {
	ctx := context.Background()
	pool := as(t, dbroles.API)
	partnerID := approvedPartner(t, pool, "Monthly Org", "MONTHLY-"+mustNewID(t).String()[:8], 0, 0)
	// A published initiative makes the partner eligible for the monthly report.
	mustExec(
		t,
		pool,
		`INSERT INTO donor_initiatives (id, slug, title, summary, type, location, region, started_on, status, publish_state, published_at, partner_signup_id, created_at, updated_at)
		 VALUES ($1,$2,'T','S','PROGRAM','Statewide','MA',now(),'ACTIVE','PUBLISHED',now(),$3,now(),now())`,
		mustNewID(t),
		"mon-"+partnerID.String()[:8],
		partnerID,
	)
	t.Cleanup(func() {
		if _, err := database(t).Exec(ctx, "DELETE FROM donor_initiatives WHERE partner_signup_id=$1", partnerID); err != nil {
			t.Error(err)
		}
	})

	sweep := PartnerMonthlyService{DB: as(t, dbroles.ProjectionWorker)}
	// A sweep step is bounded, so drain it before reading the partner's count.
	drain := func(at time.Time) int {
		t.Helper()
		total := 0
		for {
			n, err := sweep.QueueDueMonthly(ctx, at)
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				return total
			}
			total += n
		}
	}
	now := time.Now().UTC()
	if n := drain(now); n < 1 {
		t.Fatalf("first sweep queued %d reports", n)
	}
	if queued := partnerMonthlyCount(t, pool, partnerID); queued != 1 {
		t.Fatalf("first sweep queued %d monthly reports for the partner, want 1", queued)
	}
	// A second sweep in the same month queues nothing more for this partner.
	drain(now.Add(time.Hour))
	if after := partnerMonthlyCount(t, pool, partnerID); after != 1 {
		t.Fatalf("monthly re-queued within the same month: 1 -> %d", after)
	}
	// The next month is due again.
	drain(now.AddDate(0, 1, 0))
	if next := partnerMonthlyCount(t, pool, partnerID); next != 2 {
		t.Fatalf("the following month queued %d reports in total, want 2", next)
	}
	t.Cleanup(func() {
		cleanExec(t, "DELETE FROM donor_partner_messages WHERE partner_id=$1", partnerID)
	})
}

func partnerMonthlyCount(t *testing.T, pool *pgxpool.Pool, partnerID interface{ String() string }) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM donor_partner_messages WHERE partner_id=$1 AND kind='MONTHLY_REPORT'", partnerID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
