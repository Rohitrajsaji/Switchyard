# ADR 0011: Central production change policy, approved rollouts and automatic rollback

Status: accepted for M9. The AI proposer (M11) reuses the same boundary and adds only a new proposal source.

## Context

Until M9 production configuration was read-only. Controlled rollout needs production changes with review, bounded automatic progression, and a safety path that cannot be raced into re-enabling a broken treatment. Review that can be bypassed through a second endpoint, or approval that can be replayed against a different change, would add ceremony without control.

## Decision

**One boundary.** `auth.Authorize` still denies every production write. Only two callers may use the scope check without that denial (`auth.AuthorizeEnv`): the direct flag path, which accepts nothing but `flags.Exempt` changes (setting `killed`, or lowering standalone rollout traffic, with every other field identical), and the proposal service. Flag create/update share one transactional core (`flags.ApplyTx`); validation for a proposal, an approval and the application run exactly the same code, differing only in whether the transaction is kept. Experiment lifecycle in production, application keys and operator tooling remain denied by the generic gate. Production experiments are therefore not yet reviewable or runnable, so rollout plans can only be used where an experiment can run (development and staging) until a later change adds experiment proposals.

**Approval binds to an exact diff.** A proposal stores the canonical before/after configuration and a SHA-256 hash over scope, kind, base revision and both configurations. Approval requires an admin other than the proposer, the same hash and a successful revalidation against the current revision; application re-checks the hash, the 24-hour approval window, the approver's *current* admin role and the base revision under the flag row lock. A change in any of these records the proposal as `stale` or `expired` rather than applying it. Database triggers enforce immutable content, the state graph and approver ≠ proposer independently of the Go checks (removing the Go self-approval check made the test suite fail on the constraint).

**Rollout plans progress experiment traffic.** A plan is a reviewed list of strictly increasing steps (each at most 10 points above the previous, within a ceiling), with guardrail thresholds and one of two modes: `scheduled` (steps apply when due) or `metric` (a step also needs a passing, sufficient check at that moment). Plans change only the running experiment's eligible traffic, never its salts, variants or weights, so previously eligible users keep their variant on a raise. Due times are fixed when the plan starts, inside the approval window. A worker applies steps as an inactive `system:rollout` identity.

**Serialization.** Progression, rollback, plan start/approve/cancel and manual kills all take the flag row lock first, then the plan row lock. Under that lock a step must find the revision the plan expects; any other revision (a manual edit, pause, kill) stops the plan as `stale` or `cancelled`. The rollback is a single transaction that appends a killed revision, cancels pending steps, records the evidence, advances the plan state and starts a cooldown. A progression that obtains the lock afterwards sees the killed flag and stops; one that held the lock first finishes, and the rollback then disables the successor. Duplicate ticks cannot repeat a step because step state changes under the same lock.

**Guardrails are explicit about evidence.** Operational checks read accepted request facts in a sliding window per non-control variant. Fewer than the minimum requests is `insufficient`: it can neither promote nor roll back. A breach needs the configured number of consecutive checks (default two, ten seconds apart). Conversion checks use only matured (finalized) cohorts, require a concurrent control with conversions and treat a processing backlog as insufficient. These are tunable engineering defaults, not statistically validated thresholds, and the operational p95 is an exact in-window percentile of raw facts rather than the histogram bound used on the results page.

**Cooldown.** After a safety rollback no approval can re-enable that flag until the cooldown ends; re-enabling is always a reviewed change.

## Consequences

Review adds latency by design, and the emergency path (kill or lower traffic) stays fast and audited as `emergency_exempt`. Guardrails read raw facts inside a short window, so they depend on raw retention and on the ingestion path being healthy; an unhealthy ingestion path produces `insufficient`, never a promotion. The detection delay is bounded below by the check cadence times the consecutive-breach count (about 20 seconds in the drill). A killed flag stops being served as soon as snapshots refresh, which is bounded by the existing two-second refresh and thirty-second maximum staleness.
