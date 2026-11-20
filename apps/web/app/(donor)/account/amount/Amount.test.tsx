import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import AmountPage from "./Amount";

const options = {
  state: "ready",
  subscription_id: "owned-subscription",
  amount_minor: 3000,
  effective_at: "2026-11-01T12:00:00Z",
  change: null,
};
const response = (value: unknown) => ({ ok: true, json: async () => value });
afterEach(() => vi.unstubAllGlobals());

it("confirms the daily gift × 30 in integer cents from the next billing date, with no charge today", async () => {
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(response(options))
    .mockResolvedValueOnce(
      response({
        request_id: "command",
        amount_minor: 21000,
        previous_minor: 3000,
        effective_at: options.effective_at,
        confirmed: true,
      }),
    );
  vi.stubGlobal("fetch", fetch);
  render(<AmountPage />);
  fireEvent.change(await screen.findByLabelText("Dollars a day"), { target: { value: "7" } });
  fireEvent.click(screen.getByRole("button", { name: "Review change" }));
  expect(fetch).toHaveBeenCalledTimes(1);
  expect(screen.getByText(/Past gifts and outstanding invoices keep/)).toBeVisible();
  expect(screen.getByRole("heading", { name: "Confirm your new daily gift" })).toHaveFocus();
  fireEvent.click(screen.getByRole("button", { name: "Confirm amount change" }));
  await waitFor(() =>
    expect(screen.getByRole("status")).toHaveTextContent("Last amount change confirmed: $210.00/month"),
  );
  expect(JSON.parse(fetch.mock.calls[1][1].body)).toEqual({
    subscription_id: options.subscription_id,
    amount_minor: 21000,
    previous_minor: 3000,
    effective_at: options.effective_at,
  });
});

it("retries a lost response with an unchanged command", async () => {
  const fetch = vi.fn().mockResolvedValueOnce(response(options)).mockRejectedValue(new Error("Offline"));
  vi.stubGlobal("fetch", fetch);
  render(<AmountPage />);
  fireEvent.change(await screen.findByLabelText("Dollars a day"), { target: { value: "3" } });
  fireEvent.click(screen.getByRole("button", { name: "Review change" }));
  fireEvent.click(screen.getByRole("button", { name: "Confirm amount change" }));
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("Amount change is not confirmed"));
  fireEvent.click(screen.getByRole("button", { name: "Confirm amount change" }));
  await waitFor(() => expect(fetch).toHaveBeenCalledTimes(3));
  expect(fetch.mock.calls[1][1].headers["Idempotency-Key"]).toBe(fetch.mock.calls[2][1].headers["Idempotency-Key"]);
  expect(fetch.mock.calls[1][1].body).toBe(fetch.mock.calls[2][1].body);
});

it.each(["2.50", "31"])("refuses %s dollars a day before confirmation", async (value) => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(response(options)));
  render(<AmountPage />);
  fireEvent.change(await screen.findByLabelText("Dollars a day"), { target: { value } });
  fireEvent.click(screen.getByRole("button", { name: "Review change" }));
  const input = screen.getByLabelText("Dollars a day");
  expect(screen.getByRole("alert")).toHaveTextContent("whole number of dollars a day from $1 to $30");
  expect(input).toHaveAttribute("aria-invalid", "true");
  expect(input).toHaveAccessibleDescription(/Larger gifts are arranged with DAD directly/);
  expect(input).toHaveFocus();
  expect(screen.queryByRole("button", { name: "Confirm amount change" })).not.toBeInTheDocument();
});

it("returns focus to the review control when backing out of confirmation", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(response(options)));
  render(<AmountPage />);
  fireEvent.click(await screen.findByRole("button", { name: "$3 a day · $90/month" }));
  expect(screen.getByLabelText("Dollars a day")).toHaveValue("3");
  fireEvent.click(screen.getByRole("button", { name: "Review change" }));
  fireEvent.click(screen.getByRole("button", { name: "Back" }));
  expect(screen.getByRole("button", { name: "Review change" })).toHaveFocus();
});

it("states every amount with its daily framing and marks a typed preset as chosen", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(response(options)));
  render(<AmountPage />);
  expect(await screen.findByText(/Your gift from the next billing date/)).toHaveTextContent("$30/month · $1 a day");
  fireEvent.change(screen.getByLabelText("Dollars a day"), { target: { value: "3" } });
  expect(screen.getByRole("button", { name: "$3 a day · $90/month" })).toHaveAttribute("aria-pressed", "true");
  expect(screen.getByText(/^New amount:/)).toHaveTextContent("$90/month · $3 a day");
  fireEvent.click(screen.getByRole("button", { name: "Review change" }));
  expect(screen.getByText(/^From /)).toHaveTextContent("From $30/month · $1 a day to $90/month · $3 a day");
});

it("starts empty for an amount set before the daily rule, so the donor chooses a daily gift", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(response({ ...options, amount_minor: 4500 })));
  render(<AmountPage />);
  expect(await screen.findByText(/Your gift from the next billing date/)).toHaveTextContent(
    "$45/month · about $1.50 a day",
  );
  expect(screen.getByLabelText("Dollars a day")).toHaveValue("");
});

it("explains an unchanged gift separately from one out of range", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(response(options)));
  render(<AmountPage />);
  fireEvent.change(await screen.findByLabelText("Dollars a day"), { target: { value: "1" } });
  fireEvent.click(screen.getByRole("button", { name: "Review change" }));
  expect(screen.getByRole("alert")).toHaveTextContent("already your daily gift");
  fireEvent.change(screen.getByLabelText("Dollars a day"), { target: { value: "40" } });
  fireEvent.click(screen.getByRole("button", { name: "Review change" }));
  expect(screen.getByRole("alert")).toHaveTextContent("from $1 to $30");
});

it("does not offer a refresh beside a validation error, and keeps the typed amount", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(response(options)));
  render(<AmountPage />);
  fireEvent.change(await screen.findByLabelText("Dollars a day"), { target: { value: "2.50" } });
  fireEvent.click(screen.getByRole("button", { name: "Review change" }));
  expect(screen.getByRole("alert")).toHaveTextContent("whole number of dollars");
  expect(screen.queryByRole("button", { name: "Refresh amount details" })).not.toBeInTheDocument();
  expect(screen.getByLabelText("Dollars a day")).toHaveValue("2.50");
});

it("offers a refresh when a save is not confirmed", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValueOnce(response(options)).mockRejectedValue(new Error("Offline")));
  render(<AmountPage />);
  fireEvent.change(await screen.findByLabelText("Dollars a day"), { target: { value: "3" } });
  fireEvent.click(screen.getByRole("button", { name: "Review change" }));
  fireEvent.click(screen.getByRole("button", { name: "Confirm amount change" }));
  expect(await screen.findByRole("button", { name: "Refresh amount details" })).toBeVisible();
});

it("says so when the amount cannot be changed", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(response({ state: "unavailable" })));
  render(<AmountPage />);
  expect(await screen.findByText(/Your monthly amount can’t be changed right now/)).toBeVisible();
  expect(screen.queryByRole("button", { name: "Review change" })).not.toBeInTheDocument();
});

it("asks the donor to sign in when saving returns 401", async () => {
  vi.stubGlobal(
    "fetch",
    vi
      .fn()
      .mockResolvedValueOnce(response(options))
      .mockResolvedValue({ ok: false, status: 401, json: async () => ({}) }),
  );
  render(<AmountPage />);
  fireEvent.change(await screen.findByLabelText("Dollars a day"), { target: { value: "3" } });
  fireEvent.click(screen.getByRole("button", { name: "Review change" }));
  fireEvent.click(screen.getByRole("button", { name: "Confirm amount change" }));
  expect(await screen.findByRole("link", { name: "Sign in to manage your giving" })).toBeVisible();
});

it("resumes an unconfirmed change with its original request id and cannot back out", async () => {
  const pending = {
    request_id: "pending-command",
    amount_minor: 9000,
    previous_minor: 3000,
    effective_at: options.effective_at,
    confirmed: false,
  };
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(response({ ...options, change: pending }))
    .mockResolvedValueOnce(response({ ...pending, confirmed: true }));
  vi.stubGlobal("fetch", fetch);
  render(<AmountPage />);
  expect(await screen.findByRole("heading", { name: "Confirm your new daily gift" })).toBeVisible();
  expect(screen.getByRole("button", { name: "Back" })).toBeDisabled();
  fireEvent.click(screen.getByRole("button", { name: "Confirm amount change" }));
  await waitFor(() => expect(fetch).toHaveBeenCalledTimes(2));
  expect(fetch.mock.calls[1][1].headers["Idempotency-Key"]).toBe("pending-command");
});
