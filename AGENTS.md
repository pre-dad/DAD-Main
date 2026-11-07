# Agent guide — DAD-Main

Start with these; this file only adds what they do not say.

- **[CONTRIBUTING.md](CONTRIBUTING.md)** — the contract: gates (`make test`, `make lint`,
  `make test-browser-e2e`), regenerate-don't-hand-edit, where working docs go, the
  financial-change review checklist. Read it before any change.
- **[README.md](README.md)** — local setup, checks, layout.
- **Sub-directory guides** — [`apps/donor-api/AGENTS.md`](apps/donor-api/AGENTS.md) (Go
  backend), [`apps/web/AGENTS.md`](apps/web/AGENTS.md) (Next.js web). Read the relevant one
  before touching that tree.
- **Cross-repo system docs** live in the separate **DAD-Docs** repo (topology, the
  mobile contract, branch conventions). Read them when a change crosses repo boundaries.

## Branch and push discipline

- Work on `dev`; ship via `dev` → PR → `main`. Never push to `main`/`master` directly.
- Run `git commit` and `git push` as **two separate commands** — do not chain
  `commit && push`; a long commit message can fuse into the push-target safety scan and
  get the push blocked.

## Code comments

- Comments explain **why** (a constraint, a trade-off, a non-obvious behavior), not what the
  code already says.
- No commented-out code (git keeps history). A `TODO` names a tracked issue.

## Gotchas not written in the docs

- **OpenAPI/schema changes fan out.** Adding a field to a Go struct that maps to the
  donor `Profile` means also updating `apps/donor-api/api/openapi.json` (properties +
  `required` for non-omitempty) **and both hand-maintained Swift `Profile` fixtures** in
  `contracts/mobile/swift/Tests/DADAPITests/ContractTests.swift`, or the contract / swift-compatibility
  CI jobs fail. Also regenerate `apps/web/lib/openapi.d.ts` (`make generate`).
- **`openapi.json` uses one-space indent.** A script that rewrites it must use
  `indent=1` + trailing newline, or the whole 12k-line file reformats into a noisy diff.
- **A staff role-model change spans Go + OpenAPI + the web staff console + its tests**
  (`apps/web/app/staff/Staff.tsx` renders `· {role}` verbatim; E2E and vitest hardcode
  role strings). Backend-only edits leave the web job red while Go is green.
- **dbroles grants apply at migrate/provision time.** Adding a grant and re-running
  against an existing test DB won't pick it up — drop/create/migrate a fresh DB. A new
  table's privileges go in `apps/donor-api/internal/dbroles` in the same change.
- **The mobile-foundation bundle is a hash-locked snapshot**, not live `dev`. Re-cutting
  it (`node scripts/mobile-foundation.mjs`) is a deliberate tag, never off an unverified
  commit. See the mobile contract in DAD-Docs.
- **Never track working documents** (plans, status, findings, session notes). They go in
  the git-ignored `docs/requirements/`. Tracked `docs/` is durable product docs only.
