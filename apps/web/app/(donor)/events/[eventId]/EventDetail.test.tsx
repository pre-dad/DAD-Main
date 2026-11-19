import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import EventDetail from "./EventDetail";

afterEach(() => vi.unstubAllGlobals());

const event = {
  id: "e-1",
  slug: "beach",
  title: "Beach cleanup",
  type: "VOLUNTEER",
  description: "Bring gloves.",
  venue: "Ocean Beach",
  address: "Great Hwy",
  city: "San Francisco",
  starts_at: "2099-11-07T17:00:00Z",
  ends_at: "2099-11-07T19:00:00Z",
  capacity: 20,
  spots_left: 0,
  reservation_status: null,
  invitation_note: null,
  report_issue_url: "mailto:help@example.org",
  cancelled: false,
  booking_opens_at: null,
  early_booking: false,
  time_zone: "America/Los_Angeles",
  checked_in_at: null,
};

it("lets a signed-in donor join the waitlist of a full event", async () => {
  const fetch = vi.fn((_input: RequestInfo | URL, init?: RequestInit) => {
    if (init?.method === "POST") return Promise.resolve(Response.json({ status: "WAITLISTED" }));
    return Promise.resolve(Response.json(event));
  });
  vi.stubGlobal("fetch", fetch);
  render(<EventDetail event="e-1" />);
  expect(await screen.findByText("Full (20 spots) — join the waitlist")).toBeVisible();
  expect(screen.getByText("Volunteer event")).toBeVisible();
  expect(screen.getByText("Nov 7, 2099, 9:00 AM PST")).toBeVisible();
  expect(screen.getByRole("link", { name: "Add to calendar" })).toHaveAttribute("href", "/api/events/e-1/calendar.ics");
  fireEvent.click(screen.getByRole("button", { name: "Join the waitlist" }));
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("You’re on the waitlist."));
  expect(String(fetch.mock.calls[0][0])).toBe("/api/donor/me/events/e-1");
  expect(
    fetch.mock.calls.some(([u, i]) => String(u) === "/api/donor/me/events/e-1/reserve" && i?.method === "POST"),
  ).toBe(true);
});

it("falls back to the public listing when signed out", async () => {
  const fetch = vi.fn((input: RequestInfo | URL) =>
    Promise.resolve(
      String(input).includes("/donor/me/")
        ? Response.json({ detail: "Sign in required" }, { status: 401 })
        : Response.json({ ...event, spots_left: 5 }),
    ),
  );
  vi.stubGlobal("fetch", fetch);
  render(<EventDetail event="e-1" />);
  expect(await screen.findByRole("link", { name: "Sign in to reserve a spot" })).toBeVisible();
  expect(screen.getByText("5 of 20 spots left")).toBeVisible();
  expect(screen.queryByRole("button", { name: /Reserve/ })).not.toBeInTheDocument();
});

it("offers an authenticated calendar download for an invite-only event", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(() => Promise.resolve(Response.json({ ...event, type: "INVITE_ONLY" }))),
  );
  render(<EventDetail event="e-1" />);
  expect(await screen.findByRole("link", { name: "Add to calendar" })).toHaveAttribute(
    "href",
    "/api/donor/me/events/e-1/calendar.ics",
  );
});

it("explains a cancelled event", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(() => Promise.resolve(Response.json({ ...event, cancelled: true }))),
  );
  render(<EventDetail event="e-1" />);
  expect(await screen.findByText(/This event has been cancelled/)).toBeVisible();
  expect(screen.queryByRole("button")).not.toBeInTheDocument();
  expect(screen.queryByRole("link", { name: "Add to calendar" })).not.toBeInTheDocument();
});

it("waits for booking to open, except for a Team Captain's early window", async () => {
  const opening = { ...event, spots_left: 5, booking_opens_at: "2099-11-01T17:00:00Z" };
  vi.stubGlobal(
    "fetch",
    vi.fn(() => Promise.resolve(Response.json(opening))),
  );
  const { unmount } = render(<EventDetail event="e-1" />);
  expect(await screen.findByText(/^Booking opens/)).toBeVisible();
  expect(screen.queryByRole("button", { name: "Reserve a spot" })).not.toBeInTheDocument();
  unmount();

  vi.stubGlobal(
    "fetch",
    vi.fn(() => Promise.resolve(Response.json({ ...opening, early_booking: true }))),
  );
  render(<EventDetail event="e-1" />);
  expect(await screen.findByText(/As a Team Captain, you can reserve now/)).toBeVisible();
  expect(screen.getByRole("button", { name: "Reserve a spot" })).toBeEnabled();
});

it("recovers from a failed event read without repeating a reservation", async () => {
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(Response.json({ detail: "Unavailable" }, { status: 503 }))
    .mockResolvedValueOnce(Response.json(event));
  vi.stubGlobal("fetch", fetch);
  render(<EventDetail event="e-1" />);
  fireEvent.click(await screen.findByRole("button", { name: "Retry event" }));
  expect(await screen.findByRole("button", { name: "Join the waitlist" })).toBeVisible();
  expect(fetch).toHaveBeenCalledTimes(2);
  expect(fetch.mock.calls.every(([, init]) => !init.method)).toBe(true);
});
