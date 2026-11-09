GOVULNCHECK := golang.org/x/vuln/cmd/govulncheck@v1.8.0
SQLC := sqlc/sqlc:1.31.1@sha256:70f53171d27b2424e9358869975455a6e955a5aa8e58a998a270a6e34e525537
SQLC_GO := github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1
# SQLC_MODE=docker or go forces one way to run sqlc; by default Docker is used when its daemon answers.
SQLC_MODE ?=
GOLINES := go tool -modfile=tools/go.mod golines -m 140
GENERATED := apps/donor-api/internal/store/queries apps/web/lib/openapi.d.ts apps/web/design/tokens.ts

# Load testing against a disposable, seeded deployment; see docs/operations.md.
SEED_DONORS ?= 50000
LOAD_API ?= http://127.0.0.1:8002
LOAD_MIX ?=
LOAD_SOAK_DURATION ?= 1h
LOAD_RESULTS_DIR ?=
# Must equal the API's DONOR_PUBLIC_ORIGIN, or referral captures get 403.
LOAD_ORIGIN ?= http://localhost:3000
LOADTEST = cd apps/donor-api && go run ./cmd/loadtest -api "$(LOAD_API)" -fixture "$(LOAD_FIXTURE)" -mix "$(LOAD_MIX)" -origin "$(LOAD_ORIGIN)" -client-ips

.PHONY: install db-up migrate provision api donor-go web dev worker reconcile seed
.PHONY: test test-api test-donor-go test-web test-ledger test-browser-e2e
.PHONY: lint lint-go lint-web lint-ledger lint-scripts format format-go format-web
.PHONY: check-ingress smoke-ingress check-images generate check-generated loadtest-seeded loadtest-soak
.PHONY: deploy-build deploy-up deploy-smoke deploy-down deploy-logs
.PHONY: generate-mobile check-mobile bundle-mobile
.PHONY: test-native-clients
.PHONY: check-deploy-secrets

install:
	cd apps/donor-api && go mod download
	cd apps/web && npm ci
	cd apps/atm-ledger && npm ci

db-up:
	docker compose up -d --wait postgres

migrate:
	cd apps/donor-api && go run ./cmd/migrate

provision:
	cd apps/donor-api && go run ./cmd/provision

api donor-go:
	cd apps/donor-api && go run ./cmd/api

web:
	cd apps/web && npm run dev

dev:
	@echo "Run 'make api' and 'make web' in separate terminals after loading your environment."

worker:
	cd apps/donor-api && go run ./cmd/worker --kind "$(KIND)" $(WORKER_ARGS)

reconcile:
	cd apps/donor-api && go run ./cmd/reconcile $(RECONCILE_ARGS)

generate:
	@if [ "$(SQLC_MODE)" = go ] || { [ "$(SQLC_MODE)" != docker ] && ! docker info >/dev/null 2>&1; }; then \
		echo "sqlc $(SQLC_GO) (go run)"; cd apps/donor-api && go run $(SQLC_GO) generate; \
	else \
		echo "sqlc $(SQLC) (docker)"; docker run --rm -v "$(CURDIR)/apps/donor-api:/src" -w /src $(SQLC) generate; \
	fi
	cd apps/web && npm run --silent generate:api
	node scripts/mobile-foundation.mjs generate

generate-mobile:
	node scripts/mobile-foundation.mjs generate

check-mobile:
	node scripts/mobile-foundation.mjs check
	node --test scripts/mobile-foundation.test.mjs

test-native-clients:
	bash scripts/check-native-clients.sh

check-deploy-secrets:
	node --test scripts/check-deploy-secrets.test.mjs scripts/ecs-rollout.test.mjs scripts/ecs-pin-image.test.mjs scripts/run-task.test.mjs

bundle-mobile: check-mobile
	node scripts/mobile-foundation.mjs bundle "$${MOBILE_BUNDLE_DIR:?set MOBILE_BUNDLE_DIR outside the repository}"

check-generated: generate
	@test -z "$$(git status --porcelain -- $(GENERATED))" || \
		{ git status --porcelain -- $(GENERATED); echo "Generated code differs from its sources; run make generate and commit the result." >&2; exit 1; }

test: test-donor-go test-web test-ledger

test-api test-donor-go:
	cd apps/donor-api && test -z "$$(gofmt -l .)" && test -z "$$($(GOLINES) -l .)" && go vet ./... && go test -race ./... && go build ./...

format: format-go format-web

# golines wraps lines past 140 columns and otherwise formats exactly as gofmt does.
format-go:
	cd apps/donor-api && $(GOLINES) -w .

format-web:
	cd apps/web && npm run format
	cd scripts && ../apps/web/node_modules/.bin/biome format --write .

test-web:
	cd apps/web && npm run lint && npm test -- --run && npm exec next typegen && npm run typecheck && npm run build

test-browser-e2e:
	cd apps/web && npm run test:e2e

test-ledger:
	cd apps/atm-ledger && npm run typecheck && npm test && npm run build
	node --test scripts/atm-ledger.test.mjs

lint: lint-go lint-web lint-ledger lint-scripts

lint-ledger:
	cd apps/atm-ledger && npm run lint && npm audit --audit-level=high

# staticcheck is pinned in apps/donor-api/tools/go.mod with a newer x/tools than its
# release requires, which is what reads the export data of the Go release CI uses.
lint-go:
	cd apps/donor-api && bash ../../scripts/check-go-dependencies.sh && go tool -modfile=tools/go.mod staticcheck ./... && go run $(GOVULNCHECK) ./... && go run $(GOVULNCHECK) -C tools tool

# The Node scripts share the web app's Biome install, so they are checked here.
lint-web:
	cd apps/web && npm audit --audit-level=high
	cd scripts && ../apps/web/node_modules/.bin/biome check --error-on-warnings .

lint-scripts:
	@if command -v shellcheck >/dev/null || [ -n "$$CI" ]; then shellcheck scripts/*.sh deploy/nginx/*.sh deploy/nginx/*.envsh; else echo "shellcheck not installed; skipping script lint" >&2; fi

check-ingress:
	./scripts/check-ingress.sh config

smoke-ingress:
	./scripts/check-ingress.sh smoke

# The web image is built for the ingress check's hosts so it can be served through nginx.
check-images:
	docker build -f apps/donor-api/Dockerfile -t dad/api:check .
	docker run --rm --entrypoint sh dad/api:check -c \
		'for binary in api worker migrate provision monitor reconcile staff rekey; do command -v "$$binary" >/dev/null || exit 1; done'
	docker build -f apps/web/Dockerfile -t dad/web:check \
		--build-arg DONOR_PUBLIC_ORIGIN=https://public.test --build-arg DONOR_STAFF_ORIGIN=https://staff.test .
	./scripts/check-ingress.sh web dad/web:check
	./scripts/check-nginx-image.sh

seed:
	cd apps/donor-api && go run ./cmd/seed -database-url "$(SEED_DATABASE_URL)" -confirm "$(SEED_CONFIRM)" -donors "$(SEED_DONORS)" -fixture "$(LOAD_FIXTURE)"

loadtest-seeded:
	$(LOADTEST) -duration 60s -concurrency 256 -max-p99 250ms
	$(LOADTEST) -duration 5m -concurrency 32 -max-p99 100ms
	$(LOADTEST) -duration 60s -concurrency 256 \
		-mix "referral-lookup=0,referral-capture=0,referral-missing=0,spend=0,dashboard=0,gifts=0,sharing=0,profile=0,recurring=0,annual-summary=0,events=55,event-detail=45" \
		-max-p99 400ms
	$(LOADTEST) -duration 5m -concurrency 32 \
		-mix "referral-lookup=0,referral-capture=0,referral-missing=0,spend=0,gifts=0,sharing=0,profile=0,recurring=0,annual-summary=0,dashboard=40,today-initiative=30,team=30" \
		-max-p99 150ms

# Three bounded segments retain separate p99 distributions and stop on the first failure.
loadtest-soak:
	@test -n "$(LOAD_RESULTS_DIR)" || { echo "Set LOAD_RESULTS_DIR to a new results directory" >&2; exit 1; }
	@umask 077; set -eC; mkdir -p "$(LOAD_RESULTS_DIR)"; \
	for segment in 1 2 3; do \
		($(LOADTEST) -duration "$(LOAD_SOAK_DURATION)" -concurrency 32 -max-p99 100ms) \
			> "$(LOAD_RESULTS_DIR)/soak-$$segment.json"; \
	done

deploy-build:
	./scripts/deploy.sh build

deploy-up:
	./scripts/deploy.sh up

deploy-smoke:
	./scripts/deploy.sh smoke

deploy-down:
	./scripts/deploy.sh down

deploy-logs:
	./scripts/deploy.sh logs
