# ADR 0007: Transactional publication intent and bounded delivery

Status: accepted for M7; transactional outbox checkpoint verified. NATS transport, consumer aggregation and final failure/load gates are in progress.

## Decision

Keep raw events and flag revisions in PostgreSQL. Record a small scoped reference in `outbox` inside the same domain transaction. The event path records new accepted/quarantined facts once; duplicate receipts do not create another intent. The shared flag revision function covers ordinary configuration edits and experiment lifecycle transitions. An outbox insert failure rolls back the entire domain change. This is explicit Go transaction composition, with no hidden database publication trigger and no network call inside a domain transaction.

The reference carries kind, project/environment, object ID and revision. It carries no user attributes, bearer credentials or full configuration payload. The publisher will use the persisted identity as its stable message ID across ambiguous acknowledgements. A unique scoped reference prevents duplicate intent. Migration 0007 backfills retained MVP facts and revisions once so upgrading does not omit historical work.

Claim at most 100 rows in a short PostgreSQL transaction with `FOR UPDATE SKIP LOCKED`. Increment attempt counts and issue a random expiring claim token. Network publication happens after commit, without holding row locks. Only the current unexpired claim can mark publication complete or failed; stale publishers cannot overwrite a replacement lease. Retry uses bounded exponential delay from one to sixty-four seconds. Ten exhausted attempts become an inspectable dead row, including a final-attempt crash whose lease expires. Failure codes are a small fixed vocabulary; external error strings are not stored. Replay controls and publisher error classification will be completed before the M7 gate.

JetStream will use file storage, publisher acknowledgements and bounded stream/consumer settings. Its message-ID deduplication has a finite window, so PostgreSQL consumer idempotency remains necessary. A consumer commits processing state before acknowledging; delivery is at least once. This follows the [JetStream model](https://docs.nats.io/learn/jetstream/) and [durable consumer semantics](https://docs.nats.io/nats-concepts/jetstream/consumers). Stream limits must reject new publication rather than silently discard unprocessed messages; see [stream configuration](https://docs.nats.io/nats-concepts/jetstream/streams).

## Verified checkpoint and pending work

Race-enabled real PostgreSQL tests cover rollback, duplicate intent, disjoint concurrent claims, bounded batches, retry backoff, ambiguous-ack identity, stale-lease fencing and exhausted-attempt crashes. Upgrade fixtures prove retained event/configuration intents are backfilled once. Domain failure injection proves no event acknowledgement or flag revision escapes without its publication intent. Docker migration and existing live flag/experiment/measurement/health journeys passed.

There is no publisher or consumer runtime at this checkpoint. Pending rows are expected to accumulate; no queue loss/restart guarantee, aggregate read migration, retention cleanup or ingestion throughput is claimed. M7 remains in progress until NATS/worker/replay/parity/failure/load gates pass. Existing raw SQL remains the results oracle and read path.
