# Switchyard

A Go experimentation and feature-flag platform, built as a modular monolith.

Implementation follows [the approved plan](docs/implementation-plan.md) in milestone order. Current verified progress is recorded in [the milestone ledger](docs/progress.md); features not marked complete are still being built.

## Local setup

Requires Docker Desktop with Compose. Images support Apple Silicon. Start the API and PostgreSQL:

```sh
make up
make smoke
```

`make up` creates an ignored `.env` from the local-only example, migrates PostgreSQL and starts the API at `http://127.0.0.1:8080`. Never use example credentials outside this local stack. `make down` preserves database volumes.

For host Go development, export `DATABASE_URL` from `.env`, then `make migrate` and `go run ./cmd/api`. Stop the Docker API first to free port 8080. Go 1.27.1 is pinned in `go.mod`.

```sh
make check
make race
TEST_DATABASE_URL='postgres://switchyard:switchyard-local-only@127.0.0.1:54329/switchyard_test?sslmode=disable' make integration
```

Integration tests require an isolated test database and create/drop their own schema. See [local development](docs/local-development.md). Local evaluator microbenchmarks are recorded in [the benchmark report](docs/benchmark-report.md); end-to-end load results remain unmeasured.

The M5 dashboard checkpoint runs on the host with Node 22.23.2: `make web-install web-dev`, then open `http://localhost:3000`. Seed demo accounts explicitly as described in the local guide. Login, project/environment selection, flags, preview, kill and audit are available; experiment screens and the listing demo are being built. Docker dashboard setup follows before the MVP release.
