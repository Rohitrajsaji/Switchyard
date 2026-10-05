import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import { bucket, evaluate } from "./evaluate.mjs";

const golden = JSON.parse(readFileSync(new URL("../../testdata/evaluation/golden.json", import.meta.url), "utf8"));

test("shared golden buckets match the published hash", () => {
  for (const row of golden.buckets) {
    assert.equal(bucket(row.project_id, row.environment_id, row.flag_id, row.run_id, row.purpose, row.salt, row.user_id), row.bucket);
  }
});

test("evaluate posts the HTTP contract and returns the decision", async () => {
  let seen;
  const decision = await evaluate({
    baseUrl: "http://api.local",
    token: "app-key",
    projectId: "prj_demo",
    environmentId: "env_demo",
    key: "copy",
    userId: "user_123",
    attributes: { country: "JP" },
    fallback: { type: "string", data: "safe" },
    fetchImpl: async (url, init) => {
      seen = { url: String(url), init };
      return { ok: true, json: async () => ({ value: { type: "string", data: "checkout" }, reason: "default", revision: 1 }) };
    },
  });
  assert.equal(seen.url, "http://api.local/v1/evaluate");
  assert.equal(seen.init.headers.authorization, "Bearer app-key");
  assert.deepEqual(JSON.parse(seen.init.body), {
    project_id: "prj_demo",
    environment_id: "env_demo",
    key: "copy",
    user_id: "user_123",
    attributes: { country: "JP" },
    fallback: { type: "string", data: "safe" },
  });
  assert.equal(decision.value.data, "checkout");
});
