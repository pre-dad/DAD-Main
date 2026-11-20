import { render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import ReceiptsPage from "./page";
import Receipts from "./Receipts";

vi.mock("next/form", () => ({ default: (props: React.ComponentProps<"form">) => <form {...props} /> }));
afterEach(() => vi.unstubAllGlobals());
const response = (value: unknown) => ({ ok: true, json: async () => value });
const gift = {
  id: "g1",
  amount_minor: 3000,
  refunded_minor: 0,
  net_minor: 3000,
  paid_at: "2026-10-01T12:00:00Z",
  frequency: "MONTHLY",
  refunds: [],
};

it("opens the gift named in the address", async () => {
  const fetch = vi.fn().mockResolvedValue(response({ as_of: "2026-10-04T12:00:00Z", receipt_id: "R-1", gift }));
  vi.stubGlobal("fetch", fetch);
  render(<Receipts gift="g/1" defaultYear={2026} latestYear={2026} />);
  expect(await screen.findByRole("heading", { name: "Test gift record" })).toBeVisible();
  expect(screen.getByText("Reference").closest("li")).toHaveTextContent("R-1");
  expect(fetch.mock.calls[0][0]).toBe("/api/donor/me/receipts/g%2F1");
});

it("waits for a chosen year, then shows that year's summary with linkable gift records", async () => {
  const fetch = vi.fn().mockResolvedValue(
    response({
      as_of: "2028-01-04T12:00:00Z",
      year: 2027,
      timezone: "America/New_York",
      gifts: [gift],
      amount_minor: 3000,
      refunded_minor: 0,
      net_minor: 3000,
    }),
  );
  vi.stubGlobal("fetch", fetch);
  const { unmount } = render(<Receipts defaultYear={2027} latestYear={2028} />);
  const year = screen.getByLabelText("Calendar year");
  expect(year).toHaveValue(2027);
  expect(year).toHaveAttribute("name", "year");
  expect(year.closest("form")).toHaveAttribute("action", "/account/receipts");
  expect(fetch).not.toHaveBeenCalled();
  unmount();
  render(<Receipts year={2027} defaultYear={2026} latestYear={2028} />);
  expect(await screen.findByRole("heading", { name: "2027 annual test summary" })).toBeVisible();
  expect(fetch.mock.calls[0][0]).toBe("/api/donor/me/annual-summary/2027");
  expect(screen.getByRole("link", { name: /\$30\.00/ })).toHaveAttribute("href", "/account/receipts?gift=g1");
});

it.each([["1999"], ["9999"], ["20x7"], [["2027", "2026"]]])(
  "ignores an out-of-range or malformed year in the address: %j",
  async (year) => {
    const fetch = vi.fn();
    vi.stubGlobal("fetch", fetch);
    render(await ReceiptsPage({ params: Promise.resolve({}), searchParams: Promise.resolve({ year }) }));
    expect(screen.getByLabelText("Calendar year")).toBeVisible();
    expect(fetch).not.toHaveBeenCalled();
  },
);

it("says when a linked gift record does not exist, rather than blaming the year", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue({ ok: false, status: 404, json: async () => ({ detail: "Record not found" }) }),
  );
  render(<Receipts gift="missing" defaultYear={2026} latestYear={2026} />);
  expect(
    await screen.findByText("There is no gift record at this link. Choose a gift from My giving."),
  ).toHaveAttribute("role", "alert");
});

const refunded = {
  ...gift,
  refunded_minor: 1000,
  net_minor: 2000,
  refunds: [
    { id: "r1", amount_minor: 1000, status: "succeeded", observed_at: "2026-10-03T12:00:00Z" },
    { id: "r2", amount_minor: 500, status: "pending", observed_at: "2026-10-04T12:00:00Z" },
    { id: "r3", amount_minor: 300, status: "reversed", observed_at: "2026-10-05T12:00:00Z" },
  ],
};

it("shows each refund with a readable status and the net amount", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue(response({ as_of: "2026-10-05T12:00:00Z", receipt_id: "R-2", gift: refunded })),
  );
  render(<Receipts gift="g1" defaultYear={2026} latestYear={2026} />);
  expect(await screen.findByText(/^Refund · Completed · observed/)).toBeVisible();
  expect(screen.getByText(/^Refund · Pending · observed/)).toBeVisible();
  expect(screen.getByText(/^Refund · reversed · observed/)).toBeVisible();
  expect(screen.queryByText(/succeeded/)).not.toBeInTheDocument();
  expect(screen.getByText("Successful refunds").closest("li")).toHaveTextContent("$10.00");
  expect(screen.getByText("Net test amount").closest("li")).toHaveTextContent("$20.00");
});

it("asks a signed-out donor to sign in to view a record", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false, status: 401, json: async () => ({}) }));
  render(<Receipts gift="g1" defaultYear={2026} latestYear={2026} />);
  expect(await screen.findByRole("link", { name: "Sign in to view your gift records" })).toBeVisible();
});

it("says when a year has no confirmed gifts", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue(
      response({
        as_of: "2028-01-04T12:00:00Z",
        year: 2027,
        timezone: "America/New_York",
        gifts: [],
        amount_minor: 0,
        refunded_minor: 0,
        net_minor: 0,
      }),
    ),
  );
  render(<Receipts year={2027} defaultYear={2027} latestYear={2028} />);
  expect(await screen.findByText("No confirmed gifts in this year.")).toBeVisible();
});
