// Command migrate applies the donor database migrations as the schema owner.
// With -adopt-existing it validates and adopts an existing R1 schema instead.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/migrations"
	"dad/donor-api/internal/store"
)

func run(args []string, getenv func(string) string) error {
	flags := flag.NewFlagSet("migrate", flag.ContinueOnError)
	adopt := flags.Bool("adopt-existing", false, "Validate and adopt the existing R1 donor schema without changing its data")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("invalid migrate arguments")
	}
	dsn := dbroles.ConnectionURL(dbroles.OwnerVariable, getenv)
	if dsn == "" {
		return errors.New(dbroles.OwnerVariable + " is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := store.Open(ctx, dsn, 2)
	if err != nil {
		return errors.New("migration database unavailable")
	}
	defer pool.Close()
	return migrations.Apply(ctx, pool, *adopt)
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(os.Args[1:], os.Getenv); err != nil {
		slog.Error("migration failed", "error", err)
		os.Exit(1)
	}
	slog.Info("database migrations complete")
}
