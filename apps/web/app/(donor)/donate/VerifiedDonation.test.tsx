import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import VerifiedDonation from "./VerifiedDonation";

afterEach(() => {
  vi.unstubAllGlobals();
  sessionStorage.clear();
});

it("offers verification and an allowlisted return path without exposing checkout", async () => {
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Sign in required" }), { status: 401 }))
    .mockResolvedValueOnce(new Response(JSON.stringify({ message: "Check your inbox" }), { status: 202 }));
  vi.stubGlobal("fetch", fetch);
  render(<VerifiedDonation />);
  expect(screen.queryByRole("button", { name: /^Continue to secure checkout/ })).not.toBeInTheDocument();
  fireEvent.change(await screen.findByLabelText("Email"), { target: { value: "ben@example.org" } });
  fireEvent.click(screen.getByRole("button", { name: "Email me a sign-in link" }));
  await waitFor(() => expect(fetch).toHaveBeenCalledTimes(2));
  expect(JSON.parse(fetch.mock.calls[1][1].body)).toEqual({ email: "ben@example.org", next: "/donate" });
});

it("opens checkout only after loading the verified account", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({ email: "ben@example.org" }))));
  render(<VerifiedDonation />);
  expect(await screen.findByRole("button", { name: /^Continue to secure checkout/ })).toBeEnabled();
  expect(screen.getByLabelText("Email")).toHaveValue("ben@example.org");
});

it("fails closed on an account lookup outage", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue(new Response(JSON.stringify({ detail: "Temporary failure" }), { status: 500 })),
  );
  render(<VerifiedDonation />);
  expect(await screen.findByRole("alert")).toHaveTextContent("Please try again");
  expect(screen.queryByRole("button", { name: /^Continue to secure checkout/ })).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Try again" })).toBeVisible();
});
