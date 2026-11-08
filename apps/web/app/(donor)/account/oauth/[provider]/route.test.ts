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

import { GET } from "./route";

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

it("binds a fresh random nonce to every attempt and sends the same one to the provider", async () => {
  const first = new URL(
    (await GET(new NextRequest("https://dad.test/account/oauth/google"), context)).headers.get("location") ?? "",
  );
  const saved = await begin();
  expect(first.searchParams.get("nonce")).toMatch(/^[\w-]{43}$/);
  expect(first.searchParams.get("nonce")).not.toBe(saved.nonce);
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
