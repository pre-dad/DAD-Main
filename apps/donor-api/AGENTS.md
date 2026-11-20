# donor-api — agent rules

Hard-won rules for working in the Go backend. Read this before changing code
here. These are constraints the codebase enforces (often via tests that fail
loudly) or that cost real rediscovery time when ignored. Keep it short and
true; add a rule when a mistake would otherwise be repeated.

## Database and migrations

- **Migrations are immutable and checksum-checked.** Never edit an applied
  migration in `db/migrations/`. Add a new one with the next number (confirm
  the highest existing number first — do not assume). The migrate tool refuses
  to start if a stored migration's checksum changed or if the DB has a
  migration the code does not.
- **Per-process least privilege is enforced.** Each process logs in as its own
  role (`internal/dbroles`), and `grants`/`Drift`/`Undeclared` tests fail until
  a new table or role is declared. The API role cannot write `donor_gifts` /
  `donor_billing_periods`; only the stripe-worker role can. Do not widen
  `dad_api` to make something compile — add a new role or use the right one.
- **Financial facts are append-only and written by one role.** Gifts, billing
  periods, observations and refunds are written by the stripe worker. A test
  that needs to insert a gift/refund as setup must do it as the owner
  (`mustExec` after `RESET ROLE`) or the stripe-worker role — not as the
  projections/API role, which holds only SELECT and will fail with
  `permission denied`.
- **sqlc is generated; never hand-edit `internal/store/queries/*`.** Change the
  `.sql` source, run `make generate`, and `make check-generated` must pass. The
  pinned sqlc runs in Docker (`sqlc/sqlc:<pinned>`); the web `generate:api` step
  of `make check-generated` fails with `openapi-typescript: command not found`
  when that binary is absent — that failure is unrelated to Go sqlc drift.

## Testing

- **Integration tests need a synthetic Postgres and never touch the app DB.**
  Set `TEST_DATABASE_URL`, migrate it first
  (`DONOR_GO_DATABASE_URL=$TEST_DATABASE_URL go run ./cmd/migrate`), then
  `go test`. Tests run each process's code as that process's DB role, so a
  missing grant fails a test by design.
- **Use an isolated database per worktree.** Parallel worktrees share one
  Postgres; a second worktree's migration set collides with the first
  ("database has unknown migration"). Create a per-worktree DB
  (`CREATE DATABASE donor_go_<name>`) and point `TEST_DATABASE_URL` at it.
- **Never weaken, skip, or delete a test to reach green.** Fix the cause. A
  failing test is usually either a real defect or a stale expectation that must
  be updated to the *intended* new behavior (and justified), not relaxed.
- **A recognition / projection logic change must bump `projectionRules`**
  (`internal/store/financial.go`). Saved projections recompute under the new
  rules only when the version is raised. When a migration also backfills, make
  the backfill and the recompute converge (compute incremental deltas as
  new-minus-old so a forced recompute is a no-op, not a double-count).

## Payments / Stripe

- **Test mode only.** Keys must be `sk_test_`; the webhook rejects any
  `livemode:true` event. Keep both.
- **The webhook receiver strips to an allowlist.** `minimalObject`
  (`internal/payments/webhook.go`) keeps only the fields the worker verifies.
  Webhooks cannot be expanded, so a `payment_intent.succeeded` payload carries
  `latest_charge` as a bare id, never the expanded object — you cannot "trust
  the payload" for charge facts that live on the expanded charge.
- **Preserve the fetch-outside-lock discipline.** The Stripe worker fetches
  provider state *before* taking the donor lock; a stale snapshot rolls back
  the transaction before a bounded re-fetch outside the lock. Do not move a provider
  round trip inside the lock — a test (`TestEventWorkerReadsProcessorOutsideDonorLock`)
  enforces this.
- **Every Stripe call spends from the account's request budget**
  (`internal/payments/ratebudget.go`, buckets worker/checkout/staff/reconcile).
  Route new calls and every list page through the appropriate bucket. The API
  and Stripe worker share it through PostgreSQL (`store.StripeBudget`), so
  replicas cannot exceed it together; the daily reconcile job keeps its own
  process-local slice. SDK retries are not budgeted.

## Workflow / process

- **Plan first; isolate work in a git worktree + branch; never merge/rebase/
  fast-forward `dev` or `main` from inside a feature worktree.** The owner
  merges after review.
- **Verify before claiming done:** `make format-go` (wraps past 140 columns), `go vet ./...`,
  `go build ./...`, and `go test -race` on the affected packages against a
  synthetic DB. State explicitly what you could not run and why.
- **Correctness-first on money paths.** This is a donation platform; never describe the
  receiving entity as a 501(c)(3), since its status is unconfirmed (PRD G-11).
  On financial-fact handling, recognition rules, migrations, auth, and the
  Founding Donor cutoff, prefer a correct fetch/behavior over a clever
  optimization. "Ship imperfect and iterate" does not apply to the ledger.
