SHELL := /bin/sh
export GOCACHE := $(CURDIR)/.cache/go-build
export GOMODCACHE := $(CURDIR)/.cache/go-mod
# Bound package concurrency for the supported 8 GB development machine.
INTEGRATION_PACKAGES ?= 1

.PHONY: test vet race fmt fmt-check build check integration up down migrate smoke logs foundation-drill
test:
	go test -count=1 ./...
vet:
	go vet ./...
race:
	go test -race -count=1 ./...
fmt:
	gofmt -w $$(find . -name '*.go' -not -path './.cache/*')
fmt-check:
	@test -z "$$(gofmt -l $$(find . -name '*.go' -not -path './.cache/*'))"
build:
	mkdir -p bin
	go build -o bin/api ./cmd/api
	go build -o bin/migrate ./cmd/migrate
	go build -o bin/seed ./cmd/seed
	go build -o bin/worker ./cmd/worker
	go build -o bin/check-metrics ./cmd/check-metrics
	go build -o bin/workctl ./cmd/workctl
check: fmt-check vet test
integration:
	@test -n "$(TEST_DATABASE_URL)" || (echo 'Set TEST_DATABASE_URL to an isolated test database'; exit 1)
	@test -n "$(TEST_REDIS_URL)" || (echo 'Set TEST_REDIS_URL for scoped cache integration'; exit 1)
	@test -n "$(TEST_NATS_URL)" || (echo 'Set TEST_NATS_URL for scoped durable messaging integration'; exit 1)
	go test -p $(INTEGRATION_PACKAGES) -tags=integration -count=1 ./...
up:
	@test -f .env || cp .env.example .env
	docker compose build api web
	docker compose --profile cache --profile async up --no-build -d --wait
down:
	docker compose --profile cache --profile async down
migrate:
	go run ./cmd/migrate
smoke:
	python3 scripts/smoke.py
logs:
	docker compose logs --tail=100
foundation-drill:
	python3 scripts/foundation_drill.py
seed:
	@test -n "$(SWITCHYARD_DEMO_PASSWORD)" || (echo 'Set SWITCHYARD_DEMO_PASSWORD explicitly'; exit 1)
	docker compose run --rm --no-deps -e SWITCHYARD_SEED_DEMO=true -e SWITCHYARD_DEMO_PASSWORD --entrypoint /app/seed migrate
.PHONY: seed-demo mvp-drill
seed-demo: seed
	python3 scripts/seed_demo.py
mvp-drill:
	python3 scripts/mvp_drill.py
.PHONY: cache-storage-check
cache-storage-check:
	@test -n "$(TEST_REDIS_URL)" || (echo 'Set TEST_REDIS_URL to the local test Redis service'; exit 1)
	go test -race -tags=integration -count=1 ./internal/cache ./pkg/snapshot
.PHONY: cache-drill
cache-drill:
	python3 scripts/cache_drill.py
.PHONY: async-up messaging-check publication-drill
async-up:
	docker compose --profile async up --no-build -d --wait nats worker
messaging-check:
	@test -n "$(TEST_NATS_URL)" || (echo 'Set TEST_NATS_URL to local JetStream'; exit 1)
	go test -race -tags=integration -count=1 ./internal/platform/messaging
publication-drill:
	python3 scripts/publication_drill.py
.PHONY: aggregation-parity
aggregation-parity:
	docker compose --profile async run --rm --no-deps --entrypoint /app/check-metrics worker
management-smoke:
	python3 scripts/management_smoke.py
fuzz-smoke:
	go test ./pkg/evaluation -run '^$$' -fuzz FuzzCompile -fuzztime=10s
benchmark-evaluator:
	go test ./pkg/evaluation -run '^$$' -bench BenchmarkEvaluate -benchmem -count=3
flags-smoke:
	python3 scripts/flags_smoke.py
.PHONY: experiments-smoke
experiments-smoke:
	python3 scripts/experiments_smoke.py
.PHONY: events-smoke
events-smoke:
	python3 scripts/events_smoke.py
.PHONY: measurement-smoke
measurement-smoke: events-smoke
.cache/check-tools/.ready: scripts/check-requirements.txt
	python3 -m venv .cache/check-tools
	PIP_CACHE_DIR="$(CURDIR)/.cache/pip" .cache/check-tools/bin/python -m pip install -r scripts/check-requirements.txt
	touch .cache/check-tools/.ready
api-check: .cache/check-tools/.ready
	.cache/check-tools/bin/python scripts/check_api.py
.PHONY: web-install web-dev web-check e2e
web-install:
	cd web && npm ci --cache ../.cache/npm
web-dev:
	cd web && npm run dev
web-check:
	cd web && npm run format-check && npm run typecheck && npm test && npm run build
e2e:
	cd web && PLAYWRIGHT_BROWSERS_PATH="$(CURDIR)/.cache/playwright" npm run e2e

.PHONY: recovery-smoke
recovery-smoke:
	python3 scripts/recovery_smoke.py

.PHONY: retention-smoke
retention-smoke:
	python3 scripts/retention_smoke.py

.PHONY: event-drill
event-drill:
	python3 scripts/event_drill.py

.PHONY: ingestion-trial
ingestion-trial:
	python3 scripts/ingestion_trial.py

# Normal builds use checked-in generated sources. These targets are for contract edits.
PROTOC ?= protoc
.cache/proto-tools/.v1.36.12-v1.6.2:
	mkdir -p .cache/proto-tools
	GOBIN="$(CURDIR)/.cache/proto-tools" go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
	GOBIN="$(CURDIR)/.cache/proto-tools" go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2
	touch $@
.PHONY: proto-generate proto-check sdk-contract
proto-generate: .cache/proto-tools/.v1.36.12-v1.6.2
	@test "$$($(PROTOC) --version)" = 'libprotoc 33.1' || (echo 'Use protoc 33.1 for reproducible sources'; exit 1)
	PATH="$(CURDIR)/.cache/proto-tools:$$PATH" $(PROTOC) --go_out=. --go_opt=module=switchyard --go-grpc_out=. --go-grpc_opt=module=switchyard api/proto/switchyard/v1/evaluation.proto
proto-check: .cache/proto-tools/.v1.36.12-v1.6.2
	@test "$$($(PROTOC) --version)" = 'libprotoc 33.1' || (echo 'Use protoc 33.1 for reproducible sources'; exit 1)
	mkdir -p .cache/proto-check
	PATH="$(CURDIR)/.cache/proto-tools:$$PATH" $(PROTOC) --go_out=.cache/proto-check --go_opt=module=switchyard --go-grpc_out=.cache/proto-check --go-grpc_opt=module=switchyard api/proto/switchyard/v1/evaluation.proto
	diff -u pkg/api/switchyard/v1/evaluation.pb.go .cache/proto-check/pkg/api/switchyard/v1/evaluation.pb.go
	diff -u pkg/api/switchyard/v1/evaluation_grpc.pb.go .cache/proto-check/pkg/api/switchyard/v1/evaluation_grpc.pb.go
sdk-contract:
	@test -n "$(TEST_DATABASE_URL)" || (echo 'Set TEST_DATABASE_URL to an isolated test database'; exit 1)
	go test -p 1 -race -tags=integration -count=1 ./pkg/sdk ./internal/transport/grpc
.PHONY: sdk-drill
sdk-drill:
	python3 scripts/sdk_drill.py
.PHONY: approval-drill
approval-drill:
	python3 scripts/approval_drill.py
.PHONY: agent-test
agent-test:
	@test -x .cache/agent-venv/bin/python || python3 -m venv .cache/agent-venv
	@.cache/agent-venv/bin/python -c 'import pydantic,pytest' || .cache/agent-venv/bin/pip install pydantic pytest
	cd agent && ../.cache/agent-venv/bin/python -m pytest -q
.PHONY: rollout-drill
rollout-drill:
	python3 scripts/rollout_drill.py
.PHONY: observability-up observability-down observability-smoke
# Optional profile: Collector, Tempo, Prometheus and Grafana. The API and worker are recreated with
# OTLP export enabled; observability-down recreates them with export disabled again.
observability-up:
	@test -f .env || cp .env.example .env
	docker compose build api
	OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317 docker compose --profile cache --profile async --profile observability up --no-build -d --wait
observability-down:
	docker compose --profile observability stop otel-collector tempo prometheus grafana
	docker compose --profile cache --profile async up --no-build -d --wait api worker
observability-smoke:
	python3 scripts/observability_smoke.py
.PHONY: load-fixture load-evaluation load-events load-mixed load-soak failure-drills require-demo-password
require-demo-password:
	@test -n "$(SWITCHYARD_DEMO_PASSWORD)" || (echo 'Set SWITCHYARD_DEMO_PASSWORD explicitly'; exit 1)
# Durations are the scaled local protocol recorded in docs/benchmark-report.md; override the
# variables for the plan's longer runs. Results are written to docs/benchmarks/m10-*.json.
LOAD_EVAL_STEPS ?= 500,1000,2000,4000,6000,8000,10000
LOAD_STEP_SECONDS ?= 45
load-fixture: require-demo-password
	cd scripts && python3 load_fixture.py
load-evaluation: require-demo-password
	cd scripts && python3 load_run.py evaluation --steps $(LOAD_EVAL_STEPS) --step-seconds $(LOAD_STEP_SECONDS)
load-events: require-demo-password
	cd scripts && python3 load_run.py events --batch-rate 10 --seconds 120
load-mixed: require-demo-password
	cd scripts && python3 load_run.py mixed --steps 2000 --step-seconds 120 --batch-rate 10
load-soak: require-demo-password
	cd scripts && python3 load_run.py soak --steps 1000 --step-seconds 600 --batch-rate 5
failure-drills: cache-drill event-drill rollout-drill
