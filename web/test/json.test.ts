import { test } from "node:test";
import assert from "node:assert/strict";
import { jsonText, parseJSON, parseResponse } from "../src/lib/json.ts";
test("flag value decimals and targeting numbers survive API/editor round trips", () => {
  const text =
    '{"revision":3,"safe":{"type":"json","data":{"n":0.123456789012345678901}},"rules":[{"values":[0.123456789012345678901]}]}';
  const value = parseResponse<{ revision: number; safe: { data: unknown } }>(
    text,
  );
  assert.equal(value.revision, 3);
  assert.equal(jsonText(value), text);
  assert.equal(
    jsonText(parseJSON(jsonText(value.safe.data))),
    '{"n":0.123456789012345678901}',
  );
});
test("invalid JSON and unsafe metadata fail explicitly", () => {
  assert.throws(() => parseJSON('{"n":NaN}'));
  assert.throws(() => parseJSON('{"n":1,"n":2}'));
  assert.throws(() => parseResponse('{"revision":9007199254740993}'));
});
