# Interview walkthrough

Switchyard is a local feature-flag and experiment service. One Go process family owns evaluation, revisions, proposals, and metric folding. PostgreSQL is the source of truth. Redis and NATS are replaceable.

## Why this shape

A modular monolith keeps the flag write, the audit row, and the outbox insert in one transaction. Splitting those into services would add network failure between the decision and its record before this workload needs separate scaling. The worker is a second process, not a second owner: it consumes work the API already committed.

Evaluation is a pure function of a compiled revision and a user id. The hash input excludes traffic and revision, so raising a rollout from 10% to 20% keeps the users who were already in. Kill wins over targeting, which wins over an experiment. That order is the product rule a reviewer can point at in `pkg/evaluation`.

## What is intentionally small

The JavaScript client calls `POST /v1/evaluate`. It does not evaluate flag values. It does repeat the published bucket hash so `testdata/evaluation/golden.json` can be checked in a second language. The Go SDK is the other remote client. A second evaluator would drift.

The Python agent proposes. Go checks the document, the sensitive-attribute list, and the 1,000 basis-point cap. A different human approves. The model never receives a credential that can apply a change.

String and number flags use the same revision path as boolean and JSON. Strings are bounded JSON strings. Numbers are bounded decimals, compared exactly, including exponent spelling. They are not a new storage system.

## Measured limits

On the Apple M1 with 8 GiB RAM, warm HTTP evaluation held p99 under 50 ms through 4,000 requests/s in one ladder. At 6,000 the p99 was 318 ms and the generator was dropping work. That is one run, not three repeats.

Event acceptance and fresh metrics are different. Before the user-receipt index, 990 accepted events/s left freshness p95 at 585 s. The index and four reconcile loops cut a single-user plan from about 1.3 s and 218,000 filtered rows to about 11 ms, and a 5,242-user backlog drained in 15 s with ingest stopped. Under a real 10-batch/s offer after that change, freshness p95 was 227 s and most batches failed. The later 10-minute soak, 1,000 evaluations/s beside 495 events/s, achieved 799.5 evaluations/s and 202.5 accepted events/s. Freshness p95 on that run was 93.0 s. The five-second target is missed, and so is the 50 ms evaluation p99.

A mixed k6 process was killed at 2,993 MiB. Splitting evaluation onto the Go generator still saturated the API. Worker CPU during reconcile was about 9%, waiting on PostgreSQL. The next scaling step that the profiles justify is more database capacity for per-user folding, not more Go goroutines and not a second evaluator.

Tracing on the hot path is optional and expensive. A profile with export enabled collapsed 2,000 requests/s to 22. Leave the collector off for a capacity number.

## What I would not claim

Hosted deployment was not done. GitHub Actions is defined and has not been run on a remote repository. Failure drills from earlier milestones were not all repeated after the reconcile change. Performance targets that failed stay failed in `docs/benchmark-report.md`.
