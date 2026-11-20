package migrations

import (
	"context"
	"errors"
	"net"

	"github.com/jackc/pgx/v5/pgconn"
)

// Cause names why a database or migration step failed in words that are safe to
// log. Driver errors can carry the connection string or row values, so only a
// class, a SQLSTATE or an embedded migration file name is ever returned.
func Cause(err error) string {
	var server *pgconn.PgError
	var network net.Error
	switch {
	case errors.Is(err, ErrPending):
		return err.Error()
	case errors.Is(err, ErrHistoryMismatch):
		return ErrHistoryMismatch.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.As(err, &server):
		return "database rejected the request, SQLSTATE " + server.Code
	case errors.As(err, &network):
		return "network error"
	default:
		return "unclassified"
	}
}
