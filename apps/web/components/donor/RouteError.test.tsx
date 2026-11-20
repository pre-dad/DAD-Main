import { render, screen } from "@testing-library/react";
import { expect, it } from "vitest";
import DonorError from "@/app/(donor)/error";
import StaffError from "@/app/staff/error";

const error = Object.assign(new Error("boom"), { digest: "abc123" });

it("sends donors home and points them to their giving history after a checkout problem", () => {
  render(<DonorError error={error} retry={() => {}} />);
  expect(screen.getByText(/check your giving history/)).toBeVisible();
  expect(screen.getByText(/to DAD\./)).toHaveTextContent("quote reference abc123 to DAD.");
  expect(screen.getByRole("link", { name: "Back to home" })).toHaveAttribute("href", "/");
});

it("sends staff to the staff home and does not address the reference to DAD", () => {
  render(<StaffError error={error} retry={() => {}} />);
  expect(screen.getByText(/quote reference/)).not.toHaveTextContent("to DAD");
  expect(screen.getByRole("link", { name: "Go to staff home" })).toHaveAttribute("href", "/staff");
});
