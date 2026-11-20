import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import Network from "./Network";

afterEach(() => vi.unstubAllGlobals());

it("shows the network's annual giving, next milestone, and named invitees", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(() =>
      Promise.resolve(
        Response.json({
          active_count: 3,
          annualized_minor: 474500,
          next_milestone: { label: "Connector", target: 5, remaining: 2 },
          invitees: [
            { name: "Ann B.", active: true },
            { name: "Cy D.", active: false },
          ],
          anonymous_active: 1,
        }),
      ),
    ),
  );
  render(<Network />);
  expect(await screen.findByText("Together your network funds $4,745 a year.")).toBeVisible();
  expect(screen.getByText("2 more to reach Connector (5 active).")).toBeVisible();
  expect(screen.getByText("Cy D. · Not giving")).toBeVisible();
  expect(screen.getByText(/1 more person is giving but chose not to be named/)).toBeVisible();
});

const serve = (body: unknown, status = 200) =>
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(Response.json(body, { status })));

it("invites a donor with no active network to share", async () => {
  serve({ active_count: 0, annualized_minor: 0, next_milestone: null, invitees: [], anonymous_active: 0 });
  render(<Network />);
  expect(await screen.findByText(/No one you invited is giving yet/)).toBeVisible();
  expect(screen.getByText("0 active donors you brought in.")).toBeVisible();
});

it("uses the singular for one active donor and one unnamed giver", async () => {
  serve({ active_count: 1, annualized_minor: 36000, next_milestone: null, invitees: [], anonymous_active: 1 });
  render(<Network />);
  expect(await screen.findByText("1 active donor you brought in.")).toBeVisible();
  expect(screen.getByText(/1 more person is giving/)).toBeVisible();
});

it("asks a signed-out donor to sign in", async () => {
  serve({}, 401);
  render(<Network />);
  expect(await screen.findByRole("link", { name: "Sign in to see your network" })).toBeVisible();
});

it("offers a retry when the network cannot load", async () => {
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(Response.json({}, { status: 500 }))
    .mockResolvedValue(
      Response.json({
        active_count: 2,
        annualized_minor: 72000,
        next_milestone: null,
        invitees: [],
        anonymous_active: 0,
      }),
    );
  vi.stubGlobal("fetch", fetch);
  render(<Network />);
  expect(await screen.findByText(/could not be loaded/)).toHaveAttribute("role", "alert");
  fireEvent.click(screen.getByRole("button", { name: /Retry|Refresh/ }));
  expect(await screen.findByText("2 active donors you brought in.")).toBeVisible();
});
