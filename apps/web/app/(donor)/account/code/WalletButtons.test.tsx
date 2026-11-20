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

it.each([
  ["a path outside the save flow", "https://pay.google.com/other/signed-test"],
  ["a malformed URL", "not a url"],
])("rejects a Google Wallet link with %s", async (_name, url) => {
  vi.stubGlobal(
    "fetch",
    vi
      .fn()
      .mockResolvedValueOnce(response({ apple: false, google: true }))
      .mockResolvedValueOnce(response({ url })),
  );
  render(<WalletButtons />);
  fireEvent.click(await screen.findByRole("button", { name: "Add to Google Wallet" }));
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("Your on-screen code still works"));
  expect(screen.queryByRole("link", { name: "Save to Google Wallet" })).not.toBeInTheDocument();
});

it("downloads an Apple Wallet pass", async () => {
  const pass = new Blob(["pass"], { type: "application/vnd.apple.pkpass" });
  vi.stubGlobal(
    "fetch",
    vi
      .fn()
      .mockResolvedValueOnce(response({ apple: true, google: false }))
      .mockResolvedValueOnce({ ok: true, status: 200, blob: async () => pass }),
  );
  const createObjectURL = vi.fn(() => "blob:pass");
  Object.defineProperty(URL, "createObjectURL", { value: createObjectURL, configurable: true });
  const downloads: string[] = [];
  const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
    downloads.push(`${this.download}|${this.href}`);
  });
  try {
    render(<WalletButtons />);
    fireEvent.click(await screen.findByRole("button", { name: "Add to Apple Wallet" }));
    await waitFor(() => expect(downloads).toEqual(["dad-referral.pkpass|blob:pass"]));
    expect(createObjectURL).toHaveBeenCalledWith(pass);
    expect(screen.queryByRole("alert")).toBeEmptyDOMElement();
  } finally {
    click.mockRestore();
    Reflect.deleteProperty(URL, "createObjectURL");
  }
});

it("rejects an Apple response that is not a pass", async () => {
  vi.stubGlobal(
    "fetch",
    vi
      .fn()
      .mockResolvedValueOnce(response({ apple: true, google: false }))
      .mockResolvedValueOnce({ ok: true, status: 200, blob: async () => new Blob(["x"], { type: "text/html" }) }),
  );
  render(<WalletButtons />);
  fireEvent.click(await screen.findByRole("button", { name: "Add to Apple Wallet" }));
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("pass could not be prepared"));
});
