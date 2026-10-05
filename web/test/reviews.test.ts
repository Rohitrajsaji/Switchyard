import { test } from "node:test";
import assert from "node:assert/strict";
import {
  canApprove,
  checkSummary,
  stepSummary,
  summarizeDiff,
} from "../src/lib/reviews.ts";
import { proxy } from "../src/lib/proxy.ts";

const value = (data: boolean) => ({ type: "boolean" as const, data });
const config = (traffic: number, killed = false) => ({
  default: value(false),
  safe: value(false),
  rules: [],
  killed,
  rollout: { traffic_bp: traffic, salt: "standalone-v1", value: value(true) },
});

test("diff summary highlights only the changed lines", () => {
  const s = summarizeDiff({ before: config(1000), after: config(2000) });
  assert.deepEqual(s.changed, ["rollout 20.00% → true"]);
  const created = summarizeDiff({ before: null, after: config(500) });
  assert.equal(created.before.length, 0);
  assert.ok(created.changed.includes("enabled"));
  const killed = summarizeDiff({
    before: config(1000),
    after: config(1000, true),
  });
  assert.deepEqual(killed.changed, ["disabled (kill switch)"]);
});

test("approval is only offered to a different admin", () => {
  assert.equal(canApprove("admin", "a", "b"), true);
  assert.equal(canApprove("admin", "a", "a"), false);
  assert.equal(canApprove("developer", "a", "b"), false);
  assert.equal(canApprove("viewer", "a", "b"), false);
});

test("step and guardrail summaries are explicit about insufficient evidence", () => {
  assert.match(
    stepSummary({
      ordinal: 1,
      traffic_bp: 2000,
      offset_seconds: 60,
      state: "applied",
      applied_revision: 4,
    }),
    /20.00% · applied at revision 4/,
  );
  assert.match(
    checkSummary({
      id: 1,
      checked_at: "2026-10-05T12:00:00Z",
      decision: "insufficient",
      evidence: {
        operational: [
          {
            variant_id: "treatment",
            requests: 19,
            errors: 19,
            error_rate: 1,
            p95_ms: 900,
            sufficient: false,
          },
        ],
      },
    }),
    /not enough requests to decide/,
  );
});

test("proxy allows the review routes with exact methods only", async () => {
  const config2 = {
    baseURL: "http://api:8080",
    origin: "http://localhost:3000",
  };
  const ok = (async () => new Response("{}")) as typeof fetch;
  const call = (path: string[], method: string) =>
    proxy(
      new Request(`http://localhost:3000/api/backend/${path.join("/")}`, {
        method,
        headers: { origin: "http://localhost:3000" },
      }),
      path,
      config2,
      ok,
    );
  for (const [path, method] of [
    ["v1/projects/p1/proposals", "GET"],
    ["v1/projects/p1/proposals/prp_1/approve", "POST"],
    ["v1/projects/p1/proposals/prp_1/apply", "POST"],
    ["v1/projects/p1/rollouts", "POST"],
    ["v1/projects/p1/rollouts/rlp_1/checks", "GET"],
    ["v1/projects/p1/rollouts/rlp_1/start", "POST"],
    ["v1/projects/p1/rollouts/rlp_1/cancel", "POST"],
    ["v1/projects/p1/experiments/run_1/traffic", "POST"],
  ] as const)
    assert.equal((await call(path.split("/"), method)).status, 200, path);
  assert.equal(
    (await call("v1/projects/p1/proposals/prp_1/approve".split("/"), "GET"))
      .status,
    405,
  );
  assert.equal(
    (await call("v1/projects/p1/rollouts/rlp_1/delete".split("/"), "POST"))
      .status,
    404,
  );
});
