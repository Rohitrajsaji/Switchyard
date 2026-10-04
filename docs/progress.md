# Milestone verification ledger

The approved M1–M12 plan is authoritative. A row is complete only after its gates have actual verification evidence. No benchmark result is inferred from a target.

| Milestone | Status | Evidence |
|---|---|---|
| M1 Foundation | Complete | 5 Oct 2026: `make check race build`, ARM64 Docker build/start, Compose validation, `make smoke foundation-drill`, and real PostgreSQL 17.11 integration tests passed. |
| M2 Identity, scope, audit | Complete | 5 Oct 2026: `make check race build`, race-enabled PostgreSQL integration, rebuilt Docker API, live management smoke and seed idempotence passed. |
| M3 Flags and evaluator | Complete | 5 Oct 2026: unit/property/golden/fuzz/race checks, PostgreSQL revision integration, final Docker HTTP flag journey, OpenAPI reference checks and recorded local baseline passed. |
| M4 Experiments and measurement | Complete | 5 Oct 2026: lifecycle/ingestion/SQL attribution/statistics gates, race-enabled PostgreSQL fixtures, final Docker measurement journey and API contracts passed. |
| M5 Dashboard / MVP | In progress | Dashboard/experiment/listing browser journeys verified; seed dataset and clean Docker gates pending. |
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

M5: reproducible marketplace seed dataset, Docker dashboard and fresh-volume setup gates before the MVP tag. No V2 milestones are complete yet.

## M5 verified checkpoints

- Added pinned Next.js 16.3.8 / React 19.3.0 / TypeScript 7.0.2 dashboard, with host Node 22.23.2. Login/session reload/logout and project/environment navigation call the existing Go API through a restricted same-origin proxy. Go remains authoritative for all roles, membership, CSRF, validation and revisions.
- Proxy unit tests cover cross-site/missing Origin denial, path/method allowlisting, exact body relay, credential isolation, body bounds, redirect denial and backend outage. Dashboard sessions never use application bearer credentials; unrelated browser cookies/role headers are stripped.
- Boolean/JSON flag creation/editing, ordered targeting and gradual-rollout JSON fields, evaluation preview, immediate kill, audit pagination and visible production/viewer read-only states are implemented. Lossless JSON parsing/serialization preserves decimal tokens in values/rule operands; unsafe integer response metadata fails explicitly rather than rounding.
- Browser Chromium journey passed against the real Docker Go API: seeded admin login, project creation, targeted flag, kill-safe preview, high-precision JSON create/edit/preview, explicit JSON null safe-value preservation, audit reasons, production write-control absence, reload and logout. A separate viewer project fixture proves member flag/preview/audit reads, absence of write controls and HTTP 403 for a forged-role write. Results screens and the full M5 experiment journey remain pending.
- Source formatting, TypeScript, six proxy/JSON unit tests and production Next build are checked by `make web-check`; browser tests use `make e2e`. `make check api-check` also passed. No dashboard Docker container, fresh-volume MVP rehearsal, complete M5 gate or release tag is claimed at this checkpoint. GitHub frontend CI is defined but has not run remotely.
- Experiment/demo checkpoint: A/B creation supports unequal control allocation and explicit eligible traffic, converts percentages to exact basis points, and sends the current `configuration_revision` for start/pause/resume/complete. Boolean/JSON treatment values remain precise; multiple variants are displayed when returned by Go, while creating more than two from the dashboard remains the planned M9 addition.
- Results fetch Go snapshots every five seconds without overlapping requests. Cohort counts/rates, Wilson intervals, control comparisons, SRM status, measurement quality and product-request metrics are shown. Empty rates/p-values remain unavailable, and finalized cohorts are distinct from provisional measurements. A stale read error retains and labels the last successful snapshot.
- The marketplace renders classic two-step and simplified one-step forms. An effect submits an explicit exposure after DOM commit; only a randomized decision for the selected run is measured. Outcome/completion events copy that decision context and reuse stable IDs/timestamps across retries. A small synthetic Next submission endpoint validates at most 4 KiB, stores no real listing, and gives an actual HTTP duration for product-request metrics.
- Demo keys have only evaluate/events-write permissions and stay in browser memory. Normal tab/project/environment navigation and logout await revocation; browser exit/reload cleanup is best effort. The browser is a trusted local operator demo, not a public production SDK or a claim of durable offline client delivery. PostgreSQL still guarantees durability once Go acknowledges a batch.
- Chromium passed all three current browser journeys against the actual Go API, including an 80%-traffic, 30/70 A/B run; both listing flows; lost acknowledgement after commit then duplicate receipts; exact exposure counts and one completion/request per variant; finalized zero for fresh users; revoked-key denial; lifecycle audit; viewer results and HTTP 403 for viewer transitions. Final `make web-check` passed formatting, type generation/TypeScript, ten frontend unit tests and the production build. Final `make e2e` passed all three journeys after the cleanup/context changes; the full M5 Docker/seed gate remains pending.

## M4 verified evidence

- Added experiment runs with draft/running/paused/completed lifecycle, immutable population/treatment snapshots, generated independent salts and an explicit control variant. Draft and paused runs reserve their flag/environment; completed runs retain historical definitions.
- Lifecycle and ordinary flag mutation share the same PostgreSQL flag-row lock. Transitions compare revisions, create a flag revision and append audit in one transaction. Failed audit insertion rolls everything back.
- Reserved flags reject population edits while allowing an emergency kill with all other configuration unchanged. A killed run cannot start/resume. Production remains read-only pending M9.
- Unit lifecycle tests and real PostgreSQL tests cover concurrent starts, role/scope checks, freezing, audit atomicity, pause/resume assignment stability, historical definitions and successive runs. `make check` and full race-enabled integration passed for the initial lifecycle checkpoint; the strengthened active-run kill regression is also verified before commit.
- HTTP lifecycle checkpoint: create/list/get/transition endpoints share the existing human session and CSRF boundary. Responses separate frozen `definition.revision` from the current `configuration_revision`; historical runs remain readable by viewers. Invalid control IDs, production writes, unknown treatment fields, application-key management and stale transitions are rejected.
- `make check api-check` and full race-enabled PostgreSQL integration passed for the HTTP checkpoint. Docker rebuilt and applied migration 0004; the live lifecycle checks unequal 30/70 Python buckets, targeting exclusion, pause/resume stability and history before commit.
- A new large-JSON regression first failed: a valid run could not be killed because full-definition comparison incorrectly applied a single-value size cap. Configuration equality now uses validated PostgreSQL jsonb equality; the regression is included in the final verification.
- Event ingestion checkpoint: exposure/listing_completion/request_outcome schemas, at most 100 events per batch, scoped `events:write` authorization inside the transaction, immutable raw facts, ordered durable receipts and project/environment event-ID uniqueness. Equivalent timestamp zones/object order/numeric spellings are duplicates; altered identities conflict and roll back the batch.
- Reported historical flag revision validates run assignment after pause/completion. Wrong variants, targeting/nonrandomized flows, unknown revisions, events older than 24 hours and events more than five minutes ahead are quarantined. Unknown runs/malformed schemas reject the batch. Completion/request facts can arrive before exposure; business attribution is still pending.
- `make check`, OpenAPI reference checks and full race-enabled PostgreSQL integration passed. Tests cover concurrent reversed batches, identical/different payload retries, historical receipt replay, wrong permissions/scope/revocation, timezone/numeric equivalence, quarantine boundaries, insert failure and an actual deferred COMMIT failure with no acknowledgement/partial facts.
- Docker rebuilt and applied migration 0005. Live `make events-smoke experiments-smoke smoke` verified all three fact types, retry receipts, atomic conflict, quarantine, scope and lifecycle. No event throughput or aggregate correctness is claimed yet.
- SQL attribution uses a repeatable-read snapshot and set-based joins. Earliest valid event-time exposure anchors each user/run, with event-ID byte order breaking ties. Outcome references must match a valid exposure's run/user/variant and temporal order. Existence of an attributed completion deduplicates the business numerator; a late earlier exposure can subtract a previously counted conversion.
- PostgreSQL fixtures verify missing exposure then reconciliation, both earlier-exposure arrival orders, new-ID completion deduplication, exact thirty-minute bounds, invalid references, future facts, targeting/quarantine exclusion and provisional/final classification at the inclusive late deadline. Successor runs/changed flag revisions cannot move historical results.
- Wilson 95% intervals, absolute/relative lift, pooled two-proportion tests, insufficient-data states and multi-variant/unequal-allocation SRM checks are implemented. Tests match NIST examples/critical values and independently bisected score-test roots. Endpoint coverage at zero/100% conversion was corrected after a failing floating-point regression. A/A fixtures return zero lift/p=1; severe sample-ratio mismatch suppresses inference. No sequential/multiple-comparison validity is claimed.
- Product-request samples require valid exposure references and return error rate, fixed-bin histograms and an explicitly approximate p95 upper bound. Empty rates/percentiles remain null. These do not represent platform API latency.
- Final source passed `make check race api-check`, `make integration`, and race-enabled measurement integration after the set-based SQL refinement. Full race-enabled PostgreSQL integration also passed for the results API; HTTP tests verify scoped human/viewer reads, app-key denial, no-store responses and exact counts.
- Final Docker rebuild applied migration 0006 and passed `make measurement-smoke smoke`. The live journey verifies pending outcomes become one conversion, extra completion IDs cannot inflate counts, histograms/intervals are returned, quarantine is excluded and a fresh cohort remains provisional. No load target or V2 aggregation/retention behavior is claimed.
