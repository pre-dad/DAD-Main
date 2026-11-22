package store

import (
	"context"
	"fmt"
	"strings"

	"dad/donor-api/internal/credentials"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EncryptedColumn is a column of Fernet tokens in a table keyed by id.
type EncryptedColumn struct{ Table, Column string }

// The columns each encryption key setting protects. A new encrypted column
// must be listed here, or rotating its key would strand it on a retired key.
var (
	EmailKeyColumns = []EncryptedColumn{
		{"donor_login_emails", "encrypted_token"},
		{"donor_partner_login_emails", "encrypted_token"},
		{"donor_email_changes", "encrypted_token"},
	}
	ContentKeyColumns = []EncryptedColumn{{"donor_content_sources", "encrypted_data"}}
)

// rekeyPageBytes bounds a page's tokens as well as its rows, since one source
// document's token can be over 13 MiB. A page ends with the token that
// crosses it, so it always holds at least one row.
const rekeyPageBytes = 8 << 20

// KeyUsage counts one column's tokens: how many there are, how many need a
// key other than the primary, how many no configured key decrypts, and how
// many a rekey re-encrypted.
type KeyUsage struct {
	Table         string `json:"table"`
	Column        string `json:"column"`
	Tokens        int64  `json:"tokens"`
	Retired       int64  `json:"retired"`
	Undecryptable int64  `json:"undecryptable"`
	Reencrypted   int64  `json:"reencrypted"`
}

// UndecryptableError identifies, by table and id only, tokens that no
// configured key decrypts. It lists at most the first few of them.
type UndecryptableError struct {
	Rows  []string
	Count int64
}

const undecryptableListed = 20

func (e *UndecryptableError) Error() string {
	return fmt.Sprintf("%d tokens decrypt under no configured key: %s", e.Count, strings.Join(e.Rows, ", "))
}

// Rekeyer re-encrypts tokens under its keyring's primary key, a page of at
// most Batch rows at a time. Each token is replaced by a single-row
// compare-and-swap committed on its own: it holds one row lock at a time, so
// it cannot deadlock with or stall the API and workers, and it never
// overwrites a token they wrote after it was read. Progress lives in the data
// itself, since tokens already on the primary key are skipped, so an
// interrupted run resumes by running again.
type Rekeyer struct {
	DB    *pgxpool.Pool
	Keys  credentials.Keyring
	Batch int32
}

// Survey counts every token in columns, changing nothing. It returns an
// *UndecryptableError, with the counts, when any token decrypts under no
// configured key.
func (r Rekeyer) Survey(ctx context.Context, columns []EncryptedColumn) ([]KeyUsage, error) {
	usage := make([]KeyUsage, len(columns))
	bad := &UndecryptableError{}
	for i, column := range columns {
		u := &usage[i]
		*u = KeyUsage{Table: column.Table, Column: column.Column}
		err := r.scan(ctx, column, func(id pgtype.UUID, token []byte) (bool, error) {
			u.Tokens++
			if r.Keys.IsCurrent(token) {
				return true, nil
			}
			if _, err := r.Keys.Decrypt(token); err == nil {
				u.Retired++
				return true, nil
			}
			u.Undecryptable++
			bad.Count++
			if bad.Count <= undecryptableListed {
				bad.Rows = append(bad.Rows, column.Table+" "+id.String())
			}
			return true, nil
		})
		if err != nil {
			return usage, err
		}
	}
	if bad.Count > 0 {
		return usage, bad
	}
	return usage, nil
}

// Rekey re-encrypts every token in columns that needs a retired key. It
// surveys first and changes nothing if any token is undecryptable.
func (r Rekeyer) Rekey(ctx context.Context, columns []EncryptedColumn) ([]KeyUsage, error) {
	usage, err := r.Survey(ctx, columns)
	if err != nil {
		return usage, err
	}
	for i, column := range columns {
		if usage[i].Retired == 0 {
			continue
		}
		table, value := column.identifiers()
		swap := "UPDATE " + table + " SET " + value + " = $3 WHERE id = $1 AND " + value + " = $2"
		err = r.scan(ctx, column, func(id pgtype.UUID, token []byte) (bool, error) {
			if r.Keys.IsCurrent(token) {
				return true, nil
			}
			plaintext, err := r.Keys.Decrypt(token)
			if err != nil {
				return false, &UndecryptableError{Rows: []string{column.Table + " " + id.String()}, Count: 1}
			}
			fresh, err := r.Keys.Encrypt(plaintext)
			if err != nil {
				return false, err
			}
			tag, err := r.DB.Exec(ctx, swap, id, token, fresh)
			// A swap that matched nothing lost to a writer; scan rereads the row.
			if err != nil || tag.RowsAffected() == 0 {
				return false, err
			}
			usage[i].Reencrypted++
			return true, nil
		})
		if err != nil {
			return usage, err
		}
	}
	return usage, nil
}

// identifiers quotes the column's names, which come only from the fixed
// lists above.
func (c EncryptedColumn) identifiers() (table, value string) {
	return pgx.Identifier{c.Table}.Sanitize(), pgx.Identifier{c.Column}.Sanitize()
}

type storedToken struct {
	ID    pgtype.UUID
	Token []byte
}

// scan calls visit with each of column's tokens in id order, a page at a
// time, each page read in its own short statement. When visit reports that
// the row changed under it, scan reads again from that row.
func (r Rekeyer) scan(ctx context.Context, column EncryptedColumn, visit func(pgtype.UUID, []byte) (bool, error)) error {
	if r.Batch < 1 {
		return fmt.Errorf("rekey batch size is %d, want at least 1", r.Batch)
	}
	table, value := column.identifiers()
	page := "SELECT id, " + value + " FROM (SELECT id, " + value + ", sum(octet_length(" + value + ")) OVER (ORDER BY id) AS through FROM " + table +
		" WHERE " + value + " IS NOT NULL AND id > $1 ORDER BY id LIMIT $2) page WHERE through - octet_length(" + value + ") < $3 ORDER BY id"
	after := pgtype.UUID{Valid: true}
	for {
		rows, err := r.DB.Query(ctx, page, after, r.Batch, rekeyPageBytes)
		if err != nil {
			return err
		}
		tokens, err := pgx.CollectRows(rows, pgx.RowToStructByPos[storedToken])
		if err != nil || len(tokens) == 0 {
			return err
		}
		for _, stored := range tokens {
			done, err := visit(stored.ID, stored.Token)
			if err != nil {
				return err
			}
			if !done {
				break
			}
			after = stored.ID
		}
	}
}
