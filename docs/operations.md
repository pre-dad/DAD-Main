# Operating the R1 sandbox

Follow the README for local setup. All money records are test-mode records; do not mix
these with a later production database or describe sandbox awards as real recognition.

## Database and containers

Use a distinct PostgreSQL database and cluster for each environment. Every process
connects with its own login, which holds only what that process needs
([database roles](#database-roles)). Each process's database pool is sized for its role:
the API opens up to 40 connections, and each worker, the monitor, the reconcile job,
migrations, the staff command, and `rekey` up to 2 (`Pool` in `internal/dbroles/roles.go`;
each login's `CONNECTION LIMIT` is set to the same number when you provision it). One
replica of every process therefore needs up to 60 connections: the API's 40, six workers
at 2, and the monitor, reconcile job, staff command and `rekey` at 2 each. A migration
adds 2 while it runs. PostgreSQL's default `max_connections` is 100, of which 3 are
reserved for superusers. When adding API replicas, raise `max_connections` (or put a
[transaction-mode pooler](#connection-pooling) in front) so the total stays below it; `pool_max_conns=N` in a
process's database URL overrides its size.

Back up the database together with its separately stored encryption keys, then verify a
restore. Losing the email or content encryption key makes its encrypted records
unreadable. No automatic backup service is included, and keys are rotated by hand
([encryption keys](#encryption-keys)).

```sh
cp .env.production.example .env.production
cp .env.database.example .env.database
# Fill in IMAGE_TAG (for example the git commit), the public and staff HTTPS origins, the
# ingress mode and certificate directory, and required test-mode secrets in
# .env.production, and the database password and every process's login in .env.database.
# Keep the previous IMAGE_TAG for rollback.
./scripts/deploy.sh build
./scripts/deploy.sh migrate
./scripts/deploy.sh provision-roles
./scripts/deploy.sh up
./scripts/deploy.sh smoke
```

`ENV_FILE` (default `.env.production`) supplies application settings for Compose
interpolation; each container receives only its explicitly listed settings and secrets.
`DATABASE_ENV_FILE` (default `.env.database`) holds database credentials and is not mounted
or passed wholesale to containers: Compose gives each service its own login's URL, and the scripts
refuse database credentials in `ENV_FILE`. Both may be relative to the current directory.

`migrate` starts PostgreSQL, applies embedded forward SQL migrations, and brings the
process roles and their privileges to their declaration. `provision-roles` then creates or
updates each process's login ([database roles](#database-roles)). An existing
pre-rewrite database, one whose `alembic_version` table holds `0030_donor_public_content`, requires `./scripts/deploy.sh adopt`
instead of the initial migrate step. Adoption compares the complete donor schema, records
the baseline checksum, and preserves data. Stop all older API/worker processes before
adoption; never run two versions of financial writers. Unsupported or divergent schemas
require a reviewed migration, not a forced baseline stamp.

`up` starts the API, web service, and nginx, not workers. Set `DONOR_GO_WORKERS_ENABLED=true`
only when ready to process test payments and deliver configured email, then run
`./scripts/deploy.sh workers`; the script refuses while the flag is unset. `down` stops
services without deleting database volumes. `logs` follows service logs, which Docker's
default `json-file` driver keeps without limit; they include nginx's access log of client
addresses, so set rotation in the Docker daemon's `log-opts` and keep logs no longer than
the approved retention policy allows. No command
pushes images or activates live donations. The web container runs with a read-only filesystem and tmpfs mounts for
`/tmp` and `/app/.next/cache`. `up` and `reload-ingress` recreate nginx when its
configuration has changed ([edge](edge.md#modes)), and `reload-ingress` makes it reread
renewed certificates without dropping connections.

The API refuses to start with only one of `STRIPE_SECRET_KEY` and
`STRIPE_WEBHOOK_SECRET`, with a malformed key in an encryption key list (the error names
the variable and the key's position, never the key), with a trusted proxy range
broader than an IPv4 /8 or IPv6 /32, with a `DONOR_STAFF_ORIGIN` that is not an exact
HTTPS origin or that is the public host or a parent domain of it, or with the retired
`DONOR_STAFF_ENCRYPTION_KEY` set but no staff origin. Both origins must be written as
browsers send them: lowercase, with no default port, path, or trailing slash. It logs which optional features are disabled.
Every process except migrations and provisioning refuses to start when its database login
can change the schema, even while assuming a role, or lacks its role
([database roles](#database-roles)).
Workers that hit a database or provider failure log the error type and retry with
exponential backoff up to one minute; persistent failures show up as queue age.

Browsers reach the app only through nginx, which publishes ports 80 and 443, terminates
TLS, separates the public and staff hosts, and applies per-client limits. In production it
runs behind Cloudflare, which absorbs attacks, applies distributed limits, and caches
static pages; it also runs without Cloudflare. [Edge and ingress](edge.md) covers modes,
certificates, the Cloudflare configuration, and the limits at each layer. The web service
publishes no host port and the API binds to loopback. Set the exact `DONOR_PUBLIC_ORIGIN`
and `DONOR_STAFF_ORIGIN`. Configure database TLS for remote database connections.

The back office has its own hostname, such as `staff.donors.example.org`. Point DNS for it
at the ingress and include it in the TLS certificate; nginx forwards the original `Host`
header, and the web server chooses what to serve by host. Both origins are build arguments
of the web image as well as API settings, so rebuild the web image when either changes.
Staff passkeys belong to the staff hostname; changing it means every staff member enrols
again.

Rate limits are per visitor. nginx sets `X-Real-IP` to the client address, which the API
trusts only from the `app` network, `172.30.43.0/24`, that the web containers alone share
with it ([edge](edge.md#client-address)). Setting `DONOR_CLIENT_IP_HEADER` empty makes all
visitors share the proxy's quota. Edit the Compose networks if `172.30.42.0/24` or
`172.30.43.0/24` clashes on the host.

The API's in-process limiter keeps a token bucket per client and per class, so a burst
on one surface cannot starve another for visitors sharing an address; the classes and
the limits at each layer are in [edge](edge.md#rate-limits). Rejections carry an exact
`Retry-After`. It tracks at most 65,536 buckets and evicts an existing bucket rather than refusing new
clients when full. It is the per-instance last line behind the nginx and Cloudflare
limits. Capacity must still be validated against the agreed launch spike.

`/health/live` checks process availability. `/health/ready` checks the migration history
through PostgreSQL with a deadline. It can be ready while Stripe or SMTP is intentionally
unconfigured. Readiness is not payment-provider acceptance or live-release authorization.

Features that are not configured answer 503 with a `code` (`PAYMENTS_UNCONFIGURED`,
`EMAIL_UNCONFIGURED`, `SOURCE_STORAGE_UNCONFIGURED`, `PUSH_UNCONFIGURED`, or
`FOUNDING_CLAIMS_UNCONFIGURED`) and are not logged as errors; a
503 without a code is a transient failure and is logged.

### Upgrading an existing deployment

1. Set a new `IMAGE_TAG`, keeping the previous one for rollback, and run
   `./scripts/deploy.sh build`.
2. Run `./scripts/deploy.sh migrate` before starting any new API or worker process, which
   requires the complete migration history.
3. Run `./scripts/deploy.sh up`, then `workers` if they are enabled, then `smoke`.

Some migrations and releases need more:

- Projection rules version 11 prevents expired continuity from awarding new streak
  badges. Run the projections worker after upgrading so overdue projections refresh;
  previously earned badges remain permanent.

- The DAD rebrand changes image names, example database names/logins, and the default
  TLS directory. Existing PostgreSQL volumes keep their original database, owner, and
  password: changing `POSTGRES_*` does not rename or update an initialized cluster.
  Keep the deployed values and matching process URLs in `.env.database`; use explicit
  `INGRESS_TLS_DIR` if certificates remain at their previous path. The production
  Compose project is now `dad`, and Compose names volumes after the project, so a host
  deployed before the rebrand would start an empty `dad_postgres-data` volume beside its
  existing `impactgraph_postgres-data`. On such a host, add
  `COMPOSE_PROJECT_NAME=impactgraph` to `.env.production`, which overrides the project
  name, until the database has been backed up, restored into the new project, and
  verified ([backup and restore drill](#backup-and-restore-drill)). Never delete the old
  volume to complete the rebrand. Local Compose
  now uses `dad_postgres`; the former `impactgraph_postgres` volume is separate and
  must be backed up and deliberately migrated if its data is needed. A checkout made
  before the rename may still run that project's PostgreSQL container, which holds port
  55432 so that `make db-up` fails; stop it with `docker stop impactgraph-postgres-1`,
  which keeps its volume.
- The release that binds donor passkey registration to its originating session rejects
  registrations started on an older image. These challenges last five minutes; donors
  can restart registration after the upgrade. Existing passkeys and sessions still work.
- `0007_staff_passkeys` replaces staff passwords and authenticator codes with passkeys.
  It deletes the old credentials and ends every staff session and open invitation, so
  every staff member enrols again: run break-glass recovery
  ([staff and content](#staff-and-content)) for one administrator, who creates a passkey
  from the printed invitation and then issues recovery invitations to the remaining staff
  from `/staff`.
- `0008_referral_touch_retention` builds its indexes without `CONCURRENTLY`, blocking
  checkout writes while it runs; on a large database, migrate at a quiet time.
- `0083_scan_free_lookups_and_credential_retention` builds its indexes without
  `CONCURRENTLY` (migrations run in one transaction), blocking writes to each table while its
  index builds. It is quick on a launch-sized database; on a large one, migrate at a quiet
  time. It also lets a session be deleted while the audit rows that named it stay.
- `0009_subscription_sync` adds the billing anchor that dates renewals. A
  subscription synced before it shows no anchor until the Stripe worker next syncs it, so
  start the workers promptly after `up`.
- `0015_pending_refund_reconciliation` indexes unsettled refunds for bounded batch reads.
  Index creation blocks refund writes; migrate at a quiet time on a large database.
- The release that introduces [database roles](#database-roles) runs each process as its
  own login, and the processes refuse to start as the schema owner. Before step 2, create
  `.env.database` from `.env.database.example`: move `POSTGRES_USER`, `POSTGRES_PASSWORD`,
  `POSTGRES_DB`, and `DONOR_GO_DATABASE_URL` out of `.env.production` into it, and give
  every process URL its own generated password. Between steps 2 and 3, run
  `./scripts/deploy.sh provision-roles`. Rolling back to an earlier image also needs that
  release's Compose file and environment files, since earlier processes read the owner's
  URL.

## Database roles

Each process connects as a login that is a member of one `NOLOGIN` group role, and the
role holds only the privileges that process's statements need, so a fault or compromise in
one process reaches only that process's data. No process role owns objects, can create
them, or can grant anything; only the schema owner, which migrations use, changes the
schema.

| Role | Process | URL variable | Access |
|---|---|---|---|
| `dad_api` | API | `DONOR_API_DATABASE_URL` | Reads every table for views and exports; writes donor, session, checkout, recurring-command, projection, staff, content, and webhook-inbox records; cannot change gifts or billing periods, and deletes only reservations, subscription leases, passkey ceremonies, and passkeys |
| `dad_worker_stripe` | Stripe worker | `DONOR_WORKER_STRIPE_DATABASE_URL` | Processes the webhook inbox and writes gifts, billing periods and observations, refunds, subscriptions, card fingerprints, projections, and the mail and push queues; reads sessions only to find app devices; no staff or content records |
| `dad_worker_projections` | Projection worker | `DONOR_WORKER_PROJECTIONS_DATABASE_URL` | Reads financial facts; writes projections and queues mail and push; deletes expired referral touches |
| `dad_worker_login_email` | Login-email worker | `DONOR_WORKER_LOGIN_EMAIL_DATABASE_URL` | Sign-in email queue and the links it issues |
| `dad_worker_email_changes` | Email-change worker | `DONOR_WORKER_EMAIL_CHANGES_DATABASE_URL` | Email-change proofs |
| `dad_worker_messages` | Message worker | `DONOR_WORKER_MESSAGES_DATABASE_URL` | Financial mail queue; reads the facts a message renders |
| `dad_worker_push` | Push worker | `DONOR_WORKER_PUSH_DATABASE_URL` | Push queue and session push tokens; reads the facts it rechecks |
| `dad_monitor` | Monitor | `DONOR_MONITOR_DATABASE_URL` | Reads queue status columns only: no addresses, bodies, tokens, or payloads |
| `dad_reconcile` | `reconcile` | `DONOR_RECONCILE_DATABASE_URL` | Reads financial facts to reconcile Stripe charges and refunds against the database; SELECT only, no writes |
| `dad_staff_cli` | Staff command | `DONOR_STAFF_CLI_DATABASE_URL` | Staff accounts, invitations, passkeys, sessions; writes audit records |
| `dad_rekey` | `rekey` | `DONOR_REKEY_DATABASE_URL` | Reads and rewrites the encrypted columns and their ids only |

Every role reads the migration history. A worker that only locks a donor (`SELECT … FOR
UPDATE`, which PostgreSQL allows only with `UPDATE` on some column) holds `UPDATE` on the
`id` column alone, so it cannot change an address or any other field.
`DONOR_GO_DATABASE_URL` is the schema owner's URL, used only by `migrate` and
`provision-roles`.

### Supplying a password as a secret

Each URL variable (`DONOR_API_DATABASE_URL`, `DONOR_WORKER_STRIPE_DATABASE_URL`,
`DONOR_GO_DATABASE_URL`, and the rest) can also be given as parts, so the password
reaches the process as its own secret (an ECS `secrets` entry) and never sits in a
plaintext task-definition `environment` value. For a variable `X_URL`, with prefix `X`
(the variable without `_URL`, for example `DONOR_API_DATABASE`):

| Form | Variables |
|---|---|
| Full URL (unchanged) | `X_URL` |
| URL plus separate password | `X_URL` without a password, and `X_PASSWORD`; the separate password replaces any in the URL |
| Parts | `X_HOST`, `X_NAME`, `X_USER`, `X_PASSWORD`, optional `X_PORT` (default `5432`) and `X_SSLMODE` |

On AWS every task uses the middle form: a password-less `X_URL` plus `X_PASSWORD` as an
ECS secret (see `docs/aws-deployment.md` section 5.1). The DAD-Main release that accepts
it is deployed before the DAD-infra change that relies on it.

A set `X_URL` always wins over the parts. The password is never logged: the process
reports only variable names in its errors.

The privileges are declared table by table in `apps/donor-api/internal/dbroles/privileges.go`,
in `GRANT` syntax, derived from the statements each process runs. Every `migrate` run
creates any missing role and brings privileges to the declaration in the same transaction
as the migrations, revoking anything else granted to `PUBLIC` or a process role, so a
manual grant does not survive the next deployment. Privileges of other roles, such as an
operator's reporting role, are left alone. The tests compare the database catalog with the
declaration, fail for any table without one, and run each process's code with exactly its
role's privileges.

To add a table, create it in a new migration and, in the same change, give it an entry in
the declaration listing every role whose statements touch it. A table no process uses
still needs an entry, empty. A table in a new schema, such as R2's recipient schema, is
declared as `schema.table`; roles receive `USAGE` on exactly the schemas holding their
tables. A new process needs a new role in `internal/dbroles/roles.go`, its URL in
`.env.database.example` and its Compose service, and a provisioned login. `make test`
fails until the declaration matches.

### Provisioning logins

`./scripts/deploy.sh provision-roles` (or `make provision` with the variables loaded) runs
as the schema owner after migrations. For each process it reads the user and password from
the process's URL, creates or updates that login, makes it a member of the process's role,
and sets its connection limit to the process's pool size, or to `pool_max_conns` in its
URL. Only a SCRAM verifier computed locally reaches the server, never the password. It
refuses a URL naming the owner, a group role, a superuser or other privileged role, the
same user for two processes, a password shorter than 16 printable ASCII characters, or a
database other than the owner's, and it refuses while a role's privileges reach any user
besides its configured login, such as a login left from a rename: revoke the role from it
or drop it, then run provisioning again. It can be run at any time and changes nothing
else. Connection limits assume one instance of each process, as Compose runs them; a
one-off run of a process, such as the Stripe worker's `--retry-event`, shares its login's
limit, and more replicas of a process need a higher limit than provisioning sets.

Under Compose, `./scripts/deploy.sh monitor`, `reconcile`, `rekey`, and `staff` run those
commands as their own services with their own logins, and the Stripe worker's retry runs as
`docker compose … run --rm stripe-worker worker --kind stripe --retry-event evt_example --once`
with both environment files.

### Rotating database passwords

1. Generate a new password (`openssl rand -hex 32`) and put it in the process's URL in
   `.env.database` (or, with a password-less URL, in its `X_PASSWORD` variable or secret).
2. Run `./scripts/deploy.sh provision-roles`. The login accepts only the new password
   from now on: connections already open stay open, but the running process cannot open
   new ones, so go straight on.
3. Restart the process: `./scripts/deploy.sh up` for the API, `workers` for workers.
   Commands such as `monitor` read the URL on each run.

The owner's password is changed with `ALTER ROLE` by a superuser, then in
`DONOR_GO_DATABASE_URL`; the bundled PostgreSQL reads `POSTGRES_PASSWORD` only when it
initializes an empty data volume. A suspected leak of any database credential is handled
the same way, together with [incident response](#queue-alerts-and-incident-response).

### Local development and managed PostgreSQL

Locally, processes connect as the owner when `DONOR_ALLOW_OWNER_CONNECTION=true` and
their own URL is unset, as `.env.example` sets. To run a process with only its role's
privileges, give it a URL that assumes the role, which a superuser may, such as
`DONOR_API_DATABASE_URL="$DONOR_GO_DATABASE_URL?role=dad_api"`; since the session's login
could still reset the role, a process accepts that only with the same setting, and checks
that the role is its own. For the closest match to a deployment, set every process URL to
a login of its own and run `make provision`. Tests and browser tests run the API, workers,
and commands with assumed roles, so the test database's user must be a superuser. The
role tests also require a database migrated to the checkout and fail saying so when it is
behind; the local recipe is under Checks in the README.

Roles belong to the whole PostgreSQL cluster, not one database, so run each environment
in its own cluster: provisioning refuses a role whose privileges reach another
environment's login. The migration user needs `CREATEROLE` the first time `migrate`
creates the roles, and provisioning needs `CREATEROLE` with admin rights over the roles
and logins it manages, which PostgreSQL 16 grants to the role that creates them. The
bundled Compose database's owner is its superuser. On Amazon RDS, create a dedicated
owner with `CREATE ROLE owner LOGIN CREATEROLE` as the master user (a member of
`rds_superuser`) and give it the application database; Cloud SQL's
`cloudsqlsuperuser` members can do the same. If a DBA creates the group roles instead,
they must be plain `NOLOGIN` roles and the owner needs `ADMIN OPTION` on each. Configure
TLS (`sslmode=verify-full`) in every URL for a remote database.

Backups are taken without privileges. After restoring one into an application database,
run `migrate`, which recreates missing roles and grants the declared privileges, then
`provision-roles`.

### Connection pooling

Every process works through a transaction-mode pooler, such as PgBouncer, RDS Proxy, or a
managed provider's pooler: none keeps session state between transactions, and migrations
take only transaction-scoped advisory locks. Point each process's URL at the pooler with
its own login. pgx prepares and caches statements on each connection, so either the
pooler must track protocol-level prepared statements (PgBouncer 1.21 or later with
`max_prepared_statements` above zero), or every URL must add
`default_query_exec_mode=exec`, which uses unnamed statements only. A pooler that does
neither fails most requests with SQLSTATE 42P05 or 26000. The `role` URL parameter that
local development uses is a startup parameter poolers do not forward; deployed processes
use their own logins instead. Size the pooler's server connections to the database's
`max_connections`, and leave each process's own pool size unchanged.

## Stripe

Stripe limits requests per account: by default 25 per second in a sandbox and 100 in
live mode ([rate limits](https://docs.stripe.com/rate-limits)). Set the account's limit
as `DONOR_STRIPE_REQUESTS_PER_SECOND` (default 25) for every Stripe caller. It is divided
among four buckets so that one kind of traffic cannot starve another: event-processing
reads 40%, donor checkout and card setup 30%, staff and donor subscription commands
20%, and the daily reconciliation 10%. Each bucket admits a burst of one second of its
rate. The API and Stripe worker reserve every request in the `donor_stripe_budgets`
table, so any number of their replicas together stay within the limit; a request whose
turn would come after its deadline fails at once and is retried like any processor
outage. The reconciliation job, which runs once a day as a single process, paces itself
within its own share. The SDK's automatic retries are not budgeted, so leave headroom
below the account's real limit. The local API load test excludes provider calls and
cannot validate that capacity.

Use the organization's verified Stripe **test** account and snapshot webhook events at
API version `2026-09-30.endive`. Subscribe to:

- `invoice.payment_succeeded` and `invoice.payment_failed` for monthly periods.
- `refund.created`, `refund.updated`, and `refund.failed`.
- `checkout.session.completed` and `checkout.session.expired`.
- `customer.subscription.created`, `customer.subscription.updated`,
  `customer.subscription.deleted`, `customer.subscription.pending_update_applied`, and
  `customer.subscription.pending_update_expired`.

New `payment_intent.succeeded` events are ignored by the R1 receiver. Retained
pre-R1 one-time gift records remain readable; new giving is monthly only.

Point the endpoint at `/api/donor/stripe/webhook` on the public origin. nginx gives it
its own generous limit ([edge](edge.md#rate-limits)); Cloudflare must skip its bot and
rate rules for it ([edge](edge.md#waf)). Verify signatures over original bytes.
Live events, Connect events, and mismatched API versions are rejected. No card details
belong in DAD requests, storage, logs, or support tools.

The Stripe worker persists verified collection, refunds, and billing schedules.
Checkout completion alone is not a gift. Incomplete or unsupported invoices remain
unprocessed rather than becoming guessed financial facts. A single-item monthly USD
subscription with a complete card payment is the supported billing model. Between
events, the worker syncs any subscription stored without a billing anchor (one saved
before migration 0009), one at a time and checking hourly; one that fails to sync is
logged as a worker step failure and retried in the next hourly pass.

Account deletion waits two hours for an open checkout, but Stripe can still report one
later. When a gift is paid after its donor deleted the account, the worker queues a full
refund of that payment and an immediate cancellation of its subscription, then makes
both Stripe calls between events. The gift stays in the ledger, and the refund and
cancellation are recorded from their own events. A failed call is retried every ten
minutes; after ten attempts it parks and the monitor's `reversals` queue counts it.
Check the error class and the payment in Stripe, then refund or cancel it there.

Monitor the oldest unprocessed `donor_stripe_events`, attempt counts, and worker health.
A failed event waits before its next attempt, doubling from one minute up to an hour,
so a provider outage or an event that arrives before its gift is retried later rather
than immediately. Events park after ten failed attempts. Investigate the error class
(`PaymentUnavailable`, `MissingRecord`, `InvalidFacts`, or `ProcessingError`) and current
Stripe state before explicitly retrying:

```sh
cd apps/donor-api
go run ./cmd/worker --kind stripe --retry-event evt_example --once
```

`--once` processes at most one eligible event, which may not be the reset event. Keep the
worker running to drain the queue. Never delete financial facts to make a retry succeed.

### Daily Stripe reconciliation

The monitor only sees the `donor_stripe_events` inbox, so a webhook Stripe never delivered
leaves no row to alarm on. The `reconcile` command closes that gap: once a day it lists the
charges and refunds Stripe reports and diffs them against the gifts and refunds the database
recorded.

```sh
./scripts/deploy.sh reconcile                 # yesterday, full New York calendar day
./scripts/deploy.sh reconcile --date 2027-03-01   # re-run a specific day deterministically
```

- **Schedule.** Run it ~00:30 America/New_York, once yesterday's New York calendar day has
  closed. New York is the system's authoritative civil-time zone, so the day finance reasons
  about and the day the job reconciles are the same. The NY-day bounds are converted to a UTC
  `created` range for Stripe. Database rows match by processor ID across day boundaries;
  local `paid_at`/`observed_at` rows also receive a reverse provider check when needed.
  Older pending or action-required refunds are checked daily until their terminal status
  is recorded, so a dropped status-update event cannot leave them unsettled indefinitely.
- **Processing delay.** `--max-lag` (default 30m) requires waiting until the window's end
  plus that delay. Earlier runs fail. Once settled, the entire window is checked,
  including its final 30 minutes; repeating an older day cannot hide a dropped event.
- **Scope.** R1 reconciles charges and refunds only. Invoices, billing periods and
  subscriptions are a phase-2 concern and are deliberately not reconciled yet; the role's
  SELECT grant on `donor_billing_periods` and `donor_billing_observations` leaves the seam.
- **Output and alarm.** The job prints one aggregate JSON object — counts per discrepancy
  class (`missing_gifts`, `missing_refunds`, `amount_mismatches`, `status_mismatches`,
  `db_without_stripe`, `currency_mismatches`, `invalid_objects`), the window, and ids only,
  never donor identifiers or payloads — and
  exits non-zero when any discrepancy count is greater than zero, so a CloudWatch alarm fires
  on the exit code. A clean day prints the JSON and exits 0. It holds no state and writes no
  report, so a re-run is safe and compares the latest available processor and database
  state. Failed and uncollected charges do not require gifts. Captured amounts, currency,
  and refund statuses must agree; malformed facts cannot produce a clean verdict.
- **Bounds and account scope.** Window records are limited to 10,000 per side; exceeding
  that fails, so use smaller explicit `--since`/`--until` ranges. Older unsettled refunds
  are read in pages of 100, up to 100,000 records, with full discrepancy counts and
  bounded identifier lists. The report includes `pending_refunds_checked` and
  `pending_refund_batches` for these additional reads. The default job deadline remains
  60 seconds; use `--timeout 15m` for a large backlog (allowed range 1s to 1h).
  Deadline or provider failure produces a nonzero exit and no partial verdict.
  Reads are sequential with a 100 ms pause before each provider request, leaving capacity
  for workers and staff. A longer run holds a read-only snapshot, so schedule backlog
  checks away from peak traffic and monitor database transaction age.
  The job audits the entire configured Stripe test account. Unrelated collected payments
  in a shared account are flagged too; use a dedicated account for DAD acceptance.
  For legacy invoice gifts missing a PaymentIntent ID, the job uses Stripe's read-only
  Invoice Payments API to prove the link in either direction, including across day
  boundaries. It does not backfill or alter the stored gift. A missing proof is flagged;
  multiple payments, partial allocations, unsupported payment types, or truncated lists
  fail for operator review. The configured test key needs Invoice Payments read access.
- **Read-only.** The job is strictly read-only against both Stripe (List/Retrieve only) and
  the database (its `dad_reconcile` role has SELECT alone). It never repairs, deletes or
  writes any financial fact.
- **Remediation is manual.** When the job flags a missing gift or refund, re-send the event
  in Stripe, then run `worker --kind stripe --retry-event evt_example --once` to process it.
  Never delete financial facts to make a reconciliation pass.

Financial commands retain immutable inputs and processor idempotency keys. Unknown
requests older than 23 hours require reconciliation. Hosted card-setup retries stop
before their fixed expiry; an old unknown request is not permission to create a new one.
Recurring views, card update status, and staff refund and cancellation previews never
call Stripe: they show the state the worker last synced from verified webhooks, and
`observed_at` says when that was. Pending refunds reserve their amount in previews. A
change reads Stripe first and syncs what it finds, even when it then refuses the change,
so the donor's next view is current. While the Stripe worker is behind, views lag by the
age of the oldest unprocessed event, and changes refused as out of date answer 409. A
staff refund whose outcome was lost stays pending until the original action is retried.
A second recurring change submitted while another is still with Stripe receives 409; the
subscription lease expires after two minutes if a writer stops mid-command, and the same
request ID can then resume it. Historical one-time gift facts take their paid time from
the successful charge, so a restricted key needs read access to PaymentIntents and Charges.
Refund and recurring cancellation are separate actions requiring named permissions,
confirmation, reasons, and audit records. Never edit processor identifiers by hand.

## Email and projections

Run projection, login-email, email-changes, and message workers separately. Email changes
use the same mail configuration, require current-address proof before new-address proof,
and park delivery after five failures. Requesting a replacement invalidates prior proofs.
The queue workers (`login-email`, `email-changes`, `messages`, `push`) accept
`--concurrency N` (up to 8) to run N claim loops in one process. Each loop claims a row with
`FOR UPDATE SKIP LOCKED`, so none is delivered twice; N may not exceed the login's pool size,
so raise `pool_max_conns` in the worker's database URL and re-run `make provision` first.
Projection refresh handles grace expiry, recurring boundaries, permanent recognition,
and prior-year summaries. A projection is recomputed when facts that change it arrive and
at its next time boundary: a due date, period end, grace deadline, cancellation,
refund, or New Year in New York, and at least every 25 to 35 days, a span fixed per
donor so that projections computed together fall due over ten days. A release that changes
recognition, or the messages and push intents queued with it, raises `projectionRules` in
`internal/store/financial.go`; the projection worker then recomputes every older
projection whenever no projection is due. A donor whose facts cannot be projected is deferred with
backoff, records an error class, and counts as parked in the monitor; other donors keep
refreshing. Unknown streak state displays Updating; it must not silently become zero.

Global rankings use `donor_projection_schedules` to claim one refresh every 15 minutes
across projection replicas, even when no donors qualify. The schedule and ranking changes
commit together, so a failed refresh remains due. Publishing an event records a durable
alert job in the same transaction; projection workers resume it in batches of at most 256
eligible volunteers. Republishing does not restart the job. Each batch rechecks publication,
cancellation, start time, and notification choices; delivery rechecks the recipient again.
Apply migration `0054_projection_schedules_and_event_fanout.sql` before these binaries.

History: migration `0017_referral_corrections.sql` had to be applied before binaries
using projection rules version 6 (the current version is 12, `projectionRules` in
`internal/store/financial.go`). Older projections then converge on the earliest verified successful payment
through the normal refresh worker. Corrections, transferred referral totals, and new
notification intents commit together. Monitor projection errors during convergence and
use the staff `referral-corrections` export to inspect changed attribution. The migration
does not rewrite gift facts or reconstruct corrections from before this release.

The projection worker also applies referral-touch retention ([donor domain](donor-domain.md#referrals)):
on start and then hourly it deletes touches that led to no attributed gift and were
captured more than 13 months ago, at most 2,000 per transaction and one batch between projection
refreshes, until none remain. Concurrent projection workers share the work. A failed pass
is logged as a worker step failure and retried an hour later without holding up
projections. Retention is not a monitored queue, since a touch past retention waits up
to an hour by design.

The same pass deletes company-code touches once they expire, analytics events after 13
months, and credentials once they have lapsed: sessions 90 days after their last credential expired, sign-in links and completed
sign-in emails after 30 days, and a phone code a day after it expired. Session audit rows
(`donor_session_events`) are kept with no session.
Mail workers claim a job, then send outside the donor lock, and Stripe calls for commands,
refunds, card setup, and webhooks likewise run without it, so a slow provider cannot block
payments or staff actions for the same donor.

SMTP requires STARTTLS, certificate validation, and configured sender credentials.
Delivery is at least once; acknowledgement loss may cause a duplicate. Stable Message-IDs
support retries, not exactly-once guarantees. Login payloads are encrypted; financial
message bodies require the database's access and backup protections. Login links expire
15 minutes after preparation. Jobs park after five failures. Optional referral notices
recheck consent before each send; recovery stops after payment or cancellation.

Staff can inspect delivery history and request audited receipt/summary resends or a
replacement claim email. A queued response does not mean delivery succeeded. Monitor
queue age and parked jobs; automated paging and provider acceptance remain operational
work. Test controlled mailboxes before inviting donors.

### Capturing mail in development

So development never sends real email and you can read what the app would have sent,
`docker-compose.yml` includes a Mailpit capture inbox: SMTP on `127.0.0.1:1025` and a web
UI at http://127.0.0.1:8025. The app's STARTTLS and certificate verification stay
mandatory — Mailpit uses a locally generated certificate the app is told to trust via
`DONOR_SMTP_CA`. There is no cleartext path.

One-time setup, from the repository root:

```sh
mkdir -p certs
# A self-signed cert whose name matches DONOR_SMTP_HOST (localhost):
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
  -keyout certs/mailpit-key.pem -out certs/mailpit.pem -days 365 \
  -subj "/CN=localhost" -addext "subjectAltName=DNS:localhost"
```

Then set in `.env` and reload it in each terminal that runs a worker:

```sh
DONOR_SMTP_HOST=localhost
DONOR_SMTP_PORT=1025
DONOR_SMTP_CA=certs/mailpit.pem
# DONOR_SMTP_USERNAME / DONOR_SMTP_PASSWORD stay blank; Mailpit requires no auth.
```

Start Mailpit once the cert exists (it mounts the cert, so generate it first):

```sh
docker compose up -d --wait mailpit
```

The `certs/` directory holds a local, disposable key and is git-ignored; never reuse it
anywhere real.

## App notifications

The push worker (`make worker KIND=push`, or `push-worker` in Compose) sends donor and partner app
notifications through Apple Push Notification service for iOS and Firebase Cloud
Messaging for Android. The API reads the same settings: it accepts device registrations
only for a configured platform, answering `PUSH_UNCONFIGURED` otherwise, so no
notification is queued for a device nothing can reach. Both refuse to start with
incomplete or unusable settings and log which platforms are disabled. The worker runs
without any provider and completes notifications for unconfigured platforms as
suppressed `push_unconfigured`; notifications are queued only for donors with a
registered device and are dropped as `expired` 24 hours after queuing, so none wait
indefinitely.

Partner installations register through `POST /donor/partner/push-device` after a native
partner sign-in; browsers and donor credentials cannot register partner devices.
`POST /donor/partner/push-device/unregister` removes the registration. Devices receive
approval (when registered within 24 hours of approval), verified-metric, and initiative
completion notices. Approval email remains the way into a newly approved portal.
Each installation has its own bounded-retry job and stable notification identifier.
Notices contain generic copy and open `/partner`; they contain no organization, donor,
or financial details. The normal push queue monitor includes parked partner jobs.
Registrations end with the existing 12-hour partner session, sign-out, or user removal;
a new sign-in must register again. An installation moves between donor and partner
accounts when it registers, so a shared device cannot retain both recipients.

| Variable | Meaning |
|---|---|
| `DONOR_PUSH_APNS_KEY_FILE` | Path to the APNs authentication key (`AuthKey_<key ID>.p8`, PKCS#8 P-256). |
| `DONOR_PUSH_APNS_KEY_ID` | The key's 10-character ID. |
| `DONOR_PUSH_APNS_TEAM_ID` | The Apple Developer team's 10-character ID. |
| `DONOR_PUSH_APNS_TOPIC` | The app's bundle ID. |
| `DONOR_PUSH_FCM_KEY_FILE` | Path to the Firebase service-account JSON key; its `project_id` receives the sends. |

Key material is read only from files, at most 1 MiB, like the Wallet keys: mount them
read-only into the push-worker container (readable by uid 10001), keep them
in the deployment secret manager, and never commit them or copy them into images.

APNs setup: in Apple Developer, Certificates, Identifiers & Profiles, enable Push
Notifications on the app's identifier, then under Keys create a key with Apple Push
Notifications service enabled and download the `.p8` file, which Apple offers once.
Set the four APNs variables. One key serves both environments; each device reports
whether its token is from the sandbox (development builds) or production, and the
worker sends to the matching host. The worker signs an ES256 provider token and reuses
it for 40 minutes. Revoking the key in Apple Developer stops delivery at once.

FCM setup: in the Firebase console, add the Android app to the project, then under
Project settings, Service accounts, generate a private key and set
`DONOR_PUSH_FCM_KEY_FILE` to the downloaded JSON. The account needs permission to send
Firebase Cloud Messaging messages (the default Firebase Admin SDK account has it) and
the project must have the Firebase Cloud Messaging API enabled. The worker exchanges a
signed assertion for an access token and caches it until five minutes before expiry.

Delivery reaches each live app session holding a token once; a retry after a partial
failure sends only to the rest. A token the provider reports unregistered or invalid is
removed from its session without retry. Throttling, outages, and network failures retry
with the financial-message backoff, doubling from two minutes up to an hour, or after the provider's
`Retry-After` if longer, and park after five attempts. A provider refusing the
credentials or the request (`ProviderRejected`) parks the notification at once without
touching tokens, since every send would fail the same way: fix the key, team, topic,
or project, and later notifications go out. The monitor counts a parked notification
until it expires, since an expired one is never sent. Before delivery the worker
rechecks the donor's choices and that the fact still holds, completing the
notification as suppressed (`opted_out`, `referral_no_longer_qualifies`,
`payment_recovered`, `streak_ended`, `no_device`, and similar) otherwise. Local tests
use a fake provider and local HTTP servers; they do not prove acceptance by Apple or
Google, which needs real accounts, a signed build, and physical devices.

## Phone and social sign-in

Phone sign-in and Apple/Google sign-in stay off until configured; the API refuses to
start with an invalid configuration and names the variables to fix.

- Phone: `DONOR_SMS_PROVIDER=twilio` with `DONOR_SMS_ACCOUNT_ID` (the `AC…` account
  SID), `DONOR_SMS_AUTH_TOKEN`, and `DONOR_SMS_FROM` (an E.164 number or an `MG…`
  Messaging Service SID). Codes are sent after the response, so a failed send reaches
  the donor only as a code that never arrives; it is logged as `sign-in code not sent`,
  and the donor can ask again after a minute. The per-phone cooldown preserves the
  previous code and its attempt budget during that minute, including across API replicas.
- Apple/Google: `DONOR_OIDC_MODE=live` with `DONOR_OIDC_APPLE_CLIENT_ID` and/or
  `DONOR_OIDC_GOOGLE_CLIENT_ID`, the audience each provider puts in its ID tokens. List
  several, separated by commas, when the app and the website use different ones (Apple's
  bundle ID and Services ID). The
  API fetches signing keys from `appleid.apple.com` and `www.googleapis.com`, so it needs
  outbound HTTPS to both; keys are cached for an hour and a known key keeps working
  through a provider outage.

`DONOR_SMS_PROVIDER=stub` (with `DONOR_SMS_FROM`) records codes instead of sending them,
and `DONOR_OIDC_MODE=stub` (with `DONOR_OIDC_STUB_KEY`) accepts HMAC-signed test tokens.
Both are for development and tests only. The API refuses to start with either unless the
public origin is on this machine (`localhost`, `*.localhost`, `127.0.0.1`), or a shared
test deployment sets `DONOR_ALLOW_SIGN_IN_STUBS=true`; production must never set it.

## App associations

The web app serves `/.well-known/apple-app-site-association` and
`/.well-known/assetlinks.json` on the public host, so the donor app opens the account
pages donors reach from emails and referral links, and passkeys donors add on the site
also sign the app in. Both files are prerendered from build arguments; with none set
they answer 404, and a malformed value fails the build rather than publish an
association Apple or Google would reject and cache.

| Variable | Meaning |
|---|---|
| `DONOR_IOS_APP_ID` | `<team ID>.<bundle ID>`, the same values as `DONOR_PUSH_APNS_TEAM_ID` and `DONOR_PUSH_APNS_TOPIC`. |
| `DONOR_ANDROID_APP_PACKAGE` | The Android app's package name. |
| `DONOR_ANDROID_APP_CERT_SHA256` | Comma-separated SHA-256 fingerprints of the app's signing certificates, colon-separated hex, from Play Console, App integrity. Include the upload key's while testing sideloaded builds. |

Set them in the deployment environment before building the web image, and rebuild it
when they change. Apple's CDN caches the iOS file for up to a day, so allow that long
after a change before testing on a device. The iOS app needs the Associated Domains
entitlement with `applinks:` and `webcredentials:` entries for the public host.

## Starting a monthly gift from the app

`POST /donor/me/recurring/checkout` opens Stripe-hosted Checkout for a signed-in donor with
no active or pending monthly gift. It is the website's guest checkout run for the donor's own
account, so the same verified webhooks complete the gift; only the pages Checkout returns to
differ. They are `/app/return?result=success&session_id=…` after payment and
`/app/return?result=cancel` if the donor backs out, both served by the web app, which shows
a short "return to the app" notice. The app watches for that address, or opens from it once
the association below covers the path.

| Variable | Meaning |
|---|---|
| `DONOR_NATIVE_RETURN_ORIGIN` | Optional. The exact HTTPS origin (HTTP on localhost) the return pages are served from. Defaults to `DONOR_PUBLIC_ORIGIN`; set it only if the web app's `/app/return` page lives on another origin. A path, trailing slash or plain-HTTP remote host stops the API at startup. |

The return address is frozen into each Checkout session when it is created, so a change
applies to checkouts begun afterwards. To have the installed app open from the return page
rather than watch for it, add `/app/return` to the Apple association's `components` and the
Android intent filter's path list, then redeploy the web image; the associations in the
section above do not cover it today.

A donor who closes the hosted page without paying keeps their recurring reservation until
the session lapses (an hour). Starting again from the app does not wait for that. Under the
donor's row lock the API finds the open session holding the reservation: if the amount and
return pages match and it has more than five minutes left, it returns that session; otherwise
it expires the session at Stripe (outside any database lock), then marks the attempt
`EXPIRED`, releases the reservation and creates the replacement. If Stripe reports the session
already paid, the answer is `409` and the webhook activates the gift; if the expiry call
fails, the reservation is untouched and the app retries. The `checkout.session.expired` webhook
for a replaced session is harmless: it releases only the reservation of its own attempt.

### Restarting a website checkout

The website's guest checkout (`POST /donor/checkouts`) restarts the same way but never hands
a session back. Its caller is whoever typed the email, so returning the open session's URL
would let anyone who knows an address resume, and pay into, someone else's checkout. Instead
a new start for an email whose earlier checkout is open and unpaid expires that session at
Stripe and creates a new one, for the same amount or another, through the same race-safe
steps as the app (donor row lock, Stripe call outside any lock, the reservation re-locked
before the attempt is marked `EXPIRED`). Paid just as the request arrives is `409`; an expiry
that fails is a retryable `503` with the reservation untouched. A retry with the same
`Idempotency-Key` returns the same session; the key of a replaced checkout answers `409`.
An app start that finds a website checkout holding the slot replaces it the same way.

The trade-off is that a stranger who knows an email can make a donor's open checkout close:
the donor sees Stripe's expired page and starts again, and no payment is lost, because an
expired session cannot be paid. Three limits bound it. A checkout begun in the last 15 seconds
is never expired by another website request, so concurrent restarts cannot expire each
other's replacement and leave nothing open (`409`; retry). At most 10 checkouts can be
begun for one email in an hour, counting the new one; the next answers `429` with
`Retry-After`. That budget is per email in the database, so it holds across API instances,
and it sits beside the per-address `visitor` limit of 60 a minute. An attacker who spends the
budget also blocks that donor's own restart until it refills or the open session lapses, which
is no worse than the hour-long lockout this replaces.

## Staff and content

Staff sign in with passkeys on the staff host. Provision the first administrator with
`./scripts/deploy.sh staff --email … --name …` (or `go run ./cmd/staff` with the database
and `DONOR_STAFF_ORIGIN` configured); later access uses `/staff`. Require individual accounts and retain two
administrators, each with their own passkey, for recovery. An administrator's recovery
action deletes the account's passkeys, revokes its sessions and invitations, and issues a
private invitation from which the staff member creates a new passkey.

When no administrator can sign in, an operator with database access runs break-glass
recovery for an enabled administrator:

```sh
./scripts/deploy.sh staff --recover --email named.person@example.org
# or, with the environment loaded: cd apps/donor-api && go run ./cmd/staff --recover --email …
```

It works like the back-office recovery action and is audited as `staff.recovery_invited`
with no actor. Protect the printed invitation, and review the audit history afterwards.

At `/staff/content`, upload private source documents. Initiatives, their headline metrics and
their spend by category are created, edited and published at `/staff/initiatives`;
initiatives published from the spreadsheet before it was retired for them were copied there.
The spreadsheet's publish and public-content endpoints are gone, and migration 0077 drops
its `donor_content_publications` and `donor_content_head` tables; the publication history
is discarded by design, and the board-approval source documents stay in `donor_content_sources`.
The public `/spend` page shows only category totals summed across published initiatives; no
individual transactions are published. Sources are limited to 10 MiB, encrypted, SHA-256
checked, and permission-protected. Without `DONOR_CONTENT_ENCRYPTION_KEY`, uploads return 503
before the file is read. Donor records have linkable URLs (`/staff/donors/{id}`), and
search keeps its query in `/staff?q=`. Staff are responsible for actual accuracy; uploading a
source cannot establish it.

Publishing an initiative requires its board approval: the ID of the approval document
stored here and the date the board approved it. A published initiative's approval can be
replaced but not removed. Initiatives published before this rule stay published; record
their approvals when next editing them. Approvals given in the content spreadsheet were
carried over.

Exports use fixed field allowlists, omit credentials and message bodies, neutralize CSV
formula prefixes, and audit completion/interruption. Referral rows retain channel.
Referral-touch rows captured more than 13 months ago appear only if they led to an
attributed gift.
The `billing-history` CSV contains verified failed and successful invoice observations.
See [analytics](analytics.md) for its coverage boundary and for timestamp, replay, and
renewal-cohort rules.

At `/staff/content/wording`, content staff change badge titles, how-to-earn text, and
streak-milestone notifications; an empty entry restores the default.

At `/staff/founding-claims`, an administrator imports website waitlist sign-ups and earlier
website donors (D-6) as a CSV of at most 4 MiB and 50,000 rows:

```csv
email,signed_up_on,source
```

`signed_up_on` is a YYYY-MM-DD date before 2027-01-01 and `source` is `WAITLIST` or
`WEBSITE_DONOR`. Any invalid row refuses the whole file. The API stores only an
HMAC-SHA256 of each lower-cased address under `DONOR_FOUNDING_CLAIM_KEY`, so the stored
hashes cannot be matched against guessed addresses without the key; delete the source file
once imported. Generate the key with `openssl rand -base64 32 | tr '+/' '-_'` and set it on
the API only; without it the import answers 503 `FOUNDING_CLAIMS_UNCONFIGURED`. A donor is
granted the badge when the import finds their address, when they first give as a guest
under it, or when they change their email to it. Grants survive a key change, but claims
not yet granted no longer match under a new key, so re-import the file after changing it.
Re-importing is safe and keeps each address's earliest sign-up. Qualifying claims have
no redemption expiry; a later account claim still earns the badge.

There is no enrolment export before R2. Keep exports private and apply an approved
retention policy. Sensitive source documents have separate authenticated downloads.

## Partner report artifacts

Offline match payments use a permanent version-4 UUID request key. The staff screen
saves the key, amount and year in tab-scoped session storage before sending the request,
and preserves them after an uncertain response or navigation. Retry the saved payment
until confirmed; do not create another payment to resolve an unknown outcome. Identical
replays create neither another payment nor another audit entry. A key reused with another
partner, year or amount returns 409. Older API clients may omit `Idempotency-Key`, but
must be upgraded before retrying uncertain requests because omission cannot deduplicate.

Partner administrators upload a PNG or JPEG logo from the organization section, up to
512 by 512 pixels and 256 KiB. The API validates dimensions before decoding, removes
metadata, and stores a normalized PNG. Empty input explicitly removes the logo. No
external image URL is fetched.

New PDF exports retain their exact bytes and dashboard snapshot. A later edit to the
organization, logo, metrics or spending never changes an existing export. Authenticated
users of that organization can download retained files; only administrators can create
exports or change the logo. Historical entries created before artifact retention remain
listed as unavailable because their original contents cannot be reconstructed safely.
Unverified figures are labeled awaiting verification in the portal and omitted from the
PDF. Published zero values remain distinguishable from unknown values. PDFs carry a
sandbox label, export time, logo, and a header and footer on each page.

## Load testing with representative data

Capacity is measured against a populated staging deployment: `cmd/seed` fills a
disposable database with a synthetic donor population, and `cmd/loadtest` drives a
referral-spike mix at an API serving it. Use the intended database size, network path,
and process sizes; never point either command at production.

The seeder writes only to a database it can show is disposable. The URL comes from
`-database-url`, never from `DONOR_GO_DATABASE_URL`, and must itself name the host and
database (and the port, unless it is 5432): pgx would otherwise fill them from `PG*`
variables or a service file, so such URLs are refused. `-confirm` must repeat the
database name. The seeder refuses a database holding any donor whose address is not
`@loadtest.invalid`, and a first seed refuses one with staff accounts or Stripe events,
as a freshly launched database would have. A seed records itself in the database's
comment, so the seeding role must own the database, and `-append` adds to a database
only when that marker is present. Create a dedicated database, migrate it, then seed it
from the repository root:

```sh
(cd apps/donor-api && DONOR_GO_DATABASE_URL=postgresql://user:pass@db:5432/dad_load go run ./cmd/migrate)
make seed SEED_DATABASE_URL=postgresql://user:pass@db:5432/dad_load SEED_CONFIRM=dad_load \
  LOAD_FIXTURE=/secure/dad-load.json
```

`SEED_DONORS` (default 50,000, at most 1,000,000) donors are generated deterministically
from `-seed` and held in memory, about 8 KB of RSS per donor (400 MB at 50,000, 8 GB at 1,000,000): mostly
monthly gifts at the preselected $30, some at $90 and $150 or at other valid whole-dollar
daily amounts, abandoned checkouts, up to two years of anniversary renewals with failures
recovered within and after the 7-day grace, unrecovered failures, cancellations, refunds,
and referral trees in which most
donors refer nobody and a few refer hundreds, with Link and IRL visits that did and did
not convert. Rows are written as checkout and webhook processing store them, including
the processed Stripe events that billing and refund observations cite, and the
projection worker's code then derives every projection. Givers whose first gift precedes
the Founding Donor cutoff earn the badge. Seeded addresses use the reserved `.invalid`
domain, and the messages projections queue for seeded history are marked suppressed
however the projection pass ends. A seed that fails or is interrupted after its copy
leaves its donors in place; drop the database and start again.

The fixture is a new mode-0600 file, refused inside a git working tree (symlinks
included), holding `-sessions` (default 2,000) live session tokens that favour active
referrers, and referral codes repeated in proportion to how often each would be visited.
Treat it as a credential. The sessions last 30 days; delete the fixture and drop the
database after the run.

Run the API as for production, and run the projection worker alone, with
`DONOR_GO_WORKERS_ENABLED=true`, since projections fall due at their next boundary and
dashboards then wait for it. Never run the message worker against
a seeded database; with Compose, start only
`docker compose --env-file .env.production --env-file .env.database -f docker-compose.prod.yml --profile workers up -d projection-worker`.
Every scenario but referral capture and `event-reserve` only reads, including recurring, so Stripe
configuration does not change the result; captures insert referral touches, so the
database grows with each run. Checkout is not in the mix because it calls Stripe.

An API run outside Compose needs `DONOR_GO_LISTEN` on an address the load host can
reach (the default, `127.0.0.1:8002`, admits only local clients), `DONOR_PUBLIC_ORIGIN`
equal to the generator's `LOAD_ORIGIN`, which captures send as their `Origin` and are
refused with 403 otherwise, and `DONOR_TRUSTED_PROXY_CIDRS` including the load host.

Referral captures are rate limited per client, and one generator is one client. With
`-client-ips` it sends each user, and each captured visitor, its own `X-Real-IP` from the
198.18.0.0/15 benchmarking range, as the ingress does for real visitors. The API accepts
that header only from `DONOR_TRUSTED_PROXY_CIDRS`, so have the generator reach the API
directly, not through the ingress, which overwrites the header. Otherwise captures fail
with 429. The Compose API publishes its port on host loopback only, and connections
through it arrive from the `default` network's gateway, 172.30.42.1, not from the load
host. Either add that address to the staging API's list for the run, which lets any local
process choose its client address, or run the generator in a container on the `app`
network with `LOAD_API=http://api:8002`.

```sh
make loadtest-seeded LOAD_API=http://api.staging.internal:8002 LOAD_ORIGIN=https://staging.example.org \
  LOAD_FIXTURE=/secure/dad-load.json
```

This target runs four loads in turn: a 60-second burst at 256 users with a 250 ms p99
budget for every scenario, a five-minute soak at 32 users with 100 ms, a 60-second event
spike at 256 users (`events` 55 and `event-detail` 45, 400 ms), and a five-minute
engagement soak at 32 users (`dashboard` 40, `today-initiative` 30, `team` 30, 150 ms).
Each exits nonzero on any failed request or exceeded budget; an interrupted run writes the result so far and exits nonzero. For
other shapes run `go run ./cmd/loadtest` with `-duration`, `-concurrency` (up to 2,048),
`-mix name=weight,...` and `-max-p99 250ms,dashboard=400ms`.
The default weights are referral-lookup 30, referral-capture 12, referral-missing 3 (an
unknown code, expecting 404), spend 15, dashboard 18, gifts 7, sharing 5, profile 4,
recurring 4 and annual-summary 2. The scenarios `events`, `event-detail`, `event-reserve`,
`today-initiative` and `team` default to 0 and are enabled through `-mix`; the event
scenarios use the event IDs the seeder writes to the fixture. `event-reserve` is a write
that takes the event row lock and fills seats, so use it only on a disposable database.
Each signed-in read uses a donor drawn from the fixture;
`-native` (default 0.25) of users present it as the donor app's bearer token, the rest as
the browser cookie.

The JSON result gives requests, failures, requests per second, p50/p95/p99/max latency
in milliseconds and statuses overall, and the same per scenario with error kinds
(`transport`, `timeout`, `oversized`). Users wait for each response before sending the
next, so throughput is what the deployment sustained at that concurrency. Latency that
rises evenly across scenarios, including the single-query referral lookup, means
requests are queueing for database connections or API CPU; a tail in one scenario points
at its queries, which can be examined with `EXPLAIN (ANALYZE, BUFFERS)` against the
seeded database. A generator sharing a host with the API or database competes with them
for CPU. Keep results with the run's ticket, not in the repository.

## External acceptance

First-party browser analytics requires explicit user consent. Manual assistive-technology
acceptance, native design deliverables, and physical Wallet/QR acceptance remain external. See [analytics](analytics.md) for the collection boundary and
[design foundations](design-system.md) for the manual accessibility scope. These items
remain unverified; deferral does not establish production acceptance.

Before live solicitation, verify the receiving entity, EIN, processor account, approved
receipt and policy text, registrations, Wallet accounts/certificates, delivery reputation,
backup/restore and incident ownership. Exercise the full Stripe sandbox journey including
out-of-order webhooks, refund transitions, failed renewals, cancellation,
month-end boundaries, and ambiguous responses. Local test doubles do not prove these
provider behaviors. Physical QR scanning, assistive-technology checks, and viral-load
acceptance also require explicit evidence.

## Queue alerts and incident response

Run `./scripts/deploy.sh monitor [--max-age 10m]` (or `go run ./cmd/monitor` from
`apps/donor-api` with the database environment loaded). It emits only aggregate JSON counts,
parked counts, and oldest due age for Stripe, projections, financial mail (including
partner messages), login mail, email changes, push notifications, deleted-account
reversals, and subscriptions. It exits nonzero if any job is parked or a due item is
older than five minutes; `--max-age 10m` changes the age threshold. Future scheduled sends
are excluded. An ongoing subscription last synced before its billing period ended is due
a sync one day after that end, when Stripe has renewed or ended it: one still due after
that has missed its events or keeps failing to sync, so views of it are out of date.
Check the Stripe worker and that subscription's events in Stripe. Schema/database failure is also nonzero. Run this every minute from your
private scheduler, alert on two consecutive failures, and send recovery notification
when it returns to zero. Monitor process/container restarts and both health URLs too:
queue age alone cannot detect an idle stopped worker. No public monitoring endpoint or
third-party telemetry is enabled by this command.

An alert owner must distinguish unavailable dependencies, invalid data, and a halted
worker before retrying. Stop only the affected writer if facts may be wrong; preserve
inbox entries, command keys, audit records, and database evidence. Check current Stripe
state before ambiguous financial retries. Never reset all attempts or delete rows as a
repair. For mail incidents, restore delivery configuration before replay and expect
possible duplicate delivery after acknowledgement loss. Escalate incorrect recognition
or money totals to the finance owner; publish corrections only after reconciliation.
Assign primary/backup incident owners and test paging in the chosen hosting environment.

## Encryption keys

`DONOR_EMAIL_ENCRYPTION_KEY` protects queued sign-in links and email-change proofs
(`donor_login_emails.encrypted_token` and `donor_email_changes.encrypted_token`), and
`DONOR_CONTENT_ENCRYPTION_KEY` protects private source documents
(`donor_content_sources.encrypted_data`). Each is a comma-separated list of Fernet keys,
newest first: the first key encrypts everything written, and any key in the list
decrypts. A single key is a list of one. The API and workers refuse to start if any key
in a list is malformed. Session, link, and referral-visitor lookups use unkeyed SHA-256
hashes of random tokens, so no stored hash depends on these keys and rotation re-hashes
nothing. `DONOR_FOUNDING_CLAIM_KEY` is not an encryption key and `rekey` does not
handle it; see [staff and content](#staff-and-content) for changing it. Wallet signing
keys and staff passkeys are not data-encryption keys and are not rotated this way.

`rekey --key email` (or `content`) re-encrypts every token of that setting still on a
later key under the first key. It reads each table in id order in pages of `--batch`
rows (default 500), each cut short once it passes 8 MiB of tokens, skips tokens the first key already
decrypts, and replaces each other token with a single-row compare-and-swap. It therefore
runs safely beside the API and workers: it holds one row lock at a time and never
overwrites a token they wrote after it was read. An interrupted run keeps what it
finished; run it again to resume. Before changing anything it reads every token and
refuses if any decrypts under no key in the list, naming each such row by table and id.
`--dry-run` changes nothing and prints, per table, the tokens found (`tokens`), those
still needing a later key (`retired`), and those no key decrypts (`undecryptable`); it
exits nonzero while either count is above zero. Under Compose run it as
`./scripts/deploy.sh rekey …`; locally run `go run ./cmd/rekey …` from `apps/donor-api`
with the environment loaded.

### Rotating a key

Rotate one setting at a time. For the email key:

1. Generate a new key with `openssl rand -base64 32 | tr '+/' '-_'` and add it to the
   secret store beside the current one.
2. Put it first in the list: `DONOR_EMAIL_ENCRYPTION_KEY=<new>,<current>`.
3. Deploy the API and every worker with the new list: `./scripts/deploy.sh up`, then
   `./scripts/deploy.sh workers` if workers are enabled. A worker still on the old list
   cannot read an email-change proof the new API encrypted; it retries a minute later,
   by when it has restarted. Where processes cannot restart together, such as several
   replicas behind a rolling deploy, first deploy the new key last in the list
   (`<current>,<new>`), then move it first and deploy again, so that every process
   reads the new key before any writes with it.
4. Count what remains: `./scripts/deploy.sh rekey --key email --dry-run`. It exits
   nonzero now, with `retired` counts. If any token is `undecryptable`, stop: it was
   written under a key missing from the list. Find that key and add it at the end of the
   list before going on.
5. Re-encrypt: `./scripts/deploy.sh rekey --key email`. Its summary adds `reencrypted`
   per table. If it is interrupted, run it again.
6. Verify: `./scripts/deploy.sh rekey --key email --dry-run` exits zero, with `retired`
   zero for every table.
7. Remove the old key, `DONOR_EMAIL_ENCRYPTION_KEY=<new>`, and deploy the API and workers
   again as in step 3. A dry run with only the new key then still exits zero.
8. Keep the retired key in the secret store as described below.

The content key rotates the same way with `--key content` and `DONOR_CONTENT_ENCRYPTION_KEY`;
only the API uses it.

### Suspected key compromise

A disclosed key exposes its data only together with a copy of the database or a backup,
but treat the database as exposed until shown otherwise and follow
[incident response](#queue-alerts-and-incident-response).

1. Preserve evidence, and rotate the database credentials too if they may have leaked.
2. Rotate the key at once (steps 1 to 7 above). Rotation protects the live database
   from then on; it cannot protect dumps and backups taken earlier, which remain readable
   with the compromised key and belong in the incident's assessment.
3. Record in the secret store that the key is compromised. Keep it only while it is needed
   to restore backups that predate the rotation, and destroy it with them.
4. For the email key, sign-in links and email-change proofs expire 15 minutes after they
   are issued, so those encrypted before the rotation stop working on their own. For the
   content key, presume the source documents stored before the rotation disclosed.

### Backups and retired keys

A backup holds tokens as they were when it was taken, so restoring it needs every key in
use then. Keep each retired key in the secret store, labelled with its setting and the
date its rekey completed, until every backup taken before that date has passed the
retention policy; then destroy it. To restore an older backup, add the retired keys it
needs at the end of the list (`<current>,<retired>`), restore, run `rekey`, verify with a
dry run, and remove them again.

## Backup and restore drill

With PostgreSQL running, `ENV_FILE` pointing to the Compose environment, and
`DATABASE_ENV_FILE` to the database logins (default `.env.database`):

```sh
./scripts/backup.sh /secure/backups/dad-2026-10-04.dump
./scripts/verify-restore.sh /secure/backups/dad-2026-10-04.dump
```

Both scripts require an absolute dump path. The backup uses PostgreSQL's consistent
custom-format dump and creates a private file without overwriting an existing path. Keep dumps outside the repository on encrypted
storage with restricted access, retention, and independent copies. The drill creates a
random disposable database, restores with fail-on-error, reads core donor/gift/identity
tables, and drops only that temporary database. It never restores over the running app.
A successful drill proves the dump restores, not disaster recovery time or key recovery.
Rehearse the restored app in an isolated environment with the encryption keys in use
when the dump was taken ([retired keys](#backups-and-retired-keys)) and
outbound Stripe/email disabled. Establish recovery-point and recovery-time objectives,
backup scheduling, retention, alerting, and off-host recovery ownership before launch.

## Continuous integration

CI runs in GitHub Actions on ephemeral GitHub-hosted runners. Linux jobs use
`ubuntu-24.04`; Swift compatibility uses `macos-latest`. Fork pull requests run
ordinary CI without production credentials. Repository environment protections
continue to govern the separate ATM ledger reader, publisher, and administrative
jobs. Those jobs accept trusted repository events only and require a
`github-hosted` OIDC identity.

The main workflow runs Go formatting, vet, race tests, static analysis and
vulnerability scanning; web lint, unit tests, types and production build; ledger
checks; generated-code validation; browser journeys in development and production;
and production image, Compose and ingress checks. Changes to shared scripts,
contracts, Makefile and Docker inputs trigger their dependent jobs. Pushes to
`dev` and `main` run the full suite.

`deploy.yml` rolls the application to ECS: staging on a merge to `dev`, production on a
merge to `main` behind the `production` environment's reviewer. `cloudflare-ranges.yml`
checks weekly that the committed Cloudflare address ranges are current.

The mobile workflow validates and bundles the contract, then compiles and tests
it with the pinned Swift package dependencies. Only matching mobile version tags
publish a release; pull requests cannot publish artifacts as releases.

Branch protection should require the CI and mobile checks and ATM acceptance.
A green application test suite alone does not authorize deployment. All
workflows run on GitHub-hosted runners.


## One email, one role

New identities are refused for an address held by another role (see
[donor domain](donor-domain.md#giving-and-identity)), but addresses that already held two
roles are left as they are. List them with the owner role before deciding, with each
person, which login keeps the address:

```sql
SELECT email, array_agg(role ORDER BY role) AS roles FROM (
  SELECT email, 'DONOR' AS role FROM donors WHERE email IS NOT NULL
  UNION ALL SELECT email, 'STAFF' FROM donor_staff WHERE NOT disabled
  UNION ALL SELECT email, 'PARTNER:' || partner_id FROM donor_partner_users WHERE NOT disabled
) held GROUP BY email HAVING count(*) > 1;
```

## Process secret boundaries

The production Compose file never injects an entire environment file into a process.
The application and database files are interpolation inputs; each service has an explicit
allowlist. Keep operator files outside Git, protect them on the host, and use separate
provider keys for independently operated capabilities.

The API receives its database role, Stripe/webhook credentials, source-document and
email-token encryption keys, configured SMS/OIDC settings, Wallet signing files and
check-in secret. It queues email without SMTP login credentials. Mail workers alone
receive SMTP authentication. Stripe processing and reconciliation receive the Stripe
API key; projection and monitor processes receive no provider credentials. Rekey receives
only the encryption keyrings it rotates. The push worker alone mounts APNs/FCM delivery
keys; the API receives `DONOR_PUSH_PLATFORMS` to accept device registration without
opening those keys. Compose derives the platforms from configured delivery paths unless
an explicit platform list or `none` overrides it.

`make check-deploy-secrets` renders Compose with canary settings and verifies these
boundaries, including that unrelated secrets never reach a container. CI runs this before
image checks. Direct API deployments retain the original key-loading configuration when
`DONOR_PUSH_PLATFORMS` is unset; set explicit platform names to isolate delivery keys there.

## Partner application receipts

The web application form uses `POST /donor/partner/applications` with a
client-generated 32-byte random base64url receipt and a stable idempotency key. The
receipt and application commit together. Application submission remains browser-only. A retry must retain both values and the same
form. The browser receives an HttpOnly, SameSite=Lax cookie lasting 30 days. `GET /donor/partner/application-status` also accepts the receipt
as a native bearer credential; native clients must obtain and securely retain that
separate receipt through an explicit application-submission integration. No receipt is
implicitly shared from the browser to an app.

`/apply/status` shows the organization and current review decision after navigation or
refresh. A receipt proves possession of the submission, not ownership of its work
email; it grants no partner or donor access and exposes no contact details or staff
notes. It is never put in a URL. The database stores only its SHA-256 hash. Browsers
without the original receipt, including another device, need the partnerships team's
help; approval continues to require the emailed sign-in proof. Existing application
clients remain supported through the original submission endpoint.

## Recognition repair

A super admin can use **Repair recognition after a system error** on a donor record.
`POST /donor/staff/donors/{donor_id}/recognition/recompute` requires `donor.recompute`
and a 10–1000-character reason identifying the documented error and resolution.
The repair uses the normal locked projection path and records the reason, before/after
current and best streaks, and rules version in the same transaction's audit event.
Payment history remains authoritative; permanent earned badges and historical bests
are preserved. This operation repairs derived state after facts or code have been
corrected. It does not provide an arbitrary manual continuity override.

### Web signup and identity providers

Donor signup collects a private name, email, ZIP, and city before emailing a
single-use verification link. Existing donor profiles are not overwritten by an
anonymous signup. After following the link, donors may set a password in their
profile. Passwords use bcrypt at cost 12, accept 12–72 bytes, and are never logged.
Changing a password requires the current password or an email proof completed in
the same session within 15 minutes. The email sign-in flow is also password recovery.
Account deletion removes the password hash and private signup name.

The web Apple and Google authorization-code flows use these server-only variables:

- `DONOR_OAUTH_APPLE_CLIENT_ID` and `DONOR_OAUTH_APPLE_CLIENT_SECRET` (Apple's signed
  client-secret JWT, renewed before its expiry).
- `DONOR_OAUTH_GOOGLE_CLIENT_ID` and `DONOR_OAUTH_GOOGLE_CLIENT_SECRET`.

`DONOR_PUBLIC_ORIGIN` is compiled into the web image. The shipped `docker-compose.prod.yml`
does not yet pass these four variables to the `web` container's runtime environment, and the
callback route answers 503 without them, so add them to the `web` service's `environment`
before enabling these flows with Compose. Register the exact redirect URLs `${DONOR_PUBLIC_ORIGIN}/account/oauth/apple` and
`${DONOR_PUBLIC_ORIGIN}/account/oauth/google` with the providers. Include the web
client IDs in the corresponding `DONOR_OIDC_*_CLIENT_ID` audience allowlists and use
`DONOR_OIDC_MODE=live`. Apple requires HTTPS for its cross-site form-post callback.
Each attempt has a short-lived HttpOnly state/nonce cookie; Google also uses PKCE.
The Go verifier validates the resulting token's signature, issuer, audience, and
expiry before issuing the normal HttpOnly donor session. Provider errors return a
sign-in failure without logging codes, tokens, or response bodies. These flows need
real-provider acceptance with the deployment's registered credentials.

### Initiative co-funders and verified spending

An initiative may have several approved co-funders, each with its own committed
amount. Partnerships staff set the initiative ID and that partner's commitment in
partner detail. Linking another partner preserves all existing links and amounts.
Partner committed totals use those individual commitments; initiative metrics and
category spending describe the whole initiative. Published metric/completion
notifications fan out to every co-funder. The older optional primary-partner field
remains compatible and adds a link without removing other co-funders.

Spend changes are drafts. Impact Ops must supply a verification note and date and
explicitly publish each category. An edit leaves the previously published total
visible until the verified replacement is published. Verification evidence is kept
with the published total. Existing published totals are preserved on migration;
the migration does not manufacture historical verification evidence.

### Recognition controls

Super administrators can reset a donor's current streak with a reason. The reset is
stored separately from gift facts and survives projection recomputation; giving
history, historical best, and already earned badges are preserved. A future paid
gift starts the new run. Repairing a faulty projection remains a separate action.

Content administration includes badge names, rule text, numeric earning thresholds,
PNG/JPEG artwork, and increasing streak milestone thresholds. Supported badge signals
remain fixed; staff cannot supply executable rules. Changes are audited and schedule
projection refreshes. Past awards remain permanent. Company month winners use combined
active giving, matching the displayed leaderboard. Qualifying website-waitlist claims
have no redemption expiry; only their keyed address hashes are retained.
