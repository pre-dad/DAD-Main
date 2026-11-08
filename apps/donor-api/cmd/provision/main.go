// Command provision creates or updates the database login of every process
// from that process's database URL: the user it names, its password, and a
// connection limit of its pool size times the replicas it may run. It runs as the schema owner
// after migrations, which create the roles the logins join, and can be run
// again at any time, such as after changing a password.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/migrations"
	"dad/donor-api/internal/store"
)

func run(args []string, getenv func(string) string, out io.Writer) error {
	if len(args) != 0 {
		return errors.New("provision takes no arguments")
	}
	ownerURL := dbroles.ConnectionURL(dbroles.OwnerVariable, getenv)
	if ownerURL == "" {
		return errors.New(dbroles.OwnerVariable + " is required")
	}
	ownerConfig, err := pgx.ParseConfig(ownerURL)
	if err != nil {
		return errors.New(dbroles.OwnerVariable + " is not a valid database URL")
	}
	replicas, err := dbroles.ParseReplicas(getenv(dbroles.ReplicasVariable))
	if err != nil {
		return err
	}
	budget, err := dbroles.ParseBudget(getenv(dbroles.BudgetVariable))
	if err != nil {
		return err
	}
	var logins []dbroles.Login
	var missing []string
	for _, role := range dbroles.Roles {
		url := dbroles.ConnectionURL(role.Variable(), getenv)
		if url == "" {
			missing = append(missing, role.Variable())
			continue
		}
		login, err := dbroles.ParseLogin(role, url)
		if err != nil {
			return err
		}
		if login.Database != ownerConfig.Database {
			return fmt.Errorf("%s names database %q, not the owner's %q", role.Variable(), login.Database, ownerConfig.Database)
		}
		logins = append(logins, login)
	}
	if len(missing) > 0 {
		return errors.New("set " + strings.Join(missing, ", "))
	}
	if err = dbroles.Plan(logins, replicas, budget); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := store.Open(ctx, ownerURL, 2)
	if err != nil {
		return errors.New("database is unavailable")
	}
	defer pool.Close()
	if err = migrations.Verify(ctx, pool); err != nil {
		return errors.New("donor schema is not current; run migrations first")
	}
	if err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error { return dbroles.Provision(ctx, tx, logins) }); err != nil {
		return err
	}
	for _, login := range logins {
		if _, err = fmt.Fprintf(out, "%s: login %s, connection limit %d (%d x %d replicas)\n", login.Role.Name, login.User, login.ConnectionLimit(), login.Pool, login.Replicas); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	if err := run(os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
