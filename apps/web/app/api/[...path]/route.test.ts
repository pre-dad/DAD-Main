// @vitest-environment node
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import { NextRequest } from "next/server";
import { afterEach, expect, it, vi } from "vitest";
import { GET, POST } from "./route";

afterEach(() => {
  vi.unstubAllGlobals();
  vi.unstubAllEnvs();
});

it("strips untrusted client address headers at the browser boundary", async () => {
  vi.stubEnv("DONOR_CLIENT_IP_HEADER", "");
  const fetch = vi.fn().mockResolvedValue(Response.json({ ok: true }));
  vi.stubGlobal("fetch", fetch);
  await GET(
    new NextRequest("http://localhost:3000/api/donor/public/spend", {
      headers: { "X-Real-IP": "203.0.113.1", "X-Forwarded-For": "203.0.113.2" },
    }),
    { params: Promise.resolve({ path: ["donor", "public", "spend"] }) },
  );
  const headers = new Headers(fetch.mock.calls[0][1].headers);
  expect(headers.has("x-real-ip")).toBe(false);
  expect(headers.has("x-forwarded-for")).toBe(false);
});

it.each(["203.0.113.1", "2001:db8::1", "203.0.113.1, 203.0.113.2", "unknown"])(
  "forwards only a single IP from the configured ingress header: %s",
  async (address) => {
    vi.stubEnv("DONOR_CLIENT_IP_HEADER", "x-ingress-client");
    const fetch = vi.fn().mockResolvedValue(Response.json({ ok: true }));
    vi.stubGlobal("fetch", fetch);
    await GET(
      new NextRequest("http://localhost:3000/api/donor/public/spend", {
        headers: { "X-Real-IP": "192.0.2.99", "x-ingress-client": address },
      }),
      { params: Promise.resolve({ path: ["donor", "public", "spend"] }) },
    );
    expect(new Headers(fetch.mock.calls[0][1].headers).get("x-real-ip")).toBe(
      ["203.0.113.1", "2001:db8::1"].includes(address) ? address : null,
    );
  },
);

const context = (...path: string[]) => ({ params: Promise.resolve({ path }) });

const publicID = "40fd6316-889e-4cf0-b5df-81b589a390b3";

it.each([
  ["events", publicID],
  ["events", publicID, "calendar.ics"],
  ["initiatives", publicID, "spend"],
])("relays the published read at %j", async (...path) => {
  vi.stubEnv("DONOR_API_BASE_URL", "http://api.test:8002");
  const fetch = vi.fn().mockResolvedValue(new Response("published", { status: 200 }));
  vi.stubGlobal("fetch", fetch);
  const response = await GET(new NextRequest(`http://localhost:3000/api/${path.join("/")}`), context(...path));
  expect(response.status).toBe(200);
  expect(await response.text()).toBe("published");
  expect(fetch.mock.calls[0][0].href).toBe(`http://api.test:8002/${path.join("/")}`);
});

it.each([
  ["events"],
  ["events", publicID, "check-in"],
  ["events", publicID, "attendees"],
  ["initiatives", publicID],
  ["initiatives", publicID, "metrics"],
  ["analytics", "events"],
])("keeps unpublished proxy paths closed: %j", async (...path) => {
  const fetch = vi.fn();
  vi.stubGlobal("fetch", fetch);
  expect((await GET(new NextRequest("http://localhost:3000/api/x"), context(...path))).status).toBe(404);
  expect(fetch).not.toHaveBeenCalled();
});

it("refuses mutations through the public event read proxy", async () => {
  const fetch = vi.fn();
  vi.stubGlobal("fetch", fetch);
  const response = await POST(
    new NextRequest(`http://localhost:3000/api/events/${publicID}`, { method: "POST", body: "{}" }),
    context("events", publicID),
  );
  expect(response.status).toBe(404);
  expect(fetch).not.toHaveBeenCalled();
});

it.each([
  [["claims"]],
  [["staff", "donor"]],
  [["donor", "..", "admin"]],
  [["donor", "."]],
  [["donor", "me%2F..%2Fadmin"]],
  [["donor", "me/../admin"]],
  [["donor", "a%2Fb"]],
  [["donor", "a\\b"]],
])("refuses %j without contacting the API", async (path) => {
  const fetch = vi.fn();
  vi.stubGlobal("fetch", fetch);
  const response = await GET(new NextRequest("http://localhost:3000/api/x"), context(...path));
  expect(response.status).toBe(404);
  expect(fetch).not.toHaveBeenCalled();
});

it("forwards only the allowlisted request headers", async () => {
  vi.stubEnv("DONOR_CLIENT_IP_HEADER", "");
  vi.stubEnv("DONOR_API_BASE_URL", "http://api.test:8002");
  const fetch = vi.fn().mockResolvedValue(Response.json({ ok: true }));
  vi.stubGlobal("fetch", fetch);
  await POST(
    new NextRequest("http://localhost:3000/api/donor/checkouts", {
      method: "POST",
      body: "{}",
      headers: {
        cookie: "dad_session=s",
        "content-type": "application/json",
        accept: "application/json",
        "accept-language": "en-US",
        "idempotency-key": "k",
        origin: "http://localhost:3000",
        "user-agent": "browser",
        "stripe-signature": "t=1,v1=x",
        "sec-fetch-site": "same-origin",
        "sec-fetch-mode": "cors",
        authorization: "Bearer stolen",
        "x-dad-client": "native",
        "x-forwarded-host": "evil.example",
        "x-staff-role": "SUPER_ADMIN",
        forwarded: "for=1.2.3.4",
        host: "evil.example",
      },
    }),
    context("donor", "checkouts"),
  );
  const sent = new Headers(fetch.mock.calls[0][1].headers);
  expect(Object.fromEntries(sent)).toEqual({
    cookie: "dad_session=s",
    "content-type": "application/json",
    accept: "application/json",
    "accept-language": "en-US",
    "idempotency-key": "k",
    origin: "http://localhost:3000",
    "user-agent": "browser",
    "stripe-signature": "t=1,v1=x",
    "sec-fetch-site": "same-origin",
  });
  expect(fetch.mock.calls[0][0].href).toBe("http://api.test:8002/donor/checkouts");
});

it("preserves every Set-Cookie and drops hop-by-hop response headers", async () => {
  const upstream = new Headers({ "content-type": "application/json", connection: "keep-alive", "x-request-id": "r1" });
  upstream.append("set-cookie", "dad_session=a; Path=/; HttpOnly");
  upstream.append("set-cookie", "dad_referral=b; Path=/; HttpOnly");
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("{}", { status: 201, headers: upstream })));
  const response = await GET(new NextRequest("http://localhost:3000/api/donor/me"), context("donor", "me"));
  expect(response.status).toBe(201);
  expect(response.headers.getSetCookie()).toEqual([
    "dad_session=a; Path=/; HttpOnly",
    "dad_referral=b; Path=/; HttpOnly",
  ]);
  expect(response.headers.get("x-request-id")).toBe("r1");
  expect(response.headers.has("connection")).toBe(false);
});

it("answers 502 with a structured error when the API is unreachable", async () => {
  vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new TypeError("fetch failed")));
  const response = await GET(new NextRequest("http://localhost:3000/api/donor/me"), context("donor", "me"));
  expect(response.status).toBe(502);
  expect(await response.json()).toEqual({
    detail: { code: "API_UNREACHABLE", message: "The requested service could not be reached." },
  });
});

it("relays an upstream rejection of a request body instead of reporting the API unreachable", async () => {
  // A real HTTP upstream that answers before reading the body, as the API does for a bad session.
  const upstream = createServer((_request, response) => {
    response.writeHead(401, { "content-type": "application/json" });
    response.end(JSON.stringify({ detail: "This link has expired." }));
  });
  await new Promise<void>((resolve) => upstream.listen(0, "127.0.0.1", resolve));
  try {
    vi.stubEnv("DONOR_API_BASE_URL", `http://127.0.0.1:${(upstream.address() as AddressInfo).port}`);
    const response = await POST(
      new NextRequest("http://localhost:3000/api/donor/claim", {
        method: "POST",
        body: JSON.stringify({ token: "x".repeat(64 * 1024) }),
        headers: { "content-type": "application/json" },
      }),
      context("donor", "claim"),
    );
    expect(response.status).toBe(401);
    expect(await response.json()).toEqual({ detail: "This link has expired." });
  } finally {
    upstream.close();
  }
});

it("refuses a body larger than the API accepts without contacting it", async () => {
  const fetch = vi.fn();
  vi.stubGlobal("fetch", fetch);
  const response = await POST(
    new NextRequest("http://localhost:3000/api/donor/staff/content/sources", {
      method: "POST",
      body: new Uint8Array((10 << 20) + 1),
    }),
    context("donor", "staff", "content", "sources"),
  );
  expect(response.status).toBe(413);
  expect(fetch).not.toHaveBeenCalled();
});

it("refuses an oversized body that declares no length", async () => {
  const fetch = vi.fn();
  vi.stubGlobal("fetch", fetch);
  const chunk = new Uint8Array(1 << 20);
  let sent = 0;
  const body = new ReadableStream<Uint8Array>({
    pull(controller) {
      if (sent++ > 12) controller.close();
      else controller.enqueue(chunk);
    },
  });
  const request = new NextRequest("http://localhost:3000/api/donor/staff/content/sources", {
    method: "POST",
    body,
    duplex: "half",
  } as ConstructorParameters<typeof NextRequest>[1]);
  expect(request.headers.get("content-length")).toBeNull();
  const response = await POST(request, context("donor", "staff", "content", "sources"));
  expect(response.status).toBe(413);
  expect(fetch).not.toHaveBeenCalled();
  expect(sent).toBeLessThan(13);
});

it("relays the analytics event POST with its body", async () => {
  vi.stubEnv("DONOR_API_BASE_URL", "http://api.test:8002");
  const fetch = vi.fn().mockResolvedValue(new Response(null, { status: 204 }));
  vi.stubGlobal("fetch", fetch);
  const response = await POST(
    new NextRequest("http://localhost:3000/api/analytics/events", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: '{"name":"view"}',
    }),
    context("analytics", "events"),
  );
  expect(response.status).toBe(204);
  expect(String(fetch.mock.calls[0][0])).toBe("http://api.test:8002/analytics/events");
  expect(fetch.mock.calls[0][1].method).toBe("POST");
  expect(new TextDecoder().decode(fetch.mock.calls[0][1].body)).toBe('{"name":"view"}');
});

it("forwards the query string to the upstream URL", async () => {
  vi.stubEnv("DONOR_API_BASE_URL", "http://api.test:8002");
  const fetch = vi.fn().mockResolvedValue(Response.json({ ok: true }));
  vi.stubGlobal("fetch", fetch);
  await GET(
    new NextRequest("http://localhost:3000/api/donor/public/spend?a=b&c=d"),
    context("donor", "public", "spend"),
  );
  expect(String(fetch.mock.calls[0][0])).toBe("http://api.test:8002/donor/public/spend?a=b&c=d");
});

it("passes an upstream redirect through without following it", async () => {
  vi.stubEnv("DONOR_API_BASE_URL", "http://api.test:8002");
  const fetch = vi
    .fn()
    .mockResolvedValue(new Response(null, { status: 302, headers: { location: "https://elsewhere.example/" } }));
  vi.stubGlobal("fetch", fetch);
  const response = await GET(new NextRequest("http://localhost:3000/api/donor/login"), context("donor", "login"));
  expect(fetch.mock.calls[0][1].redirect).toBe("manual");
  expect(response.status).toBe(302);
  expect(response.headers.get("location")).toBe("https://elsewhere.example/");
});

it("answers 502 when the upstream times out", async () => {
  vi.stubEnv("DONOR_API_BASE_URL", "http://api.test:8002");
  vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new DOMException("The operation timed out.", "TimeoutError")));
  const response = await GET(new NextRequest("http://localhost:3000/api/donor/me"), context("donor", "me"));
  expect(response.status).toBe(502);
  expect((await response.json()).detail.code).toBe("API_UNREACHABLE");
});

it("holds an ordinary route to a small body but lets the upload routes take what the API accepts", async () => {
  const fetch = vi.fn().mockResolvedValue(Response.json({}));
  vi.stubGlobal("fetch", fetch);
  const post = (size: number, ...path: string[]) =>
    POST(
      new NextRequest(`http://localhost:3000/api/${path.join("/")}`, { method: "POST", body: new Uint8Array(size) }),
      context(...path),
    );
  expect((await post((1 << 20) + 1, "donor", "sign-in", "email")).status).toBe(413);
  expect(fetch).not.toHaveBeenCalled();
  expect((await post(1 << 20, "donor", "sign-in", "email")).status).toBe(200);
  expect((await post((5 << 20) + 1, "donor", "staff", "founding-claims")).status).toBe(413);
  expect((await post(5 << 20, "donor", "staff", "founding-claims")).status).toBe(200);
  expect((await post(10 << 20, "donor", "staff", "content", "rules")).status).toBe(200);
  expect((await post(10 << 20, "donor", "staff", "content", "sources")).status).toBe(200);
});
