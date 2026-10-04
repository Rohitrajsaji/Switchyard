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
check: fmt-check vet test
integration:
	@test -n "$(TEST_DATABASE_URL)" || (echo 'Set TEST_DATABASE_URL to an isolated test database'; exit 1)
	go test -tags=integration -count=1 ./...
up:
	@test -f .env || cp .env.example .env
	docker compose up --build -d --wait
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
