import { createHmac } from "node:crypto";
import { NextRequest } from "next/server";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

const { values, jar } = vi.hoisted(() => {
  const values = new Map<string, string>();
  return {
    values,
    jar: {
      get: (key: string) => (values.has(key) ? { value: values.get(key) } : undefined),
      set: vi.fn((key: string, value: string, _options?: Record<string, unknown>) => values.set(key, value)),
    },
  };
});
vi.mock("next/headers", () => ({ cookies: async () => jar }));

import { GET, POST } from "./route";

const context = { params: Promise.resolve({ provider: "google" }) };
beforeEach(() => {
  values.clear();
  jar.set.mockClear();
  vi.stubEnv("DONOR_PUBLIC_ORIGIN", "https://dad.test");
  vi.stubEnv("DONOR_OAUTH_GOOGLE_CLIENT_ID", "web-client");
  vi.stubEnv("DONOR_OAUTH_GOOGLE_CLIENT_SECRET", "provider-secret");
});
afterEach(() => {
  vi.unstubAllGlobals();
  vi.unstubAllEnvs();
});
it("starts authorization with signed HttpOnly state, a nonce and PKCE", async () => {
  const r = await GET(new NextRequest("https://dad.test/account/oauth/google"), context);
  const url = new URL(r.headers.get("location") ?? "");
  expect(url.origin).toBe("https://accounts.google.com");
  expect(url.searchParams.get("code_challenge_method")).toBe("S256");
  expect(url.searchParams.get("nonce")).toBeTruthy();
  expect(jar.set.mock.calls[0][2]).toMatchObject({ httpOnly: true, secure: true, sameSite: "lax", maxAge: 600 });
  const [encoded, sig] = (values.get("dad_oauth_google") ?? "").split(".");
  expect(sig).toBe(
    createHmac("sha256", "provider-secret").update(Buffer.from(encoded, "base64url").toString()).digest("base64url"),
  );
});
it("rejects a forged transaction before contacting either provider or API", async () => {
  const fetch = vi.fn();
  vi.stubGlobal("fetch", fetch);
  values.set("dad_oauth_google", "forged.invalid");
  const r = await GET(new NextRequest("https://dad.test/account/oauth/google?code=secret-code&state=forged"), context);
  expect(r.headers.get("location")).toBe("https://dad.test/account/sign-in?social_error=1");
  expect(fetch).not.toHaveBeenCalled();
});
it("checks nonce then exchanges the provider proof for an HttpOnly API session", async () => {
  await GET(new NextRequest("https://dad.test/account/oauth/google"), context);
  const [encoded] = (values.get("dad_oauth_google") ?? "").split(".");
  const saved = JSON.parse(Buffer.from(encoded, "base64url").toString());
  const token = `header.${Buffer.from(JSON.stringify({ nonce: saved.nonce })).toString("base64url")}.signature`;
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(Response.json({ id_token: token }))
    .mockResolvedValueOnce(
      new Response("{}", { headers: { "set-cookie": "dad_donor_session=session; HttpOnly; Secure" } }),
    );
  vi.stubGlobal("fetch", fetch);
  const r = await GET(new NextRequest(`https://dad.test/account/oauth/google?code=code&state=${saved.state}`), context);
  expect(r.headers.get("location")).toBe("https://dad.test/account/profile");
  expect(r.headers.get("set-cookie")).toContain("HttpOnly");
  expect(JSON.parse(fetch.mock.calls[1][1].body)).toEqual({ id_token: token, nonce: saved.nonce });
  expect(values.get("dad_oauth_google")).toBe("");
});

async function begin() {
  await GET(new NextRequest("https://dad.test/account/oauth/google"), context);
  const [encoded] = (values.get("dad_oauth_google") ?? "").split(".");
  return JSON.parse(Buffer.from(encoded, "base64url").toString()) as { state: string; nonce: string };
}
const idToken = (claims: Record<string, unknown>) =>
  `header.${Buffer.from(JSON.stringify(claims)).toString("base64url")}.signature`;

it("gives every attempt a fresh random nonce", async () => {
  const nonce = async () => {
    const url = new URL(
      (await GET(new NextRequest("https://dad.test/account/oauth/google"), context)).headers.get("location") ?? "",
    );
    return url.searchParams.get("nonce");
  };
  const [first, second] = [await nonce(), await nonce()];
  expect(first).toMatch(/^[\w-]{43}$/);
  expect(second).toMatch(/^[\w-]{43}$/);
  expect(first).not.toBe(second);
});

it("sends the provider the same nonce it saved in the transaction cookie", async () => {
  const url = new URL(
    (await GET(new NextRequest("https://dad.test/account/oauth/google"), context)).headers.get("location") ?? "",
  );
  const [encoded] = (values.get("dad_oauth_google") ?? "").split(".");
  const saved = JSON.parse(Buffer.from(encoded, "base64url").toString());
  expect(url.searchParams.get("nonce")).toBe(saved.nonce);
  expect(url.searchParams.get("state")).toBe(saved.state);
});

it.each([
  ["a different nonce", { nonce: "replayed-from-another-attempt" }],
  ["no nonce claim", {}],
  ["a non-string nonce", { nonce: 7 }],
])("refuses a provider token with %s before reaching the API", async (_name, claims) => {
  const saved = await begin();
  const fetch = vi.fn().mockResolvedValueOnce(Response.json({ id_token: idToken(claims) }));
  vi.stubGlobal("fetch", fetch);
  const r = await GET(new NextRequest(`https://dad.test/account/oauth/google?code=code&state=${saved.state}`), context);
  expect(r.headers.get("location")).toBe("https://dad.test/account/sign-in?social_error=1");
  expect(fetch).toHaveBeenCalledTimes(1);
});

it("accepts a transaction once: the cookie is spent, so a replayed callback cannot sign in", async () => {
  const saved = await begin();
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(Response.json({ id_token: idToken({ nonce: saved.nonce }) }))
    .mockResolvedValueOnce(new Response("{}"));
  vi.stubGlobal("fetch", fetch);
  const url = `https://dad.test/account/oauth/google?code=code&state=${saved.state}`;
  expect((await GET(new NextRequest(url), context)).headers.get("location")).toBe("https://dad.test/account/profile");
  expect(values.get("dad_oauth_google")).toBe("");
  const again = await GET(new NextRequest(url), context);
  expect(again.headers.get("location")).toBe("https://dad.test/account/sign-in?social_error=1");
  expect(fetch).toHaveBeenCalledTimes(2);
});

it("refuses a callback whose state does not match the transaction", async () => {
  await begin();
  const fetch = vi.fn();
  vi.stubGlobal("fetch", fetch);
  const r = await GET(new NextRequest("https://dad.test/account/oauth/google?code=code&state=attacker"), context);
  expect(r.headers.get("location")).toBe("https://dad.test/account/sign-in?social_error=1");
  expect(fetch).not.toHaveBeenCalled();
});

const apple = { params: Promise.resolve({ provider: "apple" }) };
const social = "https://dad.test/account/sign-in?social_error=1";

it("starts Apple sign-in with form_post, a cross-site cookie and no PKCE", async () => {
  vi.stubEnv("DONOR_OAUTH_APPLE_CLIENT_ID", "apple-client");
  vi.stubEnv("DONOR_OAUTH_APPLE_CLIENT_SECRET", "apple-secret");
  const r = await GET(new NextRequest("https://dad.test/account/oauth/apple"), apple);
  const url = new URL(r.headers.get("location") ?? "");
  expect(url.origin).toBe("https://appleid.apple.com");
  expect(url.searchParams.get("response_mode")).toBe("form_post");
  expect(url.searchParams.has("code_challenge")).toBe(false);
  expect(jar.set.mock.calls[0][2]).toMatchObject({ sameSite: "none", path: "/account/oauth/apple" });
});

it("completes Apple sign-in from the form_post callback", async () => {
  vi.stubEnv("DONOR_OAUTH_APPLE_CLIENT_ID", "apple-client");
  vi.stubEnv("DONOR_OAUTH_APPLE_CLIENT_SECRET", "apple-secret");
  await GET(new NextRequest("https://dad.test/account/oauth/apple"), apple);
  const [encoded] = (values.get("dad_oauth_apple") ?? "").split(".");
  const saved = JSON.parse(Buffer.from(encoded, "base64url").toString());
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(Response.json({ id_token: idToken({ nonce: saved.nonce }) }))
    .mockResolvedValueOnce(new Response("{}"));
  vi.stubGlobal("fetch", fetch);
  const r = await POST(
    new NextRequest("https://dad.test/account/oauth/apple", {
      method: "POST",
      body: new URLSearchParams({ code: "code", state: saved.state }),
    }),
    apple,
  );
  expect(r.status).toBe(303);
  expect(r.headers.get("location")).toBe("https://dad.test/account/profile");
  expect(fetch.mock.calls[0][0]).toBe("https://appleid.apple.com/auth/token");
  expect(String(fetch.mock.calls[0][1].body)).not.toContain("code_verifier");
});

it("returns 404 for an unknown provider", async () => {
  const r = await GET(new NextRequest("https://dad.test/account/oauth/github"), {
    params: Promise.resolve({ provider: "github" }),
  });
  expect(r.status).toBe(404);
});

it("reports sign-in as unconfigured without an origin", async () => {
  vi.stubEnv("DONOR_PUBLIC_ORIGIN", "");
  const r = await GET(new NextRequest("https://dad.test/account/oauth/google"), context);
  expect(r.status).toBe(503);
});

it("sends a donor back to sign-in when the provider is not configured", async () => {
  vi.stubEnv("DONOR_OAUTH_GOOGLE_CLIENT_ID", "");
  const r = await GET(new NextRequest("https://dad.test/account/oauth/google"), context);
  expect(r.status).toBe(303);
  expect(r.headers.get("location")).toBe("https://dad.test/account/sign-in?social_error=unavailable");
  expect(jar.set).not.toHaveBeenCalled();
});

it("refuses an oversized callback body", async () => {
  const r = await POST(
    new NextRequest("https://dad.test/account/oauth/apple", { method: "POST", headers: { "content-length": "16385" } }),
    apple,
  );
  expect(r.status).toBe(413);
});

it("returns the donor to sign-in when the provider reports an error", async () => {
  const saved = await begin();
  const fetch = vi.fn();
  vi.stubGlobal("fetch", fetch);
  const r = await GET(
    new NextRequest(`https://dad.test/account/oauth/google?error=access_denied&state=${saved.state}`),
    context,
  );
  expect(r.headers.get("location")).toBe(social);
  expect(fetch).not.toHaveBeenCalled();
});

it("refuses an oversized chunked callback body that declares no length", async () => {
  const fetch = vi.fn();
  vi.stubGlobal("fetch", fetch);
  const chunk = new TextEncoder().encode("x".repeat(9000));
  const body = new ReadableStream({
    start(controller) {
      controller.enqueue(chunk);
      controller.enqueue(chunk);
      controller.close();
    },
  });
  const request = new NextRequest("https://dad.test/account/oauth/google", {
    method: "POST",
    body,
    duplex: "half",
  } as RequestInit);
  expect(request.headers.get("content-length")).toBeNull();
  expect((await POST(request, context)).status).toBe(413);
  expect(fetch).not.toHaveBeenCalled();
});

it("refuses to start Apple sign-in on an origin that cannot keep its cross-site cookie", async () => {
  vi.stubEnv("DONOR_PUBLIC_ORIGIN", "http://dad.test");
  vi.stubEnv("DONOR_OAUTH_APPLE_CLIENT_ID", "apple-client");
  vi.stubEnv("DONOR_OAUTH_APPLE_CLIENT_SECRET", "apple-secret");
  const r = await GET(new NextRequest("http://dad.test/account/oauth/apple"), apple);
  expect(r.status).toBe(503);
  expect(jar.set).not.toHaveBeenCalled();
});

it("still starts Google sign-in on a plain http origin for local development", async () => {
  vi.stubEnv("DONOR_PUBLIC_ORIGIN", "http://dad.test");
  const r = await GET(new NextRequest("http://dad.test/account/oauth/google"), context);
  expect(r.status).toBe(307);
  expect(jar.set.mock.calls[0][2]).toMatchObject({ secure: false, sameSite: "lax" });
});
