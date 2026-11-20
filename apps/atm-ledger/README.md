# atm-ledger

Append-only transparency ledger for the ATM acceptance run: a Cloudflare Worker
fronting a Durable Object with SQLite storage and an R2 archive. GitHub OIDC
identities are mapped to reader, publisher and administrator roles.

It is an acceptance artifact, not an R1 PRD feature: R1 does not publish
blockchain or external ledger transactions (see the [repository README](../../README.md)).
The donor product's runtime does not call it.

Design, storage layout and the recovery procedure are in
[docs/atm-ledger.md](../../docs/atm-ledger.md). From the repository root,
`make test-ledger` runs typecheck, tests and a deploy dry run, and `make lint-ledger`
runs lint and the dependency audit. Requires Node 22.19 or newer.
