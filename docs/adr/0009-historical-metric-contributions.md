# ADR 0009: Historical contributions and retained attribution metadata

Status: accepted as an M7 retention foundation. Bounded folding, identity/work expiry and ninety-day summary-expiry primitives are implemented and tested; automatic scheduling and final M7 retention/recovery/read-transition gates remain pending.

## Decision

Keep a per-user historical contribution separate from the contribution recomputed from retained raw facts. Their receipt intervals must be disjoint. Request counts, histograms and resolved measurement quality are additive. Exposure and conversion are unique run/user facts, so merge them once rather than adding two numerators. If both intervals contain an attributed completion, add one duplicate-completion count to correct the two independently deduplicated intervals.

Preserve the original earliest exposure as compact anchor metadata. A returning user cannot acquire a second cohort or silently change the variant after raw deletion. Reconciliation combines that anchor with retained exposures using the existing event-time/tie-break rules. Conflicting/provisional historical cohorts fail without modifying counters.

Retained outcomes can reference purged facts, including an invalid reference to a non-exposure. Compact reference projections retain scoped identity, run/user, kind/status, variant and occurrence/receipt times. They omit attributes, original payload, credentials and latency samples. A still-retained raw fact takes precedence over its projection. These projections are attribution metadata, not enough information to reconstruct the original event history.

Historical unresolved future/pending counts cannot be frozen silently: reconciliation rejects them and preserves durable due work. The cleanup phase must retain enough compact unresolved-outcome state for references to resolve, or prove resolution before folding. It must also preserve event identity through the eight-day deduplication interval and enforce supported replay age before expiring receipts/history.

The normal worker can read historical contributions, but all existing historical fields remain empty. No production cleanup path populates them or deletes raw facts at this checkpoint. HTTP continues to use the MVP raw oracle until the complete M7 gates. Folding, bounded cleanup, configurable seven/ninety-day retention, deduplication expiry and retained-interval parity/replay remain required work.

## Evidence

Disposable PostgreSQL fixtures capture the independent full-raw SQL result before deleting old raw facts. Results after deletion match for a returning user, previous conversion, new product request/histogram, outside-window completion and invalid non-exposure reference. Repeated reconciliation remains unchanged. Exact 24-hour late receipts cross the seven-day cutoff: a retained first completion produces one conversion; an already converted historical user produces one conversion and one duplicate. Unresolved historical quality fails with unchanged counters and a retained due job.

An initial fixture was rejected because a completion copied request-only latency fields; the fixture was corrected to obey the existing event contract. This was a test-payload error, not an ingestion bypass. Source/race and integration evidence are recorded in the milestone ledger; the retention milestone remains incomplete.

## Eight-day identity checkpoint

Migration 0012 adds an identity archive copied only by a bounded raw-folding primitive, rather than duplicating all incoming raw facts. It keeps normalized payload and original receipt status/reason through eight times twenty-four hours from the original PostgreSQL receipt. Exact PostgreSQL jsonb equality preserves numeric/object/time canonical retry behavior; a textual checksum would incorrectly distinguish equivalent decimals. This intentionally retains identity payload data for the one-day interval beyond seven-day analytical raw retention. Compact attribution references omit that payload and remain separate.

Ingestion checks raw/archived receipts before validating a new fact. Identical post-folding retries receive the old receipt and do not add raw facts/outbox intent. Changed identities conflict. After receipt expiry, an original occurrence older than seven days rejects the batch. A new timestamp cannot reuse an identity still known through publication/reference state. These checks prevent expired replay becoming a new accepted fact.

The preservation primitive shares the folding transaction. A separate prune primitive deletes at most one hundred expired identities using row locks and SKIP LOCKED, with expiry based on original receipt rather than retry/folding time. Neither primitive is automatically scheduled yet; the full folding/expiry transaction and unresolved-outcome handling are the next phase.

## Bounded folding checkpoint

Migration 0013 adds compact unresolved outcomes and contributions partitioned by original UTC receipt day. `FoldOne` locks one user and selects at most 100 delivered facts from that user's earliest eligible receipt day. A durable processing receipt is required before deleting a raw source. The same user lock serializes folding, reconciliation and arrival notifications. It copies identity/reference projections, updates historical state and deletes selected sources in one transaction. A failed delete leaves all sources and projections unchanged. Existing processing receipts remain intact.

An outcome whose exposure reference has not arrived keeps its measurement fields in a compact pending table. It contributes through the same attribution query, can schedule its referencing user on later exposure arrival, and folds into history only after resolution. Original payload and application credential identifiers are omitted. A still-present raw fact shadows its compact projection. Pending and future counts are never added to frozen historical totals.

Each day's request/histogram/resolved-quality segment owns disjoint facts; cohort identity belongs to one archived anchor rather than each day. The fold transaction first preserves the global earliest finalized anchor, even if an earlier exposure arrived late on a different receipt day. It merges exposed/converted users once and carries duplicate-completion corrections into the owning segment. This gives the next expiry phase a bounded historical unit without creating extra cohorts per day.

The source checks and complete PostgreSQL/Redis/NATS race integration suite passed. Independent full-raw oracle fixtures verify 108 facts across 100/8 pages, rollback after a late deletion failure, late earlier anchors across UTC receipt days, compact pending resolution, undelivered-source exclusion and concurrent folders. Reconciliation is checked after each fold against a separate table retaining the full original raw history.

This primitive is not scheduled by the normal worker yet. The development database retains all raw facts. Ninety-day summary expiry, bounded processing/publication cleanup, configurable retention, retained replay/parity and final fault/load/read-transition gates remain required before M7 completion.

## Completed-work expiry checkpoint

`processing.PruneCompleted` removes at most 100 completed publication/processing-receipt pairs and 100 resolved dead letters per transaction. Publication creation, confirmed publication and processing must all be at least 192 hours old. It preserves event sources still present in raw storage, active leases, unpublished intents, unprocessed notifications and references with unresolved dead letters. Processing locks its source publication with `FOR KEY SHARE`; cleanup locks selected publications with `FOR UPDATE SKIP LOCKED`, so in-flight source validation cannot race paired deletion.

Receipt and publication deletion commit together. An expired broker delivery then fails source validation instead of generating new work. Immutable configuration history, metric user state and counters remain intact. Resolved dead-letter deletion releases its database admission slot; unresolved failures remain inspectable and consume the existing bounded capacity. This policy measures resolved failure retention from resolution rather than first failure, preserving the inspection period after repair.

Exact-boundary, 105-row concurrent paging, rollback after receipt deletion, pending/raw/lease/failure protection, admission-counter consistency, configuration history and active source-lock fixtures verify the primitive. Its scheduling still depends on the remaining M7 retention/failure gates. Summary expiry is separate; deleting receipts does not claim to expire ninety-day analytical data.

## Ninety-day summary expiry checkpoint

Migration 0015 adds a monotonic per-user reporting floor. The summary window contains the current UTC receipt day and the preceding 89 UTC days; day ownership is based on original receipt, not folding/retry time. The internal expiry function accepts 8–365 days so the analytical window exceeds the fixed seven-day raw replay horizon. Increasing retention or moving the clock backward does not reconstruct already-expired summaries.

`ExpireOne` holds the common user lock, deletes at most 100 expired daily segments and 100 expired compact pending outcomes, and rebuilds historical contribution from the bounded retained daily window plus the original anchor's cohort. A cohort/conversion expires with its original anchor receipt day. Requests, histograms and resolved quality expire with their owning daily segment. Unresolved outcomes stop contributing when below the reporting floor, even if another bounded deletion page remains. Historical update, floor advancement, deletion and reconciliation scheduling are atomic. Counter differences are committed by ordinary reconciliation afterward.

Anchor/reference identity is retained separately from reporting counts. Reconciliation chooses the global earliest accepted exposure across raw and archived sources before applying the reporting floor to cohort counts. Consequently a returning user cannot turn a newer exposure into a replacement cohort/variant. Request references can still validate an older exposure. These compact identities are deliberately not raw analytical payloads; their lifecycle remains separate from ninety-day summary data. Source attributes/payloads are governed by raw/identity retention.

The duplicate-completion SQL now subtracts converted users directly rather than visible reporting cohorts; these are identical before expiry, and the former remains valid when an anchor's reporting cohort has expired. Tests cover exact UTC boundaries, partial expiry of two receipt days, returning users, pending outcomes, concurrent expiry, failed-delete rollback, 105 pending rows and 111 historical-day rows requiring bounded pages. Existing full-raw attribution/folding fixtures remain unchanged in meaning.

Automatic scheduling and the final retained replay/parity, failure/load and HTTP read-transition gates remain pending. Normal development data still has no reporting floor advancement or deletion at this checkpoint.
