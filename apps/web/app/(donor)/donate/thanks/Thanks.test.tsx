import { render, screen, waitFor } from "@testing-library/react";
import { expect, it } from "vitest";
import ThankYouPage from "./page";

async function renderWith(params: Record<string, string | string[] | undefined>) {
  render(await ThankYouPage({ params: Promise.resolve({}), searchParams: Promise.resolve(params) }));
}

it("restates a monthly gift with the charge and daily framing at equal weight", async () => {
  await renderWith({ session_id: "cs_test", amount_minor: "3000", frequency: "MONTHLY" });
  const summary = screen.getByText(/Your gift:/);
  expect(summary).toHaveTextContent("Your gift: $30/month · $1 a day");
  const parts = [...summary.querySelectorAll("strong")].map((part) => part.textContent);
  expect(parts).toEqual(["$30/month", "$1 a day"]);
});

it("carries the Founding Donor timing, receipt notice, and dashboard link", async () => {
  await renderWith({ session_id: "cs_test", amount_minor: "9000", frequency: "MONTHLY" });
  expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("You’re in. Welcome to the beginning.");
  expect(screen.getByText(/launches April 22, 2027/)).toHaveTextContent(
    "A first gift completed on or before December 31, 2026 (New York time) earns a permanent Founding Donor badge.",
  );
  expect(screen.getByRole("heading", { level: 2, name: "What happens next" })).toBeVisible();
  expect(document.querySelector("main .button, .button")).toBeNull();
  expect(screen.getByText(/receipt email follows/)).toBeVisible();
  expect(screen.getByText(/test gift record, not a tax receipt/)).toBeVisible();
  expect(screen.getByText(/giving streak and your personal referral link/)).toBeVisible();
  expect(screen.getByRole("link", { name: "dashboard" })).toHaveAttribute("href", "/account/dashboard");
  expect(screen.queryByText(/claim/i)).not.toBeInTheDocument();
});

it.each([
  {},
  { session_id: "cs_test" },
  { amount_minor: "3000" },
  { amount_minor: "3000", frequency: "WEEKLY" },
  { amount_minor: "30.00", frequency: "MONTHLY" },
  { amount_minor: "-3000", frequency: "MONTHLY" },
  { amount_minor: "99", frequency: "MONTHLY" },
  { amount_minor: "4550", frequency: "MONTHLY" },
  { amount_minor: "4550", frequency: "ONE_TIME" },
  { amount_minor: "100000000", frequency: "MONTHLY" },
  { amount_minor: ["3000", "9000"], frequency: "MONTHLY" },
  { amount_minor: "3000", frequency: ["MONTHLY", "ONE_TIME"] },
])("falls back to a generic thank-you for missing or invalid parameters: %o", async (params) => {
  await renderWith(params);
  expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("You’re in. Welcome to the beginning.");
  expect(screen.queryByText(/Your gift:/)).not.toBeInTheDocument();
  expect(screen.getByRole("link", { name: "dashboard" })).toBeVisible();
});
it("clears the remembered choice without handing anything to sign-in", async () => {
  sessionStorage.setItem("dad.donate.choice", JSON.stringify({ amount: "1", email: "d@example.org", zip: "10001" }));
  await renderWith({});
  await waitFor(() => expect(sessionStorage.getItem("dad.donate.choice")).toBeNull());
  expect(sessionStorage.getItem("dad.signin.email")).toBeNull();
  sessionStorage.clear();
});
