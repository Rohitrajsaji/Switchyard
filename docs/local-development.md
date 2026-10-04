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

`SWITCHYARD_DEMO_PASSWORD='switchyard-demo-only' make experiments-smoke` runs the lifecycle against Docker, independently checks 30/70 variant assignment in Python, verifies targeting exclusion and confirms pause/resume preserves assignment. Event ingestion and measured results are the remaining M4 work; evaluation alone still records no exposure.
