# ADR 0001: modular monolith and explicit Go wiring

Status: accepted in the approved plan.

Switchyard uses a single Go module, shared domain packages and one PostgreSQL schema. API and eventual worker binaries are process roles, released together. HTTP handlers adapt requests; domain packages own behavior. Constructors pass dependencies explicitly.

This keeps deployment and debugging manageable on an 8 GB machine while still permitting independent worker concurrency. Separate services would add coordination, deployment and network failure modes before there is evidence they are useful. Revisit only when workload/team boundaries require independent ownership or scaling.

Use `net/http`, `pgx` and handwritten parameterized SQL. Embedded numbered migrations run transactionally under a PostgreSQL advisory lock; stored SHA-256 checksums reject edited history. Applied migrations are immutable. Forward corrective migrations preserve data. The initial runner deliberately supports transactional PostgreSQL SQL only; online/nontransactional index migrations would need an explicit extension.
