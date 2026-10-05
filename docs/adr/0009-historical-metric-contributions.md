# ADR 0009: Historical contributions and retained attribution metadata

Status: accepted as an M7 retention foundation. Automatic folding/deletion, identity expiry and 90-day summary expiry are not yet implemented.

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
