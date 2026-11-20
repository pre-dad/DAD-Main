import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import Status from "./Status";

afterEach(() => vi.unstubAllGlobals());

it.each(["PENDING", "APPROVED", "REJECTED"])("shows the persisted %s decision", async (status) => {
  vi.stubGlobal(
    "fetch",
    vi.fn(() => Promise.resolve(Response.json({ organization: "Delta", status }))),
  );
  render(<Status />);
  expect(await screen.findByRole("heading", { name: "Delta" })).toBeVisible();
  expect(screen.getByRole("link", { name: "About DAD" })).toHaveAttribute("href", "/how-it-works");
  if (status === "PENDING") expect(screen.getByText(/Your application is pending review/)).toBeVisible();
  if (status === "APPROVED")
    expect(screen.getByRole("link", { name: "Partner sign-in" })).toHaveAttribute("href", "/partner/sign-in");
  if (status === "REJECTED") expect(screen.getByText(/Your application was not approved/)).toBeVisible();
});

it("explains an absent or expired receipt without offering portal access", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(() => Promise.resolve(Response.json({ detail: "Partner sign-in required" }, { status: 401 }))),
  );
  render(<Status />);
  expect(await screen.findByText(/This browser has no current application receipt/)).toBeVisible();
  expect(screen.queryByRole("link", { name: "Partner sign-in" })).not.toBeInTheDocument();
});

it("retries an unavailable status without losing the receipt", async () => {
  vi.stubGlobal(
    "fetch",
    vi
      .fn()
      .mockResolvedValueOnce(Response.json({}, { status: 503 }))
      .mockResolvedValueOnce(Response.json({ organization: "Delta", status: "PENDING" })),
  );
  render(<Status />);
  expect(await screen.findByRole("alert")).toHaveTextContent("Your application status could not be loaded");
  fireEvent.click(screen.getByRole("button", { name: "Try again" }));
  expect(await screen.findByRole("heading", { name: "Delta" })).toBeVisible();
});
