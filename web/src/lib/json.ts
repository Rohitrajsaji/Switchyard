import { parse, stringify, isLosslessNumber } from "lossless-json";
export function parseJSON(text: string): unknown {
  return parse(text);
}
export function jsonText(value: unknown, pretty = false): string {
  return stringify(value, null, pretty ? 2 : undefined) ?? "null";
}
// Domain JSON values retain decimal tokens. Counts/revisions/weights become UI
// numbers; integers outside the safe numeric range fail explicitly.
export function parseResponse<T>(text: string): T {
  function normalize(value: unknown): unknown {
    if (isLosslessNumber(value)) {
      const number = value.valueOf();
      if (typeof number !== "number")
        throw new Error("API integer exceeds the dashboard numeric range");
      return number;
    }
    if (Array.isArray(value)) return value.map(normalize);
    if (value && typeof value === "object")
      return Object.fromEntries(
        Object.entries(value).map(([key, item]) => [
          key,
          ["data", "values", "attributes", "details"].includes(key)
            ? item
            : normalize(item),
        ]),
      );
    return value;
  }
  return normalize(parse(text)) as T;
}
