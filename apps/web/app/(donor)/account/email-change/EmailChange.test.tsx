import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { StrictMode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import EmailChangePage from "./EmailChange";

afterEach(() => {
  vi.unstubAllGlobals();
  window.history.replaceState(null, "", "/");
});

it.each(["NEW", "COMPLETED"])("requires explicit confirmation and shows the %s result", async (stage) => {
  window.history.replaceState(null, "", "/account/email-change#token=private-proof");
  const fetch = vi.fn().mockResolvedValue(new Response(JSON.stringify({ stage }), { status: 200 }));
  vi.stubGlobal("fetch", fetch);
  render(
    <StrictMode>
      <EmailChangePage />
    </StrictMode>,
  );
  expect(window.location.hash).toBe("");
  expect(fetch).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole("button", { name: "Confirm this address" }));
  await waitFor(() =>
    expect(screen.getByRole("status")).toHaveTextContent(
      stage === "NEW" ? "has not changed yet" : "all devices have been signed out",
    ),
  );
  expect(JSON.parse(fetch.mock.calls[0][1].body)).toEqual({ token: "private-proof" });
  expect(screen.queryByRole("button")).not.toBeInTheDocument();
});

it("retains the proof after a failed request so the donor can retry", async () => {
  window.history.replaceState(null, "", "/account/email-change#token=retry-proof");
  const fetch = vi.fn().mockRejectedValue(new TypeError("Failed to fetch"));
  vi.stubGlobal("fetch", fetch);
  render(<EmailChangePage />);
  fireEvent.click(screen.getByRole("button", { name: "Confirm this address" }));
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("offline"));
  expect(screen.getByRole("alert")).not.toHaveTextContent("Failed to fetch");
  fireEvent.click(screen.getByRole("button", { name: "Confirm this address" }));
  expect(fetch).toHaveBeenCalledTimes(2);
  expect(JSON.parse(fetch.mock.calls[1][1].body)).toEqual({ token: "retry-proof" });
});

it("treats an unrecognised stage as a received confirmation", async () => {
  window.history.replaceState(null, "", "/account/email-change#token=private-proof");
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({ stage: "LATER" }), { status: 200 })));
  render(<EmailChangePage />);
  fireEvent.click(screen.getByRole("button", { name: "Confirm this address" }));
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Your confirmation was received."));
  expect(screen.queryByRole("button")).not.toBeInTheDocument();
  expect(screen.queryByText("Open the confirmation link from your email.")).not.toBeInTheDocument();
});
