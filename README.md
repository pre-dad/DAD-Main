# DAD — donor release

The R1 application for Dynamic Aid Delivery: guest giving, direct Link and IRL
referrals, a donor dashboard with streak recognition, badges, rankings and network
milestones, funded initiatives with a daily rotation, events and volunteering, recurring-gift
controls, a partner portal, employee giving with matching and teams, and an audited staff
back office. The backend is Go, the web application is TypeScript/Next.js, and persistence is
PostgreSQL; a generated OpenAPI contract and a shared mobile-foundation bundle back the
web client and the native iOS and Android clients from one source of truth. Licensed under MIT.

**This build accepts Stripe test payments only.** Live donations, approved tax receipts,
Wallet passes, and release acceptance are not established by running it locally. See
[the product contract](docs/donor-domain.md) and [security](SECURITY.md).
Optional [Wallet signing](docs/wallet.md), [analytics definitions](docs/analytics.md),
and [shared design values](docs/design-system.md) have separate setup and acceptance boundaries.

R1 raises and recognises recurring giving and routes it to funded initiatives. It does not
itself deliver aid, assess recipients, run AI services, or publish blockchain transactions,
and no money moves for employer matching inside the app — the match owed is computed and an
administrator records payment received out of band.

## Local setup

Requires Go 1.27.3, Node, npm, and Docker Compose, which runs PostgreSQL 16 on 127.0.0.1.
The web app needs Node 22.13+ and the ATM ledger Worker needs Node 22.19+ (Node 24 serves
both), so use 22.19+ or 24 if you work on both.
Commands below run from the repository root. The local database uses port 55432.

```sh
cp .env.example .env
# Edit .env, then load it in each terminal that runs an application command:
set -a
. ./.env
set +a
make install
make db-up
make migrate
make api                      # terminal 1
make web                      # terminal 2, with the same environment loaded
```

Locally every process connects as the database owner, which `.env.example` allows with
`DONOR_ALLOW_OWNER_CONNECTION=true`; deployed processes each use their own login and refuse
the owner ([database roles](docs/operations.md#database-roles)).

Open http://localhost:3000. Without Stripe, SMTP, and encryption configuration, public
pages and empty states work; giving and account email are unavailable. The staff back
office runs only on its own host, `DONOR_STAFF_ORIGIN`: locally
http://staff.localhost:3000/staff, which browsers resolve to this machine.
The frontend proxies `/api/donor/*` to `DONOR_API_BASE_URL`; the API listens on 8002.
Do not put backend secrets in `NEXT_PUBLIC_*` variables.

Use two independently generated encryption keys, for email and for content:

```sh
openssl rand -base64 32 | tr '+/' '-_'
```

Each setting also accepts a comma-separated list, newest key first, which is how keys
are rotated ([operations](docs/operations.md#encryption-keys)).

Staff sign in with passkeys. With `DONOR_STAFF_ORIGIN` set, provision the first named
administrator:

```sh
cd apps/donor-api
go run ./cmd/staff --email named.person@example.org --name 'Named Person'
```

The command prints a private, one-use invitation to the staff host, where the
administrator creates a passkey. Protect that output. It refuses if any staff account
already exists. Later invitations and recovery use `/staff` and require administrator
permission. Provision two individual administrators, each with their own passkey.

## Payments and workers

Configure a Stripe **test** secret key and webhook signing secret; the API refuses to
start with only one of them. The receiver accepts
snapshot events at `2026-09-30.endive`, matching stripe-go v87. Card entry and card updates
are hosted by Stripe. Use `/api/donor/stripe/webhook` as the externally reachable route.
See [operations](docs/operations.md) for supported events and recovery behavior.

Workers start only with `DONOR_GO_WORKERS_ENABLED=true`. Run each kind in a separate
terminal with the environment loaded:

```sh
make worker KIND=stripe
make worker KIND=projections
make worker KIND=login-email
make worker KIND=email-changes
make worker KIND=messages
make worker KIND=push
```

The push worker runs without provider keys, discarding app notifications it cannot
deliver; with APNs or FCM keys it sends real notifications to registered devices
([operations](docs/operations.md#app-notifications)).

SMTP delivery requires STARTTLS and the configured encryption key. Workers send real
email to the configured addresses, even while gifts are test payments. Local tests use
injected transports. Never point synthetic test data at a real mail provider.

## Checks

Integration tests require a dedicated synthetic database named `donor_go` and a superuser
test role: migration tests create and remove isolated databases, and the tests run each
process's code as that process's [database role](docs/operations.md#database-roles), so a
privilege the process lacks fails a test. Tests must never run against the application
database.

```sh
docker run -d --name dad-go-test-postgres -p 127.0.0.1:55440:5432 \
  -e POSTGRES_USER=go_test -e POSTGRES_PASSWORD=go_test -e POSTGRES_DB=donor_go \
  postgres:16-alpine@sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea
# Wait for: docker exec dad-go-test-postgres pg_isready -U go_test -d donor_go
export TEST_DATABASE_URL=postgresql://go_test:go_test@localhost:55440/donor_go
(cd apps/donor-api && DONOR_GO_DATABASE_URL="$TEST_DATABASE_URL" go run ./cmd/migrate)
make test
make lint
make check-ingress smoke-ingress check-images
(cd apps/web && npx playwright install chromium)
DONOR_GO_DATABASE_URL="$TEST_DATABASE_URL" make test-browser-e2e
```

The database must be created fresh for this checkout (a name starting `donor_go`) and
migrated with the command above; an older database fails the role-catalog tests with
"TEST_DATABASE_URL is not migrated to this checkout". Use PostgreSQL 16 with password
authentication, as in the container above. A default local install (such as Homebrew
PostgreSQL 14) also works for the role-catalog tests, but `cmd/provision`'s
password-replacement test fails there because its `pg_hba.conf` trusts every local login.
Run packages that share the database one at a time: `go test -race -p 1 ./...`.

Browser tests run against `next dev` by default; set `E2E_PRODUCTION=1` to run them
against a production build (`npm run build && npm start`), where account and staff pages
use the nonce-based CSP, as do partner-portal pages. Next allows
one dev server per checkout, so stop any running `make web` first. Staff pages are tested
on `DONOR_STAFF_ORIGIN` (default http://staff.localhost:3000) with Chromium's virtual
authenticator; the staff journey provisions or recovers `e2e.administrator@example.org`
with `cmd/staff`, so point `DONOR_GO_DATABASE_URL` at a synthetic database. The signed-in
donor and partner journeys run against the real API: `cmd/browserfixture` stands in for
the inbox, creating an account in that database and printing the sign-in link its email
would carry.

`make test` checks Go formatting, vet, race-enabled tests, compilation, frontend lint and formatting,
unit tests, generated route types, TypeScript, and the production build. Browser tests
start a local Go API and Next server on 127.0.0.1:8002 and 127.0.0.1:3000, with matching
origins regardless of the local application's origin settings. They require a database
named `donor_go` and refuse the application database. Stop any running `make api` as
well as `make web` before testing, so a test cannot reuse an application server.
Use a synthetic application database and no live
SMTP settings for those tests. `make lint` runs staticcheck, govulncheck for the API and its separate Go tooling module,
`npm audit`, and shellcheck when installed. `make check-ingress` runs `nginx -t` on the production ingress
in both modes; `make smoke-ingress` checks its hosts, client addresses, body sizes, and
rate limits in front of a stub web service; `make check-images` builds both production
images, verifies the deployment commands are included, and serves the web image through nginx. CI runs all of these through the same
`make` targets, with PostgreSQL and Chromium, and also validates the production Compose
file. CI runs browser tests in both development and production modes. `npm start` serves
the standalone build and copies its static assets first, matching the container's server.
For a local production preview, run `HOSTNAME=127.0.0.1 PORT=3000 npm start` from
`apps/web` after building.

SQL lives in `apps/donor-api/db`. `make generate` runs pinned sqlc and regenerates the web
app's API types from the OpenAPI contract; commit each source with its generated code.
`make check-generated` fails if either has drifted. sqlc runs in its pinned Docker image
when the Docker daemon answers, and otherwise as `go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1`
(needs a C compiler); force either with `make generate SQLC_MODE=docker` or `SQLC_MODE=go`.
Both produce identical files.
Applied migrations are immutable and checksum-checked.

With the iOS and Android toolchains and sibling checkouts installed,
`make test-native-clients` proves both generated clients' session lifecycle against a
temporary HTTPS Go API and the synthetic test database. See
[native client acceptance](docs/mobile-foundation.md#real-native-client-acceptance)
for the command, checkout overrides, and its provider/device acceptance boundary.

The ATM acceptance ledger uses a dedicated [durable store and recovery
procedure](docs/atm-ledger.md). `make test-ledger lint-ledger` validates its Worker,
transactional storage, OIDC access, archive recovery and migration client. The original
Git ledger history is retained as an archive tag; workflows require durable publication.

Capacity is measured against a seeded, disposable deployment: `make seed` fills a
dedicated database with 50,000 synthetic donors — with their partners, employees, teams,
initiatives, events and reservations — and writes a private fixture of session tokens,
referral codes and event ids, and `make loadtest-seeded` runs a referral-spike burst and
soak, an event-day burst, and a dashboard soak against an API serving it. See
[operations](docs/operations.md#load-testing-with-representative-data)
for the guard, the mix, budgets, and reading results. For a quick check of a few
endpoints, `-url` reads a comma-separated list in equal measure:

```sh
cd apps/donor-api
go run ./cmd/loadtest -url "http://localhost:8002/donor/public/spend" -duration 30s -concurrency 128 -max-p99 250ms
```

The runner reports throughput, statuses, and latency percentiles overall and per
endpoint as JSON. It fails on non-200 responses, timeouts, oversized bodies, or a p99
above `-max-p99`; `DONOR_LOAD_COOKIE` sends a synthetic donor's session cookie. Results
from an empty database do not establish capacity.

## Containers and deployment

See [operations](docs/operations.md) and [edge and ingress](docs/edge.md). Docker images
run as non-root users, and each process connects to PostgreSQL with its own login, holding
only the privileges that process needs. Only nginx publishes ports: it terminates HTTPS in front of the web
service, behind Cloudflare or on its own, and the API binds to loopback. No command
enables workers or deletes database volumes implicitly. Test-only readiness is not approval to solicit live donations.

## Layout

- `apps/donor-api/cmd`: API, workers, migrations, database login provisioning, administrator bootstrap, monitoring, reconciliation, key rotation, load-test seeding and load testing.
- `apps/donor-api/internal`: HTTP, domain rules, persistence, Stripe, email, push, credentials.
- `apps/donor-api/db`: versioned SQL migrations and sqlc queries.
- `apps/donor-api/api`: the OpenAPI contract for web and native clients, checked by tests.
- `apps/donor-api/testdata`: recognition scenarios and fixtures shared by tests.
- `apps/donor-api/tools`: a module pinning the staticcheck build `make lint-go` runs.
- `apps/web`: public website, donor account, partner portal, staff console, browser and unit tests.
- `apps/atm-ledger`: the Cloudflare Worker that stores the ATM acceptance ledger ([atm-ledger](docs/atm-ledger.md)).
- `design/tokens.json`: the shared design values behind the web and native clients ([design system](docs/design-system.md)).
- `contracts/mobile`: the generated mobile-foundation bundle the native iOS and Android clients build against, checked by `make check-mobile`.
- `deploy/nginx`: the production ingress configuration.
- `scripts`: deployment, backup, restore verification, and ingress checks.
- `docs`: product rules, architecture, operations, and security guidance.

[Contributing](CONTRIBUTING.md) · [Architecture](docs/architecture.md) · [MIT license](LICENSE)
