package store

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestEveryEncryptedColumnIsRotatedWithItsKey(t *testing.T) {
	pool := database(t)
	rows, err := pool.Query(context.Background(), `SELECT table_name, column_name FROM information_schema.columns
WHERE table_schema='public' AND column_name LIKE 'encrypted\_%' ORDER BY table_name, column_name`)
	if err != nil {
		t.Fatal(err)
	}
	columns, err := pgx.CollectRows(rows, pgx.RowToStructByPos[EncryptedColumn])
	if err != nil {
		t.Fatal(err)
	}
	rotated := slices.Concat(EmailKeyColumns, ContentKeyColumns)
	for _, column := range columns {
		if !slices.Contains(rotated, column) {
			t.Errorf("%s.%s is not in any key's column list, so rekey would never re-encrypt it", column.Table, column.Column)
		}
	}
	if len(columns) != len(rotated) {
		t.Errorf("found %d encrypted columns, want the %d listed", len(columns), len(rotated))
	}
}

func TestRekeyerRejectsNonPositiveBatch(t *testing.T) {
	r := Rekeyer{Batch: 0}
	for name, run := range map[string]func() error{
		"survey": func() error { _, err := r.Survey(context.Background(), EmailKeyColumns); return err },
		"scan": func() error {
			return r.scan(context.Background(), EmailKeyColumns[0], func(pgtype.UUID, []byte) (bool, error) { return true, nil })
		},
	} {
		if err := run(); err == nil || !strings.Contains(err.Error(), "batch") {
			t.Errorf("%s with batch 0: err=%v, want a batch size error", name, err)
		}
	}
}
