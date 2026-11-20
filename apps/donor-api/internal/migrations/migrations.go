package migrations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"sync"

	"dad/donor-api/db"
	"dad/donor-api/internal/dbroles"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrAdoptionRequired = errors.New("existing donor database requires explicit baseline adoption")
var ErrHistoryMismatch = errors.New("migration history mismatch")
var ErrPending = errors.New("database migrations are pending")
var ErrSchemaMismatch = errors.New("existing donor schema does not match the R1 baseline")

type migration struct{ name, sql, checksum string }

var migrationName = regexp.MustCompile(`^\d{4}_[a-z0-9_]+\.sql$`)

// embedded is the binary's own manifest, read and hashed once, since readiness
// probes verify it on every request.
var embedded = sync.OnceValues(func() ([]migration, error) { return manifest(db.Migrations) })

func manifest(fsys fs.FS) ([]migration, error) {
	paths, err := fs.Glob(fsys, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	slices.Sort(paths)
	result := make([]migration, 0, len(paths))
	for _, path := range paths {
		name := strings.TrimPrefix(path, "migrations/")
		if !migrationName.MatchString(name) {
			return nil, fmt.Errorf("invalid migration file name: %s", name)
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(data)
		result = append(result, migration{name: name, sql: string(data), checksum: hex.EncodeToString(digest[:])})
	}
	if len(result) == 0 {
		return nil, errors.New("no embedded migrations")
	}
	return result, nil
}

// Apply runs every embedded migration the database lacks, then brings the
// process roles and their privileges to their declaration, in one
// transaction under an advisory lock. A database that already holds the donor
// tables but no history is adopted only when adopt is set and its schema
// matches the baseline exactly.
func Apply(ctx context.Context, pool *pgxpool.Pool, adopt bool) error {
	files, err := embedded()
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		for _, statement := range []string{
			"SELECT pg_advisory_xact_lock(714728310)",
			"SET LOCAL lock_timeout = '5s'",
			"SET LOCAL search_path TO public",
			`CREATE TABLE IF NOT EXISTS donor_schema_migrations(version text PRIMARY KEY,checksum text NOT NULL,applied_at timestamptz NOT NULL DEFAULT now())`,
		} {
			if _, err := tx.Exec(ctx, statement); err != nil {
				return err
			}
		}
		applied, err := appliedMigrations(ctx, tx, files)
		if err != nil {
			return err
		}
		for index, file := range files {
			if _, ok := applied[file.name]; ok {
				continue
			}
			if err = apply(ctx, tx, file, index == 0, adopt); err != nil {
				return err
			}
		}
		return dbroles.Reconcile(ctx, tx)
	})
}

// appliedMigrations returns the recorded history, which must be a prefix of
// files with matching checksums.
func appliedMigrations(ctx context.Context, tx pgx.Tx, files []migration) (map[string]string, error) {
	rows, err := tx.Query(ctx, `SELECT version,checksum FROM donor_schema_migrations ORDER BY version COLLATE "C"`)
	if err != nil {
		return nil, err
	}
	applied := map[string]string{}
	var version, checksum string
	if _, err = pgx.ForEachRow(rows, []any{&version, &checksum}, func() error {
		applied[version] = checksum
		return nil
	}); err != nil {
		return nil, err
	}
	known := map[string]bool{}
	missing := false
	for _, file := range files {
		known[file.name] = true
		if checksum, ok := applied[file.name]; ok {
			if missing || checksum != file.checksum {
				return nil, fmt.Errorf("migration history mismatch: %s", file.name)
			}
		} else {
			missing = true
		}
	}
	for version := range applied {
		if !known[version] {
			return nil, fmt.Errorf("database has unknown migration: %s", version)
		}
	}
	return applied, nil
}

// apply runs file and records it. The baseline instead adopts an existing
// donor schema, which it only validates.
func apply(ctx context.Context, tx pgx.Tx, file migration, baseline, adopt bool) error {
	existing := false
	if baseline {
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_tables WHERE schemaname='public' AND (tablename='donors' OR tablename LIKE 'donor\_%' ESCAPE '\') AND tablename<>'donor_schema_migrations')`).Scan(&existing); err != nil {
			return err
		}
	}
	if existing {
		if !adopt {
			return ErrAdoptionRequired
		}
		if err := validateBaseline(ctx, tx, file.sql); err != nil {
			return err
		}
	} else if _, err := tx.Exec(ctx, file.sql); err != nil {
		return fmt.Errorf("apply %s: %w", file.name, err)
	}
	_, err := tx.Exec(ctx, "INSERT INTO donor_schema_migrations(version,checksum) VALUES($1,$2)", file.name, file.checksum)
	return err
}

// Verify reports whether the database has applied exactly the embedded
// migrations, which every service requires before it starts.
func Verify(ctx context.Context, pool *pgxpool.Pool) error {
	files, err := embedded()
	if err != nil {
		return err
	}
	rows, err := pool.Query(ctx, `SELECT version,checksum FROM donor_schema_migrations ORDER BY version COLLATE "C"`)
	if err != nil {
		return err
	}
	defer rows.Close()
	index := 0
	for rows.Next() {
		var name, checksum string
		if err = rows.Scan(&name, &checksum); err != nil {
			return err
		}
		if index >= len(files) || files[index].name != name || files[index].checksum != checksum {
			return ErrHistoryMismatch
		}
		index++
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if index != len(files) {
		return fmt.Errorf("%w: first missing is %s", ErrPending, files[index].name)
	}
	return nil
}

func validateBaseline(ctx context.Context, tx pgx.Tx, sql string) error {
	var revision string
	if err := tx.QueryRow(ctx, "SELECT version_num FROM alembic_version").Scan(&revision); err != nil {
		return ErrSchemaMismatch
	}
	if revision != "0030_donor_public_content" {
		return ErrSchemaMismatch
	}
	check, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer check.Rollback(ctx)
	const schema = "dad_baseline_validation"
	if _, err = check.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		return err
	}
	if _, err = check.Exec(ctx, "SET LOCAL search_path TO "+schema); err != nil {
		return err
	}
	if _, err = check.Exec(ctx, sql); err != nil {
		return err
	}
	if _, err = check.Exec(ctx, "SET LOCAL search_path TO public"); err != nil {
		return err
	}
	expected, err := schemaSignature(ctx, check, schema)
	if err != nil {
		return err
	}
	actual, err := schemaSignature(ctx, check, "public")
	if err != nil {
		return err
	}
	if !slices.Equal(expected, actual) {
		for _, item := range expected {
			if !slices.Contains(actual, item) {
				return fmt.Errorf("%w: missing or different %s", ErrSchemaMismatch, item)
			}
		}
		return fmt.Errorf("%w: unexpected schema objects", ErrSchemaMismatch)
	}
	return check.Rollback(ctx)
}

func schemaSignature(ctx context.Context, tx pgx.Tx, schema string) ([]string, error) {
	rows, err := tx.Query(ctx, `
WITH tables AS (
 SELECT c.oid,c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname=$1 AND c.relkind='r' AND (c.relname='donors' OR c.relname LIKE 'donor\_%' ESCAPE '\') AND c.relname<>'donor_schema_migrations'
), items AS (
 SELECT t.relname,'column' AS kind,a.attname AS name,
 format_type(a.atttypid,a.atttypmod)||':'||a.attnotnull::text||':'||COALESCE(pg_get_expr(d.adbin,d.adrelid),'') AS definition
 FROM tables t JOIN pg_attribute a ON a.attrelid=t.oid AND a.attnum>0 AND NOT a.attisdropped
 LEFT JOIN pg_attrdef d ON d.adrelid=t.oid AND d.adnum=a.attnum
 UNION ALL
 SELECT t.relname,'constraint','',pg_get_constraintdef(c.oid) FROM tables t JOIN pg_constraint c ON c.conrelid=t.oid
 UNION ALL
 SELECT t.relname,'index','',x.indisunique::text||substring(pg_get_indexdef(x.indexrelid) FROM position(' ON ' IN pg_get_indexdef(x.indexrelid))) FROM tables t JOIN pg_index x ON x.indrelid=t.oid
)
SELECT relname||':'||kind||':'||name||':'||definition FROM items ORDER BY relname,kind,name`, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []string{}
	for rows.Next() {
		var definition string
		if err = rows.Scan(&definition); err != nil {
			return nil, err
		}
		definition = strings.ReplaceAll(definition, schema+".", "")
		result = append(result, definition)
	}
	slices.Sort(result)
	return result, rows.Err()
}
