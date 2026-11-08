import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import Exports from "./Exports";

const reply = (status: number, body: unknown) => ({ ok: status < 400, status, json: async () => body });
const requestedAt = "2026-11-08T12:00:00Z";
const job = (state: string, rows: number, extra: Record<string, unknown> = {}) => ({
  id: "6f0d7c3a-5b1e-4c8e-9a52-0d1f3a7b9c11",
  dataset: "gifts",
  state,
  requested_at: requestedAt,
  expires_at: "2026-11-09T12:00:00Z",
  row_count: rows,
  ...extra,
});

/** Serves the dataset list and a job list that the test advances. */
function backend(jobs: () => unknown[], onRequest = () => reply(200, job("queued", 0))) {
  const calls: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      calls.push(`${init?.method ?? "GET"} ${url}`);
      if (url === "/api/donor/staff/exports") return reply(200, { datasets: ["donors", "gifts"] });
      if (init?.method === "POST") return onRequest();
      return reply(200, { jobs: jobs() });
    }),
  );
  return calls;
}

beforeEach(() => vi.useFakeTimers({ shouldAdvanceTime: true }));
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

it("requests an export in the background and offers the download once it is ready", async () => {
  let current: unknown[] = [];
  const calls = backend(() => current);
  render(<Exports />);
  expect(await screen.findByText("You have no exports in the last 24 hours.")).toBeVisible();
  expect(screen.queryByRole("link", { name: /Download/ })).toBeNull();

  current = [job("queued", 0)];
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Request gifts CSV" }));
  });
  expect(calls).toContain("POST /api/donor/staff/exports/jobs");
  expect(await screen.findByText("Waiting to start")).toBeVisible();
  expect(screen.getByRole("status")).toHaveTextContent("The gifts export was requested. It builds in the background.");

  current = [job("running", 5000)];
  await act(async () => {
    await vi.advanceTimersByTimeAsync(1_500);
  });
  expect(await screen.findByText("Building · 5,000 rows so far")).toBeVisible();

  current = [job("complete", 7321, { completed_at: "2026-11-08T12:00:09Z" })];
  await act(async () => {
    await vi.advanceTimersByTimeAsync(3_000);
  });
  const link = await screen.findByRole("link", { name: "Download gifts CSV" });
  expect(link).toHaveAttribute("href", "/api/donor/staff/exports/jobs/6f0d7c3a-5b1e-4c8e-9a52-0d1f3a7b9c11/file");
  expect(screen.getByRole("status")).toHaveTextContent("The gifts export is ready.");

  // A finished export needs no more polling.
  const listed = () => calls.filter((call) => call === "GET /api/donor/staff/exports/jobs").length;
  const before = listed();
  await act(async () => {
    await vi.advanceTimersByTimeAsync(60_000);
  });
  expect(listed()).toBe(before);
});

it("backs off while a long export makes no visible progress", async () => {
  const calls = backend(() => [job("queued", 0)]);
  render(<Exports />);
  await screen.findByText("Waiting to start");
  const listed = () => calls.filter((call) => call === "GET /api/donor/staff/exports/jobs").length;
  const initial = listed();
  for (let second = 0; second < 60; second++) {
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
  }
  const polls = listed() - initial;
  expect(polls).toBeGreaterThan(3);
  expect(polls).toBeLessThan(20);
});

it.each([
  ["too_large", "The export is larger than the 50 MB limit. Nothing was kept."],
  ["forbidden", "Your access changed before the export was built. Nothing was kept."],
  ["error", "The export could not be built. Request it again, and contact an administrator if it fails again."],
])("explains a %s failure and offers no download", async (failure, copy) => {
  backend(() => [job("failed", 0, { failure, completed_at: requestedAt })]);
  render(<Exports />);
  expect(await screen.findByText(`Failed · ${copy}`)).toBeVisible();
  expect(screen.queryByRole("link", { name: /Download/ })).toBeNull();
});

it("tells the staff member when an export cannot be requested", async () => {
  backend(
    () => [],
    () => reply(409, { detail: "Staff access changed or conflicts with this request" }),
  );
  render(<Exports />);
  const request = await screen.findByRole("button", { name: "Request donors CSV" });
  await act(async () => {
    fireEvent.click(request);
  });
  await waitFor(() =>
    expect(screen.getByRole("alert")).toHaveTextContent("Staff access changed or conflicts with this request"),
  );
  expect(screen.getByRole("button", { name: "Request donors CSV" })).toBeEnabled();
});

it("says so when the role permits no exports", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => reply(403, { detail: "Permission denied" })),
  );
  render(<Exports />);
  expect(await screen.findByText("Your role does not permit exports.")).toBeVisible();
  expect(screen.queryByRole("button", { name: /Request/ })).toBeNull();
});
