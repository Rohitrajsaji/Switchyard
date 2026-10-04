SHELL := /bin/sh
export GOCACHE := $(CURDIR)/.cache/go-build
export GOMODCACHE := $(CURDIR)/.cache/go-mod

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
check: fmt-check vet test
integration:
	@test -n "$(TEST_DATABASE_URL)" || (echo 'Set TEST_DATABASE_URL to an isolated test database'; exit 1)
	go test -tags=integration -count=1 ./...
up:
	@test -f .env || cp .env.example .env
	docker compose build api
	docker compose up --no-build -d --wait
down:
	docker compose down
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
.cache/check-tools/.ready: scripts/check-requirements.txt
	python3 -m venv .cache/check-tools
	PIP_CACHE_DIR="$(CURDIR)/.cache/pip" .cache/check-tools/bin/python -m pip install -r scripts/check-requirements.txt
	touch .cache/check-tools/.ready
api-check: .cache/check-tools/.ready
	.cache/check-tools/bin/python scripts/check_api.py
