import { test } from "node:test";
import assert from "node:assert/strict";
import { basisPoints, percent } from "../src/lib/experiments.ts";
test("percentage input converts exact hundredths without silently rounding", () => {
  assert.equal(basisPoints("0.01"), 1);
  assert.equal(basisPoints("33.33"), 3333);
  assert.equal(basisPoints("100"), 10000);
  assert.equal(basisPoints("0"), 0);
  for (const invalid of ["-1", "100.01", "1.001", "NaN", "1e2", ""])
    assert.throws(() => basisPoints(invalid));
});
test("unavailable rates remain distinct from measured zero", () => {
  assert.equal(percent(null), "—");
  assert.equal(percent(0), "0.00%");
});
