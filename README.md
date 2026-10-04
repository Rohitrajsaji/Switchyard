# Switchyard

A Go experimentation and feature-flag platform, built as a modular monolith.

Implementation follows [the approved plan](docs/implementation-plan.md) in milestone order. Current verified progress is recorded in [the milestone ledger](docs/progress.md); features not marked complete are still being built.

## Local setup

Requires Docker Desktop with Compose, Make and Python 3. Images support Apple Silicon. Start the API, PostgreSQL and dashboard:

```sh
make up
make smoke
SWITCHYARD_DEMO_PASSWORD='switchyard-demo-only' make seed-demo
```

Open [the dashboard](http://localhost:3000) and sign in as `admin@example.test` with the explicitly supplied local demo password. Choose **Marketplace demo** → **development** → **Experiments** to inspect the synthetic fixture, or **Listing demo** to submit a measured synthetic listing.

`make up` creates an ignored `.env` from the local-only example, migrates PostgreSQL and starts the API at `http://127.0.0.1:8080`. The opt-in seed imports 100 synthetic exposures and 50 completions, clearly labelled as a fixture; it invents no latency measurements. Never use example credentials outside this local stack. `make down` preserves database volumes.

For host Go development, export `DATABASE_URL` from `.env`, then `make migrate` and `go run ./cmd/api`. Stop the Docker API first to free port 8080. Go 1.27.1 is pinned in `go.mod`.

```sh
make check
make race
TEST_DATABASE_URL='postgres://switchyard:switchyard-local-only@127.0.0.1:54329/switchyard_test?sslmode=disable' make integration
```

Integration tests require an isolated test database and create/drop their own schema. See [local development](docs/local-development.md). Local evaluator microbenchmarks are recorded in [the benchmark report](docs/benchmark-report.md); end-to-end load results remain unmeasured.

Login, projects/environments, boolean/JSON flags, preview, kill, audit, A/B lifecycle/results and a listing demo comprise the MVP. Production configuration stays read-only until M9. Redis, durable workers, SDKs, approved rollouts, telemetry/load testing and the governed agent follow at their V2 milestones.

For host dashboard development with Node 22.23.2, stop the Docker dashboard (`docker compose stop web`), then run `make web-install web-dev`. See [local development](docs/local-development.md) for browser tests and the fresh-volume MVP rehearsal.
