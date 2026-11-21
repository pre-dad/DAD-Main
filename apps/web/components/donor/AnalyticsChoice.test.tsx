import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { AnalyticsChoice } from "./AnalyticsChoice";

let path = "/r/CODE";
vi.mock("next/navigation", () => ({ usePathname: () => path }));

const sent = (fetch: ReturnType<typeof vi.fn>) =>
  fetch.mock.calls.map(([, init]) => JSON.parse(init.body).events[0] as { name: string; properties: unknown });

let fetch: ReturnType<typeof vi.fn>;
beforeEach(() => {
  fetch = vi.fn().mockResolvedValue(new Response("{}"));
  vi.stubGlobal("fetch", fetch);
  window.history.replaceState(null, "", "/r/CODE?via=irl");
});
afterEach(() => {
  localStorage.clear();
  sessionStorage.clear();
  vi.unstubAllGlobals();
  path = "/r/CODE";
});

it("captures the referral source when a first-time visitor allows analytics on the landing page", async () => {
  const view = render(<AnalyticsChoice />);
  fireEvent.click(await screen.findByRole("button", { name: "Allow usage analytics" }));
  expect(sent(fetch)).toEqual([
    expect.objectContaining({ name: "app_opened" }),
    expect.objectContaining({ name: "attribution_captured", properties: { source: "IRL" } }),
  ]);
  path = "/account/dashboard";
  view.rerender(<AnalyticsChoice />);
  expect(sent(fetch).filter((event) => event.name === "attribution_captured")).toHaveLength(1);
});

it("sends nothing from the landing page when the visitor declines", async () => {
  render(<AnalyticsChoice />);
  fireEvent.click(await screen.findByRole("button", { name: "Decline usage analytics" }));
  expect(fetch).not.toHaveBeenCalled();
});

it("records a company link with its source", async () => {
  path = "/c/ACME";
  window.history.replaceState(null, "", "/c/ACME");
  render(<AnalyticsChoice />);
  fireEvent.click(await screen.findByRole("button", { name: "Allow usage analytics" }));
  expect(sent(fetch)).toContainEqual(
    expect.objectContaining({ name: "attribution_captured", properties: { source: "company" } }),
  );
});

it("lets a visitor reopen the choice without recording an opt-out", async () => {
  localStorage.setItem("dad.analytics.consent", "yes");
  render(<AnalyticsChoice />);
  fireEvent.click(await screen.findByRole("button", { name: "Change analytics preference" }));
  expect(screen.getByRole("button", { name: "Allow usage analytics" })).toBeInTheDocument();
  expect(localStorage.getItem("dad.analytics.consent")).toBe("yes");
});
