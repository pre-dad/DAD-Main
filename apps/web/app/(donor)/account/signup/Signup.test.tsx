import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import Signup from "./Signup";

afterEach(() => vi.unstubAllGlobals());

function fill() {
  fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Ben Donor" } });
  fireEvent.change(screen.getByLabelText("Email"), { target: { value: "ben@example.org" } });
  fireEvent.change(screen.getByLabelText("ZIP code"), { target: { value: "10001" } });
  fireEvent.change(screen.getByLabelText("City"), { target: { value: "New York" } });
}

it("asks for a verification link without confirming whether the address exists", async () => {
  const fetch = vi.fn().mockResolvedValue(Response.json({}));
  vi.stubGlobal("fetch", fetch);
  render(<Signup />);
  fill();
  fireEvent.click(screen.getByRole("button", { name: "Verify my email" }));
  expect(await screen.findByRole("status")).toHaveTextContent("If this address can be used for a donor account");
  expect(fetch.mock.calls[0][0]).toBe("/api/donor/signup");
  expect(JSON.parse(fetch.mock.calls[0][1].body)).toEqual({
    name: "Ben Donor",
    email: "ben@example.org",
    zip: "10001",
    city: "New York",
  });
  expect(screen.getByRole("button", { name: "Verify my email" })).toBeEnabled();
});

it("says so when the account could not be started, and lets the donor retry", async () => {
  vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new TypeError("Failed to fetch")));
  render(<Signup />);
  fill();
  fireEvent.click(screen.getByRole("button", { name: "Verify my email" }));
  expect(await screen.findByRole("alert")).toHaveTextContent(/offline/);
  expect(screen.getByRole("button", { name: "Verify my email" })).toBeEnabled();
});

it("offers the social sign-in alternatives", () => {
  render(<Signup />);
  expect(screen.getByRole("link", { name: "Continue with Apple" })).toHaveAttribute("href", "/account/oauth/apple");
  expect(screen.getByRole("link", { name: "Continue with Google" })).toHaveAttribute("href", "/account/oauth/google");
});
