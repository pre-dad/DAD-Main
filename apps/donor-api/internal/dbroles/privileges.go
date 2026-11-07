package dbroles

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// lock lets a role take row locks on a table it never changes: PostgreSQL
// allows SELECT … FOR UPDATE only with UPDATE on some column, and no process
// changes an id.
const lock = "SELECT, UPDATE (id)"

// grants is what each role may do to each relation, in GRANT syntax, derived
// from the statements each process runs. Every relation in the database is
// listed, and a new one needs its entry in the change that creates it;
// TestCatalogMatchesTheDeclaration fails until it has one.
var grants = map[string]map[Role]string{
	// Every process verifies the migration history at startup.
	"donor_schema_migrations": {
		API:               "SELECT",
		StripeWorker:      "SELECT",
		ProjectionWorker:  "SELECT",
		LoginEmailWorker:  "SELECT",
		EmailChangeWorker: "SELECT",
		MessageWorker:     "SELECT",
		PushWorker:        "SELECT",
		Monitor:           "SELECT",
		StaffCLI:          "SELECT",
		Rekey:             "SELECT",
		ReconcileJob:      "SELECT",
	},
	"donors": {
		API:               "SELECT, INSERT, UPDATE",
		StripeWorker:      lock,
		ProjectionWorker:  lock,
		LoginEmailWorker:  lock,
		EmailChangeWorker: lock,
		MessageWorker:     lock,
		PushWorker:        lock,
		StaffCLI:          "SELECT (email)",
	},

	"donor_sessions": {
		API:              "SELECT, INSERT, UPDATE",
		StripeWorker:     "SELECT",
		ProjectionWorker: "SELECT, DELETE",
		PushWorker:       "SELECT, UPDATE",
	},
	"donor_session_events": {API: "SELECT, INSERT"},
	"donor_login_links":    {API: "SELECT, UPDATE", LoginEmailWorker: "INSERT", ProjectionWorker: "SELECT (id, expires_at), DELETE"},
	"donor_login_emails": {API: "SELECT, INSERT", StripeWorker: "SELECT, INSERT", LoginEmailWorker: "SELECT, UPDATE",
		ProjectionWorker: "SELECT (id, completed_at), DELETE",
		Monitor:          "SELECT (attempts, available_at, completed_at)", Rekey: "SELECT (id, encrypted_token), UPDATE (encrypted_token)"},
	"donor_email_changes": {API: "SELECT, INSERT, UPDATE", EmailChangeWorker: "SELECT, UPDATE",
		Monitor: "SELECT (attempts, available_at, sent_at, stage)", Rekey: "SELECT (id, encrypted_token), UPDATE (encrypted_token)"},
	"donor_identity_events": {API: "SELECT, INSERT"},
	"donor_preferences": {
		API:              "SELECT, INSERT, UPDATE, DELETE",
		StripeWorker:     "SELECT",
		MessageWorker:    "SELECT",
		PushWorker:       "SELECT",
		ProjectionWorker: "SELECT",
	},
	"donor_preference_events": {API: "SELECT, INSERT"},

	"donor_checkouts": {
		API:              "SELECT, INSERT, UPDATE (journey_id)",
		StripeWorker:     "SELECT",
		ProjectionWorker: "SELECT",
		ReconcileJob:     "SELECT",
	},
	"donor_stripe_checkouts": {
		API:           "SELECT, INSERT, UPDATE",
		StripeWorker:  "SELECT, UPDATE",
		MessageWorker: "SELECT",
		PushWorker:    "SELECT",
	},
	"donor_stripe_events": {
		API:          "SELECT, INSERT",
		StripeWorker: "SELECT, UPDATE",
		Monitor:      "SELECT (attempts, available_at, processed_at)",
		ReconcileJob: "SELECT",
	},
	"donor_gifts": {
		PushWorker:       "SELECT",
		API:              lock,
		StripeWorker:     "SELECT, INSERT, UPDATE",
		ProjectionWorker: "SELECT",
		MessageWorker:    "SELECT",
		ReconcileJob:     "SELECT",
	},
	"donor_billing_periods": {
		API:              "SELECT",
		StripeWorker:     "SELECT, INSERT, UPDATE",
		ProjectionWorker: "SELECT",
		MessageWorker:    "SELECT",
		PushWorker:       "SELECT",
		ReconcileJob:     "SELECT",
	},
	"donor_billing_observations": {API: "SELECT", StripeWorker: "SELECT, INSERT", ReconcileJob: "SELECT"},
	"donor_retention_milestones": {
		API:              "SELECT, INSERT, UPDATE",
		StripeWorker:     "SELECT, INSERT, UPDATE",
		ProjectionWorker: "SELECT, INSERT, UPDATE",
	},
	"donor_projection_schedules": {ProjectionWorker: "SELECT, INSERT, UPDATE"},
	"donor_event_alert_jobs":     {API: "SELECT, INSERT", ProjectionWorker: "SELECT, UPDATE"},
	"donor_refunds": {
		PushWorker:       "SELECT",
		API:              "SELECT, INSERT, UPDATE",
		StripeWorker:     "SELECT, INSERT, UPDATE",
		ProjectionWorker: "SELECT",
		MessageWorker:    "SELECT",
		ReconcileJob:     "SELECT",
	},
	"donor_refund_observations": {
		API:              "SELECT, INSERT",
		StripeWorker:     "SELECT, INSERT",
		ProjectionWorker: "SELECT",
		ReconcileJob:     "SELECT",
	},
	"donor_subscriptions": {
		API:              "SELECT, INSERT, UPDATE",
		StripeWorker:     "SELECT, INSERT, UPDATE",
		ProjectionWorker: "SELECT",
		MessageWorker:    "SELECT",
		PushWorker:       "SELECT",
		Monitor:          "SELECT (observed_at, period_end, status)",
	},
	"donor_subscription_leases":    {API: "SELECT, INSERT, UPDATE, DELETE"},
	"donor_recurring_reservations": {API: "SELECT, INSERT, DELETE", StripeWorker: "SELECT, DELETE"},
	"donor_amount_changes":         {API: "SELECT, INSERT, UPDATE", StripeWorker: "SELECT, UPDATE", ProjectionWorker: "SELECT"},
	"donor_cancellations":          {API: "SELECT, INSERT, UPDATE"},
	"donor_payment_updates":        {API: "SELECT, INSERT, UPDATE", StripeWorker: "SELECT, UPDATE"},
	"donor_stripe_budgets":         {API: "SELECT, INSERT, UPDATE", StripeWorker: "SELECT, INSERT, UPDATE"},

	"donor_metric_projections": {
		API:              "SELECT, INSERT, UPDATE",
		StripeWorker:     "SELECT, INSERT, UPDATE",
		ProjectionWorker: "SELECT, INSERT, UPDATE",
		MessageWorker:    "SELECT",
		PushWorker:       "SELECT",
		Monitor:          "SELECT (next_refresh_at, refresh_error)",
	},
	"donor_referral_touches": {
		API:              "SELECT, INSERT, UPDATE (referrer_id, kind, touched_at, expires_at)",
		ProjectionWorker: "SELECT, UPDATE (id), DELETE",
	},
	"donor_referral_corrections": {API: "SELECT, INSERT", StripeWorker: "SELECT, INSERT", ProjectionWorker: "SELECT, INSERT"},
	"donor_messages": {
		API:              "SELECT, INSERT",
		StripeWorker:     "SELECT, INSERT",
		ProjectionWorker: "SELECT, INSERT",
		MessageWorker:    "SELECT, UPDATE",
		Monitor:          "SELECT (attempts, available_at, completed_at)",
	},
	"donor_deleted_account_reversals": {
		StripeWorker: "SELECT, INSERT, UPDATE",
		Monitor:      "SELECT (attempts, available_at, completed_at)",
	},
	"donor_push_notifications": {
		API:              "SELECT, INSERT",
		StripeWorker:     "SELECT, INSERT",
		ProjectionWorker: "SELECT, INSERT",
		PushWorker:       "SELECT, UPDATE",
		Monitor:          "SELECT (attempts, available_at, completed_at, expires_at)",
	},

	"donor_passkeys":   {API: "SELECT, INSERT, UPDATE, DELETE"},
	"donor_ceremonies": {API: "SELECT, INSERT, DELETE"},

	"donor_passwords":       {API: "SELECT, INSERT, UPDATE, DELETE"},
	"donor_phone_codes":     {API: "SELECT, INSERT, UPDATE, DELETE", ProjectionWorker: "SELECT (phone, expires_at), DELETE"},
	"donor_oidc_identities": {API: "SELECT, INSERT, UPDATE, DELETE"},
	"donor_company_touches": {
		API:              "SELECT, INSERT, UPDATE (company_code, touched_at, expires_at), DELETE",
		ProjectionWorker: "SELECT, DELETE",
	},
	"donor_deletions": {API: "SELECT, INSERT", StripeWorker: "SELECT"},
	"analytics_events": {
		API:              "SELECT, INSERT, UPDATE (donor_id), DELETE",
		StripeWorker:     "SELECT, INSERT",
		ProjectionWorker: "SELECT, INSERT, DELETE",
	},

	"donor_staff":            {API: "SELECT, INSERT, UPDATE", StaffCLI: "SELECT, INSERT, UPDATE"},
	"donor_staff_passkeys":   {API: "SELECT, INSERT, UPDATE, DELETE", StaffCLI: "SELECT, DELETE"},
	"donor_staff_ceremonies": {API: "SELECT, INSERT, DELETE"},
	"donor_staff_invites":    {API: "SELECT, INSERT, UPDATE", StaffCLI: "SELECT, INSERT, UPDATE"},
	"donor_staff_sessions":   {API: "SELECT, INSERT, UPDATE", StaffCLI: "SELECT, UPDATE"},
	"donor_staff_audit":      {API: "SELECT, INSERT", StaffCLI: "INSERT"},
	"donor_staff_actions":    {API: "SELECT, INSERT, UPDATE"},
	"donor_partner_signups": {
		PushWorker:       "SELECT",
		API:              "SELECT, INSERT, UPDATE",
		MessageWorker:    "SELECT",
		ProjectionWorker: "SELECT",
		LoginEmailWorker: "SELECT",
	},
	"donor_partner_applications": {API: "SELECT, INSERT, UPDATE"},
	"donor_partner_users": {
		ProjectionWorker: "SELECT",
		PushWorker:       "SELECT",
		API:              "SELECT, INSERT, UPDATE",
		StaffCLI:         "SELECT (email, partner_id, disabled)",
		MessageWorker:    "SELECT",
		LoginEmailWorker: "SELECT, UPDATE (invite_token_hash, invite_expires_at)",
	},
	"donor_partner_sessions": {API: "SELECT, INSERT, UPDATE", PushWorker: "SELECT, UPDATE", ProjectionWorker: "SELECT"},
	"donor_partner_login_emails": {API: "SELECT, INSERT", LoginEmailWorker: "SELECT, UPDATE",
		Monitor: "SELECT (attempts, available_at, completed_at)", Rekey: "SELECT (id, encrypted_token), UPDATE (encrypted_token)"},
	"donor_partner_push_notifications": {
		API:              "SELECT, INSERT",
		ProjectionWorker: "INSERT",
		PushWorker:       "SELECT, UPDATE",
		Monitor:          "SELECT (attempts, available_at, completed_at, expires_at)",
	},
	"donor_partner_reports": {API: "SELECT, INSERT"},
	"donor_partner_messages": {
		API:              "SELECT, INSERT",
		ProjectionWorker: "SELECT, INSERT",
		MessageWorker:    "SELECT, INSERT, UPDATE",
		Monitor:          "SELECT (attempts, available_at, completed_at)",
	},
	"donor_partner_employees":       {API: "SELECT, INSERT, UPDATE, DELETE", StripeWorker: "SELECT", ProjectionWorker: "SELECT"},
	"donor_partner_teams":           {API: "SELECT, INSERT", ProjectionWorker: "SELECT"},
	"donor_team_months":             {API: "SELECT", ProjectionWorker: "SELECT, INSERT, UPDATE"},
	"donor_team_month_standings":    {API: "SELECT", ProjectionWorker: "SELECT, INSERT"},
	"donor_partner_match_payments":  {API: "SELECT, INSERT"},
	"donor_content_sources":         {API: "SELECT, INSERT", Rekey: "SELECT (id, encrypted_data), UPDATE (encrypted_data)"},
	"donor_initiative_partners":     {API: "SELECT, INSERT, UPDATE, DELETE", ProjectionWorker: "SELECT"},
	"donor_initiative_spend_drafts": {API: "SELECT, INSERT, UPDATE, DELETE"},
	"donor_streak_resets":           {API: "SELECT, INSERT", StripeWorker: "SELECT", ProjectionWorker: "SELECT"},
	"donor_badge_rules": {
		API:              "SELECT, INSERT, UPDATE",
		StripeWorker:     "SELECT",
		ProjectionWorker: "SELECT",
		PushWorker:       "SELECT",
	},
	"donor_milestone_rules": {
		API:              "SELECT, INSERT, UPDATE",
		StripeWorker:     "SELECT",
		ProjectionWorker: "SELECT",
		PushWorker:       "SELECT",
	},
	"donor_initiatives":          {API: "SELECT, INSERT, UPDATE", ProjectionWorker: "SELECT"},
	"donor_initiative_metrics":   {API: "SELECT, INSERT, UPDATE, DELETE", ProjectionWorker: "SELECT"},
	"donor_initiative_spend":     {API: "SELECT, INSERT, UPDATE, DELETE", ProjectionWorker: "SELECT"},
	"donor_initiative_exposures": {API: "SELECT, INSERT, UPDATE", StripeWorker: "SELECT", ProjectionWorker: "SELECT"},
	"donor_badges":               {API: "SELECT, INSERT, UPDATE", StripeWorker: "SELECT, INSERT", ProjectionWorker: "SELECT, INSERT"},
	"donor_payment_methods":      {API: "SELECT", StripeWorker: "SELECT, INSERT", ProjectionWorker: "SELECT"},
	"donor_device_marks":         {API: "SELECT, INSERT", StripeWorker: "SELECT", ProjectionWorker: "SELECT"},
	"donor_founding_claims":      {API: "SELECT, INSERT, UPDATE", ProjectionWorker: "SELECT, DELETE"},
	"donor_founding_grants":      {API: "SELECT, INSERT", StripeWorker: "SELECT", ProjectionWorker: "SELECT"},
	"donor_recognition_wording":  {API: "SELECT, INSERT, UPDATE, DELETE", PushWorker: "SELECT"},
	"donor_rankings":             {API: "SELECT", StripeWorker: "SELECT", ProjectionWorker: "SELECT, INSERT, UPDATE, DELETE"},
	"donor_events": {
		API:              "SELECT, INSERT, UPDATE",
		StripeWorker:     "SELECT",
		ProjectionWorker: "SELECT",
		MessageWorker:    "SELECT",
		PushWorker:       "SELECT",
	},
	"donor_event_reservations": {
		API:              "SELECT, INSERT, UPDATE",
		StripeWorker:     "SELECT",
		ProjectionWorker: "SELECT",
		MessageWorker:    "SELECT",
		PushWorker:       "SELECT",
	},
	"donor_event_guests": {API: "SELECT, INSERT, DELETE"},
}

// privileges is a canonical set of grants: "SELECT" for a table privilege,
// "UPDATE (id)" for a column privilege, sorted.
type privileges []string

// parsePrivileges reads a GRANT privilege list such as
// "SELECT (id, token), UPDATE (token)".
func parsePrivileges(list string) privileges {
	var result privileges
	for list = strings.TrimSpace(list); list != ""; {
		end := strings.IndexAny(list, ",(")
		if end < 0 {
			end = len(list)
		}
		kind := strings.TrimSpace(list[:end])
		list = list[end:]
		if strings.HasPrefix(list, "(") {
			closing := strings.Index(list, ")")
			for column := range strings.SplitSeq(list[1:closing], ",") {
				result = append(result, kind+" ("+strings.TrimSpace(column)+")")
			}
			list = list[closing+1:]
		} else {
			result = append(result, kind)
		}
		list = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(list), ","))
	}
	slices.Sort(result)
	return slices.Compact(result)
}

// sql renders the set as a GRANT privilege list with quoted columns.
func (p privileges) sql() string {
	columns := map[string][]string{}
	var kinds []string
	for _, item := range p {
		kind, column, isColumn := strings.Cut(item, " (")
		if !slices.Contains(kinds, kind) {
			kinds = append(kinds, kind)
		}
		if isColumn {
			columns[kind] = append(columns[kind], pgx.Identifier{strings.TrimSuffix(column, ")")}.Sanitize())
		}
	}
	var parts []string
	for _, kind := range kinds {
		if slices.Contains(p, kind) {
			parts = append(parts, kind)
		}
		if len(columns[kind]) > 0 {
			parts = append(parts, kind+" ("+strings.Join(columns[kind], ", ")+")")
		}
	}
	return strings.Join(parts, ", ")
}

// relation quotes a declared relation name, schema-qualified unless in public.
func relation(name string) string {
	return pgx.Identifier(strings.Split(name, ".")).Sanitize()
}

type querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// appSchemas restricts a pg_namespace alias n to the application's schemas.
const appSchemas = `n.nspname NOT LIKE 'pg\_%' AND n.nspname<>'information_schema'`

// grantee names PUBLIC and the declared roles; privileges of other roles,
// such as an operator's own reporting role, are left to the operator.
const grantee = `CASE a.grantee WHEN 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END`

// catalog reads the privileges of PUBLIC and every declared role on each
// relation, and on each schema, keyed by name and then grantee. Relations in
// public are named without their schema; an extension's own relations, such
// as pg_stat_statements, are not the application's and are left out.
func catalog(ctx context.Context, db querier) (relations, schemas map[string]map[string]privileges, err error) {
	names := make([]string, 0, len(Roles)+1)
	for _, role := range Roles {
		names = append(names, role.Name)
	}
	names = append(names, "PUBLIC")
	relations, schemas = map[string]map[string]privileges{}, map[string]map[string]privileges{}
	rows, err := db.Query(ctx, `
WITH relations AS (
 SELECT c.oid,c.relowner,c.relacl,CASE n.nspname WHEN 'public' THEN '' ELSE n.nspname||'.' END||c.relname AS name
 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE `+appSchemas+` AND c.relkind IN ('r','p','v','m','S','f')
 AND NOT EXISTS(SELECT FROM pg_depend d WHERE d.classid='pg_class'::regclass AND d.objid=c.oid AND d.deptype='e')
)
SELECT r.name,'','' FROM relations r
UNION ALL
SELECT r.name,`+grantee+`,a.privilege_type FROM relations r, aclexplode(r.relacl) a
WHERE a.grantee<>r.relowner
UNION ALL
SELECT r.name,`+grantee+`,a.privilege_type||' ('||t.attname||')'
FROM relations r JOIN pg_attribute t ON t.attrelid=r.oid AND t.attnum>0 AND NOT t.attisdropped, aclexplode(t.attacl) a
WHERE a.grantee<>r.relowner`)
	if err != nil {
		return nil, nil, err
	}
	var name, role, privilege string
	if _, err = pgx.ForEachRow(rows, []any{&name, &role, &privilege}, func() error {
		add(relations, name, role, privilege, names)
		return nil
	}); err != nil {
		return nil, nil, err
	}
	rows, err = db.Query(
		ctx,
		`SELECT n.nspname,`+grantee+`,a.privilege_type FROM pg_namespace n, aclexplode(n.nspacl) a WHERE `+appSchemas+` AND a.grantee<>n.nspowner`,
	)
	if err != nil {
		return nil, nil, err
	}
	if _, err = pgx.ForEachRow(rows, []any{&name, &role, &privilege}, func() error {
		add(schemas, name, role, privilege, names)
		return nil
	}); err != nil {
		return nil, nil, err
	}
	for _, objects := range []map[string]map[string]privileges{relations, schemas} {
		for _, byRole := range objects {
			for role := range byRole {
				slices.Sort(byRole[role])
			}
		}
	}
	return relations, schemas, nil
}

func add(objects map[string]map[string]privileges, name, role, privilege string, roles []string) {
	if objects[name] == nil {
		objects[name] = map[string]privileges{}
	}
	if privilege != "" && slices.Contains(roles, role) {
		objects[name][role] = append(objects[name][role], privilege)
	}
}

// Undeclared lists the relations in the database that have no declared
// privileges, such as a new table nobody has declared yet.
func Undeclared(ctx context.Context, db querier) ([]string, error) {
	relations, _, err := catalog(ctx, db)
	if err != nil {
		return nil, err
	}
	var names []string
	for name := range relations {
		if _, ok := grants[name]; !ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names, nil
}

// Drift returns the statements that would bring the privileges of PUBLIC and
// the declared roles to the declaration, which are none after migrations.
func Drift(ctx context.Context, db querier) ([]string, error) {
	relations, schemas, err := catalog(ctx, db)
	if err != nil {
		return nil, err
	}
	var statements []string
	usage := map[string]map[string]privileges{}
	grantees := append(slices.Clone(Roles), Role{Name: "PUBLIC"})
	for _, name := range slices.Sorted(maps.Keys(grants)) {
		actual, ok := relations[name]
		if !ok {
			return nil, fmt.Errorf("privileges are declared for %s, which does not exist", name)
		}
		schema := "public"
		if qualifier, _, found := strings.Cut(name, "."); found {
			schema = qualifier
		}
		if usage[schema] == nil {
			usage[schema] = map[string]privileges{}
		}
		for _, role := range grantees {
			want := parsePrivileges(grants[name][role])
			if len(want) > 0 {
				usage[schema][role.Name] = privileges{"USAGE"}
			}
			statements = append(statements, align("TABLE "+relation(name), role.Name, actual[role.Name], want)...)
		}
	}
	for _, schema := range slices.Sorted(maps.Keys(usage)) {
		if slices.Contains(schemas[schema]["PUBLIC"], "CREATE") {
			statements = append(statements, "REVOKE CREATE ON SCHEMA "+relation(schema)+" FROM PUBLIC")
		}
		for _, role := range Roles {
			statements = append(
				statements,
				align("SCHEMA "+relation(schema), role.Name, schemas[schema][role.Name], usage[schema][role.Name])...)
		}
	}
	return statements, nil
}

// align returns the statements that replace role's privileges on object with
// want, or none when they already match.
func align(object, role string, actual, want privileges) []string {
	if slices.Equal(actual, want) {
		return nil
	}
	grantee := role
	if role != "PUBLIC" {
		grantee = pgx.Identifier{role}.Sanitize()
	}
	statements := []string{"REVOKE ALL ON " + object + " FROM " + grantee}
	if len(want) > 0 {
		statements = append(statements, "GRANT "+want.sql()+" ON "+object+" TO "+grantee)
	}
	return statements
}

// Reconcile creates any missing role and brings privileges to the
// declaration. It needs CREATEROLE only while a role is missing.
func Reconcile(ctx context.Context, tx pgx.Tx) error {
	for _, role := range Roles {
		if err := ensureRole(ctx, tx, role); err != nil {
			return err
		}
	}
	statements, err := Drift(ctx, tx)
	if err != nil {
		return err
	}
	for _, statement := range statements {
		if _, err = tx.Exec(ctx, statement); err != nil {
			return fmt.Errorf("%s: %w", statement, err)
		}
	}
	return nil
}

// ensureRole creates role unless it exists, and refuses one that can do more
// than hold privileges: roles are shared by every database in the cluster.
func ensureRole(ctx context.Context, tx pgx.Tx, role Role) error {
	for {
		var elevated bool
		err := tx.QueryRow(ctx, `SELECT rolcanlogin OR rolsuper OR rolcreaterole OR rolcreatedb OR rolreplication OR rolbypassrls FROM pg_roles WHERE rolname=$1`, role.Name).
			Scan(&elevated)
		switch {
		case err == nil && elevated:
			return fmt.Errorf("database role %s must be a NOLOGIN role without other attributes", role.Name)
		case err == nil:
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		// Migrations of other databases in the cluster may create it at the
		// same moment, so a lost race is retried rather than fatal.
		err = pgx.BeginFunc(ctx, tx, func(savepoint pgx.Tx) error {
			_, err := savepoint.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role.Name}.Sanitize()+" NOLOGIN")
			return err
		})
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && (pgErr.Code == "42710" || pgErr.Code == "23505") {
			continue
		}
		if err != nil {
			return fmt.Errorf("create database role %s (the migration user needs CREATEROLE): %w", role.Name, err)
		}
		return nil
	}
}
