# ADR 0010: Serve durable aggregates with explicit processing backlog

Status: accepted for the M7 read transition.

## Context

Raw/materialized parity, retained replay, folding/expiry and actual failure drills passed before switching reads. The first 1,000-events/sec trial accepted 29,997 facts, but its approximate database freshness p95 was 177.5 seconds and its 180-second post-response drain timed out. Eventual counts and parity passed separately. Acceptance throughput is not fresh-metrics throughput.

## Decision

HTTP results use materialized counters, with pending event receipts, currently due user updates, oldest pending work age and latest reconciliation in the same repeatable-read database snapshot. The dashboard displays backlog separately from counts. Read time and latest reconciliation are not completeness watermarks. Future scheduled maturation is not currently due work; late events can still correct a run after a zero-backlog snapshot.

Missing or failed aggregate reads return an error rather than silently reading raw facts. The independent raw SQL oracle remains a local parity/diagnostic tool. After raw folding, raw-only serving would lose historical measurements. Bounded cleanup is enabled in the normal Docker worker, with explicit configurable opt-out; direct host worker configuration remains opt-in. Normal local startup includes NATS and the worker. Publisher-only diagnostics disable retention as well as processing.

## Consequences

Results are eventually consistent even if ingestion returns 200. Worker outages preserve committed facts and expose unfinished work. Clients and seed/smoke scripts wait for convergence where their assertions require it. Retained-history tests verify serving after raw deletion. Summary expiry deliberately removes measurements outside the configured reporting window.

The missed target is published before performance changes. M10 will profile and measure controlled workloads before adjusting query plans or bounded concurrency; this result does not justify more microservices. Backlog fields are operational observations, not statistical validity or a guarantee that all future/late events have arrived. Error/latency guardrails at M9 must check freshness before making decisions.
