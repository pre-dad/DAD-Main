package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"dad/donor-api/internal/credentials"
	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/mail"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5/pgtype"
)

var emailTestKeys, _ = credentials.ParseKeyring("MDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDA=")

type recordingMail struct {
	mu       sync.Mutex
	messages []mail.Message
	failure  error
	check    func(mail.Message)
}

func (r *recordingMail) Send(_ context.Context, m mail.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.check != nil {
		r.check(m)
	}
	r.messages = append(r.messages, m)
	return r.failure
}

func emailFixture(t *testing.T) (LoginEmailWorker, pgtype.UUID, *recordingMail, time.Time) {
	t.Helper()
	pool := database(t)
	id, email := donor(t, pool)
	accountCleanup(t, pool, id)
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "DELETE FROM donor_login_emails WHERE donor_id=$1", id); err != nil {
			t.Error(err)
		}
	})
	now := time.Now().UTC()
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = queueLoginEmail(context.Background(), queries.New(tx), email, "", now); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	transport := &recordingMail{}
	return LoginEmailWorker{
		DB:        as(t, dbroles.LoginEmailWorker),
		Settings:  mail.Settings{Host: "smtp.fixture", Port: 587, Sender: "sender@example.org", EncryptionKeys: emailTestKeys},
		Origin:    "https://donor.example.org",
		Transport: transport,
	}, id, transport, now
}

func tokenFromMail(t *testing.T, m mail.Message) string {
	t.Helper()
	_, tail, ok := strings.Cut(m.Body, "#token=")
	if !ok {
		t.Fatal("missing claim token")
	}
	token, _, _ := strings.Cut(tail, "\n")
	return token
}

// rotatedTestKeys prepends a new primary to emailTestKeys, as a key rotation does.
func rotatedTestKeys(t *testing.T) credentials.Keyring {
	t.Helper()
	return keyring(t, "MTExMTExMTExMTExMTExMTExMTExMTExMTExMTExMTE=,MDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDA=")
}

func TestLoginLinkPreparedBeforeAKeyRotationIsStillSent(t *testing.T) {
	w, id, transport, now := emailFixture(t)
	pool := database(t)
	ctx := context.Background()
	if _, _, err := w.prepare(ctx, now); err != nil {
		t.Fatal(err)
	}
	w.Settings.EncryptionKeys = rotatedTestKeys(t)
	if worked, err := w.DeliverNext(ctx, now); err != nil || !worked || len(transport.messages) != 1 {
		t.Fatal("link prepared under the previous key was not sent", worked, err)
	}
	link, err := queries.New(pool).LoginLink(ctx, HashToken(tokenFromMail(t, transport.messages[0])))
	if err != nil || link.DonorID != id {
		t.Fatal("sent link does not sign in", err)
	}
}

func TestLoginEmailCommitsProofBeforeSendAndReusesItOnRetry(t *testing.T) {
	w, id, transport, now := emailFixture(t)
	pool := database(t)
	ctx := context.Background()
	transport.failure = errors.New("lost acknowledgement with private server content")
	transport.check = func(m mail.Message) {
		token := tokenFromMail(t, m)
		link, err := queries.New(pool).LoginLink(ctx, HashToken(token))
		if err != nil || link.DonorID != id {
			t.Fatal("uncommitted credential sent", err)
		}
	}
	if worked, err := w.DeliverNext(ctx, now); err != nil || !worked {
		t.Fatal(worked, err)
	}
	var encrypted []byte
	var attempts int
	var last string
	if err := pool.QueryRow(ctx, "SELECT encrypted_token,attempts,last_error FROM donor_login_emails WHERE donor_id=$1", id).Scan(&encrypted, &attempts, &last); err != nil {
		t.Fatal(err)
	}
	token := tokenFromMail(t, transport.messages[0])
	clear, err := emailTestKeys.Decrypt(encrypted)
	if err != nil || string(clear) != token || strings.Contains(string(encrypted), token) || attempts != 1 || last != "DeliveryError" {
		t.Fatal("unsafe retry state", err)
	}
	if worked, err := w.DeliverNext(ctx, now.Add(30*time.Second)); err != nil || worked {
		t.Fatal("retry fired before availability", worked, err)
	}
	transport.failure = nil
	if worked, err := w.DeliverNext(ctx, now.Add(time.Minute)); err != nil || !worked {
		t.Fatal(worked, err)
	}
	if len(transport.messages) != 2 || transport.messages[0] != transport.messages[1] {
		t.Fatal("retry changed link or message identity")
	}
	var complete bool
	if err := pool.QueryRow(ctx, "SELECT completed_at IS NOT NULL,encrypted_token FROM donor_login_emails WHERE donor_id=$1", id).Scan(&complete, &encrypted); err != nil ||
		!complete ||
		encrypted != nil {
		t.Fatal("completed job retained credential", err)
	}
	api := AccountService{DB: as(t, dbroles.API)}
	session, err := api.ClaimAccount(ctx, token, "", Client{})
	if err != nil || session.Access == "" {
		t.Fatal("delivered proof failed claim", err)
	}
	if _, err = api.ClaimAccount(ctx, token, "", Client{}); !errors.Is(err, ErrDenied) {
		t.Fatal("proof replay allowed", err)
	}
}

func TestLoginEmailConcurrentWorkersSendOnce(t *testing.T) {
	w, _, transport, now := emailFixture(t)
	ctx := context.Background()
	start := make(chan struct{})
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { <-start; _, err := w.DeliverNext(ctx, now); done <- err }()
	}
	close(start)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if len(transport.messages) != 1 {
		t.Fatalf("deliveries=%d", len(transport.messages))
	}
}

func TestLoginEmailSuppressesDisabledExpiredAndParksFailures(t *testing.T) {
	for _, mode := range []string{"disabled", "expired", "five failures", "bad key"} {
		t.Run(mode, func(t *testing.T) {
			w, id, transport, now := emailFixture(t)
			pool := database(t)
			ctx := context.Background()
			switch mode {
			case "disabled":
				if _, err := pool.Exec(ctx, "UPDATE donors SET disabled=true WHERE id=$1", id); err != nil {
					t.Fatal(err)
				}
			case "expired":
				if _, _, err := w.prepare(ctx, now.Add(-16*time.Minute)); err != nil {
					t.Fatal(err)
				}
				// Preparation selects only available work, so explicitly prepare at queue time
				// and advance beyond the original lifetime without changing its token.
				if _, _, err := w.prepare(ctx, now); err != nil {
					t.Fatal(err)
				}
				now = now.Add(16 * time.Minute)
			case "five failures":
				transport.failure = errors.New("SMTP failed")
			case "bad key":
				w.Settings.EncryptionKeys = credentials.Keyring{}
			}
			if mode == "bad key" {
				if _, err := w.DeliverNext(ctx, now); err == nil {
					t.Fatal("bad key accepted")
				}
				return
			}
			count := 1
			if mode == "five failures" {
				count = 5
			}
			for i := 0; i < count; i++ {
				if worked, err := w.DeliverNext(ctx, now.Add(time.Duration(i)*time.Minute)); err != nil || !worked {
					t.Fatal(worked, err)
				}
			}
			if worked, err := w.DeliverNext(ctx, now.Add(10*time.Minute)); err != nil || worked {
				t.Fatal("terminal job retried", worked, err)
			}
			expected := 0
			if mode == "five failures" {
				expected = 5
			}
			if len(transport.messages) != expected {
				t.Fatal("unexpected delivery", len(transport.messages))
			}
		})
	}
}

// blockingMail holds every send open until released.
type blockingMail struct {
	started chan mail.Message
	release chan struct{}
}

func (b *blockingMail) Send(ctx context.Context, m mail.Message) error {
	b.started <- m
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestSlowSendReleasesDonorLockAndKeepsJobClaimed(t *testing.T) {
	queued := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC) // Isolates this queue from other tests' due work.
	settings := mail.Settings{Host: "smtp.fixture", Port: 587, Sender: "sender@example.org", EncryptionKeys: emailTestKeys}
	for _, kind := range []string{"login", "message"} {
		t.Run(kind, func(t *testing.T) {
			pool := database(t)
			ctx := context.Background()
			id, email := donor(t, pool)
			accountCleanup(t, pool, id)
			transport := &blockingMail{started: make(chan mail.Message, 2), release: make(chan struct{})}
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			var deliver func() (bool, error)
			completed := "SELECT completed_at IS NOT NULL FROM donor_login_emails WHERE donor_id=$1"
			if kind == "login" {
				err = queueLoginEmail(ctx, queries.New(tx), email, "", queued)
				w := LoginEmailWorker{
					DB:        as(t, dbroles.LoginEmailWorker),
					Settings:  settings,
					Origin:    "https://donor.example.org",
					Transport: transport,
				}
				deliver = func() (bool, error) { return w.DeliverNext(ctx, queued) }
			} else {
				referred := testID(t)
				t.Cleanup(func() {
					for _, c := range []struct {
						sql string
						id  pgtype.UUID
					}{{"DELETE FROM donor_preferences WHERE donor_id=$1", id}, {"DELETE FROM donor_metric_projections WHERE donor_id=$1", referred}, {"DELETE FROM donors WHERE id=$1", referred}} {
						if _, err := pool.Exec(context.Background(), c.sql, c.id); err != nil {
							t.Error(err)
						}
					}
				})
				mustExec(t, tx, "INSERT INTO donors(id,email) VALUES($1,$2)", referred, referred.String()+"@example.org")
				mustExec(t, tx, "INSERT INTO donor_preferences(id,donor_id,version,referral_emails,marketing_emails) VALUES($1,$2,1,true,false)", testID(t), id)
				mustExec(t, tx, "INSERT INTO donor_metric_projections(id,donor_id,referrer_id,referral_kind,net_minor,computed_at,next_refresh_at) VALUES($1,$2,$3,'LINK',3000,$4,$4)", testID(t), referred, id, queued)
				err = queueMessage(ctx, queries.New(tx), id, "REFERRAL", "referral:"+referred.String(), map[string]any{"referred_id": referred.String(), "kind": "LINK"}, queued)
				w := MessageWorker{DB: as(t, dbroles.MessageWorker), Settings: settings, Origin: "https://donor.example.org", Transport: transport}
				deliver = func() (bool, error) { return w.DeliverNext(ctx, queued) }
				completed = "SELECT completed_at IS NOT NULL FROM donor_messages WHERE donor_id=$1"
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, err := deliver(); done <- err }()
			var sent mail.Message
			select {
			case sent = <-transport.started:
			case <-time.After(10 * time.Second):
				t.Fatal("send did not start")
			}
			lock, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			mustExec(t, lock, "SET LOCAL lock_timeout='2s'")
			if _, err = queries.New(lock).LockDonor(ctx, id); err != nil {
				t.Fatal("send held the donor lock", err)
			}
			if err = lock.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if worked, err := deliver(); err != nil || worked {
				t.Fatal("claimed job was available to another worker", worked, err)
			}
			close(transport.release)
			if err = <-done; err != nil {
				t.Fatal(err)
			}
			var finished bool
			if err = pool.QueryRow(ctx, completed, id).Scan(&finished); err != nil || !finished || len(transport.started) != 0 ||
				sent.To != email {
				t.Fatal("send not completed exactly once", err)
			}
		})
	}
}

func TestLoginLinkReturnsOnlyToAllowedPages(t *testing.T) {
	for _, c := range []struct{ next, want string }{
		{"/account/payment-method", "&next=/account/payment-method\n"},
		{"https://elsewhere.example/account", "#token="},
		{"", "#token="},
	} {
		t.Run(c.next, func(t *testing.T) {
			w, id, transport, _ := emailFixture(t)
			pool := database(t)
			ctx := context.Background()
			if _, err := pool.Exec(ctx, "DELETE FROM donor_login_emails WHERE donor_id=$1", id); err != nil {
				t.Fatal(err)
			}
			var email string
			if err := pool.QueryRow(ctx, "SELECT email FROM donors WHERE id=$1", id).Scan(&email); err != nil {
				t.Fatal(err)
			}
			if err := (AccountService{DB: as(t, dbroles.API)}).QueueLogin(ctx, email, c.next); err != nil {
				t.Fatal(err)
			}
			if worked, err := w.DeliverNext(ctx, time.Now().Add(time.Second)); err != nil || !worked {
				t.Fatal(worked, err)
			}
			body := transport.messages[0].Body
			if !strings.Contains(body, c.want) || (c.want == "#token=" && strings.Contains(body, "&next=")) {
				t.Fatalf("link for next %q: got %q, want %q", c.next, body, c.want)
			}
		})
	}
}

func TestLoginLinksAreLimitedPerDonorPerHour(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	id, email := donor(t, pool)
	accountCleanup(t, pool, id)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	start := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	queued := func() int {
		var n int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM donor_login_emails WHERE donor_id=$1", id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for minute := range 12 {
		if err = queueLoginEmail(ctx, queries.New(tx), email, "", start.Add(time.Duration(minute)*2*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if n := queued(); n != 5 {
		t.Fatalf("%d links queued within an hour, want 5", n)
	}
	if err = queueLoginEmail(ctx, queries.New(tx), email, "", start.Add(time.Hour+time.Second)); err != nil || queued() != 6 {
		t.Fatal("a link was refused once the earliest left the hour", err)
	}
}
