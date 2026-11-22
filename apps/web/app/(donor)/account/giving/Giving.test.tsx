import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import Giving from "./Giving";

afterEach(() => vi.unstubAllGlobals());

const gift = (id: string) => ({
  id,
  amount_minor: 3000,
  currency: "USD",
  frequency: "ONE_TIME",
  paid_at: "2026-10-01T12:00:00Z",
  refunded_minor: 0,
  net_minor: 3000,
});
const page = (gifts: string[], next_offset: number | null) =>
  Response.json({ email: "donor@example.org", gifts: gifts.map(gift), next_offset });

it("returns to the offset the donor came from, whatever the server's page size", async () => {
  const fetch = vi.fn(async (url: string) => {
    if (url.endsWith("offset=0")) return page(["a"], 20);
    if (url.endsWith("offset=20")) return page(["b"], 45);
    return page(["c"], null);
  });
  vi.stubGlobal("fetch", fetch);
  render(<Giving />);
  fireEvent.click(await screen.findByRole("button", { name: "Older gifts" }));
  fireEvent.click(await screen.findByRole("button", { name: "Older gifts" }));
  expect(await screen.findByRole("button", { name: "Newer gifts" })).toBeVisible();
  fireEvent.click(screen.getByRole("button", { name: "Newer gifts" }));
  fireEvent.click(await screen.findByRole("button", { name: "Newer gifts" }));
  expect(await screen.findByRole("button", { name: "Older gifts" })).toBeVisible();
  expect(screen.queryByRole("button", { name: "Newer gifts" })).not.toBeInTheDocument();
  expect(fetch.mock.calls.map(([url]) => url)).toEqual([
    "/api/donor/me/gifts?offset=0",
    "/api/donor/me/gifts?offset=20",
    "/api/donor/me/gifts?offset=45",
    "/api/donor/me/gifts?offset=20",
    "/api/donor/me/gifts?offset=0",
  ]);
});

it("encodes the gift id in the gift record link", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(page(["a b&c"], null)));
  render(<Giving />);
  expect(await screen.findByRole("link", { name: "Gift record" })).toHaveAttribute(
    "href",
    "/account/receipts?gift=a%20b%26c",
  );
});

it("offers a refresh when the history cannot load", async () => {
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(Response.json({}, { status: 500 }))
    .mockResolvedValue(page(["a"], null));
  vi.stubGlobal("fetch", fetch);
  render(<Giving />);
  expect(await screen.findByText(/could not be loaded/)).toHaveAttribute("role", "alert");
  fireEvent.click(screen.getByRole("button", { name: "Refresh history" }));
  expect(await screen.findByRole("link", { name: "Gift record" })).toBeVisible();
});

it("keeps the donor signed in and says so when sign-out is not confirmed", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (_url: string, init?: RequestInit) =>
      init?.method === "POST" ? Response.json({ detail: "down" }, { status: 500 }) : page(["a"], null),
    ),
  );
  render(<Giving />);
  fireEvent.click(await screen.findByRole("button", { name: "Sign out" }));
  expect(await screen.findByText(/Sign-out was not confirmed/)).toHaveAttribute("role", "alert");
  expect(screen.getByRole("link", { name: "Gift record" })).toBeVisible();
});
