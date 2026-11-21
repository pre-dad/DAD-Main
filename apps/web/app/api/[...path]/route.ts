import type { NextRequest } from "next/server";
import { trustedClientIp } from "@/lib/clientIp";
import { donorApiBase } from "@/lib/donorApi";

/**
 * Runtime proxy to the donor API. A route handler rather than a rewrite: with
 * `output: standalone` a rewrite destination is fixed at build time, so one image could not
 * serve several environments.
 *
 * The proxy exists so the session cookie is first-party. Sent cross-origin it would need
 * SameSite=None, which requires HTTPS and would make the cookie usable from any site.
 */
export const dynamic = "force-dynamic";

// Only what the API reads crosses the boundary; anything else a browser or an
// intermediary adds (including spoofable forwarding headers) is dropped.
const REQUEST_HEADERS = [
  "accept",
  "accept-language",
  "content-type",
  "cookie",
  "idempotency-key",
  "origin",
  "sec-fetch-site",
  "stripe-signature",
  "user-agent",
];

// Response headers not relayed as they are: hop-by-hop ones, a length and encoding that
// fetch has already undone, and Set-Cookie, which is appended separately below.
const HOP_BY_HOP = new Set([
  "connection",
  "keep-alive",
  "transfer-encoding",
  "upgrade",
  "proxy-authenticate",
  "proxy-authorization",
  "te",
  "trailer",
  "content-length",
  "content-encoding",
  "set-cookie",
]);

// Bodies are buffered rather than streamed: undici cannot hand back a response that arrives
// before a streamed request body is consumed, so an early 401 or 422 would surface as an
// unreachable API. Each route is held to the most the API accepts there, so a sign-in or
// analytics POST cannot pin megabytes of memory per connection.
const DEFAULT_BODY_BYTES = 1 << 20;
const LARGE_BODIES: Record<string, number> = {
  "donor/staff/content/sources": 10 << 20,
  "donor/staff/content/rules": 10 << 20,
  "donor/staff/founding-claims": 5 << 20,
};

const bodyLimit = (path: string[]) => LARGE_BODIES[path.join("/")] ?? DEFAULT_BODY_BYTES;

const publicRecord = "[a-fA-F0-9]{8}(?:-[a-fA-F0-9]{4}){3}-[a-fA-F0-9]{12}";
const publicRead = new RegExp(`^(?:events/${publicRecord}(?:/calendar\\.ics)?|initiatives/${publicRecord}/spend)$`);

// Counted as it arrives, since a chunked body declares no length.
async function readBody(request: NextRequest, limit: number): Promise<Uint8Array<ArrayBuffer> | null> {
  if (Number(request.headers.get("content-length") ?? 0) > limit) return null;
  const chunks: Uint8Array[] = [];
  let size = 0;
  const reader = request.body?.getReader();
  for (let read = await reader?.read(); read && !read.done; read = await reader?.read()) {
    size += read.value.byteLength;
    if (size > limit) {
      await reader?.cancel().catch(() => {});
      return null;
    }
    chunks.push(read.value);
  }
  const body = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) {
    body.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return body;
}

async function proxy(request: NextRequest, context: { params: Promise<{ path: string[] }> }) {
  const { path } = await context.params;
  const allowed =
    path[0] === "donor" ||
    (request.method === "POST" && path.join("/") === "analytics/events") ||
    ((request.method === "GET" || request.method === "HEAD") && publicRead.test(path.join("/")));
  if (!allowed || path.some((segment) => segment === "." || segment === ".." || /[\\/?#%]/.test(segment))) {
    return Response.json({ detail: "Not found" }, { status: 404 });
  }
  const target = new URL(`${donorApiBase()}/${path.map(encodeURIComponent).join("/")}`);
  target.search = request.nextUrl.search;

  const body =
    request.method === "GET" || request.method === "HEAD" ? undefined : await readBody(request, bodyLimit(path));
  if (body === null) return Response.json({ detail: "Request body is too large." }, { status: 413 });
  const requestHeaders = new Headers();
  for (const name of REQUEST_HEADERS) {
    const value = request.headers.get(name);
    if (value !== null) requestHeaders.set(name, value);
  }
  const client = trustedClientIp(request.headers);
  if (client) requestHeaders.set("x-real-ip", client);
  let response: Response;
  try {
    response = await fetch(target, {
      method: request.method,
      headers: requestHeaders,
      body,
      redirect: "manual",
      signal: AbortSignal.any([request.signal, AbortSignal.timeout(28_000)]),
    });
  } catch {
    return Response.json(
      {
        detail: {
          code: "API_UNREACHABLE",
          message: "The requested service could not be reached.",
        },
      },
      { status: 502 },
    );
  }

  const headers = new Headers();
  response.headers.forEach((value, key) => {
    if (!HOP_BY_HOP.has(key)) headers.set(key, value);
  });
  // Appended one by one: a plain Headers copy would fold several Set-Cookie values into one.
  for (const cookie of response.headers.getSetCookie()) headers.append("set-cookie", cookie);

  return new Response(response.body, { status: response.status, headers });
}

export const GET = proxy;
export const POST = proxy;
export const PUT = proxy;
export const PATCH = proxy;
export const DELETE = proxy;
export const HEAD = proxy;
export const OPTIONS = proxy;
