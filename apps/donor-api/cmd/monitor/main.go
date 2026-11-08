// Command monitor prints the health of every outbox queue as JSON and exits
// non-zero when an item is parked or a due item is older than -max-age, so an
// alarm can watch the workers without reaching into them.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/migrations"
	"dad/donor-api/internal/store"
	"dad/donor-api/internal/store/queries"
)

func run(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	flags := flag.NewFlagSet("monitor", flag.ContinueOnError)
	maxAge := flags.Duration("max-age", 5*time.Minute, "alert when any due queue item is older than this duration")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *maxAge <= 0 {
		return errors.New("invalid monitor arguments")
	}
	dsn := dbroles.Monitor.URL(getenv)
	if dsn == "" {
		return errors.New(dbroles.Monitor.Variable() + " is required")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pool, err := store.Open(ctx, dsn, dbroles.Monitor.Pool)
	if err != nil {
		return errors.New("monitor database unavailable")
	}
	defer pool.Close()
	if err = migrations.Verify(ctx, pool); err != nil {
		return errors.New("monitor schema is not current")
	}
	if err = dbroles.Monitor.Check(ctx, pool, getenv(dbroles.AllowOwnerVariable) == "true"); err != nil {
		return err
	}
	rows, err := queries.New(pool).QueueHealth(ctx, pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true})
	if err != nil {
		return errors.New("queue health query failed")
	}
	if err = json.NewEncoder(out).Encode(rows); err != nil {
		return err
	}
	for _, row := range rows {
		if row.Parked > 0 || time.Duration(row.OldestSeconds)*time.Second > *maxAge {
			return errors.New("queue requires attention")
		}
	}
	return nil
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
