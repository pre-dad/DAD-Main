// Package store holds the donor domain: identity, giving, recognition,
// messaging and staff operations over the R1 database. Services take bearer
// tokens rather than cookies, so any transport can present them.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrDenied = errors.New("donor access denied")

// uniqueViolation is PostgreSQL's SQLSTATE for a duplicate key.
const uniqueViolation = "23505"

// processorMode is the payment processor mode every response reports. The
// sandbox only ever talks to Stripe's test mode.
const processorMode = "test"

// commandRetryWindow bounds a command's processor retries inside Stripe's
// 24-hour idempotency window. An older unfinished command needs
// reconciliation, never a fresh request that could act twice.
const commandRetryWindow = 23 * time.Hour

// Open connects a process's pool of at most maxConns connections, sized for
// its role, unless the database URL's pool_max_conns sets the size.
func Open(ctx context.Context, dsn string, maxConns int32) (*pgxpool.Pool, error) {
	if dsn == "" {
		return nil, errors.New("donor database URL is required")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	// pgx's own pool_max_conns URL parameter sizes a deployment's pool; pgx
	// keeps it as a runtime parameter only until the pool config consumes it.
	if conn, err := pgx.ParseConfig(dsn); err != nil || conn.RuntimeParams["pool_max_conns"] == "" {
		config.MaxConns = maxConns
	}
	config.MinConns = 0
	config.MaxConnLifetime = 30 * time.Minute
	config.MaxConnIdleTime = 5 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// HashToken is how every bearer secret is stored: hex SHA-256, the encoding
// existing sessions and links were issued with.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// randomToken returns size random bytes as unpadded URL-safe base64.
func randomToken(size int) (string, error) {
	secret := make([]byte, size)
	if _, err := rand.Read(secret); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(secret), nil
}

// NewID returns a random version-4 UUID.
func NewID() (pgtype.UUID, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return pgtype.UUID{}, err
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	return pgtype.UUID{Bytes: bytes, Valid: true}, nil
}

// ValidRequestID reports whether id is a version-4 UUID, the only form accepted as an idempotency key.
func ValidRequestID(id pgtype.UUID) bool {
	return id.Valid && id.Bytes[6]>>4 == 4 && id.Bytes[8]&0xc0 == 0x80
}

// plainText reports whether s is valid UTF-8 without control characters,
// which no field needs and PostgreSQL cannot store as NUL. multiline allows
// tabs and line breaks.
func plainText(s string, multiline bool) bool {
	return utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool {
		if multiline && (r == '\t' || r == '\n' || r == '\r') {
			return false
		}
		return unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r)
	})
}

func timestamp(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t.UTC(), Valid: true} }

func text(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

func textPointer(value pgtype.Text) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

func timePointer(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	return &value.Time
}

// rfc3339 formats a timestamp as staff input expects it, or "" when unset.
func rfc3339(value pgtype.Timestamptz) string {
	if !value.Valid {
		return ""
	}
	return value.Time.UTC().Format(time.RFC3339)
}

// signedIn is a donor authenticated by one of their sessions.
type signedIn struct {
	queries.Donor
	session  pgtype.UUID
	lastUsed time.Time
}

func authenticate(ctx context.Context, q *queries.Queries, token string, now time.Time) (signedIn, error) {
	row, err := q.AuthenticatedDonor(ctx, queries.AuthenticatedDonorParams{TokenHash: HashToken(token), ExpiresAt: timestamp(now)})
	if errors.Is(err, pgx.ErrNoRows) {
		return signedIn{}, ErrDenied
	}
	return signedIn{row.Donor, row.SessionID, row.LastUsedAt.Time}, err
}

// touch records the session's use, at most once per touchInterval, so that
// requests do not each write.
func (s signedIn) touch(ctx context.Context, q *queries.Queries, now time.Time) error {
	if now.Sub(s.lastUsed) < touchInterval {
		return nil
	}
	return q.TouchSession(ctx, queries.TouchSessionParams{ID: s.session, LastUsedAt: timestamp(now)})
}
