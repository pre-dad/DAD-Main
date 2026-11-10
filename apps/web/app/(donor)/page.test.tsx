import { render, screen } from "@testing-library/react";
import { expect, it } from "vitest";
import DonorHome from "./page";

it("offers one primary action and sends partners to /apply, not the /partners redirect", () => {
  render(<DonorHome />);
  expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("A dollar a day. Every dollar on the record.");
  const donor = screen.getByRole("link", { name: /Give \$1 a day/ });
  expect(donor).toHaveAttribute("href", "/donate");
  expect(document.querySelectorAll(".button")).toHaveLength(1);
  expect(screen.getByRole("link", { name: /Become a partner/ })).toHaveAttribute("href", "/apply");
  expect(screen.getByRole("link", { name: "See how it works" })).toHaveAttribute("href", "/how-it-works");
});

it("states the gift beside its daily framing and the launch date from lib/launch", () => {
  render(<DonorHome />);
  const facts = screen.getByRole("complementary", { name: "DAD at a glance" });
  expect(facts).toHaveTextContent("$30/month · $1 a day");
  expect(facts).toHaveTextContent("April 22, 2027");
  expect(screen.getByRole("heading", { level: 2, name: "Building toward April 2027" })).toBeInTheDocument();
});

it("shows three step cards under the section heading and no sandbox sentence, and the deadline at most once", () => {
  render(<DonorHome />);
  const steps = screen.getAllByRole("heading", { level: 3 }).map((h) => h.textContent);
  expect(steps).toEqual(["Create your account", "Give", "Invite someone"]);
  expect(screen.queryByText(/Stripe test payments/)).not.toBeInTheDocument();
  // FoundingLine owns the deadline (it may be hidden after the window); no other block repeats it.
  expect(screen.queryAllByText(/December 31, 2026/).length).toBeLessThanOrEqual(1);
});
