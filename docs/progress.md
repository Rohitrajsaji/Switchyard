# Milestone verification ledger

The approved M1–M12 plan is authoritative. A row is complete only after its gates have actual verification evidence. No benchmark result is inferred from a target.

| Milestone | Status | Evidence |
|---|---|---|
| M1 Foundation | Complete | 5 Oct 2026: `make check race build`, ARM64 Docker build/start, Compose validation, `make smoke foundation-drill`, and real PostgreSQL 17.11 integration tests passed. |
| M2 Identity, scope, audit | Complete | 5 Oct 2026: `make check race build`, race-enabled PostgreSQL integration, rebuilt Docker API, live management smoke and seed idempotence passed. |
| M3 Flags and evaluator | Complete | 5 Oct 2026: unit/property/golden/fuzz/race checks, PostgreSQL revision integration, final Docker HTTP flag journey, OpenAPI reference checks and recorded local baseline passed. |
| M4 Experiments and measurement | In progress | Run lifecycle, HTTP contracts and configuration freezing implemented and tested; events and results remain pending. |
| M5 Dashboard / MVP | Pending | — |
| M6 Redis snapshots | Pending | — |
| M7 Durable worker | Pending | — |
| M8 gRPC / Go SDK | Pending | — |
| M9 Rollout / approval / safety | Pending | — |
| M10 Telemetry / performance | Pending | — |
| M11 Governed agent | Pending | — |
| M12 Portfolio-ready V2 | Pending | — |

Approval authorizes local implementation/testing/commits only. No push, publication, paid service or external account changes are authorized.

## M1 verified evidence

- Actual toolchain: Go 1.27.1 darwin/arm64; Docker engine 28.3.3.
- Unit tests verify config rejection, health method behavior, sanitized request IDs and independent liveness/readiness.
- Integration tests use isolated `_test` schemas: fresh migrations, eight concurrent migration calls, idempotence, edited-checksum rejection and atomic rollback on bad SQL all passed.
- Pinned Go/distroless images built successfully on ARM64. PostgreSQL upgraded to the current supported 17-series minor, 17.11, before final verification.
- Real HTTP smoke returned live/ready=200. Stopping PostgreSQL produced ready=503/live=200; restarting restored ready=200.
- API SIGTERM produced `api shutdown complete` and container exit code 0; restart returned ready=200.
- `.env` and `.cache` are ignored. `git diff --check` passed. GitHub Actions is defined but has not run remotely; no repository has been published.

## M2 verified evidence

- Real PostgreSQL integration tests passed with `go test -race -tags=integration -count=1 ./...`.
- Forged caller roles, viewer writes, nonmember/cross-project access and production writes are denied. Database roles/memberships are authoritative.
- Password verification, hashed session/CSRF storage, session reload, expiry/logout, scoped key permissions/environment, revocation and app-key/human separation are tested.
- HTTP tests verify exact Origin, session-bound CSRF, HttpOnly/SameSite cookie settings, unknown/trailing/null/oversized JSON rejection and secret-free structured logs. Concurrent limiter tests verify the configured admission cap and expiry without sleeps.
- Audit insertion shares the domain transaction; both write failures and audit failures roll back. UPDATE/DELETE/TRUNCATE are rejected by append-only triggers.
- Rebuilt local Docker API and current migrations passed real HTTP smoke. The management journey used admin/developer/viewer, granted memberships, denied production key creation, created/revoked a development key, checked audit entries and logged out.
- Opt-in seed ran twice: authoritative database counts remained four users and four demo-creation audits. Running seed without opt-in correctly failed.
- OpenAPI contracts, local bootstrap instructions and identity/audit ADR are saved. No remote CI run or external publication is claimed.

## M3 verified evidence

- Independent Python golden buckets: eligibility 8811, variant 3818 for the published demo tuple. Unit/property tests exercise 10,000 synthetic users, growth/shrink membership, stable variants across traffic/revision changes, immutable compiled input/results, ordered targeting, invalid weights/variant identity and boolean/JSON safe values.
- A new JSON safety regression failed before its fix and now passes: equivalent decimal spellings compare exactly and huge exponents are rejected. PostgreSQL-normalized definitions are validated before committing.
- Flag creation/update and environment revisions are persisted with audit. Concurrent expected-revision updates yield exactly one success and one conflict; failed audit insertion leaves revision/history unchanged; history rewrite is denied.
- Evaluation scope, missing-flag fallback, unsafe fallback rejection and absence of evaluation audit writes are tested against PostgreSQL. Production writes remain denied.
- Final source passed `make check race`; full integration passed `go test -race -tags=integration -count=1 ./...`. Ten-second fuzz smoke passed with a valid seeded definition and malformed inputs.
- Final Docker build/migrations and `make flags-smoke smoke` passed. The live script independently recomputed all 100 user buckets in Python at 10% and 20% traffic, verified targeting, conflict, boolean/JSON kill safety and revoked-key denial.
- `make api-check` validates YAML, internal references, path parameters and security references; it does not claim full OpenAPI schema conformance testing. Contracts and ADR document actual behavior.
- Exploratory local rollout evaluator baseline: 205.8/200.7/200.2 ns/op, 40 B/op, two allocations/op. Raw output and measurement limits are in `docs/benchmark-report.md` and `docs/benchmarks/m3-evaluator.txt`. Network/load/ingestion results remain unmeasured until M10.

## Next implementation checkpoint

M4: persist A/B experiment runs and immutable variants; then explicit exposure/conversion/request events, duplicate protection, event-time attribution, SQL results, confidence intervals, significance and data-quality checks. Follow M4's gate before building the M5 dashboard. No V2 milestones are complete yet.

## M4 partial checkpoint

- Added experiment runs with draft/running/paused/completed lifecycle, immutable population/treatment snapshots, generated independent salts and an explicit control variant. Draft and paused runs reserve their flag/environment; completed runs retain historical definitions.
- Lifecycle and ordinary flag mutation share the same PostgreSQL flag-row lock. Transitions compare revisions, create a flag revision and append audit in one transaction. Failed audit insertion rolls everything back.
- Reserved flags reject population edits while allowing an emergency kill with all other configuration unchanged. A killed run cannot start/resume. Production remains read-only pending M9.
- Unit lifecycle tests and real PostgreSQL tests cover concurrent starts, role/scope checks, freezing, audit atomicity, pause/resume assignment stability, historical definitions and successive runs. `make check` and full race-enabled integration passed for the initial lifecycle checkpoint; the strengthened active-run kill regression is also verified before commit.
- HTTP lifecycle checkpoint: create/list/get/transition endpoints share the existing human session and CSRF boundary. Responses separate frozen `definition.revision` from the current `configuration_revision`; historical runs remain readable by viewers. Invalid control IDs, production writes, unknown treatment fields, application-key management and stale transitions are rejected.
- `make check api-check` and full race-enabled PostgreSQL integration passed for the HTTP checkpoint. Docker rebuilt and applied migration 0004; the live lifecycle checks unequal 30/70 Python buckets, targeting exclusion, pause/resume stability and history before commit.
- A new large-JSON regression first failed: a valid run could not be killed because full-definition comparison incorrectly applied a single-value size cap. Configuration equality now uses validated PostgreSQL jsonb equality; the regression is included in the final verification.
- Event ingestion, attribution, statistics and M4 completion remain pending. Next: explicit exposure/completion/request schemas, bounded ingestion and payload identity conflicts, then SQL measurement.
