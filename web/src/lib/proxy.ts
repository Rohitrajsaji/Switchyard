// Transport only: Go owns authentication, authorization and domain validation.
const segment = "[A-Za-z0-9_-]+";
const project = `/v1/projects/${segment}`;
const routes: Array<[RegExp, string[]]> = [
  [/^\/v1\/session$/, ["GET", "POST", "DELETE"]],
  [/^\/v1\/projects$/, ["GET", "POST"]],
  [/^\/v1\/users$/, ["POST"]],
  [new RegExp(`^${project}/environments$`), ["GET"]],
  [new RegExp(`^${project}/members$`), ["POST"]],
  [new RegExp(`^${project}/application-keys$`), ["POST"]],
  [new RegExp(`^${project}/application-keys/${segment}$`), ["DELETE"]],
  [new RegExp(`^${project}/audit$`), ["GET"]],
  [new RegExp(`^${project}/flags$`), ["GET", "POST"]],
  [new RegExp(`^${project}/flags/${segment}$`), ["GET", "PUT"]],
  [new RegExp(`^${project}/flags/${segment}/preview$`), ["POST"]],
  [new RegExp(`^${project}/experiments$`), ["GET", "POST"]],
  [new RegExp(`^${project}/experiments/${segment}$`), ["GET"]],
  [new RegExp(`^${project}/experiments/${segment}/results$`), ["GET"]],
  [new RegExp(`^${project}/experiments/${segment}/transitions$`), ["POST"]],
  [/^\/v1\/(evaluate|events)$/, ["POST"]],
];
const maxBody = 1024 * 1024;
function failure(status: number, error: string): Response {
  return Response.json(
    { error },
    { status, headers: { "Cache-Control": "no-store" } },
  );
}
async function boundedBody(request: Request): Promise<Uint8Array | undefined> {
  if (!request.body) return undefined;
  const reader = request.body.getReader();
  const chunks: Uint8Array[] = [];
  let size = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      size += value.byteLength;
      if (size > maxBody) {
        await reader.cancel();
        throw new Error("too_large");
      }
      chunks.push(value);
    }
  } finally {
    reader.releaseLock();
  }
  const body = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) {
    body.set(chunk, offset);
    offset += chunk.length;
  }
  return body;
}
export async function proxy(
  request: Request,
  path: string[],
  config: { baseURL: string; origin: string },
  fetcher: typeof fetch = fetch,
): Promise<Response> {
  if (!path.length || path.some((p) => !/^[A-Za-z0-9_-]+$/.test(p)))
    return failure(404, "not_found");
  const pathname = `/${path.join("/")}`;
  const route = routes.find(([pattern]) => pattern.test(pathname));
  if (!route) return failure(404, "not_found");
  if (!route[1].includes(request.method))
    return failure(405, "method_not_allowed");
  // Never manufacture an Origin to make a cross-site request pass Go's CSRF checks.
  if (
    request.method !== "GET" &&
    request.headers.get("origin") !== config.origin
  )
    return failure(403, "forbidden");
  if (Number(request.headers.get("content-length")) > maxBody)
    return failure(413, "payload_too_large");
  const headers = new Headers();
  for (const name of ["content-type", "origin", "x-csrf-token"]) {
    const value = request.headers.get(name);
    if (value) headers.set(name, value);
  }
  const appRoute = pathname === "/v1/evaluate" || pathname === "/v1/events";
  if (appRoute) {
    const token = request.headers.get("authorization");
    if (token) headers.set("authorization", token);
  } else {
    const cookies = (request.headers.get("cookie") ?? "")
      .split(";")
      .map((c) => c.trim());
    const sessions = cookies.filter((c) =>
      /^switchyard_session=sws_[a-f0-9]{64}$/.test(c),
    );
    if (sessions.length === 1) headers.set("cookie", sessions[0]);
  }
  let body: Uint8Array | undefined;
  try {
    body = await boundedBody(request);
  } catch {
    return failure(413, "payload_too_large");
  }
  const incoming = new URL(request.url);
  const target = new URL(pathname, config.baseURL);
  target.search = incoming.search;
  try {
    const upstream = await fetcher(target, {
      method: request.method,
      headers,
      body: body as BodyInit | undefined,
      redirect: "manual",
      cache: "no-store",
      signal: AbortSignal.any([request.signal, AbortSignal.timeout(8000)]),
    });
    if (upstream.status >= 300 && upstream.status < 400) {
      await upstream.body?.cancel();
      return failure(502, "upstream_error");
    }
    const output = new Headers({
      "Cache-Control": "no-store",
      "X-Content-Type-Options": "nosniff",
    });
    for (const name of ["content-type", "retry-after", "x-request-id"]) {
      const value = upstream.headers.get(name);
      if (value) output.set(name, value);
    }
    // Only session endpoints are allowed to set the browser session cookie.
    if (pathname === "/v1/session")
      for (const cookie of upstream.headers.getSetCookie())
        output.append("set-cookie", cookie);
    return new Response(upstream.body, {
      status: upstream.status,
      headers: output,
    });
  } catch {
    return failure(503, "backend_unavailable");
  }
}
