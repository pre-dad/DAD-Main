// Package dbroles declares the database role of every process and what each
// role may do to each table, so that a fault in one process reaches only the
// data that process needs. Migrations keep the database to the declaration,
// operators provision one login per role, and each process refuses to start
// with more than its role.
package dbroles

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Role is a process's NOLOGIN group role. The process connects with a login
// that is a member of it.
type Role struct {
	Name string
	// Pool is the number of connections one instance of the process opens.
	// Its login's connection limit is Pool times the replicas provisioned
	// (see Replicas).
	Pool int32
}

// Key names the role in a replica list: the role name without its dad_ and
// worker_ prefixes, with hyphens (api, projections, email-changes, staff-cli).
func (r Role) Key() string {
	return strings.ReplaceAll(strings.TrimPrefix(strings.TrimPrefix(r.Name, "dad_"), "worker_"), "_", "-")
}

var (
	// Load tests at 50,000 donors found a 10-connection API pool, not
	// PostgreSQL, capped throughput; 40 doubled it.
	API               = Role{"dad_api", 40}
	StripeWorker      = Role{"dad_worker_stripe", 2}
	ProjectionWorker  = Role{"dad_worker_projections", 2}
	LoginEmailWorker  = Role{"dad_worker_login_email", 2}
	EmailChangeWorker = Role{"dad_worker_email_changes", 2}
	MessageWorker     = Role{"dad_worker_messages", 2}
	PushWorker        = Role{"dad_worker_push", 2}
	Monitor           = Role{"dad_monitor", 2}
	StaffCLI          = Role{"dad_staff_cli", 2}
	Rekey             = Role{"dad_rekey", 2}
	// ReconcileJob is the daily Stripe reconciliation task's SELECT-only role.
	// The Go identifier is ReconcileJob, not Reconcile, because privileges.go
	// already declares a Reconcile function (the privilege reconciler); the
	// database role name is still dad_reconcile.
	ReconcileJob = Role{"dad_reconcile", 2}
)

// Roles lists every process role.
var Roles = []Role{
	API,
	StripeWorker,
	ProjectionWorker,
	LoginEmailWorker,
	EmailChangeWorker,
	MessageWorker,
	PushWorker,
	Monitor,
	StaffCLI,
	Rekey,
	ReconcileJob,
}

// Workers maps each worker kind to its role.
var Workers = map[string]Role{
	"stripe":        StripeWorker,
	"projections":   ProjectionWorker,
	"login-email":   LoginEmailWorker,
	"email-changes": EmailChangeWorker,
	"messages":      MessageWorker,
	"push":          PushWorker,
}

const (
	// OwnerVariable holds the schema owner's database URL, which only
	// migrations and provisioning use.
	OwnerVariable = "DONOR_GO_DATABASE_URL"
	// AllowOwnerVariable, set to true, lets a local process without its own
	// URL connect as the schema owner.
	AllowOwnerVariable = "DONOR_ALLOW_OWNER_CONNECTION"
)

// Variable names the environment variable holding the process's database
// URL: DONOR_API_DATABASE_URL for dad_api.
func (r Role) Variable() string {
	return "DONOR_" + strings.ToUpper(strings.TrimPrefix(r.Name, "dad_")) + "_DATABASE_URL"
}

// URL returns the database URL the process connects with: its own, or the
// owner's where the environment allows that. It is empty when neither is set.
// Each may be given whole or as separate parts with the password on its own
// (see ConnectionURL).
func (r Role) URL(getenv func(string) string) string {
	if url := ConnectionURL(r.Variable(), getenv); url != "" {
		return url
	}
	if getenv(AllowOwnerVariable) == "true" {
		return ConnectionURL(OwnerVariable, getenv)
	}
	return ""
}

// ErrCheckFailed reports that the connection's privileges could not be read.
var ErrCheckFailed = errors.New("database role check failed")

// Check refuses a connection that holds more or other than the role. A login
// that can change the schema, as the owner and superusers can, could let a
// faulty or compromised process rewrite or drop any table, even while the
// session acts as the role (SET ROLE, or a role parameter in the URL), since
// the process can reset it. Such a login is accepted only when allowOwner is
// set, and then a role it assumes must still be this one. Every error message
// is safe to log.
func (r Role) Check(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, allowOwner bool) error {
	var schemaChanger, assumed, member bool
	err := db.QueryRow(ctx, `
SELECT has_database_privilege(session_user,current_database(),'CREATE') OR EXISTS(
 SELECT FROM pg_namespace n WHERE n.nspname NOT LIKE 'pg\_%' AND n.nspname<>'information_schema'
 AND (has_schema_privilege(session_user,n.oid,'CREATE') OR pg_has_role(session_user,n.nspowner,'USAGE')
  OR EXISTS(SELECT FROM pg_class c WHERE c.relnamespace=n.oid AND pg_has_role(session_user,c.relowner,'USAGE')))),
 session_user<>current_user,
 COALESCE((SELECT pg_has_role(current_user,oid,'USAGE') FROM pg_roles WHERE rolname=$1),false)`, r.Name).Scan(&schemaChanger, &assumed, &member)
	switch {
	case err != nil:
		return ErrCheckFailed
	case schemaChanger && !allowOwner:
		return fmt.Errorf(
			"%s connects as a user that can change the schema; use a login of %s (see docs/operations.md#database-roles), or set %s=true on a local machine",
			r.Variable(),
			r.Name,
			AllowOwnerVariable,
		)
	case schemaChanger && !assumed:
		return nil
	case !member:
		return fmt.Errorf("%s connects as a user that is not a member of %s", r.Variable(), r.Name)
	}
	return nil
}
