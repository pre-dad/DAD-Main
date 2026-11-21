package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"dad/donor-api/internal/credentials"
	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/dbroles/dbrolestest"
	"dad/donor-api/internal/migrations"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testDatabaseName reports whether name is a dedicated test database, which
// integration tests may freely write to.
func testDatabaseName(name string) bool {
	return name == "donor_go" || strings.HasPrefix(name, "donor_go_")
}

func database(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Fatal("TEST_DATABASE_URL is required; use the migrated donor_go test database")
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	if !testDatabaseName(config.ConnConfig.Database) {
		t.Fatal("integration tests require a dedicated donor_go database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := Open(ctx, url, 10)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = migrations.Verify(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

// as returns the test's pool acting as role, opened on first use, so the
// code under test runs with exactly the privileges of the process that runs
// it; fixtures and assertions use the owner's pool from database.
func as(t *testing.T, role dbroles.Role) *pgxpool.Pool {
	t.Helper()
	rolePools.Lock()
	defer rolePools.Unlock()
	key := rolePool{t, role.Name}
	if pool := rolePools.open[key]; pool != nil {
		return pool
	}
	pool := dbrolestest.Open(t, role)
	rolePools.open[key] = pool
	t.Cleanup(func() {
		rolePools.Lock()
		defer rolePools.Unlock()
		delete(rolePools.open, key)
	})
	return pool
}

type rolePool struct {
	t    *testing.T
	role string
}

var rolePools = struct {
	sync.Mutex
	open map[rolePool]*pgxpool.Pool
}{open: map[rolePool]*pgxpool.Pool{}}

// actAs makes the rest of tx run as role, after fixtures written as the
// owner; RESET ROLE returns to the owner.
func actAs(t *testing.T, tx pgx.Tx, role dbroles.Role) {
	t.Helper()
	mustExec(t, tx, "SET LOCAL ROLE "+pgx.Identifier{role.Name}.Sanitize())
}

func keyring(t *testing.T, value string) credentials.Keyring {
	t.Helper()
	keys, err := credentials.ParseKeyring(value)
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func donor(t *testing.T, pool *pgxpool.Pool) (pgtype.UUID, string) {
	t.Helper()
	ctx := context.Background()
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	email := HashToken(string(id.Bytes[:])) + "@example.org"
	if err = queries.New(pool).InsertGuest(ctx, queries.InsertGuestParams{ID: id, Email: email}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, query := range []string{
			"DELETE FROM donor_session_events WHERE donor_id=$1",
			"DELETE FROM donor_sessions WHERE donor_id=$1",
			"DELETE FROM donor_login_links WHERE donor_id=$1",
			"DELETE FROM donor_metric_projections WHERE donor_id=$1",
			"DELETE FROM donor_payment_methods WHERE donor_id=$1",
			"DELETE FROM donor_device_marks WHERE donor_id=$1",
			"DELETE FROM donors WHERE id=$1",
		} {
			if _, err := pool.Exec(ctx, query, id); err != nil {
				t.Error(err)
			}
		}
	})
	return id, email
}

func link(t *testing.T, pool *pgxpool.Pool, id pgtype.UUID, now time.Time) string {
	t.Helper()
	key, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	token := HashToken(string(key.Bytes[:]))
	_, err = pool.Exec(
		context.Background(),
		"INSERT INTO donor_login_links (id,donor_id,token_hash,expires_at) VALUES ($1,$2,$3,$4)",
		key,
		id,
		HashToken(token),
		now.Add(15*time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestOpenSizesThePoolFromTheDatabaseURL(t *testing.T) {
	database(t)
	url := os.Getenv("TEST_DATABASE_URL")
	if strings.Contains(url, "pool_max_conns") {
		t.Skip("TEST_DATABASE_URL already sets pool_max_conns")
	}
	separator := "?"
	if strings.Contains(url, "?") {
		separator = "&"
	}
	for dsn, want := range map[string]int32{url: 7, url + separator + "pool_max_conns=3": 3} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		pool, err := Open(ctx, dsn, 7)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if got := pool.Config().MaxConns; got != want {
			t.Errorf("pool of %d connections, want %d", got, want)
		}
		pool.Close()
	}
}

func TestClaimIsSingleUseUnderConcurrency(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	id, _ := donor(t, pool)
	now := time.Now().UTC()
	token := link(t, pool, id, now)
	sibling := link(t, pool, id, now)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	sessions := make(chan string, 2)
	start := make(chan struct{})
	for range 2 {
		wg.Go(func() {
			<-start
			ctx := context.Background()
			tx, err := api.Begin(ctx)
			if err != nil {
				results <- err
				return
			}
			defer tx.Rollback(ctx)
			tokens, _, err := claim(ctx, tx, token, Client{})
			if err == nil {
				err = tx.Commit(ctx)
				if err == nil {
					sessions <- tokens.Access
				}
			}
			results <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)
	close(sessions)
	successes, denied := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrDenied) {
			denied++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || denied != 1 {
		t.Fatalf("success=%d denied=%d", successes, denied)
	}
	for session := range sessions {
		got, err := authenticate(context.Background(), queries.New(api), session, now)
		if err != nil || got.ID != id {
			t.Fatalf("authentication: %v", err)
		}
		if len(session) != 43 {
			t.Fatalf("token encoding changed")
		}
		if _, err = authenticate(context.Background(), queries.New(api), session, time.Now().Add(30*24*time.Hour)); !errors.Is(
			err,
			ErrDenied,
		) {
			t.Fatal("session expiry boundary")
		}
	}
	tx, err := api.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, _, err = claim(context.Background(), tx, sibling, Client{}); !errors.Is(err, ErrDenied) {
		t.Fatalf("sibling remains usable: %v", err)
	}
}

func TestClaimRollbackLeavesProofUsable(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	id, _ := donor(t, pool)
	now := time.Now().UTC()
	token := link(t, pool, id, now)
	ctx := context.Background()
	tx, err := api.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tokens, _, err := claim(ctx, tx, token, Client{})
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = authenticate(ctx, queries.New(api), tokens.Access, now); !errors.Is(err, ErrDenied) {
		t.Fatalf("rolled back session: %v", err)
	}
	tx, err = api.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, _, err = claim(ctx, tx, token, Client{}); err != nil {
		t.Fatal(err)
	}
}

func TestExistingTokenHashEncoding(t *testing.T) {
	if HashToken("test") != "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08" {
		t.Fatal("invalid token hash")
	}
}

func TestDisabledDonorCannotClaim(t *testing.T) {
	pool := database(t)
	id, _ := donor(t, pool)
	now := time.Now().UTC()
	token := link(t, pool, id, now)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "UPDATE donors SET disabled=true WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	tx, err := as(t, dbroles.API).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, _, err = claim(ctx, tx, token, Client{}); !errors.Is(err, ErrDenied) {
		t.Fatalf("disabled donor claimed: %v", err)
	}
}

func TestExpiredProofCannotCreateSession(t *testing.T) {
	pool := database(t)
	id, _ := donor(t, pool)
	now := time.Now().UTC()
	token := link(t, pool, id, now.Add(-15*time.Minute))
	ctx := context.Background()
	tx, err := as(t, dbroles.API).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, _, err = claim(ctx, tx, token, Client{}); !errors.Is(err, ErrDenied) {
		t.Fatalf("expiry boundary: %v", err)
	}
	var count int
	if err = tx.QueryRow(ctx, "SELECT count(*) FROM donor_sessions WHERE donor_id=$1", id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("expired proof created a session")
	}
}
