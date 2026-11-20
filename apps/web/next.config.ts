import path from "node:path";
import { loadEnvConfig } from "@next/env";
import type { NextConfig } from "next";
import { contentSecurityPolicy } from "./lib/csp";
import { UNSERVED_HEADER } from "./lib/hosts";

loadEnvConfig(path.resolve(process.cwd(), "../.."));

const securityHeaders = [
  { key: "X-Content-Type-Options", value: "nosniff" },
  { key: "X-Frame-Options", value: "DENY" },
  { key: "Referrer-Policy", value: "no-referrer" },
  { key: "Permissions-Policy", value: "camera=(), geolocation=(), microphone=()" },
  // Only meaningful over TLS; harmless in plain-HTTP local development, and forgetting it
  // at deploy time is how a first request stays downgradeable.
  { key: "Strict-Transport-Security", value: "max-age=63072000; includeSubDomains" },
];

const nextConfig: NextConfig = {
  output: "standalone",
  logging: { incomingRequests: { ignore: [/\/account\/oauth\//] } },
  poweredByHeader: false,
  // The static headers below are fixed at build time, so proxy.ts gets the same origins
  // compiled in rather than reading a server environment that could disagree.
  env: {
    DONOR_PUBLIC_ORIGIN: process.env.DONOR_PUBLIC_ORIGIN ?? "",
    DONOR_STAFF_ORIGIN: process.env.DONOR_STAFF_ORIGIN ?? "",
  },
  // proxy.ts marks the pages a host does not serve. A rewrite here, unlike one from the proxy,
  // stays internal whatever address the server binds.
  async rewrites() {
    return {
      beforeFiles: [
        {
          source: "/:path*",
          has: [{ type: "header", key: UNSERVED_HEADER, value: "staff" }],
          destination: "/staff/_unserved",
        },
        {
          source: "/:path*",
          has: [{ type: "header", key: UNSERVED_HEADER, value: "public" }],
          destination: "/_unserved",
        },
      ],
      afterFiles: [],
      fallback: [],
    };
  },
  // The impact ledger became the spend-by-category page; keep old links working. The
  // gift-processed push opens /account/gifts in the app, so a browser follows it to the
  // giving history instead of a 404.
  async redirects() {
    return [
      { source: "/ledger", destination: "/spend", permanent: true },
      { source: "/account/gifts", destination: "/account/giving", permanent: false },
    ];
  },
  async headers() {
    return [
      { source: "/:path*", headers: securityHeaders },
      // proxy.ts sets a nonce policy on these instead. Browsers enforce every CSP header they
      // receive, so this one must not reach those responses as well.
      {
        source: "/((?!(?:account|partner|staff)(?:/|$)).*)",
        headers: [{ key: "Content-Security-Policy", value: contentSecurityPolicy() }],
      },
    ];
  },
};

export default nextConfig;
