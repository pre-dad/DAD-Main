package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/push"
	"dad/donor-api/internal/recognition"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	iosToken     = strings.Repeat("a1", 32)
	androidToken = "fcm-install:APA91b" + strings.Repeat("Q", 140)
	pushServices = PushService{Platforms: []string{push.IOS, push.Android}}
)

// pushCleanup removes the push notifications and preferences tests leave.
func pushCleanup(t *testing.T, pool *pgxpool.Pool, donors ...pgtype.UUID) {
	t.Helper()
	t.Cleanup(func() {
		for _, sql := range []string{"DELETE FROM donor_push_notifications WHERE donor_id=$1", "DELETE FROM donor_preference_events WHERE donor_id=$1", "DELETE FROM donor_preferences WHERE donor_id=$1"} {
			for _, id := range donors {
				if _, err := pool.Exec(context.Background(), sql, id); err != nil {
					t.Error(err)
				}
			}
		}
	})
}

// appDevice signs donor in on an app installation holding a push token,
// lasting until expires.
func appDevice(t *testing.T, db queries.DBTX, donor pgtype.UUID, platform, token string, expires time.Time) pgtype.UUID {
	t.Helper()
	id := testID(t)
	environment := pgtype.Text{String: "SANDBOX", Valid: platform == push.IOS}
	mustExec(
		t,
		db,
		`INSERT INTO donor_sessions(id,donor_id,token_hash,expires_at,client,platform,refresh_family_hash,refresh_hash,refresh_expires_at,push_token,push_environment)
VALUES($1,$2,$3,$4,'APP',$5,$6,$7,$4,$8,$9)`,
		id,
		donor,
		HashToken(id.String()),
		expires,
		platform,
		HashToken("family"+id.String()),
		HashToken("refresh"+id.String()),
		token,
		environment,
	)
	return id
}

func pushTokenOf(t *testing.T, pool *pgxpool.Pool, access string) (string, string, bool) {
	t.Helper()
	var token, environment pgtype.Text
	if err := pool.QueryRow(context.Background(), "SELECT push_token,push_environment FROM donor_sessions WHERE token_hash=$1", HashToken(access)).Scan(&token, &environment); err != nil {
		t.Fatal(err)
	}
	return token.String, environment.String, token.Valid
}

func TestPushRegistrationBelongsToTheAppSession(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	ctx := context.Background()
	service := pushServices
	service.DB = pool
	first, _ := donor(t, pool)
	second, _ := donor(t, pool)
	accountCleanup(t, pool, first)
	accountCleanup(t, pool, second)
	phone := claimSession(t, pool, first, Client{App: true, Device: "Phone"})

	registration, err := service.RegisterDevice(ctx, phone.Access, PushDevice{Platform: push.IOS, Token: strings.ToUpper(iosToken)})
	if err != nil || !registration.Registered || *registration.Platform != push.IOS || *registration.Environment != "PRODUCTION" {
		t.Fatalf("register: %+v %v", registration, err)
	}
	if token, environment, _ := pushTokenOf(t, pool, phone.Access); token != iosToken || environment != "PRODUCTION" {
		t.Fatalf("stored %q in %q, want the lower-case token in production", token, environment)
	}
	replacement := strings.Repeat("b2", 32)
	if _, err = service.RegisterDevice(ctx, phone.Access, PushDevice{Platform: push.IOS, Token: replacement, Environment: "SANDBOX"}); err != nil {
		t.Fatal(err)
	}
	if token, environment, _ := pushTokenOf(t, pool, phone.Access); token != replacement || environment != "SANDBOX" {
		t.Fatalf("re-registration stored %q in %q", token, environment)
	}

	// The installation signs in as someone else: its token moves with it.
	reinstalled := claimSession(t, pool, second, Client{App: true})
	if _, err = service.RegisterDevice(ctx, reinstalled.Access, PushDevice{Platform: push.IOS, Token: replacement, Environment: "SANDBOX"}); err != nil {
		t.Fatal(err)
	}
	if _, _, held := pushTokenOf(t, pool, phone.Access); held {
		t.Fatal("the previous session kept a token another session registered")
	}
	if token, _, _ := pushTokenOf(t, pool, reinstalled.Access); token != replacement {
		t.Fatal("token did not move to the registering session")
	}

	browser := claimSession(t, pool, first, Client{})
	if _, err = service.RegisterDevice(ctx, browser.Access, PushDevice{Platform: push.Android, Token: androidToken}); !errors.Is(
		err,
		ErrDenied,
	) {
		t.Fatalf("a browser session registered a device: %v", err)
	}
	for name, device := range map[string]PushDevice{
		"short APNs token":       {Platform: push.IOS, Token: "a1b2"},
		"non-hex APNs token":     {Platform: push.IOS, Token: strings.Repeat("zz", 32)},
		"unknown environment":    {Platform: push.IOS, Token: iosToken, Environment: "STAGING"},
		"FCM with environment":   {Platform: push.Android, Token: androidToken, Environment: "PRODUCTION"},
		"FCM token with a space": {Platform: push.Android, Token: androidToken[:40] + " " + androidToken[41:]},
		"oversized FCM token":    {Platform: push.Android, Token: strings.Repeat("x", 1025)},
	} {
		if _, err = service.RegisterDevice(ctx, phone.Access, device); !errors.Is(err, ErrPushToken) {
			t.Errorf("%s: got %v, want ErrPushToken", name, err)
		}
	}
	iosOnly := PushService{DB: api, Platforms: []string{push.IOS}}
	if _, err = iosOnly.RegisterDevice(ctx, phone.Access, PushDevice{Platform: push.Android, Token: androidToken}); !errors.Is(
		err,
		ErrPushUnconfigured,
	) {
		t.Fatalf("registered for an unconfigured platform: %v", err)
	}

	if registration, err = service.UnregisterDevice(ctx, reinstalled.Access); err != nil || registration.Registered {
		t.Fatalf("unregister: %+v %v", registration, err)
	}
	if _, _, held := pushTokenOf(t, pool, reinstalled.Access); held {
		t.Fatal("unregistered session kept its token")
	}
}

func TestEndingASessionClearsItsPushToken(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	ctx := context.Background()
	service := pushServices
	service.DB = pool
	donorService := DonorService{DB: api}
	id, _ := donor(t, pool)
	accountCleanup(t, pool, id)
	register := func(token string) Tokens {
		t.Helper()
		session := claimSession(t, pool, id, appClient)
		if _, err := service.RegisterDevice(ctx, session.Access, PushDevice{Platform: push.Android, Token: token}); err != nil {
			t.Fatal(err)
		}
		return session
	}
	cleared := func(name string, session Tokens) {
		t.Helper()
		if _, _, held := pushTokenOf(t, pool, session.Access); held {
			t.Errorf("%s left the session's push token", name)
		}
	}

	loggedOut := register(androidToken + "1")
	if err := (AccountService{DB: api}).SignOut(ctx, loggedOut.Access); err != nil {
		t.Fatal(err)
	}
	cleared("sign-out", loggedOut)

	reused := register(androidToken + "2")
	refresh(t, reused.Refresh)
	family := HashToken(strings.SplitN(reused.Refresh, ".", 2)[0])
	if _, err := pool.Exec(ctx, "UPDATE donor_sessions SET refreshed_at=refreshed_at-interval '2 minutes' WHERE refresh_family_hash=$1", family); err != nil {
		t.Fatal(err)
	}
	if _, err := (AccountService{DB: api}).Refresh(ctx, reused.Refresh); !errors.Is(err, ErrDenied) {
		t.Fatalf("reused refresh token accepted: %v", err)
	}
	var reusedToken pgtype.Text
	if err := pool.QueryRow(ctx, "SELECT push_token FROM donor_sessions WHERE refresh_family_hash=$1", family).Scan(&reusedToken); err != nil ||
		reusedToken.Valid {
		t.Errorf("refresh-token reuse left the session's push token: %v", err)
	}

	caller := claimSession(t, pool, id, Client{})
	byID := register(androidToken + "3")
	list, err := donorService.Sessions(ctx, caller.Access)
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range list.Sessions {
		if session.Client == "APP" {
			if _, err = donorService.SignOutSession(ctx, caller.Access, session.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	cleared("signing out a session", byID)

	other := register(androidToken + "4")
	if _, err = donorService.SignOutOthers(ctx, caller.Access); err != nil {
		t.Fatal(err)
	}
	cleared("signing out other sessions", other)

	current := register(androidToken + "5")
	if err = donorService.SignOutEverywhere(ctx, current.Access); err != nil {
		t.Fatal(err)
	}
	cleared("signing out everywhere", current)

	// A revocation that forgot the token would fail rather than leave it.
	forgetful := appDevice(t, pool, id, push.Android, androidToken+"6", time.Now().Add(time.Hour))
	_, err = pool.Exec(ctx, "UPDATE donor_sessions SET revoked_at=now() WHERE id=$1", forgetful)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); !ok || pgErr.Code != "23514" {
		t.Fatalf("revoking a session without clearing its token: %v", err)
	}
}

func TestPushPreferencesStartOnAndAreVersioned(t *testing.T) {
	s, token, id := donorServiceFixture(t)
	pool := database(t)
	pushCleanup(t, pool, id)
	ctx := context.Background()
	profile, err := s.Profile(ctx, token)
	if err != nil || !profile.PushReferralConverted || !profile.PushStreakAtRisk || !profile.PushStreakMilestone ||
		profile.ReferralEmails ||
		profile.MarketingEmails {
		t.Fatalf("defaults: %+v %v", profile, err)
	}
	saved, err := s.SavePreferences(ctx, token, Preferences{Version: 0, PushReferralConverted: true, PushStreakMilestone: true}.Replace())
	if err != nil || saved.Version != 1 || !saved.PushReferralConverted || saved.PushStreakAtRisk || !saved.PushStreakMilestone {
		t.Fatalf("saved: %+v %v", saved, err)
	}
	var referral, atRisk, milestone bool
	var consent string
	if err = pool.QueryRow(ctx, "SELECT push_referral_converted,push_streak_at_risk,push_streak_milestone,consent_version FROM donor_preference_events WHERE donor_id=$1 AND version=1", id).Scan(&referral, &atRisk, &milestone, &consent); err != nil ||
		!referral ||
		atRisk ||
		!milestone ||
		consent != consentVersion {
		t.Fatalf("preference event: %v %v %v %q %v", referral, atRisk, milestone, consent, err)
	}
	if _, err = s.SavePreferences(ctx, token, Preferences{Version: 0}.Replace()); !errors.Is(err, ErrPreferenceConflict) {
		t.Fatalf("stale version saved: %v", err)
	}
}

func pushIntents(t *testing.T, db queries.DBTX, donor pgtype.UUID) []queries.DonorPushNotification {
	t.Helper()
	rows, err := db.Query(
		context.Background(),
		"SELECT id,category,dedup_key,context::text,expires_at FROM donor_push_notifications WHERE donor_id=$1 ORDER BY category",
		donor,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []queries.DonorPushNotification
	for rows.Next() {
		var n queries.DonorPushNotification
		var raw string
		if err = rows.Scan(&n.ID, &n.Category, &n.DedupKey, &raw, &n.ExpiresAt); err != nil {
			t.Fatal(err)
		}
		n.Context = []byte(raw)
		out = append(out, n)
	}
	return out
}

func TestReferralConversionQueuesOneGenericPushWithTheGift(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	h := newHistoryFixture(t, tx)
	paid := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	appDevice(t, tx, h.donor("alice"), push.IOS, iosToken, paid.Add(24*time.Hour))
	h.gift(
		recognition.Gift{
			ID:        "first",
			DonorID:   "ben",
			Amount:    3000,
			Currency:  "USD",
			Frequency: "MONTHLY",
			PaidAt:    paid,
			Candidate: &recognition.Candidate{ReferrerID: "alice", Kind: "IRL", ExpiresAt: paid.Add(24 * time.Hour)},
		},
	)
	h.gift(recognition.Gift{ID: "unreferred", DonorID: "cara", Amount: 3000, Currency: "USD", Frequency: "MONTHLY", PaidAt: paid})
	// An old conversion is history, not news, for a referrer who installs the app later.
	h.gift(
		recognition.Gift{
			ID:        "old",
			DonorID:   "dee",
			Amount:    3000,
			Currency:  "USD",
			Frequency: "MONTHLY",
			PaidAt:    paid.Add(-48 * time.Hour),
			Candidate: &recognition.Candidate{ReferrerID: "alice", Kind: "LINK", ExpiresAt: paid},
		},
	)
	mustExec(t, tx, "UPDATE donor_gifts SET created_at=paid_at WHERE donor_id=$1", h.donor("dee"))
	actAs(t, tx, dbroles.ProjectionWorker)
	for _, at := range []time.Time{paid, paid.Add(time.Minute)} {
		for _, name := range []string{"ben", "cara", "dee"} {
			if _, err = refreshProjection(ctx, tx, h.donor(name), at); err != nil {
				t.Fatal(err)
			}
		}
	}
	intents := intentsOf(t, tx, h.donor("alice"), "REFERRAL_CONVERTED")
	if len(intents) != 1 ||
		intents[0].DedupKey != "push:referral:"+h.donor("ben").String()+":"+h.donor("alice").String()+":IRL" ||
		!intents[0].ExpiresAt.Time.Equal(paid.Add(pushLifetime)) {
		t.Fatalf("intents %+v, want one conversion notice", intents)
	}
	var referral struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(intents[0].Context, &referral); err != nil || referral.Kind != "IRL" {
		t.Fatalf("context %s lacks the IRL channel: %v", intents[0].Context, err)
	}
	note, err := pushContent(intents[0])
	if err != nil || note.Body != "Someone you invited through an IRL Referral just made their first test gift." ||
		note.Path != "/account/dashboard" {
		t.Fatalf("content %+v %v", note, err)
	}
	var earned []string
	for _, n := range intentsOf(t, tx, h.donor("alice"), "BADGE_EARNED") {
		earned = append(earned, n.DedupKey[strings.LastIndex(n.DedupKey, ":")+1:])
	}
	if slices.Sort(earned); !slices.Equal(earned, []string{"IRL", "REF1"}) {
		t.Fatalf("referrer earned %v with the conversion, want First Referral and IRL", earned)
	}
	if len(pushIntents(t, tx, h.donor("cara"))) != 0 || len(pushIntents(t, tx, h.donor("ben"))) != 0 {
		t.Fatal("pushes queued for donors without devices")
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var left int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM donor_push_notifications WHERE dedup_key=$1", intents[0].DedupKey).Scan(&left); err != nil ||
		left != 0 {
		t.Fatal("push intent outlived the gift's transaction", err)
	}
}

// intentsOf returns the donor's queued notifications of one category.
func intentsOf(t *testing.T, db queries.DBTX, donor pgtype.UUID, category string) []queries.DonorPushNotification {
	t.Helper()
	var out []queries.DonorPushNotification
	for _, n := range pushIntents(t, db, donor) {
		if n.Category == category {
			out = append(out, n)
		}
	}
	return out
}

// A streak milestone is announced on the day it is reached (S-3): a run that
// began 100 local days ago reached its 100-day milestone at today's midnight.
func TestStreakMilestonePushFollowsTheProjection(t *testing.T) {
	f := newWorkerFixture(t, "MONTHLY")
	pushCleanup(t, f.db, f.donorID)
	result := f.checkout(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	appDevice(t, f.service.DB, f.donorID, push.Android, androidToken, now.Add(time.Hour))
	today := now.In(recognition.NewYork)
	start := time.Date(today.Year(), today.Month(), today.Day()-100, 0, 1, 0, 0, recognition.NewYork).UTC()
	for i := range 4 {
		f.receive(
			t,
			"invoice.payment_succeeded",
			invoice(result.CheckoutID, "in_"+result.CheckoutID+"_"+string(rune('a'+i)), start.Add(time.Duration(i)*30*24*time.Hour), true),
		)
		f.process(t)
	}
	intents := intentsOf(t, f.service.DB, f.donorID, "STREAK_MILESTONE")
	if len(intents) != 1 || !strings.HasSuffix(intents[0].DedupKey, ":100") || !strings.Contains(intents[0].DedupKey, f.donorID.String()) {
		t.Fatalf("intents %+v, want the 100-day milestone", intents)
	}
	note, err := pushContent(intents[0])
	if err != nil || note.Title != "Your 100-day giving streak" {
		t.Fatalf("content %+v %v", note, err)
	}
	if badges := intentsOf(t, f.service.DB, f.donorID, "BADGE_EARNED"); len(badges) == 0 {
		t.Fatal("the projection earned no badge pushes for a 100-day streak")
	}
}

// A failed renewal queues the initial and day-five reminders once per period.
func TestStreakAtRiskPushFollowsTheProjection(t *testing.T) {
	f := newWorkerFixture(t, "MONTHLY")
	pushCleanup(t, f.db, f.donorID)
	result := f.checkout(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	appDevice(t, f.service.DB, f.donorID, push.Android, androidToken, now.Add(time.Hour))
	start := now.Add(-95 * 24 * time.Hour)
	for i := range 3 {
		f.receive(
			t,
			"invoice.payment_succeeded",
			invoice(result.CheckoutID, "in_"+result.CheckoutID+"_"+string(rune('a'+i)), start.Add(time.Duration(i)*30*24*time.Hour), true),
		)
		f.process(t)
	}
	f.receive(t, "invoice.payment_failed", invoice(result.CheckoutID, "in_"+result.CheckoutID+"_d", start.Add(90*24*time.Hour), false))
	f.process(t)
	tx, err := as(t, dbroles.ProjectionWorker).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = refreshProjection(ctx, tx, f.donorID, now.Add(time.Hour)); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var period string
	if err = f.db.QueryRow(ctx, "SELECT id::text FROM donor_billing_periods WHERE donor_id=$1 AND status='FAILED'", f.donorID).Scan(&period); err != nil {
		t.Fatal(err)
	}
	intents := intentsOf(t, f.service.DB, f.donorID, "STREAK_AT_RISK")
	keys := map[string]bool{}
	for _, intent := range intents {
		keys[intent.DedupKey] = true
	}
	if len(intents) != 2 || !keys["push:streak-at-risk:"+period] || !keys["push:streak-at-risk:"+period+":5"] {
		t.Fatalf("intents %+v, want initial and day-five notices for the failed period", intents)
	}
	note, err := pushContent(intents[0])
	if err != nil || note.Path != "/account/payment-method" {
		t.Fatalf("content %+v %v", note, err)
	}
}

// fakeProvider records what it is asked to send and answers with result.
type fakeProvider struct {
	mu     sync.Mutex
	sent   []push.Device
	notes  []push.Notification
	result func(push.Device) error
}

func (p *fakeProvider) Send(_ context.Context, device push.Device, note push.Notification) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, device)
	p.notes = append(p.notes, note)
	if p.result == nil {
		return nil
	}
	return p.result(device)
}

type pushFixture struct {
	pool   *pgxpool.Pool
	donor  pgtype.UUID
	intent pgtype.UUID
	now    time.Time
}

// newPushFixture queues a 30-day milestone for a donor whose current streak is
// 31 days long, available at now.
func newPushFixture(t *testing.T) *pushFixture {
	t.Helper()
	pool := database(t)
	id, _ := donor(t, pool)
	pushCleanup(t, pool, id)
	now := time.Now().UTC().Truncate(time.Microsecond)
	mustExec(
		t,
		pool,
		"INSERT INTO donor_metric_projections(id,donor_id,current_streak,best_streak,computed_at,next_refresh_at,streak_since) VALUES($1,$2,2,2,$3,$4,$5)",
		testID(t),
		id,
		now,
		now.Add(time.Hour),
		now.Add(-31*24*time.Hour),
	)
	intent := testID(t)
	mustExec(t, pool, `INSERT INTO donor_push_notifications(id,donor_id,category,dedup_key,context,available_at,expires_at)
VALUES($1,$2,'STREAK_MILESTONE',$3,'{"days":30}',$4,$5)`, intent, id, "push:test:"+intent.String(), now, now.Add(pushLifetime))
	return &pushFixture{pool, id, intent, now}
}

func (f *pushFixture) state(t *testing.T) (attempts int32, completed bool, reason, lastError string, sentTo []pgtype.UUID) {
	t.Helper()
	var r, e pgtype.Text
	if err := f.pool.QueryRow(context.Background(), "SELECT attempts,completed_at IS NOT NULL,suppressed_reason,last_error,sent_to FROM donor_push_notifications WHERE id=$1", f.intent).Scan(&attempts, &completed, &r, &e, &sentTo); err != nil {
		t.Fatal(err)
	}
	return attempts, completed, r.String, e.String, sentTo
}

func (f *pushFixture) deliver(t *testing.T, w PushWorker, at time.Time) bool {
	t.Helper()
	worked, err := w.DeliverNext(context.Background(), at)
	if err != nil {
		t.Fatal(err)
	}
	return worked
}

func TestPushDeliveryReachesEachDeviceOnce(t *testing.T) {
	f := newPushFixture(t)
	phone := appDevice(t, f.pool, f.donor, push.IOS, iosToken, f.now.Add(time.Hour))
	tablet := appDevice(t, f.pool, f.donor, push.Android, androidToken, f.now.Add(time.Hour))
	appDevice(t, f.pool, f.donor, push.Android, androidToken+"lapsed", f.now.Add(-time.Minute))
	apns := &fakeProvider{}
	fcm := &fakeProvider{result: func(push.Device) error { return push.Unavailable{After: 10 * time.Minute} }}
	w := PushWorker{DB: as(t, dbroles.PushWorker), Providers: map[string]push.Provider{push.IOS: apns, push.Android: fcm}}

	if !f.deliver(t, w, f.now) {
		t.Fatal("no work found")
	}
	if len(apns.sent) != 1 || apns.sent[0] != (push.Device{Token: iosToken, Environment: "SANDBOX"}) || len(fcm.sent) != 1 ||
		fcm.sent[0].Token != androidToken {
		t.Fatalf("sent to %v and %v, want each live device once", apns.sent, fcm.sent)
	}
	note := apns.notes[0]
	if note.ID != f.intent.String() || note.Category != "STREAK_MILESTONE" || note.Path != "/account/dashboard" ||
		!note.Expires.Equal(f.now.Add(pushLifetime)) {
		t.Fatalf("notification %+v", note)
	}
	attempts, completed, _, lastError, sentTo := f.state(t)
	if attempts != 1 || completed || lastError != "DeliveryError:Unavailable" || len(sentTo) != 1 || sentTo[0] != phone {
		t.Fatalf("after a partial failure: attempts=%d completed=%v error=%q sent=%v", attempts, completed, lastError, sentTo)
	}
	// The provider's Retry-After outlasts the two-minute backoff.
	if f.deliver(t, w, f.now.Add(5*time.Minute)) {
		t.Fatal("retried before the provider's delay")
	}
	fcm.result = nil
	if !f.deliver(t, w, f.now.Add(10*time.Minute)) {
		t.Fatal("retry not due")
	}
	if len(apns.sent) != 1 || len(fcm.sent) != 2 {
		t.Fatalf("retry resent to a reached device: %d APNs, %d FCM sends", len(apns.sent), len(fcm.sent))
	}
	attempts, completed, reason, _, sentTo := f.state(t)
	if attempts != 2 || !completed || reason != "" || len(sentTo) != 2 || sentTo[1] != tablet {
		t.Fatalf("after the retry: attempts=%d completed=%v reason=%q sent=%v", attempts, completed, reason, sentTo)
	}
}

func TestUnregisteredTokenIsDroppedWithoutRetry(t *testing.T) {
	f := newPushFixture(t)
	phone := appDevice(t, f.pool, f.donor, push.IOS, iosToken, f.now.Add(time.Hour))
	apns := &fakeProvider{result: func(push.Device) error { return push.ErrUnregistered }}
	w := PushWorker{DB: as(t, dbroles.PushWorker), Providers: map[string]push.Provider{push.IOS: apns}}
	f.deliver(t, w, f.now)
	if _, completed, reason, _, _ := f.state(t); !completed || reason != "unregistered" {
		t.Fatalf("completed=%v reason=%q, want completion without retry", completed, reason)
	}
	var token pgtype.Text
	if err := f.pool.QueryRow(context.Background(), "SELECT push_token FROM donor_sessions WHERE id=$1", phone).Scan(&token); err != nil ||
		token.Valid {
		t.Fatalf("unregistered token kept: %v", err)
	}
}

func TestProviderFailureIsRecordedAsABoundedClass(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"timeout", context.DeadlineExceeded, "DeliveryError:Network"},
		{"other", errors.New("apns: device 1234 exploded"), "DeliveryError:Provider"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPushFixture(t)
			appDevice(t, f.pool, f.donor, push.IOS, iosToken, f.now.Add(time.Hour))
			apns := &fakeProvider{result: func(push.Device) error { return tc.err }}
			f.deliver(t, PushWorker{DB: as(t, dbroles.PushWorker), Providers: map[string]push.Provider{push.IOS: apns}}, f.now)
			attempts, completed, _, lastError, _ := f.state(t)
			if attempts != 1 || completed || lastError != tc.want {
				t.Fatalf("attempts=%d completed=%v error=%q, want %q", attempts, completed, lastError, tc.want)
			}
		})
	}
}

func TestPushSendHasADeadline(t *testing.T) {
	f := newPushFixture(t)
	appDevice(t, f.pool, f.donor, push.IOS, iosToken, f.now.Add(time.Hour))
	var remaining time.Duration
	apns := &deadlineProvider{seen: func(d time.Duration) { remaining = d }}
	f.deliver(t, PushWorker{DB: as(t, dbroles.PushWorker), Providers: map[string]push.Provider{push.IOS: apns}}, f.now)
	if remaining <= 0 || remaining > pushSendBudget || pushSendBudget >= deliveryLease {
		t.Fatalf("send deadline %v, want within %v and under the %v lease", remaining, pushSendBudget, deliveryLease)
	}
}

type deadlineProvider struct{ seen func(time.Duration) }

func (p *deadlineProvider) Send(ctx context.Context, _ push.Device, _ push.Notification) error {
	if deadline, ok := ctx.Deadline(); ok {
		p.seen(time.Until(deadline))
	}
	return nil
}

func TestMalformedMilestonePayloadFailsAsInvalid(t *testing.T) {
	f := newPushFixture(t)
	appDevice(t, f.pool, f.donor, push.IOS, iosToken, f.now.Add(time.Hour))
	mustExec(t, f.pool, `UPDATE donor_push_notifications SET context='{"days":"thirty"}' WHERE id=$1`, f.intent)
	apns := &fakeProvider{}
	f.deliver(t, PushWorker{DB: as(t, dbroles.PushWorker), Providers: map[string]push.Provider{push.IOS: apns}}, f.now)
	attempts, completed, reason, lastError, _ := f.state(t)
	if attempts != 1 || completed || reason != "" || lastError != "InvalidNotification" || len(apns.sent) != 0 {
		t.Fatalf(
			"attempts=%d completed=%v reason=%q error=%q sends=%d, want a failed attempt",
			attempts,
			completed,
			reason,
			lastError,
			len(apns.sent),
		)
	}
}

func TestRejectedConfigurationParksTheNotification(t *testing.T) {
	f := newPushFixture(t)
	appDevice(t, f.pool, f.donor, push.IOS, iosToken, f.now.Add(time.Hour))
	apns := &fakeProvider{result: func(push.Device) error { return push.ErrRejected }}
	w := PushWorker{DB: as(t, dbroles.PushWorker), Providers: map[string]push.Provider{push.IOS: apns}}
	f.deliver(t, w, f.now)
	attempts, completed, _, lastError, _ := f.state(t)
	if attempts != MaxDeliveryAttempts || completed || lastError != "ProviderRejected" {
		t.Fatalf("attempts=%d completed=%v error=%q, want parked", attempts, completed, lastError)
	}
	if f.deliver(t, w, f.now.Add(time.Hour)) || len(apns.sent) != 1 {
		t.Fatal("a parked notification was retried")
	}
	var token pgtype.Text
	if err := f.pool.QueryRow(context.Background(), "SELECT push_token FROM donor_sessions WHERE donor_id=$1", f.donor).Scan(&token); err != nil ||
		!token.Valid {
		t.Fatalf("a configuration refusal dropped the device's token: %v", err)
	}
}

func TestPushRechecksItsCircumstancesAtDelivery(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(*testing.T, *pushFixture)
		at      time.Duration
		reason  string
	}{
		{"turned off", func(t *testing.T, f *pushFixture) {
			mustExec(t, f.pool, "INSERT INTO donor_preferences(id,donor_id,version,referral_emails,marketing_emails,push_streak_milestone) VALUES($1,$2,1,false,false,false)", testID(t), f.donor)
		}, 0, "opted_out"},
		{"expired", func(*testing.T, *pushFixture) {}, pushLifetime, "expired"},
		{"streak ended", func(t *testing.T, f *pushFixture) {
			mustExec(t, f.pool, "UPDATE donor_metric_projections SET current_streak=0,streak_since=NULL WHERE donor_id=$1", f.donor)
		}, 0, "streak_ended"},
		{"signed out", func(t *testing.T, f *pushFixture) {
			mustExec(t, f.pool, "UPDATE donor_sessions SET revoked_at=now(),push_token=NULL,push_environment=NULL WHERE donor_id=$1", f.donor)
		}, 0, "no_device"},
		{"renewal cancelled", func(t *testing.T, f *pushFixture) {
			checkout, attempt, period := testID(t), testID(t), testID(t)
			t.Cleanup(func() {
				for _, sql := range []string{"DELETE FROM donor_billing_periods WHERE donor_id=$1", "DELETE FROM donor_subscriptions WHERE donor_id=$1",
					"DELETE FROM donor_stripe_checkouts WHERE checkout_id IN (SELECT id FROM donor_checkouts WHERE donor_id=$1)", "DELETE FROM donor_checkouts WHERE donor_id=$1"} {
					mustExec(t, f.pool, sql, f.donor)
				}
			})
			mustExec(t, f.pool, "INSERT INTO donor_checkouts(id,donor_id,amount_minor,frequency) VALUES($1,$2,3000,'MONTHLY')", checkout, f.donor)
			mustExec(t, f.pool, "INSERT INTO donor_stripe_checkouts(id,checkout_id,request_hash,request_params,stripe_subscription_id,status) VALUES($1,$2,'hash','{}',$3,'COMPLETE')", attempt, checkout, "sub_"+attempt.String())
			mustExec(t, f.pool, "INSERT INTO donor_subscriptions(id,attempt_id,donor_id,status,period_end,stops_at,observed_at) VALUES($1,$2,$3,'active',$4,$4,$5)", testID(t), attempt, f.donor, f.now.Add(24*time.Hour), f.now)
			mustExec(t, f.pool, "INSERT INTO donor_billing_periods(id,donor_id,checkout_id,stripe_invoice_id,stripe_subscription_id,due_at,ends_at,status) VALUES($1,$2,$3,$4,$5,$6,$7,'FAILED')", period, f.donor, checkout, "in_"+period.String(), "sub_"+attempt.String(), f.now.Add(-time.Hour), f.now.Add(24*time.Hour))
			mustExec(t, f.pool, "UPDATE donor_push_notifications SET category='STREAK_AT_RISK',context=json_build_object('period_id',$2::text) WHERE id=$1", f.intent, period)
		}, 0, "subscription_cancelled"},
		{"provider unconfigured", func(t *testing.T, f *pushFixture) {
			appDevice(t, f.pool, f.donor, push.Android, androidToken, f.now.Add(time.Hour))
			mustExec(t, f.pool, "UPDATE donor_sessions SET push_token=NULL,push_environment=NULL WHERE donor_id=$1 AND platform='IOS'", f.donor)
		}, 0, "push_unconfigured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPushFixture(t)
			appDevice(t, f.pool, f.donor, push.IOS, iosToken, f.now.Add(48*time.Hour))
			tc.prepare(t, f)
			apns := &fakeProvider{}
			f.deliver(t, PushWorker{DB: as(t, dbroles.PushWorker), Providers: map[string]push.Provider{push.IOS: apns}}, f.now.Add(tc.at))
			if _, completed, reason, _, _ := f.state(t); !completed || reason != tc.reason || len(apns.sent) != 0 {
				t.Fatalf("completed=%v reason=%q after %d sends, want suppressed as %s", completed, reason, len(apns.sent), tc.reason)
			}
		})
	}
}

func TestReferralConversionsFeedShowsOnlyChannelAndTime(t *testing.T) {
	s, token, referrer := donorServiceFixture(t)
	pool := database(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	for i, kind := range []string{"LINK", "IRL", "LINK"} {
		id, _ := donor(t, pool)
		accountCleanup(t, pool, id)
		checkout := testID(t)
		mustExec(
			t,
			pool,
			"INSERT INTO donor_checkouts(id,donor_id,amount_minor,frequency,referrer_id,referral_kind,referral_expires_at) VALUES($1,$2,3000,'MONTHLY',$3,$4,$5)",
			checkout,
			id,
			referrer,
			kind,
			base.Add(30*24*time.Hour),
		)
		object := "pi_feed_" + checkout.String()
		mustExec(
			t,
			pool,
			"INSERT INTO donor_gifts(id,donor_id,checkout_id,stripe_object_id,amount_minor,currency,frequency,paid_at,referrer_id,referral_kind) VALUES($1,$2,$3,$4,3000,'USD','MONTHLY',$5,$6,$7)",
			testID(t),
			id,
			checkout,
			object,
			base.Add(time.Duration(i)*time.Hour),
			referrer,
			kind,
		)
		net := int64(3000)
		if i == 2 {
			net = 0 // fully refunded: no longer a referral
		}
		mustExec(
			t,
			pool,
			`INSERT INTO donor_metric_projections(id,donor_id,referrer_id,referral_kind,first_gift_id,net_minor,computed_at,next_refresh_at)
VALUES($1,$2,$3,$4,$5,$6,now(),now()+interval '1 day')`,
			testID(t),
			id,
			referrer,
			kind,
			object,
			net,
		)
	}
	feed, err := s.Conversions(ctx, token, nil)
	if err != nil || feed.More || len(feed.Conversions) != 2 || feed.Conversions[0].Kind != "IRL" ||
		!feed.Conversions[0].ConvertedAt.Time.Equal(base.Add(time.Hour)) ||
		feed.Conversions[1].Kind != "LINK" {
		t.Fatalf("feed %+v %v, want the two counting conversions newest first", feed, err)
	}
	since := base
	if feed, err = s.Conversions(ctx, token, &since); err != nil || len(feed.Conversions) != 1 || feed.Conversions[0].Kind != "IRL" {
		t.Fatalf("since feed %+v %v, want only the later conversion", feed, err)
	}
}
