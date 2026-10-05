# ADR 0008: Durable scheduling and per-user metric contributions

Status: accepted for M7; consumer/reconciliation checkpoint verified. Replay, retention, final event drills and the ingestion trial are pending.

## Decision

The pull consumer validates a versioned envelope, its message-ID header and its scoped PostgreSQL publication intent. It never trusts a message to create a raw fact or change a historical definition. A PostgreSQL transaction inserts a unique processing receipt and schedules affected users for reconciliation. Only after that commit does the consumer confirm its broker acknowledgement. A lost acknowledgement can redeliver the same message; the durable receipt returns a duplicate without scheduling another contribution. Receipts survive raw deletion, so expired replay cannot reset deduplication. An unseen message whose source is missing becomes an inspectable permanent failure instead of fabricating a fact.

A permanent envelope/identity/source failure is saved in a bounded (2 KiB) dead-letter record before acknowledgement. Database failures retain delivery work and receive delayed negative acknowledgements. The record keeps the original internal reference envelope and a fixed failure code; no credential or attribute is intentionally transmitted, and payloads are not logged. Local operator repair/replay controls now require a current project administrator and commit audit entries atomically with durable scheduling; finalized-summary retention and final M7 gates remain pending.

Do not recompute every event in a run for each delivery. Keep one durable contribution per run/user. Reconciliation locks one due user, derives that user's contribution using the same set-based semantics as the MVP oracle, subtracts the previous contribution and adds the new one. Counter changes, contribution replacement and the next deadline commit together. Counters cannot become negative. Counter locks use sorted dimensions; no goroutine is created per event or user. One fixed consumer loop and one fixed reconciliation loop run beside the bounded publisher.

The consumer pulls four messages at a time. Database handling has a two-second deadline, configuration cache repair one second and confirmed acknowledgement one second; the batch fits the thirty-second broker acknowledgement window. Reconciliation transactions have a two-second deadline and retry durable due work on failure. Notification and reconciliation take the same user-state row lock, so a concurrent notification cannot be overwritten by queue completion. Fanout also schedules users whose outcome references the newly arrived fact, including cross-user/run invalid references that were previously pending.

Store cohort exposure/conversion counts, request count/errors, fixed latency bins and measurement quality as bounded dimensions per run/variant. User IDs live in contribution/work state, never in metric labels. Existing statistical assembly works over either representation. `make aggregation-parity` reads raw and materialized counts in one repeatable-read snapshot; it checks all dimensions and rejects an empty fixture. HTTP results remain on the raw SQL path until complete M7 gates pass.

Future occurrence and receipt times schedule another reconciliation. Referenced receipts with future timestamps also schedule the dependent user. Finalization occurs strictly after the thirty-minute conversion window plus the twenty-four-hour late allowance, with a one-microsecond deadline matching PostgreSQL precision. The scheduler therefore updates time-dependent cohorts without requiring a new event. Raw retention and preservation of older finalized summaries are the next implementation phase; raw data is not purged at this checkpoint.

Configuration notifications re-read current PostgreSQL configuration before writing Redis. Delayed historical messages cannot restore an old enabled definition. Redis repair is best effort after the processing commit; cache failures do not block event processing, and M6 periodic API reconciliation remains the independent repair path.

## Evidence

All existing measurement fixtures compare raw SQL and materialized results: late earlier exposure, both arrival orders, unique conversion, invalid/pending references, exact attribution boundaries, quarantine, future facts, finalization, request histograms, A/A and SRM/statistical states. Separate injected-clock tests verify automatic scheduling without re-enqueueing. A cross-user future-receipt regression failed before its scheduling fix and then passed.

Race-enabled PostgreSQL tests verify twenty simultaneous deliveries produce one receipt/intent, repeated reconciliation cannot double-count, failed scheduling rolls back the receipt, failed contribution completion rolls back counter changes, and a missing unprocessed source cannot be applied. Handler tests prove database/dead-letter failure prevents acknowledgement, lost acknowledgement remains a failure after commit, and disposable Redis failure does not block a committed configuration receipt. A PostgreSQL-backed delayed-notification test verifies revision 1 delivered after revision 2 still refreshes the current kill switch.

The Docker worker consumed the existing 355 reference messages, with zero dead letters and zero currently due metric jobs; raw/materialized parity initially passed for all twenty existing runs. These are correctness observations, not an ingestion throughput or lag benchmark. Complete M7 remains unproven until replay/retention/backlog/fault/load gates and the results-read transition are verified.

Final checkpoint evidence is saved in `docs/benchmarks/m7-processing-checkpoint.json`: 377 receipts, no dead letters or currently due users, and parity across 22 runs after the restart drill restored normal processing. The complete race-enabled PostgreSQL/Redis/NATS suite passed after the referenced-clock fix.

## Recovery controls checkpoint

Migration 0009 records dead-letter resolution without deleting its original record. `workctl` exposes bounded cursor inspection, a 100-fact retained replay page, retry of an exhausted publication and retry of a stored dead-letter envelope. It cannot rewrite raw facts or message payloads. Scope/administrator checks run inside the database transaction. Production write protection still applies. Seven-day received-at/source age gates prevent a recently generated failure record from extending an old source's replay horizon. Publication retry preserves the original message ID; replay preserves receipts and replaces contributions through normal reconciliation.

A dead-letter retry shares the processing transaction, so audit failure rolls back its receipt, scheduling and resolution. Malformed payloads remain inspectable as unscoped failures. Inspection returns only identifiers/fixed codes and pages through unrelated scopes without exposing their payloads. Recovery does not yet delete raw data or receipts; retention is the next phase.
