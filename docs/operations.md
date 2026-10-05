# Operations

Local only. Do not point this stack at a hosted account or a paid provider. Demo passwords and application keys are for the machine running Compose.

## Start and stop

```sh
make up
make smoke
make down
```

`make up` builds the API and dashboard, migrates PostgreSQL, and waits until the core services are healthy. `make down` keeps the database volume. Host Go development uses `DATABASE_URL` from `.env` and must stop the Docker API before binding port 8080.

The supported machine is an Apple Silicon laptop with 8 GiB RAM and a Docker VM around 4 GiB. PostgreSQL, Redis, NATS, the API, the worker, and the dashboard share that budget. Do not add another database or a model provider to make a drill pass.

## What to check when something looks wrong

- API `GET /livez` and `GET /readyz`. Ready fails when the schema version, PostgreSQL, or a required dependency is down. Live stays up.
- `schema_migrations` must match the binary. A newer migration row with an older API image fails readiness.
- Application keys are scoped. Evaluate and event keys cannot manage flags. Proposal keys cannot approve.
- Production flag writes go through proposals. A direct production edit is limited to an emergency reduction.
- Redis may be empty. Snapshots expire. PostgreSQL is the repair source. A snapshot older than 30 seconds is not served as fresh.
- The worker publishes the outbox and reconciles `metric_user_state`. A large `due` count means results are behind, not that events were lost. Accepted events stay in `raw_events`.

## Drills

```sh
make failure-drills
SWITCHYARD_DEMO_PASSWORD='switchyard-demo-only' make agent-drill
```

`failure-drills` covers cache loss, event redelivery, and rollout safety. `agent-drill` submits a mock proposal, rejects a same-person approval, and applies once with a second person. `make observability-up` starts the local collector, Tempo, Prometheus, and Grafana. `make observability-smoke` checks that a short evaluation and event burst is visible. `make observability-down` turns export off again.

pprof listens only when `ENABLE_PPROF=true`, on the private metrics ports. Leave it off for a measurement that should match the default process.

## Load

`make load-evaluation`, `make load-events`, `make load-mixed`, and `make load-soak` write reports under `docs/benchmarks/`. A failed latency or freshness target stays in the report. The 10-minute soak was not run after the 120-second post-index event offer already failed most batches on this machine. Do not lower the target to manufacture a pass.

## Recovery

Rebuild `switchyard-api:local` after a worker or API code change, then recreate the api and worker services so they share that image. Migrations run inside one transaction. `CREATE INDEX CONCURRENTLY` does not belong in that path.

If the API restarts under load, wait until ready returns 200 before reading a load report. A connection refused during the results poll is a failed measurement, not a missing report: the harness records the error and still writes the JSON it has.
