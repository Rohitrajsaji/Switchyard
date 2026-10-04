# Benchmark report

## M3 local evaluator baseline — 5 October 2026

Actual host: Apple M1, darwin/arm64; Go 1.27.1. Command: `make benchmark-evaluator` (three runs with allocation reporting).

Fixture: one boolean flag with a 10% standalone rollout; fixed user `user-123`, no attributes. Compilation is outside the measured loop. This measures the pure local evaluator only: no authentication, HTTP, PostgreSQL, Redis, events, dashboard or telemetry.

Initial observed runs before the final M3 integration changes:

| Run | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| 1 | 203.2 | 40 | 2 |
| 2 | 201.2 | 40 | 2 |
| 3 | 200.8 | 40 | 2 |

These are baseline observations, not end-to-end latency percentiles or sustained API throughput. Updated runs and full workloads will be recorded when available. M10 still requires realistic evaluation, ingestion, mixed, saturation, fault and soak workloads, resource measurements and raw output. No 10,000 requests/sec or 1,000 events/sec result has been established.

After adding integration endpoints (same pure workload), another three runs measured 207.8, 203.9 and 205.8 ns/op, each 40 B/op and 2 allocations/op. Small variation is reported rather than selecting the fastest result. Final M3 correctness changes will be followed by an updated baseline before any release performance claim.

Final M3 source baseline: 205.8, 200.7 and 200.2 ns/op; each 40 B/op and 2 allocations/op. [Raw command output](benchmarks/m3-evaluator.txt) is retained. The measured evaluator path is unchanged by later test-only additions. Docker core services were running and other verification activity may have influenced these exploratory measurements; this remains a single-host microbenchmark, not an SLO or a production capacity result. M10 will record controlled workloads and resource measurements.
