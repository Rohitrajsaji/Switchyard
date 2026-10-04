# Local development

Run `make up` then `make smoke`. PostgreSQL binds only to localhost:54329 and the API to localhost:8080. PostgreSQL uses 17.11, the current PostgreSQL 17 minor release checked against the [official version policy](https://www.postgresql.org/support/versioning/) during setup. Go and distroless images are additionally pinned to manifest digests observed in the verified build. Runtime runs as non-root. No credentials, cached dependencies or generated binaries are tracked.

The Go module pins the installed toolchain (1.27.1) and pgx 5.11.0. `make` uses ignored project-local Go build/module caches. For direct `go` commands set `GOCACHE="$PWD/.cache/go-build"` and `GOMODCACHE="$PWD/.cache/go-mod"` where the filesystem restricts global caches.

Create the test database after starting PostgreSQL:

```sh
docker compose exec -T postgres createdb -U switchyard switchyard_test
```

If it already exists, use it without recreating it. Integration tests require a database name ending in `_test`, create a unique schema, set the pool's search path to it, and delete only that schema at cleanup. They never drop the development database.

`/health/live` checks process responsiveness; `/health/ready` checks PostgreSQL and the foundation migration. API starts even during a database outage and returns ready=503 until repaired. Readiness does not expose connection details. Request logs include method, route template, status, duration and a bounded request ID; no URLs, request bodies or database credentials.

Read-header/read/write/idle timeouts and body/header limits bound HTTP resource use. SIGTERM/SIGINT drains requests for up to ten seconds before force-close, then closes the pool. Compose normal shutdown preserves named volumes.

The dashboard, Redis, NATS, SDKs and telemetry containers arrive only at their approved milestones.

## M2 accounts and management API

Explicitly create local demo accounts after migrations:

```sh
SWITCHYARD_DEMO_PASSWORD='switchyard-demo-only' make seed
```

This local-only example creates `admin@example.test`, `reviewer@example.test`, `developer@example.test` and `viewer@example.test`. Re-running seed does not change existing credentials or duplicate creation audit entries. Login sends `POST /v1/session` with JSON email/password and `Origin: http://localhost:3000`, matching `SWITCHYARD_ORIGIN`. Keep the Set-Cookie cookie and returned `csrf_token`; mutations also send `X-CSRF-Token`. The current-session GET reissues that token after reload. No account is created unless seed is explicitly enabled.

Management endpoints and permission details are in [OpenAPI](../api/openapi.yaml). `make seed` does not automatically grant project memberships: the admin creates a project and grants others membership explicitly. Each project automatically gets development/staging/production. Production configuration is read-only until M9. Application credentials do not grant management access.

Cookie Secure is disabled only for local HTTP. A hosted deployment would need HTTPS, `COOKIE_SECURE=true`, an exact trusted origin, non-demo credentials and a separately approved deployment plan.

## M3 flags and evaluation

Management flag endpoints use the same cookie/Origin/CSRF boundary. Create a flag with `environment_id`, `key`, `type` (`boolean` or `json`), explicit typed `default`/`safe`, optional ordered rules/rollout, and an audit `reason`. An update provides `expected_revision` and the full replacement configuration. Another environment starts at expected revision zero; stale revisions return 409. Rollout percentages are integer basis points out of 10,000, and the server fixes the salt across revisions.

Example create payload:

```json
{
  "environment_id": "<development environment ID>",
  "key": "new_listing",
  "type": "boolean",
  "default": {"type": "boolean", "data": false},
  "safe": {"type": "boolean", "data": false},
  "rollout": {"traffic_bp": 1000, "value": {"type": "boolean", "data": true}},
  "reason": "Try simplified listing flow in development"
}
```

Applications call `POST /v1/evaluate` with a scoped bearer key and `project_id`, `environment_id`, `key`, `user_id`, optional primitive/custom attributes and a typed `fallback` matching the flag's safe value. The returned decision includes value/reason/revision/decision ID; it does not record exposure. Human preview is a read-only POST under the flag path. See OpenAPI for full schemas and the deterministic-evaluation ADR for bucketing and JSON number bounds.

`SWITCHYARD_DEMO_PASSWORD='switchyard-demo-only' make flags-smoke` runs a real HTTP journey and independently calculates Go's rollout buckets in Python. `make fuzz-smoke` checks malformed definitions and a valid seeded definition. `make benchmark-evaluator` measures the pure local evaluator only; recorded results are in the benchmark report and raw output file.

## M4 experiment lifecycle

Create a baseline flag without a standalone rollout, then `POST /v1/projects/{project}/experiments` with its environment/key/current revision, experiment name, control variant ID, traffic basis points, immutable variants and an audit reason. Each variant has a unique ID/ordinal, positive weight and a value matching the flag type; weights total 10,000. Two through ten variants and unequal weights are supported. Explicit targeting rules still take priority over randomized assignment. A draft reserves its flag/environment without changing evaluation; complete the draft to release the reservation if abandoning it.

`GET /v1/projects/{project}/experiments?environment_id=...` lists at most 100 recent runs. `GET /v1/projects/{project}/experiments/{run}` returns its immutable definition and `configuration_revision`, the current flag revision. Send that current revision as `expected_revision` to `POST .../{run}/transitions` with `action` (`start`, `pause`, `complete`) and `reason`. Use `start` again to resume a paused run. Start/resume attaches the experiment; pause/complete returns evaluation to the configured default or explicit targeting. Completed runs retain history and cannot restart.

Draft/running/paused runs freeze ordinary configuration changes. An emergency flag update may set `killed=true` with every other field unchanged, returning the existing safe value ahead of targeting/experiment. Starting/resuming a killed flag fails. Production remains read-only; application keys cannot manage runs. Session mutation rules and revision conflicts are the same as for flags.

`SWITCHYARD_DEMO_PASSWORD='switchyard-demo-only' make experiments-smoke` runs the lifecycle against Docker, independently checks 30/70 variant assignment in Python, verifies targeting exclusion and confirms pause/resume preserves assignment. Evaluation alone still records no exposure; use explicit event ingestion below. Explicit events and measured results are described below.

### Explicit measurement events

Use a scoped application key with `events:write` to call `POST /v1/events`. Send `project_id`, `environment_id` and up to 100 `events`. Exposure includes a stable `event_id`, `kind: exposure`, `run_id`, synthetic `user_id`, decision `variant_id`/`revision`/`decision_id`/`decision_reason`, event-time `occurred_at` and any non-sensitive evaluation attributes. Emit it only after rendering the selected flow.

Completion uses `kind: listing_completion`, its own stable event ID, the same run/user/variant/revision/reason/attributes and `exposure_id`, omitting `decision_id`. Request outcome uses `kind: request_outcome` with the same context and explicit `is_error` plus `latency_ms` (0–60,000). These represent product listing operations, not Switchyard's API performance. Keep stable IDs across retries.

The response contains ordered `receipts` with `accepted` or `quarantined`, a quarantine `reason` when applicable and a `duplicate` boolean. The response is returned after transaction commit. An altered payload under an existing ID returns 409 and rolls back new batch facts. Equivalent timestamps/JSON numeric spellings preserve identity. Structurally invalid events reject the batch; wrong assignments, nonrandomized flows, timestamps older than 24 hours or more than five minutes ahead are quarantined. Outcomes can be accepted before their exposure; accepted storage does not imply a counted conversion. SQL measurement reconciles them as described below.

`SWITCHYARD_DEMO_PASSWORD='switchyard-demo-only' make events-smoke` exercises three event types, retries, atomic identity conflict, quarantine and scope against the local Docker API. Full schemas and limits are in OpenAPI; transaction and retention boundaries are in ADR 0004.

### Experiment results

Human project members call `GET /v1/projects/{project}/experiments/{run}/results`. Every variant returns disjoint `provisional` and `finalized` conversion counts, their `total`, rates and Wilson 95% intervals. Exposures mature only after thirty minutes plus twenty-four hours; a fresh local demo will show provisional counts. Automated tests advance an injected clock to verify finalization without a day-long sleep. New listing-completion IDs cannot increase a user's numerator, and a late earlier exposure can remove a previously attributed conversion.

`quality` reports quarantined/future facts, pending outcomes, invalid references, completions outside attribution and extra completion facts already deduplicated. `sample_ratio` compares exposed-user counts to weights. Pairwise `comparisons` return lift and a two-sided p-value only with supported samples and no SRM warning. Null means unavailable; it is never a fabricated zero. See ADR 0005 for interpretation and numerical references.

Request metrics count listing operations linked to valid exposures. Histograms contain noncumulative bin counts; `p95_upper_bound_ms` is an approximate upper bound. They do not measure the platform API. MVP reads raw SQL; durable aggregates/retention and automatic guardrails arrive at their V2 milestones.

`SWITCHYARD_DEMO_PASSWORD='switchyard-demo-only' make measurement-smoke` verifies pending outcomes become counted after exposure, extra completion IDs remain one conversion, request histograms/intervals are returned, and a fresh demo stays provisional. Finalization, A/A and SRM fixtures run in PostgreSQL integration tests with an injected clock.

## M5 dashboard development (milestone in progress)

The dashboard currently supports login, project/environment selection, boolean/JSON flags, targeting/rollout editing, preview, kill switch and audit history. Experiment/results screens and the marketplace demo are the next checkpoint. Docker still starts the Go API and PostgreSQL only; the dashboard runs on the host during this checkpoint.

Use Node 22.23.2 (the pinned CI version), with the API running and demo accounts explicitly seeded:

```sh
make web-install
make web-dev
```

Open `http://localhost:3000` and sign in with a seeded account. Use localhost rather than the numeric loopback address: browser mutation Origin must match Go's `SWITCHYARD_ORIGIN` exactly. `SWITCHYARD_API_URL` defaults to `http://127.0.0.1:8080` on the Next server; it is never a browser-supplied upstream URL. Neither setting is a public Next environment variable.

The proxy forwards the existing HttpOnly session cookie and real Origin/CSRF headers. It performs transport restrictions, not domain authorization. Go's per-peer limits currently see the Next process as one peer; the small local demo shares that login/request allowance. A future approved hosted setup would need a deliberate trusted-proxy/client-identity design.

Flag updates send the displayed revision; a stale update returns a conflict and requires reloading. Targeting and rollout editors use explicit JSON examples. Their validation and the experiment freeze rules are enforced in Go. JSON values/target operands retain decimal tokens using the pinned `lossless-json` parser; native browser number conversion cannot silently change a safe value. Preview returns the Go decision and records no exposure. Production configuration controls remain hidden until M9.

```sh
make web-check
cd web
PLAYWRIGHT_BROWSERS_PATH='../.cache/playwright' npx playwright install chromium
cd ..
SWITCHYARD_DEMO_PASSWORD='switchyard-demo-only' make e2e
```

Browser tests start a host Next server if needed, use existing seeded users and create unique projects in the local database. They preserve recorded audit history. Test reports, browser binaries and screenshots stay ignored. `make e2e` is still a partial-M5 journey until the experiment and listing demo gates are added.
