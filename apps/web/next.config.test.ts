import { expect, it } from "vitest";
import nextConfig from "./next.config";

it("sends the retired /ledger address permanently to /spend", async () => {
  expect(await nextConfig.redirects?.()).toContainEqual({ source: "/ledger", destination: "/spend", permanent: true });
});

it("sends the app's gift-processed path to the giving history", async () => {
  expect(await nextConfig.redirects?.()).toContainEqual({
    source: "/account/gifts",
    destination: "/account/giving",
    permanent: false,
  });
});
