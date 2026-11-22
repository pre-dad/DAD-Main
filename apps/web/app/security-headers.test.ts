/**
 * The headers are configuration, and configuration rots quietly: nothing fails when one
 * is dropped, the page still renders, and the protection is simply gone. This asserts the
 * ones that were missing entirely until they were added.
 */

// Next exposes no public path matcher; this mirrors how it matches header sources.
import { getPathMatch } from "next/dist/shared/lib/router/utils/path-match";
import { unstable_doesMiddlewareMatch } from "next/experimental/testing/server";
import { NextRequest } from "next/server";
import { afterEach, describe, expect, it, vi } from "vitest";
import nextConfig from "../next.config";
import { proxy, config as proxyConfig } from "../proxy";

async function headerMap(): Promise<Map<string, string>> {
  const groups = await configuredHeaders();
  const all = groups.flatMap((group) => group.headers);
  return new Map(all.map((header) => [header.key, header.value]));
}

async function configuredHeaders() {
  if (!nextConfig.headers) throw new Error("Missing security headers configuration");
  return nextConfig.headers();
}

describe("security headers", () => {
  it("applies to every path rather than a subset", async () => {
    const [everywhere] = await configuredHeaders();
    expect(everywhere.source).toBe("/:path*");
    expect(everywhere.headers.map((header) => header.key)).toEqual(
      expect.arrayContaining([
        "X-Content-Type-Options",
        "X-Frame-Options",
        "Referrer-Policy",
        "Permissions-Policy",
        "Strict-Transport-Security",
      ]),
    );
  });

  it.each([
    ["http://127.0.0.1:3000", "/"],
    ["http://127.0.0.1:3000", "/spend"],
    ["http://127.0.0.1:3000", "/donate"],
    ["http://127.0.0.1:3000", "/r/code"],
    ["http://127.0.0.1:3000", "/api/donor/me"],
    ["http://127.0.0.1:3000", "/accounts"],
    ["http://127.0.0.1:3000", "/account"],
    ["http://127.0.0.1:3000", "/account/dashboard"],
    ["http://127.0.0.1:3000", "/Account/dashboard"],
    ["http://127.0.0.1:3000", "/partner"],
    ["http://127.0.0.1:3000", "/partner/sign-in"],
    ["http://127.0.0.1:3000", "/partner/accept"],
    ["http://127.0.0.1:3000", "/Partner/sign-in"],
    ["http://127.0.0.1:3000", "/partners"],
    ["http://127.0.0.1:3000", "/apply"],
    ["http://127.0.0.1:3000", "/staff"],
    ["http://staff.localhost:3000", "/staff"],
    ["http://staff.localhost:3000", "/staff/sign-in"],
    ["http://staff.localhost:3000", "/staff/donors/abc"],
    ["http://staff.localhost:3000", "/account/dashboard"],
    ["http://staff.localhost:3000", "/partner"],
    ["http://staff.localhost:3000", "/spend"],
  ])("gives %s%s exactly one content security policy", async (origin, path) => {
    // Browsers enforce every CSP header they receive, so the static policy must stay off
    // the paths where proxy.ts sets a nonce policy, and no path may be left with neither.
    vi.stubEnv("DONOR_STAFF_ORIGIN", "http://staff.localhost:3000");
    const groups = (await configuredHeaders()).filter((group) =>
      group.headers.some((header) => header.key === "Content-Security-Policy"),
    );
    const fromConfig = groups.filter((group) => getPathMatch(group.source, { strict: true })(path) !== false).length;
    const fromProxy =
      unstable_doesMiddlewareMatch({ config: proxyConfig, nextConfig, url: path }) &&
      proxy(new NextRequest(`${origin}${path}`, { headers: { host: new URL(origin).host } })).headers.has(
        "Content-Security-Policy",
      )
        ? 1
        : 0;
    expect(fromConfig + fromProxy).toBe(1);
  });

  it.each([
    ["X-Content-Type-Options", "nosniff"],
    ["X-Frame-Options", "DENY"],
    ["Referrer-Policy", "no-referrer"],
  ])("sets %s", async (key, value) => {
    expect((await headerMap()).get(key)).toBe(value);
  });

  it("pins HSTS to at least a year and covers subdomains", async () => {
    const value = (await headerMap()).get("Strict-Transport-Security") ?? "";
    expect(Number(/max-age=(\d+)/.exec(value)?.[1])).toBeGreaterThanOrEqual(31_536_000);
    expect(value).toContain("includeSubDomains");
  });

  it("refuses framing in the CSP as well as the legacy header", async () => {
    // Staff financial actions must not be exposed through clickjacking.
    // X-Frame-Options alone leaves browsers that only honour frame-ancestors unprotected.
    expect((await headerMap()).get("Content-Security-Policy")).toContain("frame-ancestors 'none'");
  });

  it.each(["default-src 'self'", "object-src 'none'", "base-uri 'self'", "form-action 'self'"])(
    "keeps %s in the policy",
    async (directive) => {
      expect((await headerMap()).get("Content-Security-Policy")).toContain(directive);
    },
  );

  it("does not widen the static script policy beyond what Next actually needs", async () => {
    /**
     * 'unsafe-inline' (and 'unsafe-eval' in development) are conceded to statically rendered
     * public pages and are documented as the ceiling; proxy.test.ts covers the nonce policy. Anything further -- a wildcard host, or data:
     * as a script source -- would be a real loosening rather than the same concession.
     */
    const policy = (await headerMap()).get("Content-Security-Policy");
    const scriptSrc = policy?.split(";").find((part) => part.trim().startsWith("script-src"));
    if (!scriptSrc) throw new Error("Missing script policy");

    expect(scriptSrc).not.toContain("*");
    expect(scriptSrc).not.toContain("data:");
    expect(scriptSrc).not.toContain("http:");
  });

  it("does not hand the camera or microphone to an embedded third party", async () => {
    const policy = (await headerMap()).get("Permissions-Policy");
    expect(policy).toContain("geolocation=()");
    expect(policy).toContain("microphone=()");
    // R1 displays a referral code; scanning is done with the visitor's native camera.
    expect(policy).toContain("camera=()");
  });
});

describe("insecure request upgrades", () => {
  async function policyFor(origin: string) {
    vi.stubEnv("DONOR_PUBLIC_ORIGIN", origin);
    vi.resetModules();
    const { default: config } = await import("../next.config");
    const groups = await config.headers?.();
    return groups?.flatMap((group) => group.headers).find((header) => header.key === "Content-Security-Policy")?.value;
  }
  afterEach(() => vi.unstubAllEnvs());

  it("are requested when the public origin is HTTPS", async () => {
    expect(await policyFor("https://donors.example.org")).toContain("upgrade-insecure-requests");
  });

  it("are not requested for plain-HTTP local development", async () => {
    expect(await policyFor("http://127.0.0.1:3000")).not.toContain("upgrade-insecure-requests");
  });
});
