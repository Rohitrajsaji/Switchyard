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

## M6 evaluator and cache failure drill — 5 October 2026

The same compiled rollout microbenchmark on Go 1.27.1 / Apple M1 observed 204.7, 200.0 and 204.5 ns/op, each 40 B/op and two allocations/op. A later gate run with concurrent integration/browser checks observed 237.6, 219.6 and 209.2 ns/op at the same allocations. Both sets are retained in [raw output](benchmarks/m6-evaluator.txt); concurrent activity makes these exploratory observations unsuitable for a capacity comparison. They still exclude cache lookup, authorization, HTTP and Redis.

The [Docker cache drill](benchmarks/m6-cache-drill.json) observed independent API disable propagation in 1.9616 s and PostgreSQL refresh during Redis outage in 1.8988 s. Configuration reads were then locked while application-key checks stayed available. Last-known-good evaluations ended and safe fallback appeared 28.6816 s after the lock; proof age started before the lock. Deletion/restart repair, recovery and revocation passed. Counters are a sampled thirty-second log, not final cumulative totals. These are single-fixture correctness/timing observations, not percentiles, load measurements or proof of high availability. M10 load targets remain pending.

## M7 first ingestion trial — 5 October 2026

Command: `SWITCHYARD_DEMO_PASSWORD='switchyard-demo-only' make ingestion-trial`. Apple M1 / ARM64, eight logical CPUs, 8 GiB host RAM, Docker 28.3.3. Core PostgreSQL/API/web/Redis/NATS/worker containers were running, without observability or concurrent test/build commands. This is one thirty-second burst, not a sustained-capacity or soak result. The generator and services share the host.

The bounded open-loop generator offered 303 batches of 99 events at a requested 1,000 events/sec, with sixteen request threads and at most 32 outstanding batches. Each of 9,999 synthetic users sends an exposure, a completion and a 200 ms request outcome; every hundredth request is an error. Completion is intentionally 100% to verify counts, not evidence of product uplift. Assignment uses the frozen allocation's independent length-prefixed SHA-256 contract, checked against ten live evaluations. Synthetic decision IDs meet the audit-identifier contract; the backend still independently validates every assignment. Payloads are prepared before timing; exposure/outcome times precede ingestion by two/one minutes.

| Measurement | Observed |
|---|---:|
| Offered / submitted / accepted facts | 29,997 / 29,997 / 29,997 |
| Skipped batches / HTTP failures / quarantines | 0 / 0 / 0 |
| Accepted per 30-second offering window | 999.9 events/sec |
| Accepted divided by last-response elapsed time | 994.60 events/sec |
| HTTP batch p95 latency | 235.44 ms |
| Generator scheduling lateness p95 | 10.12 ms |
| Per-user database freshness p50 / p95 / max | 62.27 / 177.50 / 192.31 seconds |
| Sampled peak unpublished intents / due users | 17,903 / 5,957 |

**The five-second freshness target failed.** The worker also failed the harness's 180-second post-response drain limit (248 users remained in the final direct drain check). The [original timeout report](benchmarks/m7-ingestion-trial-timeout.json) is preserved. A separate follow-up observation, without resubmitting load, verified zero due users, all 29,999 intents published/received (including two configuration intents), exactly 9,999 exposures/conversions/requests, unchanged counts after a three-event duplicate retry, and full raw/materialized parity across 33 runs. The [follow-up evidence](benchmarks/m7-ingestion-trial.json) preserves the original samples and timeout, rather than converting the initial run into a successful drain test. Follow-up timestamp is an observation time, not the exact convergence time.

Freshness is the last `metric_user_state.reconciled_at` minus that user's maximum source `received_at`, for this single-batch-per-user fixture. These database timestamps precede their transaction commits slightly; this is an approximate database processing delay, not browser-visible freshness or a commit-to-visibility trace. Publication/scheduling and aggregate replacement remain asynchronous. Do not describe HTTP acceptance rate as fresh-metrics throughput.

Fifty-six Docker resource samples were collected during offering/drain. Peak sampled CPU: PostgreSQL 214.69%, worker 50.83%, API 33.60%, NATS 22.84%, Redis 6.67%, dashboard 65.44% (Docker CPU can exceed 100% across cores). At those respective CPU peaks, memory readings were 220.2, 13.37, 17.02, 21.54, 5.164 and 90.82 MiB. These are readings at CPU peaks, not maximum memory, reservations, host totals or proof that limits cannot be exceeded. Sampling itself adds overhead and cannot capture every spike.

M10 should profile PostgreSQL query plans and worker scheduling/reconciliation before increasing concurrency or changing architecture. This result justifies measuring that path; it does not prove a specific query is the bottleneck. Repeat controlled lower-rate, sustained, saturation and mixed workloads after any optimization. The realistic M10 dataset (100 flags, ten experiments, 100,000 users and one million seeded events) and 10,000 evaluations/sec target remain unmeasured.

During final aggregate-read/replay verification, the thirty-second parity command timed out with this larger dataset. A read-only diagnostic reproduced PostgreSQL plan sensitivity: the recorded trial's literal scoped oracle completed in 491.201 ms, while the forced generic prepared plan used nested-loop CTE joins and was canceled at 35 seconds. [Query-plan evidence](benchmarks/m7-parity-query-plans.json) is retained. The oracle now sets `plan_cache_mode=force_custom_plan` locally in its read-only transaction; it does not change global settings, the deadline, aggregate serving or worker reconciliation. Actual parity then passed across 34 runs with the connection otherwise forced to generic planning. This diagnostic repair does not revise the ingestion/freshness result or establish the worker's bottleneck.

## M10 evaluation and ingestion — 5 October 2026

Host: Apple M1, 8 logical CPUs, 8 GiB RAM, Docker VM 3.8 GiB. Observability profile was off. The evaluation run used the Go generator on commit `e5e1eec` with the M10 tree dirty; the numbers are the HTTP steps in [m10-evaluation-http-baseline.json](benchmarks/m10-evaluation-http-baseline.json). Dataset: 100 flags, 10 experiments, 100,000 evaluation users. Hold was 45 seconds per step after a ramp.

| Target req/s | Achieved during hold | p50 ms | p99 ms | Dropped | Failures |
|---:|---:|---:|---:|---:|---:|
| 500 | 500 | 0.63 | 3.72 | 0 | 0 |
| 1000 | 1000 | 0.60 | 3.27 | 0 | 0 |
| 2000 | 2000 | 0.43 | 5.80 | 0 | 0 |
| 4000 | 4001 | 0.27 | 3.07 | 0 | 0 |
| 6000 | 6001 | 0.29 | 317.82 | 0 | 0 |
| 8000 | 7855 | 0.34 | 628.15 | 6526 | 0 |
| 10000 | 9642 | 0.68 | 1033.73 | 35969 | 0 |

Correctness stayed 1. The p99 < 50 ms target holds through 4,000 requests/s and fails at 6,000 and above, where in-flight requests hit the generator cap of 4,000. API CPU peaked at 242% and memory at 247 MiB of the 256 MiB limit. This is one run, not three repeats.

Event ingestion was offered at 10 batches/s (990 events/s) for 120 seconds against the 100-flag fixture. The first attempt, before deadline errors were classified, accepted 66,231 events and returned HTTP 500 for 482 batches; the harness then aborted. Evidence is [m10-events-attempt-1.json](benchmarks/m10-events-attempt-1.json). `context.DeadlineExceeded` is now HTTP 503 `overloaded` with `Retry-After`.

The rerun on commit `714cfbd` ([report](benchmarks/m10-events-20261005T134201Z.json)) accepted 118,800 events, 990/s, with one 503 and failure rate 0.083%. Batch latency p50 was 426 ms and p99 was 4,271 ms. The 600-second results poll did not reach exact counts (exposed 10,849, converted 10,849, requests 19,737 of 20,000 users at the deadline). Database freshness p95 was 585 seconds. **The five-second freshness target failed again.** PostgreSQL CPU peaked at 445%. k6 itself used up to 1,311 MiB. Do not treat 990 accepted events/s as fresh-metrics capacity.

## M10 after the user-receipt index — 5 October 2026

Commit `6653a42`. One reconciliation was scanning every raw fact: `EXPLAIN` showed `raw_events_retention_idx` with `received_at >= -infinity`, 218,245 rows removed by the user filter, and 1,317 ms execution. After the index and the null-floor predicate, a forced generic plan used `raw_events_user_receipt_idx` and executed in 10.8 ms. Four fixed loops then cleared 5,242 due users to zero in 15 seconds.

`make observability-smoke` passed with tracing on. Prometheus scraped both services. The label audit covered 250 series and rejected user, event, and decision identifiers. Grafana served the 17-panel dashboard with healthy Prometheus and Tempo datasources. Tempo trace `8bc79eefb28cccd82d853d005c44ed9` contains `POST /v1/events`, `db.insert`, `outbox.publish`, and `processing.consume` across the API and worker, without SQL text or those identifiers.

CPU profiles are in `docs/benchmarks/m10-api-cpu.pb.gz`, `m10-api-cpu-steady.pb.gz`, and `m10-worker-cpu.pb.gz`, with `go tool pprof -top` text beside them. With full trace sampling at a requested 2,000 evaluations/s the API was 83% busy and the OpenTelemetry HTTP middleware accounted for about half the cumulative time; that run achieved 22 requests/s and is not a capacity result ([m10-m10-profile-eval.json](benchmarks/m10-m10-profile-eval.json)). With tracing off, the same 2,000/s target achieved 2,000/s, zero failures, and correctness 1, while a concurrent profile overlapped the hold and p99 was 167 ms ([m10-m10-eval-2000-after-index.json](benchmarks/m10-m10-eval-2000-after-index.json)). The worker profile during ingestion was 8.6% CPU, almost all of it waiting in `ReconcileOne` on PostgreSQL.

A single k6 process for mixed evaluation and ingestion was SIGKILL'd (exit 137) at 2,993 MiB ([m10-m10-mixed.json](benchmarks/m10-m10-mixed.json)). The follow-up ran the Go generator at 2,000 evaluations/s beside k6 events at 10 batches/s. Evaluation achieved 642/s with a 97.6% failure rate and 164,673 dropped iterations. Ingestion accepted 25,443 events with a 0.65 failure rate. The API then refused connections, so the results poll did not run ([m10-mixed-concurrent-partial.json](benchmarks/m10-mixed-concurrent-partial.json)).

The post-index events rerun, 120 seconds at 10 batches/s ([m10-events-after-index.json](benchmarks/m10-events-after-index.json)), accepted 41,481 events (346/s) with failure rate 0.57 and batch p99 5,098 ms. Freshness p50 was 204 s and p95 was 227 s. **The five-second freshness target still failed.** Exact counts did not converge within 600 seconds.

`make load-soak` then ran on commit `521358d` with a clean tree and tracing off ([m10-soak-20261005T160122Z.json](benchmarks/m10-soak-20261005T160122Z.json)). The Go generator offered 1,000 evaluations/s for a 600-second hold beside k6 at 5 batches/s (495 events/s). Evaluation achieved 799.5/s, p50 1.20 ms, p99 4,874 ms, failure rate 0.538, 102,664 dropped iterations, and correctness 1. Ingestion accepted 129,591 events (202.5/s) with failure rate 0.516 and batch p99 50,758 ms. k6 exited 99. The load generator exited 0. The API was at its 256 MiB limit (CPU max 289%) and then refused the results login, so the zero cohort counts in that report are not a measurement. Postgres freshness on 5,709 users was p50 0 s, p95 93.0 s, and max 537 s. **The five-second freshness target failed again.** The p99 under 50 ms target also failed at this offered rate.

The outbox then drained to zero. Reconciliation stopped at 97,074 applied and 16 failed; those 16 failures did not grow after the drain. `due_at IS NOT NULL` stayed at 86,993 because that column also stores the next attribution-window deadline. Overdue rows (`due_at <= now`) were 0. The earliest scheduled deadline was 2026-10-05 21:33:52.551057+00. Host `go run ./cmd/check-metrics` then passed for 171 runs in 23.5 s (ended 2026-10-05T16:26:46Z). The soak run `run_5e2ebad00a89ad617f2cb14a0943cbffa94287d8911a16b9d1c33929c98dab4b` compared in 6.907 s, `run_918e5a645d0cffaaf3538364639c743b31c2e8bdf8de4c1f10ea705bf1660944` in 6.490 s, and `run_0b2c618d5bd8a360e2135bcab65b0e2f538cb70e3758226efe90ae8286975cba` in 3.277 s. The earlier mismatch was lag, not a counter disagreement after folding caught up.

While that database was quiet, retention still logged `database_or_retention_validation_failed` about every three seconds (`failed_cycles` 228) with every retention counter still zero. `EXPLAIN ANALYZE` of the fold candidate took 4,589 ms and removed all 89,059 user rows by filter. The summary-expiry candidate then took 1,892 ms because `reporting_since` is `-infinity` on every row. Every raw fact was received between 2026-10-04 21:05:52Z and 2026-10-05 16:11:52Z, inside both windows, so the two-second worker budget could never finish a no-op cycle. Folding and summary expiry now return immediately when no fact is older than the cutoff. Fold and expiry integration tests passed. After the worker image was rebuilt, the first retention cycle succeeded. `failed_cycles` was 2 one minute later, while container parity was scanning the same database; it was no longer failing every cycle. `make aggregation-parity` on that image passed for 171 runs, including the soak run in 8.202 s.
