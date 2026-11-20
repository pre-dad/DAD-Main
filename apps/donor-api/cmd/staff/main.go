// Command staff invites the first administrator. With --recover it instead
// sends an existing administrator a recovery invitation, for when no
// administrator can sign in. Other staff are invited and recovered from the
// back office by an administrator.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/store"
)

func run(args []string, getenv func(string) string, out io.Writer) error {
	flags := flag.NewFlagSet("staff", flag.ContinueOnError)
	email := flags.String("email", "", "Administrator email")
	name := flags.String("name", "", "First administrator name")
	recovery := flags.Bool("recover", false, "Replace an existing administrator's passkeys when no administrator can sign in")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *email == "" || (*name == "") != *recovery {
		return errors.New("--email and --name are required, or --recover with --email only")
	}
	origin := getenv("DONOR_STAFF_ORIGIN")
	if err := payments.ValidateOrigin(origin); err != nil {
		return fmt.Errorf("DONOR_STAFF_ORIGIN %w", err)
	}
	dsn, err := dbroles.StaffCLI.URL(getenv)
	if err != nil {
		return err
	}
	if dsn == "" {
		return errors.New(dbroles.StaffCLI.Variable() + " is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := store.Open(ctx, dsn, dbroles.StaffCLI.Pool)
	if err != nil {
		return errors.New("database is unavailable")
	}
	defer pool.Close()
	if err = dbroles.StaffCLI.Check(ctx, pool, getenv(dbroles.AllowOwnerVariable) == "true"); err != nil {
		return err
	}
	staff, err := store.NewStaffService(pool, origin)
	if err != nil {
		return errors.New("staff passkeys are not configured correctly")
	}
	var token string
	if *recovery {
		token, err = staff.Recover(ctx, *email)
	} else {
		token, err = staff.Bootstrap(ctx, *email, *name)
	}
	switch {
	case *recovery && (errors.Is(err, pgx.ErrNoRows) || errors.Is(err, store.ErrStaffInput)):
		return errors.New("no enabled administrator has that email")
	case errors.Is(err, store.ErrStaffConflict):
		return errors.New("staff already exists; use an administrator account")
	case err != nil:
		return errors.New("administrator invitation failed")
	}
	_, err = fmt.Fprintln(out, "One-use private invitation: "+origin+"/staff/activate#token="+token)
	return err
}

func main() {
	if err := run(os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
