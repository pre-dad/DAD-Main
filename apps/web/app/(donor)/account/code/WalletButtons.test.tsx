import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import WalletButtons from "./WalletButtons";

afterEach(() => vi.unstubAllGlobals());
const response = (data: unknown) => ({ ok: true, json: async () => data });
it("offers only configured providers and prepares a Google save link on request", async () => {
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(response({ apple: false, google: true }))
    .mockResolvedValueOnce(response({ url: "https://pay.google.com/gp/v/save/signed-test" }));
  vi.stubGlobal("fetch", fetch);
  render(<WalletButtons />);
  fireEvent.click(await screen.findByRole("button", { name: "Add to Google Wallet" }));
  expect(await screen.findByRole("link", { name: "Save to Google Wallet" })).toHaveAttribute(
    "href",
    "https://pay.google.com/gp/v/save/signed-test",
  );
  expect(fetch.mock.calls[1][0]).toBe("/api/donor/me/wallet/google");
  expect(fetch.mock.calls[1][1].method).toBe("POST");
  expect(screen.queryByRole("button", { name: "Add to Apple Wallet" })).not.toBeInTheDocument();
});
it("keeps provider failures recoverable and rejects an unexpected redirect host", async () => {
  vi.stubGlobal(
    "fetch",
    vi
      .fn()
      .mockResolvedValueOnce(response({ apple: false, google: true }))
      .mockResolvedValueOnce(response({ url: "https://other.example/gp/v/save/token" })),
  );
  render(<WalletButtons />);
  fireEvent.click(await screen.findByRole("button", { name: "Add to Google Wallet" }));
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("Your on-screen code still works"));
  expect(screen.queryByRole("link")).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Add to Google Wallet" })).toBeEnabled();
});
it("does not offer a broken Apple pass when issuance fails", async () => {
  vi.stubGlobal(
    "fetch",
    vi
      .fn()
      .mockResolvedValueOnce(response({ apple: true, google: false }))
      .mockResolvedValueOnce(Response.json({ detail: "Wallet passes are unavailable" }, { status: 503 })),
  );
  render(<WalletButtons />);
  fireEvent.click(await screen.findByRole("button", { name: "Add to Apple Wallet" }));
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("pass could not be prepared"));
  expect(screen.getByRole("button", { name: "Add to Apple Wallet" })).toBeEnabled();
});
it("says when a pass request was rate limited, since retrying at once cannot help", async () => {
  vi.stubGlobal(
    "fetch",
    vi
      .fn()
      .mockResolvedValueOnce(response({ apple: true, google: false }))
      .mockResolvedValueOnce(Response.json({ detail: "Too many requests" }, { status: 429 })),
  );
  render(<WalletButtons />);
  fireEvent.click(await screen.findByRole("button", { name: "Add to Apple Wallet" }));
  await waitFor(() =>
    expect(screen.getByRole("alert")).toHaveTextContent("Too many requests from this connection. Wait a minute"),
  );
});
