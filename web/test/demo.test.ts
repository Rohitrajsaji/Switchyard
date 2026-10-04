import { test } from "node:test";
import assert from "node:assert/strict";
import {
  listingFlow,
  exposureEvent,
  completionEvent,
  outcomeEvent,
  checkReceipts,
} from "../src/lib/demo.ts";
import { submitListing } from "../src/lib/listings.ts";
test("measurement keeps the rendered decision context and excludes nonrandom assignments", () => {
  const decision = {
    value: { type: "boolean" as const, data: true },
    reason: "experiment",
    revision: 4,
    run_id: "run_a",
    variant_id: "treatment",
    decision_id: "dec_a",
  };
  const exposure = exposureEvent(
    decision,
    "demo_1",
    { country: "JP" },
    "evt_a",
    "2026-10-05T12:00:00Z",
  );
  const complete = completionEvent(exposure, "evt_b", "2026-10-05T12:01:00Z");
  assert.equal(complete.exposure_id, exposure.event_id);
  assert.equal(complete.decision_id, undefined);
  assert.equal(complete.revision, 4);
  assert.equal(complete.variant_id, "treatment");
  const outcome = outcomeEvent(
    exposure,
    "evt_c",
    complete.occurred_at,
    false,
    25.5,
  );
  assert.equal(outcome.is_error, false);
  assert.equal(outcome.latency_ms, 25.5);
  assert.throws(() =>
    exposureEvent(
      { ...decision, reason: "targeting" },
      "demo_1",
      {},
      "evt_x",
      exposure.occurred_at,
    ),
  );
  assert.equal(listingFlow(decision.value), "simple");
  assert.equal(listingFlow({ type: "boolean", data: false }), "classic");
  assert.equal(
    listingFlow({ type: "json", data: { flow: "classic" } }),
    "classic",
  );
  assert.throws(() => listingFlow({ type: "json", data: null }));
  checkReceipts(
    [exposure],
    [{ event_id: "evt_a", status: "accepted", duplicate: true }],
  );
  assert.throws(() => checkReceipts([exposure], []));
  assert.throws(() =>
    checkReceipts(
      [exposure],
      [
        {
          event_id: "evt_a",
          status: "quarantined",
          reason: "assignment_mismatch",
          duplicate: false,
        },
      ],
    ),
  );
});
test("synthetic listing submission validates bounded input and acknowledges no real storage", async () => {
  const origin = "http://localhost:3000";
  const request = (body: string, headers: Record<string, string> = {}) =>
    new Request(`${origin}/api/demo/listings`, {
      method: "POST",
      headers: {
        Origin: origin,
        "Content-Type": "application/json",
        ...headers,
      },
      body,
    });
  const body = JSON.stringify({
    title: "Demo book",
    price: 500,
    category: "books",
  });
  const accepted = await submitListing(
    request(body),
    origin,
    () => "listing_a",
  );
  assert.equal(accepted.status, 201);
  assert.deepEqual(await accepted.json(), {
    listing_id: "listing_a",
    mode: "synthetic",
  });
  for (const invalid of [
    "null",
    "{}",
    body.replace("500", "0"),
    body.replace('"books"', '"private"'),
    JSON.stringify({ title: "x".repeat(4096), price: 500, category: "books" }),
  ])
    assert.equal(
      (await submitListing(request(invalid), origin, () => "unused")).status,
      400,
    );
  assert.equal(
    (
      await submitListing(
        request(body, { Origin: "https://evil.test" }),
        origin,
        () => "unused",
      )
    ).status,
    403,
  );
});
