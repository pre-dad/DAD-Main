import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { formatDate, formatDateTime } from "@/lib/dates";
import StaffFinancePage from "./Finance";

const quote = { gift_id: "gift", donor_id: "donor", amount_minor: 3000, available_minor: 3000 };
const response = (value: unknown) => ({ ok: true, json: async () => value });
const pending = (action: unknown = null) => response({ action });
const saved = {
  id: "saved-request",
  actor_id: "staff",
  kind: "REFUND",
  target_id: "gift",
  amount_minor: 1000,
  reason: "Donor requested refund",
  status: "AUTHORIZED",
};
afterEach(() => {
  vi.unstubAllGlobals();
});

it("requires confirmation of exact refund cents and the audit reason", async () => {
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(response(quote))
    .mockResolvedValueOnce(pending())
    .mockResolvedValueOnce(response({ status: "PENDING" }))
    .mockResolvedValueOnce(response({ ...quote, available_minor: 1766 }))
    .mockResolvedValueOnce(pending());
  vi.stubGlobal("fetch", fetch);
  render(<StaffFinancePage donor="donor" gift="gift" />);
  fireEvent.change(await screen.findByLabelText("Refund in USD"), { target: { value: "12.34" } });
  fireEvent.change(screen.getByLabelText("Audit reason"), { target: { value: "Donor requested partial refund" } });
  fireEvent.click(screen.getByRole("button", { name: "Review financial action" }));
  expect(fetch).toHaveBeenCalledTimes(2);
  expect(screen.getByText("Refund $12.34 USD to the original payment method.")).toBeVisible();
  expect(screen.getByRole("heading", { name: "Confirm financial action" })).toHaveFocus();
  fireEvent.click(screen.getByRole("button", { name: "Confirm financial action" }));
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Stripe refund status: PENDING"));
  expect(JSON.parse(fetch.mock.calls[2][1].body)).toEqual({
    amount_minor: 1234,
    reason: "Donor requested partial refund",
  });
  expect(await screen.findByText(/Available to refund: \$17.66/)).toBeVisible();
});

it("reuses a persisted uncertain request rather than minting another refund", async () => {
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(response(quote))
    .mockResolvedValueOnce(pending(saved))
    .mockRejectedValue(new Error("Offline"));
  vi.stubGlobal("fetch", fetch);
  render(<StaffFinancePage donor="donor" gift="gift" />);
  fireEvent.click(await screen.findByRole("button", { name: "Retry original financial action" }));
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("not confirmed"));
  expect(fetch.mock.calls[2][1].headers["Idempotency-Key"]).toBe("saved-request");
  expect(JSON.parse(fetch.mock.calls[2][1].body).amount_minor).toBe(1000);
});

it("looks up an uncertain request directly without fetching staff or action history", async () => {
  const fetch = vi.fn().mockResolvedValueOnce(response(quote)).mockResolvedValueOnce(pending(saved));
  vi.stubGlobal("fetch", fetch);
  render(<StaffFinancePage donor="donor" gift="gift" />);
  expect(await screen.findByRole("button", { name: "Retry original financial action" })).toBeVisible();
  expect(fetch.mock.calls.map(([url]) => url)).toEqual([
    "/api/donor/staff/donors/donor/gifts/gift/refund",
    "/api/donor/staff/donors/donor/actions/pending?target_id=gift&kind=REFUND",
  ]);
});

it("does not look up an action when no subscription exists", async () => {
  const fetch = vi.fn().mockResolvedValueOnce(response({ subscription: null }));
  vi.stubGlobal("fetch", fetch);
  render(<StaffFinancePage donor="donor" gift="" />);
  expect(await screen.findByText("No monthly gift is available.")).toBeVisible();
  expect(fetch).toHaveBeenCalledTimes(1);
});

it("restores a pending cancellation with its original key and period", async () => {
  const period = "2027-01-01T00:00:00Z";
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(
      response({ subscription: { id: "subscription", amount_minor: 3000, period_end: period, stops_at: null } }),
    )
    .mockResolvedValueOnce(
      pending({ ...saved, kind: "CANCEL", target_id: "subscription", amount_minor: null, period_end: period }),
    )
    .mockRejectedValue(new Error("Offline"));
  vi.stubGlobal("fetch", fetch);
  render(<StaffFinancePage donor="donor" gift="" />);
  fireEvent.click(await screen.findByRole("button", { name: "Retry original financial action" }));
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("not confirmed"));
  expect(fetch.mock.calls[1][0]).toContain("target_id=subscription&kind=CANCEL");
  expect(fetch.mock.calls[2][1].headers["Idempotency-Key"]).toBe("saved-request");
  expect(JSON.parse(fetch.mock.calls[2][1].body)).toEqual({
    subscription_id: "subscription",
    period_end: period,
    reason: saved.reason,
  });
});

it("ties refund validation errors to the field and keeps the request unsent", async () => {
  const fetch = vi.fn().mockResolvedValueOnce(response(quote)).mockResolvedValueOnce(pending());
  vi.stubGlobal("fetch", fetch);
  render(<StaffFinancePage donor="donor" gift="gift" />);
  const amount = await screen.findByLabelText("Refund in USD");
  fireEvent.change(amount, { target: { value: "45.00" } });
  fireEvent.change(screen.getByLabelText("Audit reason"), { target: { value: "Donor requested refund" } });
  fireEvent.click(screen.getByRole("button", { name: "Review financial action" }));
  expect(screen.getByRole("alert")).toHaveTextContent("within the available refund balance");
  expect(amount).toHaveAttribute("aria-invalid", "true");
  expect(amount).toHaveAccessibleDescription(/available refund balance/);
  expect(amount).toHaveFocus();
  expect(fetch).toHaveBeenCalledTimes(2);
});

it("distinguishes missing permission from a signed-out session", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue({ ok: false, status: 403, json: async () => ({ detail: "Forbidden" }) }),
  );
  const { unmount } = render(<StaffFinancePage donor="donor" gift="gift" />);
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("does not permit refunds"));
  expect(screen.queryByRole("link", { name: "Staff sign-in" })).not.toBeInTheDocument();
  unmount();
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue({ ok: false, status: 401, json: async () => ({ detail: "Sign in" }) }),
  );
  render(<StaffFinancePage donor="donor" gift="gift" />);
  expect(await screen.findByRole("link", { name: "Staff sign-in" })).toHaveAttribute("href", "/staff/sign-in");
  expect(screen.getByRole("alert")).toBeEmptyDOMElement();
});

it("treats switched-off payments as a definitive refusal, not an uncertain refund", async () => {
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(response(quote))
    .mockResolvedValueOnce(pending())
    .mockResolvedValueOnce({
      ok: false,
      status: 503,
      json: async () => ({ detail: "Payments are not configured", code: "PAYMENTS_UNCONFIGURED" }),
    });
  vi.stubGlobal("fetch", fetch);
  render(<StaffFinancePage donor="donor" gift="gift" />);
  fireEvent.change(await screen.findByLabelText("Audit reason"), { target: { value: "Donor requested refund" } });
  fireEvent.click(screen.getByRole("button", { name: "Review financial action" }));
  fireEvent.click(screen.getByRole("button", { name: "Confirm financial action" }));
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("isn’t available in this environment"));
  expect(screen.getByRole("alert")).not.toHaveTextContent(/another refund|Retry/);
  expect(screen.getByRole("button", { name: "Confirm financial action" })).toBeEnabled();
  expect(screen.getByRole("button", { name: "Back" })).toBeEnabled();
});

it("dates a quote by its last recorded refund and still allows the action, which reads Stripe itself", async () => {
  const observed = "2027-01-02T15:04:00Z";
  vi.stubGlobal(
    "fetch",
    vi
      .fn()
      .mockResolvedValueOnce(response({ ...quote, available_minor: 2000, observed_at: observed }))
      .mockResolvedValueOnce(pending()),
  );
  const { container } = render(<StaffFinancePage donor="donor" gift="gift" />);
  expect(
    await screen.findByText(
      `Original gift: $30.00. Available to refund: $20.00 USD. Last refund recorded ${formatDateTime(observed)}.`,
    ),
  ).toBeVisible();
  expect(container.querySelector(".note")).toBeNull();
  expect(screen.queryByText(/Last updated from Stripe/)).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Review financial action" })).toBeEnabled();
});

it("dates a cancellation quote by its last Stripe sync", async () => {
  const observed = "2027-01-02T15:04:00Z";
  vi.stubGlobal(
    "fetch",
    vi
      .fn()
      .mockResolvedValueOnce(
        response({
          mode: "test",
          state: "ready",
          observed_at: observed,
          subscription: {
            id: "subscription",
            amount_minor: 3000,
            currency: "USD",
            status: "active",
            period_end: "2027-02-01T00:00:00Z",
            stops_at: null,
          },
        }),
      )
      .mockResolvedValueOnce(pending()),
  );
  render(<StaffFinancePage donor="donor" gift="" />);
  const synced = await screen.findByText(/Last updated from Stripe/);
  expect(synced).toHaveTextContent(`Last updated from Stripe ${formatDateTime(observed)}.`);
  expect(synced).toHaveClass("note");
  expect(screen.getByText(`$30.00/month. Next renewal: ${formatDate("2027-02-01T00:00:00Z")}.`)).toBeVisible();
  expect(screen.getByRole("button", { name: "Review financial action" })).toBeEnabled();
});

it("says when renewals stop for a gift already ending", async () => {
  const stops = "2027-02-01T00:00:00Z";
  vi.stubGlobal(
    "fetch",
    vi
      .fn()
      .mockResolvedValueOnce(
        response({
          mode: "test",
          state: "ready",
          observed_at: "2027-01-02T15:04:00Z",
          subscription: {
            id: "subscription",
            amount_minor: 3000,
            currency: "USD",
            status: "active",
            period_end: stops,
            stops_at: stops,
          },
        }),
      )
      .mockResolvedValueOnce(pending()),
  );
  render(<StaffFinancePage donor="donor" gift="" />);
  expect(await screen.findByText(`$30.00/month. Renewals stop ${formatDate(stops)}.`)).toBeVisible();
  expect(screen.queryByText(/Next renewal/)).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Review financial action" })).not.toBeInTheDocument();
});
