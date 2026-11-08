import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import Audit from "./Audit";

afterEach(() => vi.unstubAllGlobals());

const event = (id: string) => ({
  id,
  actor_id: "staff-1",
  action: `action.${id}`,
  target: "donors",
  at: "2026-10-20T12:00:00Z",
  details: {},
});

it("pages the audit history by cursor, newest first", async () => {
  const fetch = vi.fn(async (input: string) =>
    Response.json(
      input.includes("cursor=c%2B2")
        ? { events: [event("older")], next_cursor: null }
        : { events: [event("newer")], next_cursor: "c+2" },
    ),
  );
  vi.stubGlobal("fetch", fetch);
  render(<Audit />);
  expect(await screen.findByText(/action\.newer/)).toBeVisible();
  expect(screen.queryByRole("button", { name: "Newer events" })).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Older events" }));
  expect(await screen.findByText(/action\.older/)).toBeVisible();
  expect(fetch).toHaveBeenLastCalledWith("/api/donor/staff/audit?cursor=c%2B2", expect.anything());
  expect(screen.queryByRole("button", { name: "Older events" })).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Newer events" }));
  expect(await screen.findByText(/action\.newer/)).toBeVisible();
});

it("says when no events are recorded and when the history cannot load", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => Response.json({ events: [], next_cursor: null })),
  );
  const { unmount } = render(<Audit />);
  expect(await screen.findByText("No events recorded.")).toBeVisible();
  unmount();
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => Response.json({}, { status: 500 })),
  );
  render(<Audit />);
  expect(await screen.findByText(/could not be loaded/)).toBeVisible();
});
