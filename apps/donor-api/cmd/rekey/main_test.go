package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"dad/donor-api/internal/credentials"
	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/dbroles/dbrolestest"
	"dad/donor-api/internal/migrations"
	"dad/donor-api/internal/store"
	"dad/donor-api/internal/store/queries"
)

var (
	oldKey     = base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	newKey     = base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	unknownKey = base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))
)

func TestRunRejectsInvalidInvocationBeforeTheDatabase(t *testing.T) {
	env := map[string]string{
		"DONOR_REKEY_DATABASE_URL":     "postgresql://unreachable.invalid/donor",
		"DONOR_EMAIL_ENCRYPTION_KEY":   oldKey,
		"DONOR_CONTENT_ENCRYPTION_KEY": oldKey,
	}
	for _, tc := range []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"no key setting", nil, nil, "usage"},
		{"unknown key setting", []string{"--key", "staff"}, nil, "usage"},
		{"stray argument", []string{"--key", "email", "now"}, nil, "usage"},
		{"empty batch", []string{"--key", "email", "--batch", "0"}, nil, "usage"},
		{"oversized batch", []string{"--key", "email", "--batch", "10001"}, nil, "usage"},
		{"unset keys", []string{"--key", "content"}, map[string]string{"DONOR_CONTENT_ENCRYPTION_KEY": ""}, "DONOR_CONTENT_ENCRYPTION_KEY key 1 of 1"},
		{"invalid retired key", []string{"--key", "email"}, map[string]string{"DONOR_EMAIL_ENCRYPTION_KEY": newKey + ",short"}, "DONOR_EMAIL_ENCRYPTION_KEY key 2 of 2"},
		{"no database", []string{"--key", "email"}, map[string]string{"DONOR_REKEY_DATABASE_URL": ""}, "DONOR_REKEY_DATABASE_URL"},
	} {
		getenv := func(name string) string {
			if value, ok := tc.env[name]; ok {
				return value
			}
			return env[name]
		}
		err := run(context.Background(), tc.args, getenv, io.Discard)
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), newKey) {
			t.Errorf("%s: got %v, want an error naming %q without key material", tc.name, err, tc.want)
		}
	}
}

// isolatedDatabase creates a migrated database next to the donor_go test
// database, since a rekey reads whole tables that other tests write to.
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
	name := "donor_rekey_test_" + hex.EncodeToString(suffix[:])
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

func keyring(t *testing.T, value string) credentials.Keyring {
	t.Helper()
	keys, err := credentials.ParseKeyring(value)
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func encrypt(t *testing.T, keys credentials.Keyring, plaintext string) []byte {
	t.Helper()
	token, err := keys.Encrypt([]byte(plaintext))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func newID(t *testing.T) pgtype.UUID {
	t.Helper()
	id, err := store.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// fixture holds the plaintext of every token written, by table and id.
type fixture map[string]map[pgtype.UUID]string

// seed writes logins login-email tokens, two email-change tokens, and two
// source documents, each encrypted under keys, and a completed login email
// with no token.
func seed(t *testing.T, pool *pgxpool.Pool, keys credentials.Keyring, logins int) fixture {
	t.Helper()
	ctx := context.Background()
	f := fixture{"donor_login_emails": {}, "donor_email_changes": {}, "donor_content_sources": {}}
	donor := newID(t)
	if err := queries.New(pool).InsertGuest(ctx, queries.InsertGuestParams{ID: donor, Email: "rekey@example.org"}); err != nil {
		t.Fatal(err)
	}
	for i := range logins {
		id, plaintext := newID(t), fmt.Sprintf("login token %d", i)
		if _, err := pool.Exec(ctx, "INSERT INTO donor_login_emails (id,donor_id,encrypted_token,expires_at,available_at) VALUES ($1,$2,$3,now(),now())", id, donor, encrypt(t, keys, plaintext)); err != nil {
			t.Fatal(err)
		}
		f["donor_login_emails"][id] = plaintext
	}
	if _, err := pool.Exec(ctx, "INSERT INTO donor_login_emails (id,donor_id,available_at,completed_at) VALUES ($1,$2,now(),now())", newID(t), donor); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		id, plaintext := newID(t), fmt.Sprintf("email change proof %d", i)
		changing, email := donor, "rekey@example.org"
		if i > 0 {
			changing, email = newID(t), fmt.Sprintf("rekey%d@example.org", i)
			if err := queries.New(pool).InsertGuest(ctx, queries.InsertGuestParams{ID: changing, Email: email}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := pool.Exec(ctx, "INSERT INTO donor_email_changes (id,donor_id,old_email,new_email,stage,token_hash,encrypted_token,expires_at) VALUES ($1,$2,$3,$4,'CURRENT',$5,$6,now())", id, changing, email, fmt.Sprintf("new%d@example.org", i), store.HashToken(plaintext), encrypt(t, keys, plaintext)); err != nil {
			t.Fatal(err)
		}
		f["donor_email_changes"][id] = plaintext
	}
	staff := newID(t)
	if _, err := pool.Exec(ctx, "INSERT INTO donor_staff (id,email,name,role) VALUES ($1,'admin@example.org','Admin','SUPER_ADMIN')", staff); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		id, plaintext := newID(t), fmt.Sprintf("source document %d", i)
		if _, err := pool.Exec(ctx, "INSERT INTO donor_content_sources (id,actor_id,name,sha256,encrypted_data) VALUES ($1,$2,'source.txt',$3,$4)", id, staff, strings.Repeat("0", 64), encrypt(t, keys, plaintext)); err != nil {
			t.Fatal(err)
		}
		f["donor_content_sources"][id] = plaintext
	}
	return f
}

func tokens(t *testing.T, pool *pgxpool.Pool, column store.EncryptedColumn) map[pgtype.UUID][]byte {
	t.Helper()
	rows, err := pool.Query(
		context.Background(),
		"SELECT id,"+pgx.Identifier{
			column.Column,
		}.Sanitize()+" FROM "+pgx.Identifier{
			column.Table,
		}.Sanitize()+" WHERE "+pgx.Identifier{
			column.Column,
		}.Sanitize()+" IS NOT NULL",
	)
	if err != nil {
		t.Fatal(err)
	}
	out := map[pgtype.UUID][]byte{}
	var id pgtype.UUID
	var token []byte
	if _, err = pgx.ForEachRow(rows, []any{&id, &token}, func() error {
		out[id] = token
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// verify requires every token in columns to decrypt under keys alone to the
// plaintext the fixture wrote.
func (f fixture) verify(t *testing.T, pool *pgxpool.Pool, keys credentials.Keyring, columns []store.EncryptedColumn) {
	t.Helper()
	for _, column := range columns {
		stored := tokens(t, pool, column)
		if len(stored) != len(f[column.Table]) {
			t.Fatalf("%s holds %d tokens, want %d", column.Table, len(stored), len(f[column.Table]))
		}
		for id, token := range stored {
			plaintext, err := keys.Decrypt(token)
			if err != nil || string(plaintext) != f[column.Table][id] {
				t.Fatalf("%s %s does not decrypt to what was written: %v", column.Table, id, err)
			}
		}
	}
}

// env runs rekey as its role on the database dsn names.
func env(t *testing.T, dsn, emailKeys, contentKeys string) func(string) string {
	values := map[string]string{
		dbroles.Rekey.Variable():       dbrolestest.As(t, dsn, dbroles.Rekey),
		dbroles.AllowOwnerVariable:     "true",
		"DONOR_EMAIL_ENCRYPTION_KEY":   emailKeys,
		"DONOR_CONTENT_ENCRYPTION_KEY": contentKeys,
	}
	return func(name string) string { return values[name] }
}

// rekey runs the command and returns its summary by table.
func rekey(ctx context.Context, getenv func(string) string, args ...string) (map[string]store.KeyUsage, error) {
	var out bytes.Buffer
	err := run(ctx, args, getenv, &out)
	var usage []store.KeyUsage
	if decodeErr := json.Unmarshal(out.Bytes(), &usage); decodeErr != nil {
		return nil, fmt.Errorf("summary is not JSON: %w (run: %w)", decodeErr, err)
	}
	byTable := map[string]store.KeyUsage{}
	for _, u := range usage {
		byTable[u.Table] = u
	}
	return byTable, err
}

func TestRekeyMovesEveryTokenToTheNewKey(t *testing.T) {
	pool, dsn := isolatedDatabase(t)
	ctx := context.Background()
	f := seed(t, pool, keyring(t, oldKey), 23)
	rotated := newKey + "," + oldKey
	f.verify(t, pool, keyring(t, rotated), store.EmailKeyColumns)
	written := newID(t)
	donor, err := queries.New(pool).DonorByEmail(ctx, "rekey@example.org")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, "INSERT INTO donor_login_emails (id,donor_id,encrypted_token,expires_at,available_at) VALUES ($1,$2,$3,now(),now())", written, donor.ID, encrypt(t, keyring(t, rotated), "written after the new key")); err != nil {
		t.Fatal(err)
	}
	f["donor_login_emails"][written] = "written after the new key"

	usage, err := rekey(ctx, env(t, dsn, rotated, oldKey), "--key", "email", "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "run rekey without --dry-run") {
		t.Fatalf("dry run with retired tokens: got %v", err)
	}
	if got := usage["donor_login_emails"]; got.Tokens != 24 || got.Retired != 23 || got.Reencrypted != 0 {
		t.Fatalf("login emails: %+v", got)
	}
	if got := usage["donor_email_changes"]; got.Tokens != 2 || got.Retired != 2 {
		t.Fatalf("email changes: %+v", got)
	}
	if _, ok := usage["donor_content_sources"]; ok {
		t.Fatal("email rekey surveyed source documents")
	}
	f.verify(t, pool, keyring(t, oldKey+","+newKey), store.EmailKeyColumns)

	usage, err = rekey(ctx, env(t, dsn, rotated, oldKey), "--key", "email", "--batch", "5")
	if err != nil {
		t.Fatal(err)
	}
	if got := usage["donor_login_emails"]; got.Retired != 23 || got.Reencrypted != 23 {
		t.Fatalf("login emails: %+v", got)
	}
	if got := usage["donor_email_changes"]; got.Reencrypted != 2 {
		t.Fatalf("email changes: %+v", got)
	}
	f.verify(t, pool, keyring(t, newKey), store.EmailKeyColumns)
	if _, err = rekey(ctx, env(t, dsn, newKey, oldKey), "--key", "email", "--dry-run"); err != nil {
		t.Fatalf("dry run after rekey with only the new key: %v", err)
	}

	before := tokens(t, pool, store.EmailKeyColumns[0])
	if usage, err = rekey(ctx, env(t, dsn, rotated, oldKey), "--key", "email"); err != nil || usage["donor_login_emails"].Reencrypted != 0 {
		t.Fatalf("second run: %+v, %v", usage, err)
	}
	for id, token := range tokens(t, pool, store.EmailKeyColumns[0]) {
		if !bytes.Equal(token, before[id]) {
			t.Fatal("second run rewrote a token already on the new key")
		}
	}

	f.verify(t, pool, keyring(t, oldKey), store.ContentKeyColumns)
	if usage, err = rekey(ctx, env(t, dsn, newKey, newKey+","+oldKey), "--key", "content", "--batch", "1"); err != nil ||
		usage["donor_content_sources"].Reencrypted != 2 {
		t.Fatalf("content rekey: %+v, %v", usage, err)
	}
	f.verify(t, pool, keyring(t, newKey), store.ContentKeyColumns)
}

func TestRekeyRefusesTokensNoConfiguredKeyDecrypts(t *testing.T) {
	pool, dsn := isolatedDatabase(t)
	ctx := context.Background()
	f := seed(t, pool, keyring(t, oldKey), 3)
	var stray pgtype.UUID
	for id := range f["donor_email_changes"] {
		stray = id
		break
	}
	if _, err := pool.Exec(ctx, "UPDATE donor_email_changes SET encrypted_token=$2 WHERE id=$1", stray, encrypt(t, keyring(t, unknownKey), "secret proof")); err != nil {
		t.Fatal(err)
	}
	before := tokens(t, pool, store.EmailKeyColumns[0])
	for _, args := range [][]string{{"--key", "email", "--dry-run"}, {"--key", "email"}} {
		usage, err := rekey(ctx, env(t, dsn, newKey+","+oldKey, oldKey), args...)
		if err == nil || !strings.Contains(err.Error(), "donor_email_changes "+stray.String()) ||
			strings.Contains(err.Error(), "secret proof") {
			t.Fatalf("%v: got %v, want a refusal naming the row and not its plaintext", args, err)
		}
		if got := usage["donor_email_changes"]; got.Undecryptable != 1 || got.Retired != 1 || got.Reencrypted != 0 {
			t.Fatalf("%v: email changes %+v", args, got)
		}
	}
	for id, token := range tokens(t, pool, store.EmailKeyColumns[0]) {
		if !bytes.Equal(token, before[id]) {
			t.Fatal("a refused rekey changed a token")
		}
	}
}

// awaitLockWait returns once a session of pool's database is waiting for a
// row lock.
func awaitLockWait(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		var waiting int
		if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'").Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
	}
	t.Fatal("rekey never waited for the locked row")
}

func orderedLogins(t *testing.T, pool *pgxpool.Pool) []pgtype.UUID {
	t.Helper()
	rows, err := pool.Query(context.Background(), "SELECT id FROM donor_login_emails WHERE encrypted_token IS NOT NULL ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[pgtype.UUID])
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestInterruptedRekeyResumes(t *testing.T) {
	pool, dsn := isolatedDatabase(t)
	ctx := context.Background()
	f := seed(t, pool, keyring(t, oldKey), 10)
	ids := orderedLogins(t, pool)
	held, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Rollback(ctx)
	if _, err = held.Exec(ctx, "SELECT 1 FROM donor_login_emails WHERE id=$1 FOR UPDATE", ids[4]); err != nil {
		t.Fatal(err)
	}
	interrupt, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- run(interrupt, []string{"--key", "email", "--batch", "3"}, env(t, dsn, newKey+","+oldKey, oldKey), io.Discard)
	}()
	awaitLockWait(t, pool)
	cancel()
	if err = <-done; err == nil || err.Error() != "interrupted; run again to resume" {
		t.Fatalf("got %v, want an interruption", err)
	}
	if err = held.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	rotated := keyring(t, newKey+","+oldKey)
	stored := tokens(t, pool, store.EmailKeyColumns[0])
	for i, id := range ids {
		if rotated.IsCurrent(stored[id]) != (i < 4) {
			t.Fatalf("token %d of %d on the new key: %v", i+1, len(ids), rotated.IsCurrent(stored[id]))
		}
	}
	usage, err := rekey(ctx, env(t, dsn, newKey+","+oldKey, oldKey), "--key", "email")
	if err != nil {
		t.Fatal(err)
	}
	if got := usage["donor_login_emails"]; got.Retired != 6 || got.Reencrypted != 6 {
		t.Fatalf("resumed run: %+v", got)
	}
	f.verify(t, pool, keyring(t, newKey), store.EmailKeyColumns)
}

func TestRekeyKeepsTokensWrittenWhileItRuns(t *testing.T) {
	pool, dsn := isolatedDatabase(t)
	ctx := context.Background()
	f := seed(t, pool, keyring(t, oldKey), 6)
	ids := orderedLogins(t, pool)
	rotated := newKey + "," + oldKey
	writer, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(ctx)
	if _, err = writer.Exec(ctx, "UPDATE donor_login_emails SET encrypted_token=$2 WHERE id=$1", ids[2], encrypt(t, keyring(t, rotated), "rewritten during rekey")); err != nil {
		t.Fatal(err)
	}
	if _, err = writer.Exec(ctx, "UPDATE donor_login_emails SET encrypted_token=NULL, completed_at=now() WHERE id=$1", ids[3]); err != nil {
		t.Fatal(err)
	}
	f["donor_login_emails"][ids[2]] = "rewritten during rekey"
	delete(f["donor_login_emails"], ids[3])
	type result struct {
		usage map[string]store.KeyUsage
		err   error
	}
	done := make(chan result, 1)
	go func() {
		usage, err := rekey(ctx, env(t, dsn, rotated, oldKey), "--key", "email", "--batch", "2")
		done <- result{usage, err}
	}()
	awaitLockWait(t, pool)
	if err = writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	if got := r.usage["donor_login_emails"]; got.Retired != 6 || got.Reencrypted != 4 {
		t.Fatalf("login emails: %+v", got)
	}
	f.verify(t, pool, keyring(t, newKey), store.EmailKeyColumns)
}
