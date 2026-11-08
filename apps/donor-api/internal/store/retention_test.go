package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/dbroles/dbrolestest"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// retentionFixture is a referrer whose touches, and the checkouts and gifts
// carrying them, are removed after the test.
type retentionFixture struct {
	pool     *pgxpool.Pool
	referrer pgtype.UUID
	giver    pgtype.UUID
}

// touchPasses serializes retention tests: a pass deletes every touch past
// retention in the shared database, not only the fixture's, so one test's pass
// would delete another's touches between its inserts and its assertions.
var touchPasses sync.Mutex

func newRetentionFixture(t *testing.T) retentionFixture {
	t.Helper()
	touchPasses.Lock()
	t.Cleanup(touchPasses.Unlock)
	pool := database(t)
	referrer, _ := donor(t, pool)
	giver, _ := donor(t, pool)
	t.Cleanup(func() {
		for _, sql := range []string{"DELETE FROM donor_gifts WHERE donor_id=$1", "DELETE FROM donor_checkouts WHERE donor_id=$1"} {
			if _, err := pool.Exec(context.Background(), sql, giver); err != nil {
				t.Error(err)
			}
		}
		if _, err := pool.Exec(context.Background(), "DELETE FROM donor_referral_touches WHERE referrer_id=$1", referrer); err != nil {
			t.Error(err)
		}
	})
	return retentionFixture{pool, referrer, giver}
}

func (f retentionFixture) touch(t *testing.T, kind string, at, expires time.Time) pgtype.UUID {
	t.Helper()
	id := testID(t)
	mustExec(
		t,
		f.pool,
		`INSERT INTO donor_referral_touches (id,token_hash,referrer_id,kind,touched_at,expires_at) VALUES ($1,$2,$3,$4,$5,$6)`,
		id,
		HashToken(id.String()),
		f.referrer,
		kind,
		at,
		expires,
	)
	return id
}

// checkout records a checkout carrying the referrer, kind and expiry given,
// as checkout creation copies them from a valid touch.
func (f retentionFixture) checkout(t *testing.T, kind string, expires time.Time) pgtype.UUID {
	t.Helper()
	id := testID(t)
	mustExec(
		t,
		f.pool,
		`INSERT INTO donor_checkouts (id,donor_id,amount_minor,frequency,referrer_id,referral_kind,referral_expires_at,created_at)
VALUES ($1,$2,3000,'MONTHLY',$3,$4,$5,$6)`,
		id,
		f.giver,
		f.referrer,
		kind,
		expires,
		expires.Add(-time.Hour),
	)
	return id
}

// gift records a gift on checkout, attributed as the event worker does only
// when paid before the checkout's attribution expiry.
func (f retentionFixture) gift(t *testing.T, checkout pgtype.UUID, paid time.Time, attributed bool) {
	t.Helper()
	mustExec(
		t,
		f.pool,
		`INSERT INTO donor_gifts (id,donor_id,checkout_id,stripe_object_id,amount_minor,currency,frequency,paid_at,referrer_id,referral_kind)
SELECT $1,$2,id,$3,3000,'USD','MONTHLY',$4,CASE WHEN $5::bool THEN referrer_id END,CASE WHEN $5::bool THEN referral_kind END
FROM donor_checkouts WHERE id=$6`,
		testID(t),
		f.giver,
		"pi_retention_"+checkout.String(),
		paid,
		attributed,
		checkout,
	)
}

func (f retentionFixture) remaining(t *testing.T, ids ...pgtype.UUID) map[pgtype.UUID]bool {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), "SELECT id FROM donor_referral_touches WHERE id=ANY($1)", ids)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := map[pgtype.UUID]bool{}
	for rows.Next() {
		var id pgtype.UUID
		if err = rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		found[id] = true
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return found
}

// purgeAll runs one pass with batches of size batch, as the worker does on
// its pool.
func purgeAll(t *testing.T, pool *pgxpool.Pool, now time.Time, batch int32) [][]queries.PurgeReferralTouchesRow {
	t.Helper()
	var batches [][]queries.PurgeReferralTouchesRow
	cursor := queries.PurgeReferralTouchesRow{}
	for {
		deleted, err := purgeTouches(context.Background(), pool, now, cursor, batch)
		if err != nil {
			t.Error(err)
			return batches
		}
		batches = append(batches, deleted)
		if len(deleted) < int(batch) {
			return batches
		}
		cursor = deleted[len(deleted)-1]
	}
}

func TestTouchPurgeDeletesOnlyUnconvertedTouchesPastRetention(t *testing.T) {
	t.Parallel()
	f := newRetentionFixture(t)
	now := time.Now().UTC()
	cutoff := now.AddDate(0, -touchRetentionMonths, 0)
	window := 30 * 24 * time.Hour
	old := cutoff.Add(-time.Hour)

	converted := f.touch(t, "LINK", old.Add(-time.Minute), old.Add(window-time.Minute))
	f.gift(t, f.checkout(t, "LINK", old.Add(window-time.Minute)), old, true)

	// Attributed gifts from other visitors' checkouts for the same referrer do
	// not keep these touches.
	unconverted := f.touch(t, "LINK", old, old.Add(window))
	f.gift(t, f.checkout(t, "LINK", old.Add(window+time.Second)), old, true)
	otherKind := f.touch(t, "IRL", old, old.Add(window).Add(time.Minute))
	f.gift(t, f.checkout(t, "LINK", old.Add(window).Add(time.Minute)), old, true)

	abandoned := f.touch(t, "IRL", old.Add(-2*time.Minute), old.Add(window-2*time.Minute))
	f.checkout(t, "IRL", old.Add(window-2*time.Minute))
	paidLate := f.touch(t, "LINK", old.Add(-3*time.Minute), old.Add(window-3*time.Minute))
	f.gift(t, f.checkout(t, "LINK", old.Add(window-3*time.Minute)), old.Add(window), false)

	recent := f.touch(t, "LINK", cutoff.Add(time.Minute), cutoff.Add(time.Minute+window))
	inWindow := f.touch(t, "IRL", old, now.Add(time.Hour))

	purgeAll(t, as(t, dbroles.ProjectionWorker), now, touchPurgeBatch)
	left := f.remaining(t, converted, unconverted, otherKind, abandoned, paidLate, recent, inWindow)
	if !left[converted] {
		t.Error("touch that led to an attributed gift deleted")
	}
	if left[unconverted] || left[otherKind] {
		t.Error("touch without a checkout kept")
	}
	if left[abandoned] || left[paidLate] {
		t.Error("touch whose checkout has no attributed gift kept")
	}
	if !left[recent] {
		t.Error("touch within retention deleted")
	}
	if !left[inWindow] {
		t.Error("touch whose attribution window is open deleted")
	}
}

func TestTouchPurgeBatchesAreBoundedAndConcurrentPassesShareTheWork(t *testing.T) {
	t.Parallel()
	f := newRetentionFixture(t)
	now := time.Now().UTC()
	// Capture times older than any other fixture's put these first in a pass.
	start := time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)
	var ids []pgtype.UUID
	for i := range 40 {
		at := start.Add(time.Duration(i) * time.Minute)
		ids = append(ids, f.touch(t, "LINK", at, at.Add(30*24*time.Hour)))
	}
	mine := map[pgtype.UUID]bool{}
	for _, id := range ids {
		mine[id] = true
	}

	// Three workers, each with its own pool, as separate processes would have.
	workers := []*pgxpool.Pool{
		dbrolestest.Open(t, dbroles.ProjectionWorker),
		dbrolestest.Open(t, dbroles.ProjectionWorker),
		dbrolestest.Open(t, dbroles.ProjectionWorker),
	}
	first, err := purgeTouches(context.Background(), workers[0], now, queries.PurgeReferralTouchesRow{}, 4)
	if err != nil || len(first) != 4 || first[0].ID != ids[0] || first[3].ID != ids[3] {
		t.Fatal("first batch was not the four oldest touches", err)
	}

	var mu sync.Mutex
	deletedBy := map[pgtype.UUID]int{}
	var wg sync.WaitGroup
	for runner := range 3 {
		wg.Go(func() {
			for _, batch := range purgeAll(t, workers[runner], now, 4) {
				if len(batch) > 4 {
					t.Errorf("batch of %d exceeds 4", len(batch))
				}
				mu.Lock()
				for _, row := range batch {
					if mine[row.ID] {
						if _, again := deletedBy[row.ID]; again {
							t.Error("touch deleted twice")
						}
						deletedBy[row.ID] = runner
					}
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(deletedBy) != len(ids)-4 || len(f.remaining(t, ids...)) != 0 {
		t.Fatalf("concurrent passes deleted %d of %d touches", len(deletedBy), len(ids)-4)
	}
}

func TestTouchRetentionStartsAPassOnceAnInterval(t *testing.T) {
	t.Parallel()
	f := newRetentionFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	start := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	mustExec(t, f.pool, `INSERT INTO donor_referral_touches (id,token_hash,referrer_id,kind,touched_at,expires_at)
SELECT gen_random_uuid(),md5(random()::text)||md5(random()::text),$1,'LINK',at,at+interval '30 days'
FROM generate_series(1,$2::int) n, LATERAL (SELECT $3::timestamptz+n*interval '1 second' AS at) s`, f.referrer, touchPurgeBatch+1, start)
	count := func() (n int) {
		t.Helper()
		if err := f.pool.QueryRow(ctx, "SELECT count(*) FROM donor_referral_touches WHERE referrer_id=$1", f.referrer).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	retention := &TouchRetention{DB: as(t, dbroles.ProjectionWorker)}
	if worked, err := retention.PurgeNext(ctx, now); err != nil || !worked || count() != 1 {
		t.Fatal("first batch did not delete exactly one batch", err)
	}
	if worked, err := retention.PurgeNext(ctx, now); err != nil || !worked || count() != 0 {
		t.Fatal("pass stopped after a full batch", err)
	}
	later := f.touch(t, "IRL", start, start.Add(30*24*time.Hour))
	if worked, err := retention.PurgeNext(ctx, now.Add(touchPurgeInterval-time.Second)); err != nil || worked || count() != 1 {
		t.Fatal("pass started before the interval elapsed", err)
	}
	if worked, err := retention.PurgeNext(ctx, now.Add(touchPurgeInterval)); err != nil || !worked || f.remaining(t, later)[later] {
		t.Fatal("pass did not start once the interval elapsed", err)
	}
}

// Serial, because every pass purges expired analytics first and would delete
// this test's events from under a parallel pass.
func TestTouchRetentionPurgesExpiredAnalyticsInBatches(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	journey := testID(t)
	t.Cleanup(func() { cleanExec(t, "DELETE FROM analytics_events WHERE journey_id=$1", journey) })
	mustExec(t, pool, `INSERT INTO analytics_events (id,name,source,occurred_at,received_at,journey_id)
SELECT gen_random_uuid(),'app_opened','WEB',$1,$1,$2 FROM generate_series(1,$3::int)`,
		time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), journey, analyticsPurgeBatch+1)
	fresh := testID(t)
	mustExec(
		t,
		pool,
		`INSERT INTO analytics_events (id,name,source,occurred_at,journey_id) VALUES ($1,'app_opened','WEB',now(),$2)`,
		fresh,
		journey,
	)
	count := func() (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM analytics_events WHERE journey_id=$1", journey).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	retention := &TouchRetention{DB: as(t, dbroles.ProjectionWorker)}
	if worked, err := retention.PurgeNext(ctx, time.Now().UTC()); err != nil || !worked || count() != 2 {
		t.Fatalf("first pass left %d events, want the last expired one and the fresh one: %v", count(), err)
	}
	if _, err := retention.PurgeNext(ctx, time.Now().UTC()); err != nil || count() != 1 {
		t.Fatalf("second pass left %d events, want only the fresh one: %v", count(), err)
	}
}

// Serial, because every pass purges every lapsed credential in the shared
// database and would delete this test's rows from under a parallel pass.
func TestTouchRetentionPurgesLapsedCredentialsAndKeepsLiveOnes(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	id, _ := donor(t, pool)
	now := time.Now().UTC()
	longAgo := now.Add(-sessionRetention - 24*time.Hour)
	recent := now.Add(-time.Hour)

	session := func(expires time.Time, refreshExpires *time.Time) pgtype.UUID {
		t.Helper()
		session := testID(t)
		mustExec(
			t,
			pool,
			`INSERT INTO donor_sessions(id,donor_id,token_hash,expires_at,client,refresh_family_hash,refresh_hash,refresh_expires_at)
SELECT $1,$2,$3::varchar,$4,CASE WHEN $5::timestamptz IS NULL THEN 'BROWSER' ELSE 'APP' END,
 CASE WHEN $5::timestamptz IS NULL THEN NULL ELSE $3::varchar END,CASE WHEN $5::timestamptz IS NULL THEN NULL ELSE $3::varchar END,$5`,
			session,
			id,
			HashToken(session.String()),
			expires,
			refreshExpires,
		)
		return session
	}
	lapsedBrowser := session(longAgo, nil)
	lapsedApp := session(recent, &longAgo)
	liveApp := session(longAgo, &now)
	recentBrowser := session(recent, nil)
	mustExec(
		t,
		pool,
		`INSERT INTO donor_session_events(id,donor_id,session_id,action) VALUES ($1,$2,$3,'SIGNED_IN')`,
		testID(t),
		id,
		lapsedBrowser,
	)

	oldLink, freshLink := testID(t), testID(t)
	for link, expires := range map[pgtype.UUID]time.Time{oldLink: now.Add(-loginRetention - time.Hour), freshLink: recent} {
		mustExec(t, pool, `INSERT INTO donor_login_links(id,donor_id,token_hash,expires_at) VALUES ($1,$2,$3,$4)`,
			link, id, HashToken(link.String()), expires)
	}
	oldEmail, freshEmail, openEmail := testID(t), testID(t), testID(t)
	for email, completed := range map[pgtype.UUID]*time.Time{
		oldEmail: new(now.Add(-loginRetention - time.Hour)), freshEmail: &recent, openEmail: nil,
	} {
		mustExec(
			t,
			pool,
			`INSERT INTO donor_login_emails(id,donor_id,available_at,completed_at) VALUES ($1,$2,$3,$4)`,
			email,
			id,
			now,
			completed,
		)
	}
	const oldPhone, freshPhone = "+14155550188", "+14155550189"
	mustExec(
		t,
		pool,
		`INSERT INTO donor_phone_codes(phone,code_hash,expires_at) VALUES ($1::text,'h',$2::timestamptz),($3::text,'h',$4::timestamptz)`,
		oldPhone,
		now.Add(-phoneCodeRetention-time.Hour),
		freshPhone,
		recent,
	)
	t.Cleanup(func() {
		cleanExec(t, "DELETE FROM donor_phone_codes WHERE phone IN ($1,$2)", oldPhone, freshPhone)
		cleanExec(t, "DELETE FROM donor_session_events WHERE donor_id=$1", id)
		cleanExec(t, "DELETE FROM donor_login_emails WHERE donor_id=$1", id)
		cleanExec(t, "DELETE FROM donor_login_links WHERE donor_id=$1", id)
		cleanExec(t, "DELETE FROM donor_sessions WHERE donor_id=$1", id)
	})

	retention := &TouchRetention{DB: as(t, dbroles.ProjectionWorker)}
	if _, err := retention.PurgeNext(ctx, now); err != nil {
		t.Fatal(err)
	}
	exists := func(table, key string, value any) bool {
		t.Helper()
		var found bool
		if err := pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM "+table+" WHERE "+key+"=$1)", value).Scan(&found); err != nil {
			t.Fatal(err)
		}
		return found
	}
	for name, want := range map[string]struct{ got, want bool }{
		"lapsed browser session": {exists("donor_sessions", "id", lapsedBrowser), false},
		"lapsed app session":     {exists("donor_sessions", "id", lapsedApp), false},
		"live app session":       {exists("donor_sessions", "id", liveApp), true},
		"recent browser session": {exists("donor_sessions", "id", recentBrowser), true},
		"old login link":         {exists("donor_login_links", "id", oldLink), false},
		"fresh login link":       {exists("donor_login_links", "id", freshLink), true},
		"old login email":        {exists("donor_login_emails", "id", oldEmail), false},
		"fresh login email":      {exists("donor_login_emails", "id", freshEmail), true},
		"unfinished login email": {exists("donor_login_emails", "id", openEmail), true},
		"old phone code":         {exists("donor_phone_codes", "phone", oldPhone), false},
		"fresh phone code":       {exists("donor_phone_codes", "phone", freshPhone), true},
	} {
		if want.got != want.want {
			t.Errorf("%s present = %t, want %t", name, want.got, want.want)
		}
	}
	var kept int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_session_events WHERE donor_id=$1 AND session_id IS NULL", id).Scan(&kept); err != nil ||
		kept != 1 {
		t.Fatalf("session audit rows kept without their session = %d, want 1: %v", kept, err)
	}
}
