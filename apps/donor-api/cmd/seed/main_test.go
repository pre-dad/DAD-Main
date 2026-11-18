package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/dbroles/dbrolestest"
	"dad/donor-api/internal/httpapi"
	"dad/donor-api/internal/migrations"
	"dad/donor-api/internal/recognition"
	"dad/donor-api/internal/store"
)

func TestParseRequiresAnExplicitConfirmedDatabase(t *testing.T) {
	const url = "postgresql://seed:seed@127.0.0.1:5432/loadtest_a"
	if o, err := parse([]string{"-database-url", url, "-confirm", "loadtest_a", "-fixture", "/tmp/x.json"}); err != nil ||
		o.donors != 50000 ||
		o.sessions != 2000 {
		t.Fatalf("valid invocation rejected: %v", err)
	}
	for name, args := range map[string][]string{
		"no url":             {"-confirm", "loadtest_a", "-sessions", "0"},
		"no confirmation":    {"-database-url", url, "-sessions", "0"},
		"wrong confirmation": {"-database-url", url, "-confirm", "loadtest", "-sessions", "0"},
		"sessions, no file":  {"-database-url", url, "-confirm", "loadtest_a"},
		"no donors":          {"-database-url", url, "-confirm", "loadtest_a", "-sessions", "0", "-donors", "0"},
		"too many donors":    {"-database-url", url, "-confirm", "loadtest_a", "-sessions", "0", "-donors", "1000001"},
		"stray argument":     {"-database-url", url, "-confirm", "loadtest_a", "-sessions", "0", "now"},
	} {
		if _, err := parse(args); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestDatabaseIgnoresTheEnvironment checks that the URL alone decides which
// server and database are seeded, whatever PG* variables would supply.
func TestDatabaseIgnoresTheEnvironment(t *testing.T) {
	t.Setenv("PGHOST", "127.0.0.1")
	t.Setenv("PGDATABASE", "loadtest_a")
	if name, err := database("postgres://seed@127.0.0.1/loadtest_a?sslmode=disable"); err != nil || name != "loadtest_a" {
		t.Fatalf("explicit URL rejected: %q %v", name, err)
	}
	for _, invalid := range []string{
		"postgresql:///loadtest_a",
		"postgresql://127.0.0.1:5432/",
		"postgresql://127.0.0.1:5432",
		"postgresql://127.0.0.1:5432/loadtest_a?host=db.internal",
		"postgresql://127.0.0.1:5432/loadtest_a?dbname=production",
		"postgresql://127.0.0.1:5432/loadtest_a?port=6543",
		"postgresql://127.0.0.1:5432/loadtest_a?service=production",
		"postgresql://a.internal,b.internal/loadtest_a",
		"host=127.0.0.1 dbname=loadtest_a",
	} {
		if _, err := database(invalid); err == nil {
			t.Errorf("%q accepted", invalid)
		}
	}
	t.Setenv("PGPORT", "6543")
	if _, err := database("postgresql://127.0.0.1/loadtest_a"); err == nil {
		t.Error("a port taken from PGPORT was accepted")
	}
}

func TestFixtureMustStayOutOfRepositories(t *testing.T) {
	repository := t.TempDir()
	deep := filepath.Join(repository, "deep")
	if err := os.MkdirAll(filepath.Join(repository, ".git"), 0o700); err != nil || os.Mkdir(deep, 0o700) != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Symlink(deep, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(deep, "fixture.json"), filepath.Join(link, "fixture.json")} {
		if inside, err := insideRepository(path); err != nil || !inside {
			t.Fatalf("a fixture inside a working tree was allowed at %s: %v", path, err)
		}
	}
	if inside, err := insideRepository(filepath.Join(t.TempDir(), "fixture.json")); err != nil || inside {
		t.Fatal("a fixture outside any working tree was refused")
	}
	err := run(
		context.Background(),
		[]string{
			"-database-url",
			"postgresql://x@127.0.0.1/loadtest_a",
			"-confirm",
			"loadtest_a",
			"-fixture",
			filepath.Join(link, "f.json"),
		},
		io.Discard,
	)
	if err == nil || !strings.Contains(err.Error(), "outside any git working tree") {
		t.Fatalf("run wrote a fixture into a repository: %v", err)
	}
}

// isolatedDatabase creates a migrated database next to the donor_go test
// database, since seeding refuses one that other tests have written donors to.
func isolatedDatabase(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	config, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil || (config.ConnConfig.Database != "donor_go" && !strings.HasPrefix(config.ConnConfig.Database, "donor_go_")) {
		t.Fatal("TEST_DATABASE_URL must name the dedicated donor_go test database")
	}
	ctx := context.Background()
	admin, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	var suffix [8]byte
	if _, err = rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	name := "donor_seed_test_" + hex.EncodeToString(suffix[:])
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	})
	config = config.Copy()
	config.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = migrations.Apply(ctx, pool, false); err != nil {
		t.Fatal(err)
	}
	location, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	location.Path = "/" + name
	return pool, location.String()
}

// TestGuardRefusesDatabasesThatMayBeLive covers a migrated database that has
// started live use before any donor gave.
func TestGuardRefusesDatabasesThatMayBeLive(t *testing.T) {
	pool, _ := isolatedDatabase(t)
	ctx := context.Background()
	if _, err := guard(ctx, pool, false); err != nil {
		t.Fatalf("an unused database was refused: %v", err)
	}
	if _, err := guard(ctx, pool, true); err == nil || !strings.Contains(err.Error(), "marked") {
		t.Fatalf("appended to a database no seed marked: %v", err)
	}
	for name, insert := range map[string]string{
		"Stripe event": "INSERT INTO donor_stripe_events(id,stripe_event_id,event_type,occurred_at,data) VALUES(gen_random_uuid(),'evt_live','invoice.paid',now(),'{}')",
		"staff member": "INSERT INTO donor_staff(id,email,name,role) VALUES(gen_random_uuid(),'admin@example.org','Admin','SUPER_ADMIN')",
	} {
		if _, err := pool.Exec(ctx, insert); err != nil {
			t.Fatal(err)
		}
		if _, err := guard(ctx, pool, false); err == nil || !strings.Contains(err.Error(), "staff or Stripe events") {
			t.Errorf("seeded a database holding a %s: %v", name, err)
		}
		if _, err := pool.Exec(ctx, "TRUNCATE donor_stripe_events, donor_staff CASCADE"); err != nil {
			t.Fatal(err)
		}
	}
}

// TestInterruptedSeedLeavesNoMailToSend cancels a seed once projections have
// started queueing mail for its history.
func TestInterruptedSeedLeavesNoMailToSend(t *testing.T) {
	pool, _ := isolatedDatabase(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for ctx.Err() == nil {
			var queued bool
			if pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM donor_messages)").Scan(&queued) == nil && queued {
				cancel()
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	_, r, err := seed(ctx, pool, options{donors: 3000, seed: 2, workers: 1}, time.Now())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("the seed was not interrupted while projecting: %v", err)
	}
	var pending, due int
	if err = pool.QueryRow(context.Background(), "SELECT (SELECT count(*) FROM donor_messages WHERE completed_at IS NULL),(SELECT count(*) FROM donor_metric_projections WHERE next_refresh_at<=now())").Scan(&pending, &due); err != nil {
		t.Fatal(err)
	}
	if pending != 0 || r.SuppressedMessages == 0 || due == 0 {
		t.Fatalf("interrupted with %d projections due: %d messages pending, %d suppressed", due, pending, r.SuppressedMessages)
	}
}

// TestSeededDonorsAreServedFromTheirFacts seeds a small population and reads
// every claimed donor's dashboard and monthly gift through the API, comparing
// the projections the worker derived with what the generator meant the facts
// to say. The API has no processor: donor reads must not need one.
func TestSeededDonorsAreServedFromTheirFacts(t *testing.T) {
	pool, location := isolatedDatabase(t)
	ctx := context.Background()
	// A cutoff inside the seeded history awards some givers and not others,
	// whichever side of the real cutoff the test runs on.
	now := time.Now()
	defer func(cutoff time.Time) { recognition.FoundingCutoff = cutoff }(recognition.FoundingCutoff)
	recognition.FoundingCutoff = now.Add(-60 * 24 * time.Hour)
	o := options{donors: 200, seed: 7, workers: 4}
	pop, r, err := seed(ctx, pool, o, now)
	if err != nil {
		t.Fatal(err)
	}
	if r.Rows["donors"] != 200 || r.Rows["donor_gifts"] == 0 || r.SuppressedMessages == 0 || r.LongestStreak == 0 {
		t.Fatalf("unexpected seed report: %+v", r)
	}
	var due, pending, early int
	if err = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM donor_metric_projections WHERE next_refresh_at<=now()),
(SELECT count(*) FROM donor_messages WHERE completed_at IS NULL),
(SELECT count(*) FROM donor_billing_observations o JOIN donor_billing_periods p ON p.id=o.period_id WHERE o.occurred_at<p.created_at)`).Scan(&due, &pending, &early); err != nil || due != 0 || pending != 0 || early != 0 {
		t.Fatalf(
			"seeding left %d projections due, %d messages to send and %d periods recorded after their first observation: %v",
			due,
			pending,
			early,
			err,
		)
	}

	sessions, err := pop.sessions(ctx, pool, len(pop.donors))
	if err != nil || len(sessions) < 50 {
		t.Fatalf("%d sessions: %v", len(sessions), err)
	}
	api, err := store.Open(ctx, dbrolestest.As(t, location, dbroles.API), dbroles.API.Pool)
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	const origin = "https://donor.example.org"
	server := httpapi.NewServer(
		"",
		httpapi.Services{
			Origin:    origin,
			Dashboard: store.DashboardReader{DB: api},
			Donor:     store.DonorService{DB: api, Origin: origin},
			Recurring: store.RecurringService{DB: api, Origin: origin},
		},
	)
	get := func(path, token string, native bool, into any) int {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		if native {
			request.Header.Set("X-DAD-Client", "native")
			request.Header.Set("Authorization", "Bearer "+token)
		} else if token != "" {
			request.AddCookie(&http.Cookie{Name: "dad_donor_session", Value: token})
		}
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, request)
		if response.Code == http.StatusOK {
			if err := json.Unmarshal(response.Body.Bytes(), into); err != nil {
				t.Fatal(err)
			}
		}
		return response.Code
	}
	checkedStreaks, referrers, subscriptions, founders := 0, 0, 0, map[bool]int{}
	for i, s := range sessions {
		d := s.donor
		var dashboard store.DonorDashboard
		if status := get("/donor/me/dashboard", s.token, i%2 == 0, &dashboard); status != http.StatusOK || dashboard.State != "ready" {
			t.Fatalf("donor %d dashboard: %d %+v", d.index, status, dashboard)
		}
		if want := d.firstGift().paidAt.Before(recognition.FoundingCutoff); dashboard.FoundingDonor != want {
			t.Errorf("donor %d founding donor %v, want %v", d.index, dashboard.FoundingDonor, want)
		}
		founders[dashboard.FoundingDonor]++
		link, irl := d.referred["LINK"], d.referred["IRL"]
		if got := dashboard.Referrals; got == nil || got.Link != link.count || got.LinkMinor != link.minor || got.IRL != irl.count ||
			got.IRLMinor != irl.minor {
			t.Errorf("donor %d referrals %+v, want link %+v irl %+v", d.index, got, link, irl)
		}
		if d.referrals() > 0 {
			referrers++
		}
		if d.streakKnown {
			checkedStreaks++
			if got := dashboard.Streak; got == nil || got.Current != int32(d.streak) || got.AtRisk != d.atRisk {
				t.Errorf("donor %d streak %+v, want %d at risk %v", d.index, got, d.streak, d.atRisk)
			}
		}
		var gifts store.GiftsPage
		if status := get("/donor/me/gifts?offset=0", s.token, false, &gifts); status != http.StatusOK ||
			len(gifts.Gifts) != min(50, len(d.gifts())) {
			t.Errorf("donor %d gift history: %d with %d gifts, want %d", d.index, status, len(gifts.Gifts), len(d.gifts()))
		}
		var latest *checkout
		for _, c := range d.checkouts {
			if c.frequency == "MONTHLY" && c.status != "EXPIRED" && (latest == nil || c.createdAt.After(latest.createdAt)) {
				latest = c
			}
		}
		var recurring store.RecurringDetails
		if status := get("/donor/me/recurring", s.token, i%2 == 1, &recurring); status != http.StatusOK {
			t.Fatalf("donor %d monthly gift: %d", d.index, status)
		}
		if got := recurring.Subscription; latest == nil || latest.sub == nil {
			if got != nil {
				t.Errorf("donor %d shows a monthly gift it never had: %+v", d.index, got)
			}
		} else {
			subscriptions++
			if got == nil || recurring.ObservedAt == nil || got.Status != latest.sub.status || !got.PeriodEnd.Equal(latest.sub.periodEnd) || (got.StopsAt != nil) != !latest.sub.stopsAt.IsZero() {
				t.Errorf("donor %d monthly gift %+v, want %+v", d.index, got, *latest.sub)
			}
		}
		if d.code != "" {
			var referrer map[string]string
			if status := get("/donor/referrals/"+d.code, "", false, &referrer); status != http.StatusOK ||
				referrer["public_name"] != d.publicName {
				t.Errorf("referral landing for donor %d: %d %v", d.index, status, referrer)
			}
		}
	}
	if checkedStreaks < len(sessions)*9/10 || referrers == 0 || subscriptions == 0 || founders[true] == 0 || founders[false] == 0 {
		t.Fatalf(
			"too little coverage: %d streaks, %d referrers, %d monthly gifts and founders %v among %d donors",
			checkedStreaks,
			referrers,
			subscriptions,
			founders,
			len(sessions),
		)
	}

	name := pool.Config().ConnConfig.Database
	fixturePath := filepath.Join(t.TempDir(), "fixture.json")
	refused := []string{"-database-url", location, "-confirm", name, "-donors", "10", "-sessions", "1", "-fixture", fixturePath}
	if err = run(ctx, refused, io.Discard); err == nil || !strings.Contains(err.Error(), "-append") {
		t.Fatalf("a second population was seeded without -append: %v", err)
	}
	if _, err = os.Stat(fixturePath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a refused seed left its fixture behind: %v", err)
	}
	args := []string{
		"-database-url",
		location,
		"-confirm",
		name,
		"-donors",
		"60",
		"-append",
		"-sessions",
		"5",
		"-workers",
		"2",
		"-fixture",
		fixturePath,
	}
	if err = run(ctx, args, io.Discard); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(fixturePath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("fixture must be private: %v %v", info, err)
	}
	var f fixture
	if data, err := os.ReadFile(fixturePath); err != nil || json.Unmarshal(data, &f) != nil || len(f.Sessions) != 5 ||
		len(f.ReferralCodes) != fixtureCodes {
		t.Fatalf("fixture holds %d sessions and %d codes: %v", len(f.Sessions), len(f.ReferralCodes), err)
	}
	if err = run(ctx, args, io.Discard); err == nil {
		t.Fatal("an existing fixture was overwritten")
	}

	if _, err = pool.Exec(ctx, "INSERT INTO donors(id,email) VALUES(gen_random_uuid(),'person@example.org')"); err != nil {
		t.Fatal(err)
	}
	o.appendPopulation = true
	if _, _, err = seed(ctx, pool, o, time.Now()); err == nil || !strings.Contains(err.Error(), "not seeded") {
		t.Fatalf("seeded a database holding a real donor: %v", err)
	}
}

func TestCopyFailureStopsProductionAndRollsBackTheSeedMarker(t *testing.T) {
	pool, _ := isolatedDatabase(t)
	ctx := context.Background()
	produced := 0
	tables := []table{{"donors", "id email", func(emit func(...any) bool) {
		for range 10000 {
			produced++
			if !emit("invalid UUID", "synthetic@example.invalid") {
				return
			}
		}
	}}}
	if _, err := copyAll(ctx, pool, tables); err == nil {
		t.Fatal("invalid COPY succeeded")
	}
	if produced >= 10000 {
		t.Fatal("failed COPY continued producing the whole population")
	}
	var count int
	var marked bool
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM donors),
 coalesce(shobj_description((SELECT oid FROM pg_database WHERE datname=current_database()),'pg_database')=$1,false)`, seedMarker).Scan(&count, &marked); err != nil {
		t.Fatal(err)
	}
	if count != 0 || marked {
		t.Fatalf("failed seed retained %d donors or marker=%v", count, marked)
	}
}
