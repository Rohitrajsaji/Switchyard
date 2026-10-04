# Milestone verification ledger

The approved M1–M12 plan is authoritative. A row is complete only after its gates have actual verification evidence. No benchmark result is inferred from a target.

| Milestone | Status | Evidence |
|---|---|---|
| M1 Foundation | Complete | 5 Oct 2026: `make check race build`, ARM64 Docker build/start, Compose validation, `make smoke foundation-drill`, and real PostgreSQL 17.11 integration tests passed. |
| M2 Identity, scope, audit | In progress | Starting from verified M1. |
| M3 Flags and evaluator | Pending | — |
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
