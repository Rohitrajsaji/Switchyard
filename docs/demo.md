# Demo, about six minutes

Use the local stack only. Sign in as `admin@example.test` with the demo password you exported. Do not type a real customer id or a provider key.

1. **One minute — the decision.** Open Marketplace demo, development, and the seeded listing flag. Preview a user in Japan and a user elsewhere. Say the order: kill, targeting, experiment, rollout, default. The preview does not write an exposure.

2. **One minute — a change with a record.** In development, edit the flag traffic or add a string flag named `copy` with default `"control"` and safe `"safe"`. Show the new revision and the audit reason. Production has no direct save. Open Reviews, show a proposal diff, and say a different person has to approve it.

3. **One minute — the agent boundary.** From a terminal, `make agent-drill` with `SWITCHYARD_DEMO_PASSWORD` set. The mock suggests a rollout. A same-person approval fails. A second person applies it once. Point at the application key permissions: `context:read` and `proposals:submit`.

4. **Two minutes — an experiment and its results.** Start or open the seeded experiment. Open the listing demo and submit one synthetic listing. Results lists every variant Go returned, with empty rates left blank. Say exposures are explicit events, not a side effect of evaluation. If the cohort is behind, say so: accepted events and fresh aggregates are different, and the recorded p95 is 227 seconds on this machine.

5. **One minute — the limit.** Open `docs/benchmark-report.md`. Evaluation held 4,000 requests/s with p99 under 50 ms in one run. The freshness target of 5 seconds did not. The mixed k6 job was killed for memory. The next step the profiles support is database capacity for per-user reconcile, which is still waiting on Postgres in the worker profile.

Stop. Do not start the 10-minute soak during the demo.
