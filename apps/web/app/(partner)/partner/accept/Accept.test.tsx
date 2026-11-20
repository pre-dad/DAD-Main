import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import Accept from "./Accept";

afterEach(() => {
  vi.unstubAllGlobals();
  window.history.replaceState(null, "", "/");
});

it("removes the proof from the URL and waits for explicit confirmation", async () => {
  window.history.replaceState(null, "", "/partner/accept#token=private-proof");
  const fetch = vi.fn(() => Promise.resolve(Response.json({ detail: "Unavailable" }, { status: 503 })));
  vi.stubGlobal("fetch", fetch);
  render(<Accept />);
  const confirm = await screen.findByRole("button", { name: "Confirm and sign in" });
  expect(window.location.hash).toBe("");
  expect(fetch).not.toHaveBeenCalled();
  fireEvent.click(confirm);
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("We couldn’t sign you in right now."));
  expect(fetch).toHaveBeenCalledWith(
    "/api/donor/partner/accept-invite",
    expect.objectContaining({
      method: "POST",
      body: JSON.stringify({ invite: "private-proof" }),
      credentials: "include",
    }),
  );
  fireEvent.click(screen.getByRole("button", { name: "Confirm and sign in" }));
  await waitFor(() => expect(fetch).toHaveBeenCalledTimes(2));
});

it("redirects after a successful retry without claiming the link is missing", async () => {
  window.history.replaceState(null, "", "/partner/accept#token=private-proof");
  const assign = vi.fn();
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(Response.json({ detail: "Unavailable" }, { status: 503 }))
    .mockResolvedValueOnce(Response.json({}));
  vi.stubGlobal("fetch", fetch);
  vi.stubGlobal("location", { ...window.location, assign });
  render(<Accept />);
  fireEvent.click(await screen.findByRole("button", { name: "Confirm and sign in" }));
  await screen.findByRole("alert");
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("We couldn’t sign you in right now."));
  fireEvent.click(screen.getByRole("button", { name: "Confirm and sign in" }));
  await waitFor(() => expect(assign).toHaveBeenCalledWith("/partner"));
  expect(screen.getByText("Signed in, redirecting…")).toBeVisible();
  expect(screen.queryByText(/needs the link/)).not.toBeInTheDocument();
  expect(screen.queryByRole("button")).not.toBeInTheDocument();
});

it("explains a missing proof without submitting a request", async () => {
  vi.stubGlobal("fetch", vi.fn());
  render(<Accept />);
  expect(await screen.findByText(/This page needs the link from your email/)).toBeVisible();
  expect(screen.queryByRole("button")).not.toBeInTheDocument();
  expect(fetch).not.toHaveBeenCalled();
});
