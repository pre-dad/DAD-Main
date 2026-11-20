package migrations

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestCauseNamesOnlySafeClasses(t *testing.T) {
	const secret = "postgresql://owner:hunter2@db/app"
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"pending":  {fmt.Errorf("%w: first missing is 0042_x.sql", ErrPending), "database migrations are pending: first missing is 0042_x.sql"},
		"mismatch": {fmt.Errorf("%w: wrapped", ErrHistoryMismatch), "migration history mismatch"},
		"timeout":  {fmt.Errorf("dial %s: %w", secret, context.DeadlineExceeded), "timed out"},
		"server":   {fmt.Errorf("%s: %w", secret, &pgconn.PgError{Code: "28P01", Message: secret}), "database rejected the request, SQLSTATE 28P01"},
		"other":    {errors.New(secret), "unclassified"},
	} {
		t.Run(name, func(t *testing.T) {
			got := Cause(tc.err)
			if got != tc.want || strings.Contains(got, "hunter2") {
				t.Fatalf("Cause = %q; want %q", got, tc.want)
			}
		})
	}
}
