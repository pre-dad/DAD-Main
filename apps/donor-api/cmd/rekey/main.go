// Command rekey re-encrypts everything one encryption key setting protects
// under that setting's first key, so the keys after it can be retired. With
// --dry-run it changes nothing and fails while any token still needs a key
// other than the first. It runs safely beside the API and workers, and an
// interrupted run resumes when run again.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"dad/donor-api/internal/credentials"
	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/migrations"
	"dad/donor-api/internal/store"
)

type setting struct {
	variable string
	columns  []store.EncryptedColumn
}

var settings = map[string]setting{
	"email":   {"DONOR_EMAIL_ENCRYPTION_KEY", store.EmailKeyColumns},
	"content": {"DONOR_CONTENT_ENCRYPTION_KEY", store.ContentKeyColumns},
}

func run(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	flags := flag.NewFlagSet("rekey", flag.ContinueOnError)
	name := flags.String("key", "", "the key setting to rotate: email or content")
	dryRun := flags.Bool("dry-run", false, "count tokens still needing an older key, changing nothing")
	batch := flags.Int("batch", 500, "rows read per page, from 1 to 10000")
	err := flags.Parse(args)
	target, known := settings[*name]
	if err != nil || !known || flags.NArg() != 0 || *batch < 1 || *batch > 10000 {
		return errors.New("usage: rekey --key email|content [--dry-run] [--batch 1-10000]")
	}
	keys, err := credentials.ParseKeyring(getenv(target.variable))
	if err != nil {
		return fmt.Errorf("%s %w", target.variable, err)
	}
	dsn, err := dbroles.Rekey.URL(getenv)
	if err != nil {
		return err
	}
	if dsn == "" {
		return errors.New(dbroles.Rekey.Variable() + " is required")
	}
	startup, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pool, err := store.Open(startup, dsn, dbroles.Rekey.Pool)
	if err != nil {
		return errors.New("database is unavailable")
	}
	defer pool.Close()
	if err = migrations.Verify(startup, pool); err != nil {
		return errors.New("donor schema is not current; run migrations first")
	}
	if err = dbroles.Rekey.Check(startup, pool, getenv(dbroles.AllowOwnerVariable) == "true"); err != nil {
		return err
	}
	rekeyer := store.Rekeyer{DB: pool, Keys: keys, Batch: int32(*batch)}
	var usage []store.KeyUsage
	if *dryRun {
		usage, err = rekeyer.Survey(ctx, target.columns)
	} else {
		usage, err = rekeyer.Rekey(ctx, target.columns)
	}
	if encodeErr := json.NewEncoder(out).Encode(usage); encodeErr != nil {
		return encodeErr
	}
	if undecryptable, ok := errors.AsType[*store.UndecryptableError](err); ok {
		return fmt.Errorf("refusing to rekey: %w; configure the key they were written with", undecryptable)
	}
	switch {
	case ctx.Err() != nil:
		return errors.New("interrupted; run again to resume")
	case err != nil:
		// Database errors can carry row values, so none are repeated.
		return errors.New("stopped on a database error; run again to resume")
	}
	if *dryRun && slices.ContainsFunc(usage, func(u store.KeyUsage) bool { return u.Retired > 0 }) {
		return fmt.Errorf("tokens still need a key after the first in %s; run rekey without --dry-run", target.variable)
	}
	return nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
