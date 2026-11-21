import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import FailedPayments from "./FailedPayments";

afterEach(() => vi.unstubAllGlobals());

const payment = (id: string, overrides: Record<string, unknown> = {}) => ({
  amount_minor: 3000,
  disabled: false,
  donor_id: `donor-${id}`,
  email: `${id}@example.org`,
  failed_at: "2026-10-20T12:00:00Z",
  grace_ends_at: "2026-10-27T12:00:00Z",
  period_id: `period-${id}`,
  public_name: null,
  state: "in_grace",
  ...overrides,
});

it("lists failed renewals with their state and links each to the donor record", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () =>
      Response.json({
        payments: [payment("a"), payment("b", { state: "lapsed", amount_minor: null, public_name: "Bea" })],
        next_cursor: null,
      }),
    ),
  );
  render(<FailedPayments />);
  const table = await screen.findByRole("table");
  expect(within(table).getByRole("link", { name: "a@example.org" })).toHaveAttribute("href", "/staff/donors/donor-a");
  expect(within(table).getByRole("link", { name: "Bea" })).toHaveAttribute("href", "/staff/donors/donor-b");
  expect(within(table).getByText("$30.00")).toBeVisible();
  expect(within(table).getByText("Not recorded")).toBeVisible();
  expect(within(table).getByText("In grace")).toBeVisible();
  expect(within(table).getByText("Grace ended")).toBeVisible();
  expect(screen.queryByRole("button", { name: "Older failures" })).not.toBeInTheDocument();
});

it("pages forward and back with the server's cursors and filters by state", async () => {
  const fetch = vi.fn(async (url: string) =>
    Response.json(
      url.includes("cursor=c1")
        ? { payments: [payment("old")], next_cursor: null }
        : { payments: [payment("new")], next_cursor: "c1" },
    ),
  );
  vi.stubGlobal("fetch", fetch);
  render(<FailedPayments />);
  fireEvent.click(await screen.findByRole("button", { name: "Older failures" }));
  expect(await screen.findByRole("link", { name: "old@example.org" })).toBeVisible();
  expect(fetch).toHaveBeenLastCalledWith("/api/donor/staff/failed-payments?state=all&cursor=c1", expect.anything());
  fireEvent.click(screen.getByRole("button", { name: "Newer failures" }));
  expect(await screen.findByRole("link", { name: "new@example.org" })).toBeVisible();
  fireEvent.change(screen.getByLabelText("Show"), { target: { value: "lapsed" } });
  await screen.findByRole("link", { name: "new@example.org" });
  expect(fetch).toHaveBeenLastCalledWith("/api/donor/staff/failed-payments?state=lapsed", expect.anything());
});

it("says when nothing has failed and when access is missing", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => Response.json({ payments: [], next_cursor: null })),
  );
  const { unmount } = render(<FailedPayments />);
  expect(await screen.findByText("No failed payments.")).toBeVisible();
  unmount();
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => Response.json({ detail: "Permission denied" }, { status: 403 })),
  );
  render(<FailedPayments />);
  expect(await screen.findByText(/require donors and money access/)).toBeVisible();
  expect(screen.queryByRole("link", { name: "Staff sign-in" })).not.toBeInTheDocument();
});

it("shows the nickname with the email, and flags a disabled account", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () =>
      Response.json({ payments: [payment("a", { public_name: "Ann", disabled: true })], next_cursor: null }),
    ),
  );
  render(<FailedPayments />);
  const table = await screen.findByRole("table");
  expect(within(table).getByRole("link", { name: "Ann" })).toHaveAttribute("href", "/staff/donors/donor-a");
  expect(within(table).getByText("a@example.org")).toBeVisible();
  expect(within(table).getByText("Account disabled")).toBeVisible();
});

it("explains a load failure and retries", async () => {
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(Response.json({ detail: "boom" }, { status: 500 }))
    .mockResolvedValue(Response.json({ payments: [payment("a")], next_cursor: null }));
  vi.stubGlobal("fetch", fetch);
  render(<FailedPayments />);
  expect(await screen.findByRole("alert")).toHaveTextContent("Failed payments could not be loaded right now");
  fireEvent.click(screen.getByRole("button", { name: "Retry loading" }));
  expect(await screen.findByRole("link", { name: "a@example.org" })).toBeVisible();
});

it("moves focus to the results heading after a page turn", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) =>
      Response.json(
        url.includes("cursor=c1")
          ? { payments: [payment("old")], next_cursor: null }
          : { payments: [payment("new")], next_cursor: "c1" },
      ),
    ),
  );
  render(<FailedPayments />);
  fireEvent.click(await screen.findByRole("button", { name: "Older failures" }));
  await screen.findByRole("link", { name: "old@example.org" });
  await waitFor(() => expect(screen.getByRole("heading", { name: "Unresolved renewals" })).toHaveFocus());
});

it("starts from the first page when the filter changes", async () => {
  const fetch = vi.fn(async (url: string) =>
    Response.json(
      url.includes("cursor=c1")
        ? { payments: [payment("old")], next_cursor: null }
        : { payments: [payment("new")], next_cursor: "c1" },
    ),
  );
  vi.stubGlobal("fetch", fetch);
  render(<FailedPayments />);
  fireEvent.click(await screen.findByRole("button", { name: "Older failures" }));
  await screen.findByRole("link", { name: "old@example.org" });
  fireEvent.change(screen.getByLabelText("Show"), { target: { value: "in_grace" } });
  expect(await screen.findByRole("link", { name: "new@example.org" })).toBeVisible();
  expect(fetch).toHaveBeenLastCalledWith("/api/donor/staff/failed-payments?state=in_grace", expect.anything());
});
