# Local development

Run `make up` then `make smoke`. PostgreSQL binds only to localhost:54329, the API to localhost:8080 and the dashboard to localhost:3000. PostgreSQL uses 17.11, the supported PostgreSQL 17 minor release checked against the [official version policy](https://www.postgresql.org/support/versioning/) during setup. Go, distroless and Node images are additionally pinned to manifest digests observed in the verified build. API and dashboard runtimes run as non-root. No credentials, cached dependencies or generated binaries are tracked.

The Go module pins the installed toolchain (1.27.1) and pgx 5.11.0. `make` uses ignored project-local Go build/module caches. For direct `go` commands set `GOCACHE="$PWD/.cache/go-build"` and `GOMODCACHE="$PWD/.cache/go-mod"` where the filesystem restricts global caches.

Create the test database after starting PostgreSQL:

```sh
docker compose exec -T postgres createdb -U switchyard switchyard_test
```

If it already exists, use it without recreating it. Integration tests require a database name ending in `_test`, create a unique schema, set the pool's search path to it, and delete only that schema at cleanup. They never drop the development database.

`/health/live` checks process responsiveness; `/health/ready` checks PostgreSQL and the foundation migration. API starts even during a database outage and returns ready=503 until repaired. Readiness does not expose connection details. Request logs include method, route template, status, duration and a bounded request ID; no URLs, request bodies or database credentials.

Read-header/read/write/idle timeouts and body/header limits bound HTTP resource use. SIGTERM/SIGINT drains requests for up to ten seconds before force-close, then closes the pool. Compose normal shutdown preserves named volumes.

The MVP runs three services; current M6 setup also starts disposable Redis (128 MiB): PostgreSQL (768 MiB limit), API (256 MiB) and dashboard (384 MiB, with a 256 MiB Node heap limit). These are runtime ceilings, not reserved memory or total Docker Desktop usage. Image builds can use more memory. NATS, SDKs and optional telemetry follow at later V2 milestones.

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

## M5 dashboard and marketplace fixture

The dashboard supports login, project/environment selection, boolean/JSON flags, targeting/rollout editing, preview, kill switch, audit history, A/B lifecycle/results and the marketplace demo. `make up` builds the production Next standalone server and starts all three MVP services. No host Node installation is required to use the Docker dashboard.

Bootstrap the full opt-in fixture through the Go API:

```sh
SWITCHYARD_DEMO_PASSWORD='switchyard-demo-only' make seed-demo
```

This includes the four demo accounts and grants each membership in **Marketplace demo**, which has development/staging/production environments. Development has `listing_flow` with a running 50/50 boolean A/B experiment and `listing_config` with a 10% JSON rollout. Staging has a separate baseline configuration for `listing_flow`; production remains read-only and unconfigured. The fixture imports 100 synthetic exposures and 50 completions at the run start time. Its name, attributes and audit reasons identify it as synthetic. It supplies no fake request-outcome samples or latency. Actual Listing demo submissions add separately measured facts.

Repeated seeding preserves existing flags, run state, events, keys and audit history. Only the explicitly enabled account seed writes user credentials; it does not reset existing passwords. The dataset script uses session/CSRF management endpoints and a short-lived event-write credential which it revokes after importing. Its ignored `.cache/demo-seed-*.json` marker contains IDs only; an exclusive local file lock prevents concurrent bootstraps. Removing the marker allows stable event-ID replay within the 24-hour late-event window; an older incomplete/missing-marker fixture fails rather than fabricating fresh timestamps. Human-paused/completed/killed runs are not restarted or reset. Start with a new, deliberately isolated database if a fixture has become incompatible.

For hot-reload development use Node 22.23.2 (the pinned CI version), with the API running and demo accounts explicitly seeded. Free the dashboard port first:

```sh
docker compose stop web
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

Browser tests start a host Next server if needed, use existing seeded users and create unique projects in the local database. They preserve recorded audit history. Test reports, browser binaries and screenshots stay ignored. To test the running Docker production dashboard explicitly, use `SWITCHYARD_E2E_EXTERNAL=true SWITCHYARD_DEMO_PASSWORD='switchyard-demo-only' make e2e`. `SWITCHYARD_WEB_URL` may select an alternate local dashboard URL; its origin must match both the Go API and Next server configuration.

### Fresh-volume MVP gate

After `make up`, `make web-install` and the Chromium installation above:

```sh
SWITCHYARD_DEMO_PASSWORD='switchyard-demo-only' make mvp-drill
```

The drill creates a unique Compose project on available loopback ports using the already built images, applies all migrations to a fresh volume, runs health checks, seeds twice and compares durable row/audit counts. It runs the browser journeys against the production Docker dashboard, stops/restarts the three services, checks health and verifies unchanged data/results after another bootstrap. It finally removes only its own temporary Compose project/volume and seed marker. The normal `switchyard_postgres_data` volume is preserved. This verifies a fresh database and container runtime; it does not claim a blank host, uncached image build, remote CI or enterprise availability.

Local ports are configurable with `POSTGRES_PORT`, `API_PORT` and `WEB_PORT`. When changing the browser port, set `SWITCHYARD_ORIGIN` to the exact new URL for both servers and `SWITCHYARD_URL` for host smoke/seed scripts. Host Go database settings must also use the changed PostgreSQL port.

### Experiment and listing journey

1. Sign in as a seeded developer/admin, create a project and choose development or staging.
2. Create a boolean flag such as `listing_flow` with default/safe false, no standalone rollout and an audit reason. False renders the classic two-step listing; true renders a simplified single-step listing.
3. Open Experiments, create an A/B draft with control false/treatment true, and choose traffic and control allocation. Traffic is eligibility; allocation divides eligible users. Start the draft with a lifecycle reason. Go freezes the population and treatment values; pause/resume uses the same assignment salts.
4. Open Listing demo, enable its scoped credential, choose the running experiment and render a synthetic user. Outside-traffic/targeted/killed decisions are reported without an experiment exposure. Change the user/attributes to find a randomized participant. JSON variants are supported when their value has `flow: "classic"` or `flow: "simple"`.
5. Wait for explicit exposure acknowledgement, then complete the form. The Next product endpoint acknowledges validated synthetic input without storing a marketplace listing. Request duration is measured around that actual HTTP submission, including response parsing; it is separate from Switchyard platform API latency.
6. A lost measurement acknowledgement exposes Retry measurement delivery. It resends identical event IDs/timestamps/context, rather than redoing the listing operation. Until the acknowledgement arrives, the interface says delivery is pending. Browser memory does not provide offline persistence; reload can lose unacknowledged client batches.
7. Open Experiments for provisional counts, intervals, quality and product-request metrics. Reads refresh every five seconds. Fresh users have zero finalized exposure until the 30-minute attribution window plus 24-hour lateness allowance expires. Empty/insufficient-data statistics stay unavailable; descriptive p-values are not an automatic stopping rule.
8. Inspect Audit, pause/resume/complete the run with reasons, or use the flag kill switch. The demo key is revoked before ordinary navigation, project/environment changes, or logout. It is never placed in local storage, URLs or rendered output. Browser close/reload revocation is best effort; an interrupted cleanup can leave an application key active. An admin can identify its key ID in creation audit details and revoke it through the existing application-key API. This is a local trusted-operator sample; a public integration uses server-held credentials.

Viewers can inspect flags, previews, runs and results but cannot create runs, transition them or enable a demo key. Production configuration remains read-only until M9. All authorization, experiment policy, historical assignment and event receipt decisions come from Go.

## M6 cached evaluation

`make up` enables the Redis `cache` profile at localhost:63799. Redis 8.10.2 runs non-root with a 128 MiB container ceiling, a 96 MiB eviction budget and no persistence. PostgreSQL owns configuration and authorization. API readiness does not depend on Redis.

```sh
TEST_REDIS_URL='redis://127.0.0.1:63799/0' make cache-storage-check
TEST_DATABASE_URL='postgres://switchyard:switchyard-local-only@127.0.0.1:54329/switchyard_test?sslmode=disable' TEST_REDIS_URL='redis://127.0.0.1:63799/0' TEST_NATS_URL='nats://127.0.0.1:42229' make integration
SWITCHYARD_DEMO_PASSWORD='switchyard-demo-only' make cache-drill
```

Tests use their own Redis keys and isolated PostgreSQL schemas; they never flush Redis. Run the drill separately from integration tests because it temporarily stops Redis and locks development configuration reads. It creates a temporary observer API, preserves its project/audits, revokes its key, restores Redis/releases the lock and stops only its observer. Its ignored `.cache/cache-drill-report.json` records timings and sampled counters.

Application evaluations share immutable snapshots, refresh active flags every two seconds and expire thirty seconds after the authoritative read began. Redis relay never renews age. Configuration expiry returns the safe fallback with HTTP 503; callers must handle that response. Per-request key checks remain in PostgreSQL, so complete database outage prevents authorization. Dashboard previews read PostgreSQL directly.

For host development set `REDIS_URL=redis://127.0.0.1:63799/0`; omit it for bounded PostgreSQL-only loading. `CACHE_ENABLED=false` restores direct PostgreSQL evaluation for diagnosis. See [ADR 0006](adr/0006-configuration-snapshots.md). Counters are logged every thirty seconds; metrics export follows in M10.

## M7 durable publication checkpoint

After `make up` has built the current Go image, explicitly start the optional asynchronous services:

```sh
make async-up
TEST_NATS_URL='nats://127.0.0.1:42229' make messaging-check
SWITCHYARD_DEMO_PASSWORD='switchyard-demo-only' make publication-drill
```

NATS binds to localhost:42229; its read-only monitoring endpoint binds to localhost:18229. The pinned NATS 2.15.0 Alpine image runs as UID 1000 with a 256 MiB limit. A one-shot initializer sets ownership only on the named `nats_data` volume. JetStream uses file storage, a 192 MiB server disk ceiling, and a 128 MiB/200,000-message work stream. New publication fails when capacity is reached; PostgreSQL keeps its publication intent. The local server has one replica and is not an HA deployment. Both database and NATS volumes survive `make down`.

The Go worker currently publishes references only; it does not acknowledge consumer work or change results. Messages remain in JetStream until the M7 processor is implemented. Do not purge the stream to hide the resulting backlog. This checkpoint intentionally keeps metrics on the existing raw SQL path. Admission/replay/retention and aggregate parity remain M7 work.

Run the publication drill separately from integration tests: it stops/restarts NATS and the worker, records a new project/configuration fixture, and restores services on exit. It verifies broker acknowledgements, durable pending intent and retained messages across restart; it is not the final consumer/event-failure drill. The complete integration command now requires all three explicit test URLs; messaging tests use small isolated streams and delete only their own stream.
