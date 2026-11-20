import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import CompanyLanding from "./CompanyLanding";

afterEach(() => vi.unstubAllGlobals());

it("saves the company code from the visitor's browser before they give", async () => {
  const fetch = vi.fn(() => Promise.resolve(Response.json({ captured: true })));
  vi.stubGlobal("fetch", fetch);
  render(<CompanyLanding code="DELTA-7KQ4MX" />);
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Company code saved."));
  expect(fetch).toHaveBeenCalledWith("/api/donor/company/DELTA-7KQ4MX", expect.objectContaining({ method: "POST" }));
  expect(screen.getByRole("link", { name: "Start giving $1 a day" })).toHaveAttribute("href", "/donate");
});

it("says so when the code is malformed, and still lets the visitor give", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(() => Promise.resolve(Response.json({ captured: false }))),
  );
  render(<CompanyLanding code="!!" />);
  expect(await screen.findByRole("heading", { level: 1 })).toHaveTextContent("This company code isn’t valid");
  expect(screen.getByRole("link", { name: "Give without a company code" })).toHaveAttribute("href", "/donate");
});

it("says the code was not saved when the request fails, and retries on demand", async () => {
  const fetch = vi
    .fn()
    .mockRejectedValueOnce(new TypeError("Failed to fetch"))
    .mockResolvedValue(Response.json({ captured: true }));
  vi.stubGlobal("fetch", fetch);
  render(<CompanyLanding code="DELTA-7KQ4MX" />);
  expect(await screen.findByRole("alert")).toHaveTextContent(/offline/);
  expect(screen.getByText(/was not saved for this browser/)).toBeVisible();
  expect(screen.getByRole("link", { name: "Start giving $1 a day" })).toBeVisible();
  fireEvent.click(screen.getByRole("button", { name: "Try saving the code again" }));
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Company code saved."));
  expect(screen.getByText(/is saved for this browser/)).toBeVisible();
  expect(screen.queryByRole("button", { name: "Try saving the code again" })).not.toBeInTheDocument();
  expect(fetch).toHaveBeenCalledTimes(2);
});
