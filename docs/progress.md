# Milestone verification ledger

The approved M1–M12 plan is authoritative. A row is complete only after its gates have actual verification evidence. No benchmark result is inferred from a target.

| Milestone | Status | Evidence |
|---|---|---|
| M1 Foundation | Complete | 5 Oct 2026: `make check race build`, ARM64 Docker build/start, Compose validation, `make smoke foundation-drill`, and real PostgreSQL 17.11 integration tests passed. |
| M2 Identity, scope, audit | Complete | 5 Oct 2026: `make check race build`, race-enabled PostgreSQL integration, rebuilt Docker API, live management smoke and seed idempotence passed. |
| M3 Flags and evaluator | In progress | Starting with the pure evaluator and golden/property tests. |
| M4 Experiments and measurement | Pending | — |
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
