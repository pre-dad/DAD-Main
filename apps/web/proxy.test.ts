// @vitest-environment node

import { unstable_doesMiddlewareMatch } from "next/experimental/testing/server";
import { NextRequest } from "next/server";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import nextConfig from "./next.config";
import { config, proxy } from "./proxy";

const STAFF_ORIGIN = "http://staff.localhost:3000";
beforeEach(() => vi.stubEnv("DONOR_STAFF_ORIGIN", STAFF_ORIGIN));
afterEach(() => vi.unstubAllEnvs());

/** A request to the host that serves path: staff pages on the staff host, the rest on the public one. */
function request(path: string) {
  const origin = /^\/staff/i.test(path) ? STAFF_ORIGIN : "http://127.0.0.1:3000";
  return new NextRequest(`${origin}${path}`, { headers: { host: new URL(origin).host } });
}

function respond(path: string) {
  const response = proxy(request(path));
  const policy = response.headers.get("Content-Security-Policy");
  if (!policy) throw new Error("Missing policy");
  return { response, policy };
}
const directive = (policy: string, name: string) =>
  policy
    .split(";")
    .map((part) => part.trim())
    .find((part) => part.startsWith(`${name} `)) ?? "";
const nonceOf = (policy: string) => /'nonce-([^']+)'/.exec(directive(policy, "script-src"))?.[1];

describe("signed-in surface policy", () => {
  it("allows scripts only by nonce, and lets those scripts load the rest", () => {
    const scripts = directive(respond("/account/dashboard").policy, "script-src");
    expect(scripts).toMatch(/^script-src 'self' 'nonce-[A-Za-z0-9+/=]{24,}' 'strict-dynamic'$/);
    expect(scripts).not.toContain("'unsafe-inline'");
    expect(scripts).not.toContain("'unsafe-eval'");
  });

  it("issues a fresh nonce for every request", () => {
    const nonces = new Set(Array.from({ length: 20 }, () => nonceOf(respond("/staff/sign-in").policy)));
    expect(nonces.size).toBe(20);
  });

  it("hands the same policy to rendering, which is where Next reads the nonce", () => {
    const { response, policy } = respond("/staff");
    expect(response.headers.get("x-middleware-request-content-security-policy")).toBe(policy);
  });

  it.each([
    "default-src 'self'",
    "connect-src 'self'",
    "frame-ancestors 'none'",
    "object-src 'none'",
    "base-uri 'self'",
    "form-action 'self'",
  ])("keeps %s", (expected) => {
    expect(respond("/account/dashboard").policy.split("; ")).toContain(expected);
  });

  it("permits eval only for the development server", () => {
    vi.stubEnv("NODE_ENV", "development");
    expect(directive(respond("/account/dashboard").policy, "script-src")).toContain("'unsafe-eval'");
  });

  it("upgrades insecure requests only for an HTTPS origin", () => {
    vi.stubEnv("DONOR_PUBLIC_ORIGIN", "https://donors.example.org");
    expect(respond("/account/dashboard").policy).toContain("upgrade-insecure-requests");
    vi.stubEnv("DONOR_PUBLIC_ORIGIN", "http://127.0.0.1:3000");
    expect(respond("/account/dashboard").policy).not.toContain("upgrade-insecure-requests");
  });
});

describe("hosts", () => {
  const visit = (url: string, headers: Record<string, string> = {}) =>
    proxy(new NextRequest(url, { headers: { host: new URL(url).host, ...headers } }));
  const answer = (response: Response) =>
    response.status === 404
      ? "404"
      : response.headers.has("location")
        ? `redirect ${response.headers.get("location")}`
        : response.headers.get("x-middleware-request-x-dad-unserved") === "staff"
          ? "staff not found"
          : response.headers.get("x-middleware-request-x-dad-unserved") === "public"
            ? "public not found"
            : "served";
  const staff = "https://staff.dad.example.org";
  const site = "https://dad.example.org";

  it.each([
    [`${staff}/staff`, "served"],
    [`${staff}/staff/sign-in`, "served"],
    [`${staff}/staff/donors/abc`, "served"],
    [`${staff}/api/donor/staff/me`, "served"],
    [`${staff}/api/donor/staff/sign-in/options`, "served"],
    [`${staff}/_next/data/build/staff.json`, "served"],
    [`${staff}/icon.svg?v=1`, "served"],
    [`${staff}/icon.svg/extra`, "staff not found"],
    [`${site}/icon.svg`, "served"],
    [`${staff}/`, `redirect ${staff}/staff`],
    [`${staff}/spend`, "staff not found"],
    [`${staff}/account/dashboard`, "staff not found"],
    [`${staff}/Staff/sign-in`, "staff not found"],
    [`${staff}/api/donor/me`, "404"],
    [`${staff}/api/donor/stripe/webhook`, "404"],
    [`${site}/`, "served"],
    [`${site}/account/dashboard`, "served"],
    [`${site}/api/donor/me`, "served"],
    [`${site}/staff`, "public not found"],
    [`${site}/Staff/sign-in`, "public not found"],
    [`${site}/api/donor/staff/me`, "404"],
    [`${site}/api/donor/staff/sign-in`, "404"],
    [`${site}/api/donor/%73taff/me`, "404"],
    [`${site}/api/donor/%53tAfF/me`, "404"],
    [`${site}/api/%64onor/staff/me`, "404"],
    [`${site}/%73taff/sign-in`, "public not found"],
    [`${site}/api/donor/%zz`, "404"],
    [`${staff}/api/donor/%73taff/me`, "served"],
    [`${staff}/api/donor/%6De`, "404"],
    [`${staff}/%6Cedger`, "staff not found"],
  ])("answers %s with %s", (url, expected) => {
    vi.stubEnv("DONOR_STAFF_ORIGIN", staff);
    expect(answer(visit(url))).toBe(expected);
  });

  it.each([
    ["STAFF.DAD.EXAMPLE.ORG", "served"],
    ["staff.dad.example.org:443", "served"],
    ["staff.dad.example.org.", "public not found"],
    ["staff.dad.example.org%2f@dad.example.org", "public not found"],
    ["dad.example.org", "public not found"],
  ])("decides the host from a Host header of %s, not from the URL", (host, expected) => {
    vi.stubEnv("DONOR_STAFF_ORIGIN", staff);
    expect(answer(visit(`${staff}/staff/sign-in`, { host }))).toBe(expected);
  });

  it("keeps a staff Host header on the staff surface even when the URL names the public host", () => {
    vi.stubEnv("DONOR_STAFF_ORIGIN", staff);
    expect(answer(visit(`${site}/staff/sign-in`, { host: "staff.dad.example.org" }))).toBe("served");
    expect(answer(visit(`${site}/api/donor/me`, { host: "staff.dad.example.org" }))).toBe("404");
  });

  it("serves no back office anywhere until a staff origin is set", () => {
    vi.stubEnv("DONOR_STAFF_ORIGIN", "");
    expect(answer(visit(`${site}/staff/sign-in`))).toBe("public not found");
    expect(answer(visit(`${STAFF_ORIGIN}/staff/sign-in`))).toBe("public not found");
  });

  it("keeps the staff host out of search engines", async () => {
    vi.stubEnv("DONOR_STAFF_ORIGIN", staff);
    expect(visit(`${staff}/staff/sign-in`).headers.get("X-Robots-Tag")).toBe("noindex, nofollow");
    expect(await visit(`${staff}/robots.txt`).text()).toBe("User-agent: *\nDisallow: /\n");
  });

  it("gives each not-found page the policy it renders under", () => {
    vi.stubEnv("DONOR_STAFF_ORIGIN", staff);
    const account = visit(`${staff}/account/dashboard`);
    expect(account.headers.get("Content-Security-Policy")).toMatch(/'nonce-[^']+' 'strict-dynamic'/);
    expect(account.headers.get("X-Robots-Tag")).toBe("noindex, nofollow");
    // next.config.ts already gives /spend the static policy, which the page renders under.
    const spend = visit(`${staff}/spend`);
    expect(spend.headers.has("Content-Security-Policy")).toBe(false);
    expect(spend.headers.get("X-Robots-Tag")).toBe("noindex, nofollow");
    // The public not-found page is prerendered, so it has no nonce to carry.
    expect(visit(`${site}/Staff/sign-in`).headers.get("Content-Security-Policy")).toContain(
      "script-src 'self' 'unsafe-inline'",
    );
  });

  it("drops a client's claim that a page is unserved", () => {
    vi.stubEnv("DONOR_STAFF_ORIGIN", staff);
    expect(answer(visit(`${staff}/staff`, { "x-dad-unserved": "public" }))).toBe("served");
    expect(answer(visit(`${site}/spend`, { "x-dad-unserved": "staff" }))).toBe("served");
  });

  it("upgrades insecure requests by the origin of the host answering", () => {
    vi.stubEnv("DONOR_PUBLIC_ORIGIN", "http://localhost:3000");
    vi.stubEnv("DONOR_STAFF_ORIGIN", staff);
    expect(visit(`${staff}/staff`).headers.get("Content-Security-Policy")).toContain("upgrade-insecure-requests");
    expect(visit("http://localhost:3000/account").headers.get("Content-Security-Policy")).not.toContain(
      "upgrade-insecure-requests",
    );
  });
});

describe("proxy scope", () => {
  const sets = (url: string, headers?: Record<string, string>) =>
    unstable_doesMiddlewareMatch({ config, nextConfig, url, headers }) &&
    proxy(request(url)).headers.has("Content-Security-Policy");

  it.each([
    "/account",
    "/account/dashboard",
    "/account/sign-in",
    "/partner",
    "/partner/sign-in",
    "/partner/accept",
    "/Partner/sign-in",
    "/staff",
    "/staff/sign-in",
    "/staff/donors/abc",
    "/Staff/sign-in",
  ])("sets the nonce policy on %s", (url) => {
    expect(sets(url)).toBe(true);
  });

  it.each(["/", "/spend", "/accounts", "/partners", "/apply", "/staffing", "/api/donor/me", "/_next/static/chunk.js"])(
    "leaves %s to the static policy",
    (url) => {
      expect(sets(url)).toBe(false);
    },
  );

  it.each([
    ["next-router-prefetch", "1"],
    ["purpose", "prefetch"],
  ])("keeps the policy when a document request carries %s", (name, value) => {
    const headers = { [name]: value };
    expect(sets("/account/dashboard", headers)).toBe(true);
    expect(sets("/staff/sign-in", headers)).toBe(true);
  });
});
