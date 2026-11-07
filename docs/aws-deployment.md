# AWS deployment design (DAD foundation)

Status: approved design, reconciled against the repository. This document records an
already-decided AWS deployment for the DAD donor release and reconciles every concrete
value (service names, environment variable names, ports, commands) against the real
repository files: `docker-compose.prod.yml`, `scripts/deploy.sh`, `docs/operations.md`,
`docs/edge.md`, `docs/architecture.md`, `README.md`, `Makefile`,
`apps/donor-api/Dockerfile`, `apps/web/Dockerfile`, and `scripts/refresh-cloudflare-ips.sh`.
Where the design's assumptions differed from the repository, the repository wins and the
reconciliation is noted inline.

The IaC is AWS CDK (TypeScript). This is the design; the CDK app lives in the
[DAD-infra](https://github.com/pre-dad/DAD-infra) repository at `apps/dad-main/`, and
the estate-wide design (accounts, shared packages, the DAD-Quality runner) is in
DAD-infra `docs/design/aws-infra-design.md`.

## 1. Region and accounts

- **Region:** `us-east-2` (Ohio) for all resources, including SES.
- **IaC:** AWS CDK in TypeScript, one app (`apps/dad-main` in DAD-infra), environment
  selected by CDK context `-c env=staging|production`. Account ids and region come from
  DAD-infra's shared `packages/config`; DAD-Main-specific values (hostnames, counts, task
  sizes) are an app-level extension of it.
- **Accounts / environments:**
  - `staging` → AWS account `853377042897` (org "sandbox"). Primary focus for the test.
  - `production` → AWS account `216833405626` (org "production").
- The same code targets either account. A typed per-environment config object holds
  account id, region, hostnames, desired counts, task sizes, `multiAz`, log retention,
  deletion protection, and the Cloudflare-ingress toggle. Promotion to production is a
  config change, not a code change.

## 2. Hostnames (Option A — alongside the existing sites)

DNS is on Cloudflare; the user adds records out-of-band. Existing sites
(`staging.dynamicaid.org`, `dynamicaid.org`) are untouched.

| Env | Public host | Staff host |
| --- | --- | --- |
| staging | `app.staging.dynamicaid.org` | `staff.staging.dynamicaid.org` |
| production | `app.dynamicaid.org` | `staff.dynamicaid.org` |

These map to `DONOR_PUBLIC_ORIGIN` / `DONOR_STAFF_ORIGIN` as HTTPS origins, lowercase, no
trailing slash. Reconciliation: `docs/operations.md` and `docs/edge.md` confirm both
origins must be exact HTTPS origins, lowercase, no default port/path/trailing slash, and
must differ (the API refuses a staff origin equal to or a parent of the public host).
Both are also **build args of the web image** (`apps/web/Dockerfile`: `ARG
DONOR_PUBLIC_ORIGIN`, `ARG DONOR_STAFF_ORIGIN`), so the web image must be rebuilt when
either origin changes.

## 3. Edge

Flow: Cloudflare (real edge: WAF, rate limits, cache) → NLB (TCP 443 and 80) → nginx
Fargate service (TLS termination, host split, per-client limits) → web → api.

- **nginx is the only public-facing service.** Reconciliation (`docker-compose.prod.yml`,
  `docs/edge.md`): nginx is the only service with public ports. In Compose it maps
  `80:8080` and `443:8443` — the container listens on **8080** (HTTP) and **8443**
  (HTTPS) as uid/gid `101:101`, and exposes a health endpoint on **8081** at path
  `/health`. The NLB therefore forwards TCP 443→8443 and 80→8080.
- **INGRESS_MODE=cloudflare** (default). nginx requires its Authenticated Origin Pulls CA
  and derives host names from `DONOR_PUBLIC_ORIGIN` / `DONOR_STAFF_ORIGIN`.
- **NLB** is internet-facing. As defense in depth its ingress is restricted to
  Cloudflare's published IP ranges via a security-group toggle
  (`cloudflareIngressEnabled`) plus a documented ranges input. We do **not** hardcode a
  stale list: `scripts/refresh-cloudflare-ips.sh` already fetches
  `https://www.cloudflare.com/ips-v4` and `ips-v6` for the nginx config, and the same
  lists feed the NLB SG. Reconciliation note: an NLB forwards TCP; per-IP source
  restriction is enforced by the security group on the nginx service ENIs (NLB
  target-level), and the design documents the Cloudflare "allow 443 only from Cloudflare
  ranges" intent from `docs/edge.md`.
- **nginx config on Fargate.** Compose bind-mounts `deploy/nginx` (`nginx.conf`,
  `templates/`, `conf/`, `05-ingress.envsh`, the origin-pull CA, `staff-access.conf`) and
  a TLS directory (`fullchain.pem`, `privkey.pem`, readable by uid 101). Fargate has no
  host bind mounts, so we **build a small `dad/nginx` image** from `nginx:1.30.5-alpine`
  (the pinned Compose image) that bakes in the `deploy/nginx` config and keeps the same
  `NGINX_ENVSUBST_FILTER=^INGRESS_`, `NGINX_ENTRYPOINT_QUIET_LOGS=1`, and
  `INGRESS_MODE=cloudflare` environment. This is the recommended choice over an EFS mount
  because the config is versioned in the repo and the container renders templates only at
  start. TLS material (Cloudflare Origin CA `fullchain.pem`/`privkey.pem`) and the
  origin-pull CA are supplied at runtime via Secrets Manager/SSM mounted into the task
  (or baked for the CA, which is a checked-in public cert). The edge-ranges file is
  refreshed by the existing script; a config change rebuilds and redeploys the nginx
  image (mirrors the Compose `INGRESS_CONFIG_HASH` recreate behavior).

## 4. Compute — ECS on Fargate

One ECS cluster per environment. All tasks run as non-root (api/web uid 10001 per the
Dockerfiles; nginx uid 101). All long-running services use the application image
`dad/api` except `web` (`dad/web`) and `nginx` (`dad/nginx`).

### 4.1 Long-running ECS services (one per Compose long-running service)

Reconciled exactly against `docker-compose.prod.yml`. Nine services:

| ECS service | Image | Command | Key env (exact names) | Port |
| --- | --- | --- | --- | --- |
| `api` | `dad/api` | `api` (image default CMD) | `DONOR_GO_LISTEN=0.0.0.0:8002`, `DONOR_PUBLIC_ORIGIN`, `DONOR_STAFF_ORIGIN`, `DONOR_API_DATABASE_URL`, `DONOR_TRUSTED_PROXY_CIDRS` (web subnet CIDR), plus mail + Stripe + optional secrets | 8002 |
| `web` | `dad/web` | `node server.js` (image default) | `DONOR_API_BASE_URL=http://api:8002`, `DONOR_CLIENT_IP_HEADER=X-Real-IP` | 3000 |
| `nginx` | `dad/nginx` | image default | `INGRESS_MODE=cloudflare`, `DONOR_PUBLIC_ORIGIN`, `DONOR_STAFF_ORIGIN`, `NGINX_ENVSUBST_FILTER=^INGRESS_` | 8080/8443 (health 8081) |
| `stripe-worker` | `dad/api` | `worker --kind stripe` | `DONOR_GO_WORKERS_ENABLED`, `STRIPE_SECRET_KEY`, `DONOR_STRIPE_REQUESTS_PER_SECOND`, `DONOR_WORKER_STRIPE_DATABASE_URL` | none |
| `projection-worker` | `dad/api` | `worker --kind projections` | `DONOR_GO_WORKERS_ENABLED`, `DONOR_WORKER_PROJECTIONS_DATABASE_URL` | none |
| `login-email-worker` | `dad/api` | `worker --kind login-email` | `DONOR_GO_WORKERS_ENABLED`, `DONOR_PUBLIC_ORIGIN`, mail transport (`DONOR_SMTP_*`, `DONOR_EMAIL_FROM`, `DONOR_EMAIL_ENCRYPTION_KEY`), `DONOR_WORKER_LOGIN_EMAIL_DATABASE_URL` | none |
| `email-change-worker` | `dad/api` | `worker --kind email-changes` | as login-email + `DONOR_WORKER_EMAIL_CHANGES_DATABASE_URL` | none |
| `message-worker` | `dad/api` | `worker --kind messages` | as login-email + `DONOR_WORKER_MESSAGES_DATABASE_URL` | none |
| `push-worker` | `dad/api` | `worker --kind push` | `DONOR_GO_WORKERS_ENABLED`, push delivery (`DONOR_PUSH_APNS_*`, `DONOR_PUSH_FCM_KEY_FILE`), `DONOR_WORKER_PUSH_DATABASE_URL` | none |

Reconciliation note on worker `--kind` values: the Compose service names are
`login-email-worker`, `email-change-worker`, `message-worker`, but the `worker --kind`
values are `login-email`, `email-changes`, `messages` respectively (the service name and
the kind are not identical). The ECS service names mirror the Compose service names.

Staging desired counts: `web`=1, each worker=1, `api`=1, `nginx`>=1 (default 1, raise for
AZ coverage). All counts are per-env config. Whether the workers run at all is one switch,
`workersEnabled` in DAD-infra's per-env config (off until an environment is ready): it
sets their desired count, and the app pipeline skips services at a desired count of 0
(see §8). That mirrors `scripts/deploy.sh workers`, which refuses to start workers until
they are enabled. The worker binary itself also requires `DONOR_GO_WORKERS_ENABLED=true`,
so the task definitions always set it; it is not the switch.

### 4.2 api networking and trust

- `api` listens `0.0.0.0:8002` and is reachable **only from the web tier** (api SG inbound
  8002 from web SG only).
- `DONOR_TRUSTED_PROXY_CIDRS` must be the web tier's subnet CIDR so the API trusts
  `X-Real-IP` only from web. In Compose this is the `app` network `172.30.43.0/24`; on AWS
  it becomes the web tier's private-subnet CIDR(s). The API refuses a trusted-proxy range
  broader than IPv4 /8 or IPv6 /32, so per-subnet CIDRs are used, not a supernet.
- web→api discovery uses **ECS Service Connect** (Cloud Map namespace per cluster) so that
  `DONOR_API_BASE_URL=http://api:8002` resolves to the api service by name, matching the
  Compose value verbatim. Decision: ECS Service Connect over an internal ALB because the
  Compose value is a bare `http://api:8002` service name and no L7 features are needed
  between web and api.

### 4.3 Maintenance one-off tasks (ECS run-task, not services)

Reconciled against the Compose `maintenance` profile and `scripts/deploy.sh`. These are
run-task definitions, not services, and CDK does **not** execute them:

| Task | Command | URL env (exact) | Notes |
| --- | --- | --- | --- |
| `migrate` | `migrate` | `DONOR_GO_DATABASE_URL` (schema owner) | also `migrate --adopt-existing` for the `adopt` case |
| `provision-roles` | `provision` | owner URL + every per-process `DONOR_*_DATABASE_URL` | creates/updates per-process logins; must run **after** migrate |
| `staff` | `staff` | `DONOR_STAFF_ORIGIN`, `DONOR_STAFF_CLI_DATABASE_URL` | administrator bootstrap |
| `rekey` | `rekey` | `DONOR_EMAIL_ENCRYPTION_KEY`, `DONOR_CONTENT_ENCRYPTION_KEY`, `DONOR_REKEY_DATABASE_URL` | key rotation |
| `monitor` | `monitor` | `DONOR_MONITOR_DATABASE_URL` | queue-age check |

A thin wrapper `scripts/run-task.sh` (in this repo) wraps `aws ecs run-task` with the
right task definition and command overrides; it is documented, not auto-run.

### 4.4 Scheduled task — reconcile

- `reconcile` runs via **EventBridge Scheduler**, daily ~00:30 America/New_York, as a
  Fargate run-task (command `reconcile`, env `STRIPE_SECRET_KEY`,
  `DONOR_STRIPE_REQUESTS_PER_SECOND`, `DONOR_RECONCILE_DATABASE_URL`).
- The job exits non-zero when any discrepancy count > 0 (per `docs/operations.md`). A
  **CloudWatch alarm fires on non-zero task exit code**, wired to an SNS topic (optional
  email subscription parameter).

## 5. Data — RDS + RDS Proxy

- **RDS PostgreSQL 16**, single instance for staging (`multiAz=false`), production
  `multiAz=true` via config. Isolated subnets, storage encrypted (KMS), automated backups
  and snapshots enabled, deletion protection on for production (configurable off for
  staging).
- **RDS Proxy** in front, **transaction mode**. Every process DB URL points at the proxy
  endpoint with the process's own login and includes `default_query_exec_mode=exec` and
  `sslmode=verify-full` (pgx + transaction-mode pooler requirement from
  `docs/operations.md#connection-pooling`).
- CDK provisions the RDS instance, the master credentials (Secrets Manager), a dedicated
  **owner login** concept, and generates **per-process passwords** stored one-secret-per
  login in Secrets Manager and injected into tasks as ECS secrets. CDK does **not** create the per-process Postgres roles
  (`dad_api`, `dad_worker_stripe`, `dad_worker_projections`, `dad_worker_login_email`,
  `dad_worker_email_changes`, `dad_worker_messages`, `dad_worker_push`, `dad_monitor`,
  `dad_reconcile`, `dad_staff_cli`, `dad_rekey`) — the app's `migrate` and
  `provision-roles` tasks do, after migrate.
- **Owner bootstrap (documented one-time, not automated):** per
  `docs/operations.md#local-development-and-managed-postgresql`, on RDS create the owner
  with `CREATE ROLE owner LOGIN CREATEROLE` as the master user (a member of
  `rds_superuser`) and give it the application database. The owner's URL
  (`DONOR_GO_DATABASE_URL`) is used only by migrate/provision-roles.

### 5.1 Per-process DB URL construction

Each process URL carries the login but not the password:

```
postgresql://<process-login>@<rds-proxy-endpoint>:5432/<db-name>?default_query_exec_mode=exec&sslmode=verify-full
```

The password reaches the process as an ECS secret under `<PREFIX>_DATABASE_PASSWORD`,
where `<PREFIX>_DATABASE` is the URL variable without `_URL` (for example
`DONOR_API_DATABASE_PASSWORD` beside `DONOR_API_DATABASE_URL`), injected from the
`password` key of that login's Secrets Manager secret. No task definition holds a
database password in `environment`. The password-less URL form is used rather than
separate host/name/user parts because it keeps `default_query_exec_mode=exec`, which the
RDS Proxy needs. A full URL that already contains a password still wins over the
separate secret; see `docs/operations.md#supplying-a-password-as-a-secret`.

`<process-login>` comes from that process's Secrets Manager secret, `<rds-proxy-endpoint>`
is the proxy's read/write endpoint, and `<db-name>` is the application database (default
`dad`, per `.env.database.example` `POSTGRES_DB=dad`). The owner URL
`DONOR_GO_DATABASE_URL` takes the same form with `DONOR_GO_DATABASE_PASSWORD` and is used
only by migrate and provision-roles.

Deploy order: the DAD-Main release containing commit 7506cbf, which accepts both forms,
goes out first; the DAD-infra change that switches task definitions to the password-less
form (5c952e2) goes out after it. Deploying DAD-infra first leaves the processes unable
to find a password. See `DAD-infra/docs/bootstrap.md`, "Database passwords are ECS
secrets". Login names mirror `.env.database.example`
(`dad_api_login`, `dad_worker_stripe_login`, … `dad_reconcile_login`).

## 6. Supporting services

- **ECR:** repositories `dad/api` and `dad/web` (and `dad/nginx` for the baked ingress
  image). Lifecycle policy expires untagged/old images.
- **Secrets Manager:** Stripe test keys (`STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET`),
  the two encryption keys (`DONOR_EMAIL_ENCRYPTION_KEY`, `DONOR_CONTENT_ENCRYPTION_KEY`),
  per-process DB passwords (`<PREFIX>_DATABASE_PASSWORD`), SES SMTP credentials (`DONOR_SMTP_HOST`, `DONOR_SMTP_PORT`,
  `DONOR_SMTP_USERNAME`, `DONOR_SMTP_PASSWORD`, `DONOR_EMAIL_FROM`), plus optional api
  secrets modeled with empty defaults so the app's graceful-degradation 503s apply when
  unset: `DONOR_EVENT_CHECKIN_SECRET`, `DONOR_FOUNDING_CLAIM_KEY`, `DONOR_SMS_*`,
  `DONOR_OIDC_*`, `DONOR_WALLET_*`, `DONOR_PUSH_*`, `DONOR_SUPPORT_EMAIL`, `DONOR_SMTP_CA`.
  Injected as ECS task secrets (`valueFrom`). **Crown jewels:** the two encryption keys are
  backed up separately, rotated by hand; losing them makes encrypted columns unreadable
  (README/operations call-out).
- **EFS:** for Wallet signing keys (api) and APNs/FCM key files (push-worker) mounted
  read-only via access points enforcing the container uid (api uid 10001; push-worker
  similar), matching the commented Compose volumes. Optional/toggleable.
- **SES (us-east-2):** domain identity for the sending subdomain `mail.dynamicaid.org`
  (DKIM enabled; DKIM/verification DNS records output for Cloudflare). Sender
  `no-reply@mail.dynamicaid.org`. Generate SES SMTP credentials (IAM user with an SES
  sending policy; SMTP password derived) and store SMTP host/port/username/password in
  Secrets Manager mapped to `DONOR_SMTP_*`. Per-env sending subdomain via config (e.g.
  `mail.staging.dynamicaid.org` for staging). Reminder output: SES starts in **sandbox
  mode**; production sending access must be requested.
- **CloudWatch:** one log group per service with explicit retention per env (staging 30
  days, production longer). Alarms: reconcile non-zero exit, ECS service health (running
  count below desired), placeholder for monitor queue age. Alarms → SNS topic.

## 7. Network boundaries

New dedicated VPC per env across 2+ AZs:

- **public subnets** — NLB.
- **private-with-egress subnets** — api/web/workers (NAT for Stripe, SES, APNs/FCM, image
  pulls).
- **isolated subnets** — RDS.

Security-group chain (mirrors the Compose network isolation):

- NLB → nginx (nginx SG inbound 8080/8443 from NLB / Cloudflare ranges only).
- nginx → web (web SG inbound 3000 from nginx SG only).
- web → api (api SG inbound 8002 from web SG only).
- api/workers → RDS Proxy (proxy SG inbound 5432 from app SGs only).
- RDS SG inbound 5432 from proxy SG only.
- Workers: no inbound.

## 8. CI/CD (GitHub Actions)

Extends existing workflows without breaking `ci.yml`. Auth is **GitHub OIDC → per-account
IAM deploy roles** (no static keys). The OIDC provider is the shared construct in
DAD-infra's `packages/pipeline-iam` (an account has exactly one GitHub OIDC provider, so
it is defined once and shared with DAD-Quality's stack); the DAD-Main deploy roles come
from the `pipeline-iam` stack in `apps/dad-main` (documented one-time per-account
apply). Action pinning
follows the repo convention in `ci.yml`: pin by full commit SHA with a `# vX.Y.Z` comment;
where a SHA cannot be resolved offline, pin by major tag with a NOTE.

Two pipelines:

1. **Infra pipeline — DAD-infra `.github/workflows/dad-main-infra.yml`** (CDK only; it
   lives in the DAD-infra repo, path-filtered to `apps/dad-main/**`):
   - PRs touching `apps/dad-main/**` or `packages/**` → offline `cdk synth` of both
     environments (no AWS credentials).
   - merge to `dev` → `cdk deploy -c env=staging` (sandbox role).
   - merge to `main` → `cdk deploy -c env=production`, gated by a GitHub Environment
     protection rule (manual approval).

2. **App pipeline — `.github/workflows/deploy.yml`** (direct ECS, CDK reserved for infra):
   - build `dad/api` + `dad/web` (+ `dad/nginx`), push to ECR tagged with the git SHA.
   - **pin task definitions to that tag** (`scripts/ecs-pin-image.sh`): register a new
     revision of every task-definition family (the nine services and the six maintenance
     tasks) with only the image tag changed, and save the resulting `family=ARN` lines for
     the later steps (`DAD_TASKDEFS_FILE`).
   - then **direct ECS deploy** in the exact `scripts/deploy.sh` order:
     `migrate` run-task → `provision-roles` run-task (each waited on, and failing unless
     the container exits 0) → deploy api/web/nginx on their pinned revisions and wait for
     the rollout to complete (the `up` step) → the same for the six worker services,
     skipping any DAD-infra has switched off (desired count 0).
   - a `ci-gate` job runs first: a push starts `deploy.yml` and `ci.yml` side by side, so
     the gate polls the `ci.yml` run for the same commit (up to 45 minutes) and fails unless
     it succeeded. `deploy` needs it, so a red or cancelled commit never deploys.
   - `APP_DEPLOY_ROLE_ARN` must exist at **repository** level (any non-empty value) for
     anything to run. Both jobs test it in a job-level `if:`, which GitHub evaluates before
     the job's environment is chosen, so an environment-only variable reads as empty and
     the jobs are skipped green. Set the real role ARN per environment (`staging`,
     `production`) as well: inside the steps the environment value overrides the
     repository one, which keeps each environment on its own account's role.
   - staging auto on merge to `dev`; production **only** on merge to `main`, gated by
     manual approval. There is no release- or tag-triggered path to production (releases,
     including the `mobile-foundation-*` contract releases, never start a deploy).
   - **Decision:** the app pipeline uses direct `aws ecs update-service`; CDK is reserved
     for infra only.

Why a pin step exists: the CDK task definitions carry the image tag of the last
successful release (DAD-infra reads it from the SSM parameter `/dad/<env>/image-tag`,
which `deploy.yml` writes as its last step), and an ECS service or `run-task` runs a
specific task-definition revision. The pipeline therefore
registers revisions that point at the SHA-tagged images instead of redeploying a moving
tag. Because every revision names an immutable SHA, a circuit-breaker rollback restores
the previous image. Each revision is copied from the family's latest active one, so
everything but the image tag (command, environment, secrets, health check, roles, sizing,
tags) is whatever CDK last defined.

The deploy gate is the ECS rollout itself. The api, web, and nginx task definitions carry
container health checks (api `/health/live` on 8002, nginx `/health` on 8081, web `/` on
3000), and every service uses the deployment circuit breaker with rollback.
`scripts/ecs-rollout.sh` starts each new deployment, remembers its id, and waits for that
deployment to reach `COMPLETED`; a deployment that ends `FAILED` (rolled back) fails the
step. It follows its own deployment id because a rollback is a new deployment that itself
reports `COMPLETED`. No shell into a running task is needed. The api check is liveness, not the
database-backed `/health/ready`, so a database outage does not make ECS replace every api
task; `migrate` runs first and proves database connectivity.

Document required GitHub Environments (`staging`, `production`) and protection rules, and
the repo/environment variables each workflow needs (account ids, region, cluster/service
names) — prefer reading cluster/service names from CDK outputs/SSM over hardcoding.

## 9. Open items

- Exact production desired counts and task sizes (CPU/memory) — staging defaults chosen;
  production is a config change.
- Whether the staff host sits behind Cloudflare Access (per-hostname Authenticated Origin
  Pulls CA then required — `docs/edge.md#origin-lock`).
- Per-env sending subdomain final names and SES production-access request timing.
- EFS enablement for Wallet/push keys (off until those providers are configured).

## 10. Alternatives considered

- **Internal ALB vs ECS Service Connect for web→api:** chose Service Connect so
  `DONOR_API_BASE_URL=http://api:8002` works verbatim with no L7 layer.
- **ALB vs NLB at the edge:** chose NLB (TCP 443/80) because Cloudflare is the real edge
  and nginx terminates TLS; an ALB would duplicate TLS/L7 and complicate Authenticated
  Origin Pulls.
- **EFS-mounted nginx config vs baked `dad/nginx` image:** chose a baked image because the
  `deploy/nginx` config is versioned and rendered only at container start.
- **CDK-managed app deploys vs direct ECS:** chose direct `aws ecs update-service` per the
  user's decision; CDK is reserved for infra.

---
Reconciliation summary: service names, env var names, ports (8002 api, 3000 web,
8080/8443/8081 nginx), worker `--kind` values, maintenance commands, the reconcile
schedule and non-zero-exit alarm, and the per-process DB URL query params
(`default_query_exec_mode=exec`, `sslmode=verify-full`) were all confirmed against
`docker-compose.prod.yml`, `scripts/deploy.sh`, `docs/operations.md`, `docs/edge.md`, and
`.env.database.example`. Content was rephrased for compliance with licensing restrictions
where external sources (Cloudflare/SES docs) are referenced.
