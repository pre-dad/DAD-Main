// Command seed fills a disposable database with a synthetic donor population
// for load testing: giving histories, referral trees and visits written as
// the checkout and webhook paths store them, with projections derived by the
// projection worker's own code.
//
// It refuses any database that might hold real donors. The URL must be given
// as a flag and must itself name the server and database, since pgx fills
// omitted parts from PG* variables and service files; -confirm must repeat
// the database's name. A database holding any donor without a seeded address
// is refused outright; one already seeded is refused unless -append is set.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"dad/donor-api/internal/migrations"
)

// fixtureCodes is how many referral visits the fixture describes.
const fixtureCodes = 5000

type options struct {
	databaseURL, confirm, fixture string
	donors, sessions, workers     int
	seed                          uint64
	appendPopulation              bool
}

func parse(args []string) (options, error) {
	var o options
	flags := flag.NewFlagSet("seed", flag.ContinueOnError)
	flags.StringVar(&o.databaseURL, "database-url", "", "disposable database to fill, as a URL naming its host and database (required)")
	flags.StringVar(&o.confirm, "confirm", "", "the database's name, repeated to confirm it is disposable (required)")
	flags.IntVar(
		&o.donors,
		"donors",
		50000,
		"donors to create (1–1,000,000; the population is held in memory, about 8 KB of RSS per donor)",
	)
	flags.Uint64Var(&o.seed, "seed", 1, "random seed; the same seed and size give the same population")
	flags.BoolVar(&o.appendPopulation, "append", false, "add to a database that already holds a seeded population")
	flags.IntVar(&o.sessions, "sessions", 2000, "donor sessions to create for authenticated load (0 for none)")
	flags.StringVar(&o.fixture, "fixture", "", "new file, outside any repository, for session tokens and referral codes (mode 0600)")
	flags.IntVar(&o.workers, "workers", 8, "concurrent projection refreshes (1–64)")
	if err := flags.Parse(args); err != nil {
		return o, err
	}
	switch {
	case flags.NArg() != 0:
		return o, errors.New("unexpected arguments")
	case o.databaseURL == "" || o.confirm == "":
		return o, errors.New("-database-url and -confirm are required")
	case o.donors < 1 || o.donors > 1_000_000 || o.sessions < 0 || o.workers < 1 || o.workers > 64:
		return o, errors.New("invalid population arguments")
	case o.fixture == "" && o.sessions > 0:
		return o, errors.New("-fixture is required to hand out sessions")
	}
	name, err := database(o.databaseURL)
	if err != nil {
		return o, err
	}
	if name != o.confirm {
		return o, errors.New("-confirm must repeat the database name in -database-url")
	}
	return o, nil
}

// database returns the database a URL names, refusing one whose server or
// database pgx would take from PG* variables or a service file instead.
func database(databaseURL string) (string, error) {
	invalid := errors.New("-database-url must be a postgresql:// URL naming one host and a database")
	u, err := url.Parse(databaseURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Query().Has("service") {
		return "", invalid
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return "", invalid
	}
	c, name := config.ConnConfig, strings.TrimPrefix(u.Path, "/")
	if name == "" || c.Database != name || c.Host != u.Hostname() || strconv.Itoa(int(c.Port)) != cmp.Or(u.Port(), "5432") {
		return "", invalid
	}
	return name, nil
}

// insideRepository reports whether path would land in a git working tree,
// where a fixture of live session tokens could be committed.
func insideRepository(path string) (bool, error) {
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err == nil {
		dir, err = filepath.Abs(dir)
	}
	if err != nil {
		return false, err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return true, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false, nil
		}
		dir = parent
	}
}

// createFixture opens a new private file for session tokens, refusing an
// existing file or one inside a working tree.
func createFixture(path string) (*os.File, error) {
	inside, err := insideRepository(path)
	switch {
	case err != nil:
		return nil, fmt.Errorf("create fixture: %w", err)
	case inside:
		return nil, errors.New("-fixture must be outside any git working tree")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create fixture: %w", err)
	}
	return file, nil
}

// seedMarker is the database comment a seed leaves, which -append requires.
const seedMarker = "dad synthetic load data"

// guard returns how many seeded donors the database already holds. It
// refuses any donor without a seeded address, -append without an earlier
// seed's marker, and a first seed into a database that already has staff or
// Stripe events, as a freshly launched one might. Published content needs a
// staff author, so it is refused with them.
func guard(ctx context.Context, pool *pgxpool.Pool, appendPopulation bool) (int, error) {
	var unseeded, seeded int
	var marked, used bool
	err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE email NOT LIKE $1),count(*) FILTER (WHERE email LIKE $1),
coalesce(shobj_description((SELECT oid FROM pg_database WHERE datname=current_database()),'pg_database')=$2,false),
EXISTS(SELECT 1 FROM donor_staff) OR EXISTS(SELECT 1 FROM donor_stripe_events)
FROM donors`, "%@"+emailDomain, seedMarker).Scan(&unseeded, &seeded, &marked, &used)
	switch {
	case err != nil:
		return 0, err
	case unseeded > 0:
		return 0, fmt.Errorf("refusing: the database holds %d donors that were not seeded", unseeded)
	case appendPopulation && !marked:
		return 0, errors.New("refusing: -append needs a database an earlier seed marked")
	case !appendPopulation && (marked || seeded > 0):
		return 0, fmt.Errorf("refusing: the database already holds %d seeded donors; pass -append to add more", seeded)
	case !appendPopulation && used:
		return 0, errors.New("refusing: the database holds staff or Stripe events")
	}
	return seeded, nil
}

type report struct {
	Donors             int              `json:"donors"`
	Offset             int              `json:"offset"`
	Seed               uint64           `json:"seed"`
	MostReferrals      int64            `json:"most_referrals"`
	LongestStreak      int              `json:"longest_streak"`
	Rows               map[string]int64 `json:"rows"`
	Sessions           int              `json:"sessions"`
	SuppressedMessages int64            `json:"suppressed_messages"`
	GenerateSeconds    float64          `json:"generate_seconds"`
	CopySeconds        float64          `json:"copy_seconds"`
	ProjectionSeconds  float64          `json:"projection_seconds"`
	TotalSeconds       float64          `json:"total_seconds"`
}

// seed writes a population generated as of now into pool and derives its
// projections, after checking the database may receive it.
func seed(ctx context.Context, pool *pgxpool.Pool, o options, now time.Time) (pop *population, r report, err error) {
	r = report{Donors: o.donors, Seed: o.seed}
	if err = migrations.Verify(ctx, pool); err != nil {
		return nil, r, fmt.Errorf("verify migrations (run them first): %w", err)
	}
	if r.Offset, err = guard(ctx, pool, o.appendPopulation); err != nil {
		return nil, r, err
	}
	generated := time.Now()
	pop = generate(o.donors, r.Offset, o.seed, now)
	for _, d := range pop.donors {
		r.MostReferrals, r.LongestStreak = max(r.MostReferrals, d.referrals()), max(r.LongestStreak, d.streak)
	}
	r.GenerateSeconds = time.Since(generated).Seconds()
	copied := time.Now()
	if r.Rows, err = copyAll(ctx, pool, pop.tables()); err != nil {
		return nil, r, err
	}
	r.CopySeconds = time.Since(copied).Seconds()
	// Every projection refreshed queues mail for the history just written, so
	// suppress it however refreshing ends, interrupted included.
	defer func() {
		var suppressErr error
		// Large populations queue years of history; allow cleanup to finish after cancellation.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()
		r.SuppressedMessages, suppressErr = suppressHistory(cleanup, pool)
		err = errors.Join(err, suppressErr)
	}()
	projected := time.Now()
	err = refreshProjections(ctx, pool, o.workers)
	r.ProjectionSeconds = time.Since(projected).Seconds()
	return pop, r, err
}

func run(ctx context.Context, args []string, out io.Writer) (err error) {
	o, err := parse(args)
	if err != nil {
		return err
	}
	var fixtureFile *os.File
	if o.fixture != "" {
		if fixtureFile, err = createFixture(o.fixture); err != nil {
			return err
		}
		defer func() {
			if closeErr := fixtureFile.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				_ = os.Remove(o.fixture)
			}
		}()
	}
	config, err := pgxpool.ParseConfig(o.databaseURL)
	if err != nil {
		return err
	}
	config.MaxConns = int32(o.workers + 2)
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err = pool.Ping(ctx); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	started := time.Now()
	pop, r, err := seed(ctx, pool, o, started)
	if err != nil {
		return err
	}
	if fixtureFile != nil {
		if r.Sessions, err = pop.writeFixture(ctx, pool, o.sessions, fixtureFile); err != nil {
			return err
		}
	}
	r.TotalSeconds = time.Since(started).Seconds()
	return json.NewEncoder(out).Encode(r)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
