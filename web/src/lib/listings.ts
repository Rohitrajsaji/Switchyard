import { boundedBody } from "./proxy.ts";
// A synthetic product endpoint for measuring an actual HTTP submission. It
// acknowledges validated demo input but deliberately stores no marketplace data.
export async function submitListing(
  request: Request,
  origin: string,
  newID: () => string,
): Promise<Response> {
  const response = (status: number, body: unknown) =>
    Response.json(body, { status, headers: { "Cache-Control": "no-store" } });
  if (request.headers.get("origin") !== origin)
    return response(403, { error: "forbidden" });
  if (
    request.headers.get("content-type")?.split(";")[0].trim() !==
    "application/json"
  )
    return response(400, { error: "invalid_input" });
  let input: unknown;
  try {
    input = JSON.parse(
      new TextDecoder().decode(await boundedBody(request, 4096)),
    );
  } catch {
    return response(400, { error: "invalid_input" });
  }
  if (!input || typeof input !== "object" || Array.isArray(input))
    return response(400, { error: "invalid_input" });
  const record = input as Record<string, unknown>;
  if (
    Object.keys(record).some(
      (key) => !["title", "price", "category"].includes(key),
    ) ||
    typeof record.title !== "string" ||
    record.title.trim().length < 1 ||
    record.title.length > 120 ||
    typeof record.price !== "number" ||
    !Number.isSafeInteger(record.price) ||
    record.price < 1 ||
    record.price > 1000000 ||
    !["books", "electronics", "home"].includes(String(record.category))
  )
    return response(400, { error: "invalid_input" });
  return response(201, { listing_id: newID(), mode: "synthetic" });
}
