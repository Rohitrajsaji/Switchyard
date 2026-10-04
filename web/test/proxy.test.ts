import { test } from "node:test";
import assert from "node:assert/strict";
import { proxy } from "../src/lib/proxy.ts";
const config = { baseURL: "http://api:8080", origin: "http://localhost:3000" };
const request = (path: string, init: RequestInit = {}) =>
  new Request(`http://localhost:3000/api/backend/${path}`, init);
test("unknown paths, traversal, methods and cross-site writes never reach Go", async () => {
  const never = (async () => {
    throw new Error("unexpected fetch");
  }) as typeof fetch;
  for (const path of [
    ["v1", "projects", ".."],
    ["v1", "projects", "a/b"],
    ["healthz"],
  ])
    assert.equal(
      (await proxy(request("v1/projects"), path, config, never)).status,
      404,
    );
  assert.equal(
    (
      await proxy(
        request("v1/projects", { method: "DELETE" }),
        ["v1", "projects"],
        config,
        never,
      )
    ).status,
    405,
  );
  for (const origin of [undefined, "https://evil.test"])
    assert.equal(
      (
        await proxy(
          request("v1/session", {
            method: "POST",
            headers: origin ? { origin } : {},
          }),
          ["v1", "session"],
          config,
          never,
        )
      ).status,
      403,
    );
});
test("relay exact body, session, Origin and CSRF; strip caller roles and other cookies", async () => {
  const body = '{"n":0.1234567890123456789}';
  const fetcher = (async (url: URL, init: RequestInit) => {
    assert.equal(url.href, "http://api:8080/v1/projects?before_id=123");
    const headers = new Headers(init.headers);
    assert.equal(
      headers.get("cookie"),
      `switchyard_session=sws_${"a".repeat(64)}`,
    );
    assert.equal(headers.get("origin"), config.origin);
    assert.equal(headers.get("x-csrf-token"), "csrf");
    assert.equal(headers.get("authorization"), null);
    assert.equal(headers.get("x-role"), null);
    assert.equal(new TextDecoder().decode(init.body as Uint8Array), body);
    assert.equal(init.redirect, "manual");
    return Response.json(
      { error: "conflict" },
      {
        status: 409,
        headers: { "X-Request-ID": "req_1", "Set-Cookie": "unrelated=value" },
      },
    );
  }) as typeof fetch;
  const res = await proxy(
    request("v1/projects?before_id=123", {
      method: "POST",
      body,
      headers: {
        origin: config.origin,
        "x-csrf-token": "csrf",
        "x-role": "admin",
        authorization: "Bearer secret",
        cookie: `other=secret; switchyard_session=sws_${"a".repeat(64)}`,
      },
    }),
    ["v1", "projects"],
    config,
    fetcher,
  );
  assert.equal(res.status, 409);
  assert.equal(res.headers.get("set-cookie"), null);
  assert.equal(res.headers.get("cache-control"), "no-store");
  assert.equal(res.headers.get("x-request-id"), "req_1");
});
test("login cookies and app credentials use separate paths", async () => {
  const fetcher = (async (_: URL, init: RequestInit) => {
    assert.equal(
      new Headers(init.headers).get("authorization"),
      "Bearer app-token",
    );
    assert.equal(new Headers(init.headers).get("cookie"), null);
    return Response.json({ ok: true });
  }) as typeof fetch;
  assert.equal(
    (
      await proxy(
        request("v1/events", {
          method: "POST",
          headers: {
            origin: config.origin,
            authorization: "Bearer app-token",
            cookie: `switchyard_session=sws_${"a".repeat(64)}`,
          },
        }),
        ["v1", "events"],
        config,
        fetcher,
      )
    ).status,
    200,
  );
  const cookie = `switchyard_session=sws_${"a".repeat(64)}; Path=/; HttpOnly; SameSite=Strict`;
  const res = await proxy(
    request("v1/session"),
    ["v1", "session"],
    config,
    (async () =>
      Response.json(
        { user: {} },
        { headers: { "set-cookie": cookie } },
      )) as typeof fetch,
  );
  assert.equal(res.headers.get("set-cookie"), cookie);
});
test("oversized streamed bodies, redirect and outage return explicit errors", async () => {
  let calls = 0;
  const fetcher = (async () => {
    calls++;
    return new Response(null, {
      status: 302,
      headers: { Location: "http://evil.test" },
    });
  }) as typeof fetch;
  const res = await proxy(
    request("v1/events", {
      method: "POST",
      headers: { origin: config.origin },
      body: "x".repeat(1024 * 1024 + 1),
    }),
    ["v1", "events"],
    config,
    fetcher,
  );
  assert.equal(res.status, 413);
  assert.equal(calls, 0);
  assert.equal(
    (await proxy(request("v1/session"), ["v1", "session"], config, fetcher))
      .status,
    502,
  );
  assert.equal(
    (
      await proxy(
        request("v1/session"),
        ["v1", "session"],
        config,
        (async () => {
          throw new Error("connection refused");
        }) as typeof fetch,
      )
    ).status,
    503,
  );
});
