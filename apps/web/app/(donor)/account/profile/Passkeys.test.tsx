import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { Passkeys } from "./Passkeys";

beforeEach(() => {
  vi.spyOn(window, "confirm").mockReturnValue(true);
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  Reflect.deleteProperty(navigator, "credentials");
});

const laptop = {
  id: "k1",
  name: "Work laptop",
  created_at: "2026-09-20T12:00:00Z",
  last_used_at: "2026-10-07T09:30:00Z",
};
const phone = { id: "k2", name: null, created_at: "2026-10-01T12:00:00Z", last_used_at: null };
const options = {
  challenge: "AQID",
  rp: { id: "example.org", name: "DAD" },
  user: { id: "dXNlcg", name: "alex@example.org", displayName: "alex@example.org" },
};

const json = (body: unknown, status = 200) => Promise.resolve({ ok: status < 400, status, json: async () => body });

function serve(routes: Record<string, (init?: RequestInit) => Promise<unknown>>) {
  const fetch = vi.fn((url: string, init?: RequestInit) => routes[`${init?.method ?? "GET"} ${url}`](init));
  vi.stubGlobal("fetch", fetch);
  return fetch;
}

function browser(answer: () => Promise<unknown> = async () => {}) {
  class PublicKeyCredential {
    static parseCreationOptionsFromJSON = (json: unknown) => ({ parsed: json });
    toJSON = () => ({ id: "new-passkey", type: "public-key" });
  }
  const create = vi.fn(async () => {
    await answer();
    return new PublicKeyCredential();
  });
  vi.stubGlobal("PublicKeyCredential", PublicKeyCredential);
  Object.defineProperty(navigator, "credentials", { value: { create, get: vi.fn() }, configurable: true });
  return create;
}

it("lists each passkey with when it was added and last used", async () => {
  browser();
  serve({ "GET /api/donor/me/passkeys": () => json({ passkeys: [laptop, phone] }) });
  render(<Passkeys onSignedOut={vi.fn()} />);
  expect(screen.getByRole("status")).toHaveTextContent("Loading your passkeys…");
  const rows = await screen.findAllByRole("listitem");
  expect(rows[0]).toHaveTextContent(/^Work laptopAdded Sep 20, 2026 · Last used Oct 7, 2026/);
  expect(rows[1]).toHaveTextContent(/^PasskeyAdded Oct 1, 2026 · Not used yet/);
  expect(within(rows[1]).getByRole("button")).toHaveAccessibleName("Remove Passkey, added Oct 1, 2026");
  expect(screen.getByRole("button", { name: "Add a passkey" })).toBeVisible();
});

it("leaves passkeys out of the profile where the deployment offers none", async () => {
  browser();
  const fetch = serve({
    "GET /api/donor/me/passkeys": () =>
      json({ detail: "Passkey sign-in is not set up in this environment", code: "PASSKEYS_UNCONFIGURED" }, 503),
  });
  const { container } = render(<Passkeys onSignedOut={vi.fn()} />);
  await waitFor(() => expect(fetch).toHaveBeenCalled());
  await waitFor(() => expect(container).toBeEmptyDOMElement());
});

it("adds a named passkey and shows the list the API returns", async () => {
  const create = browser();
  const fetch = serve({
    "GET /api/donor/me/passkeys": () => json({ passkeys: [] }),
    "POST /api/donor/me/passkeys/register/options": () => json({ ceremony: "ceremony-1", options }),
    "POST /api/donor/me/passkeys/register": () => json({ passkeys: [laptop] }),
  });
  render(<Passkeys onSignedOut={vi.fn()} />);
  expect(await screen.findByText("You haven’t added a passkey yet.")).toBeVisible();
  fireEvent.change(screen.getByRole("textbox", { name: "Passkey name (optional)" }), {
    target: { value: "  Work laptop " },
  });
  fireEvent.click(screen.getByRole("button", { name: "Add a passkey" }));
  await waitFor(() =>
    expect(screen.getByRole("status")).toHaveTextContent(
      "Added Work laptop. You can now sign in with it instead of an email link.",
    ),
  );
  expect(create).toHaveBeenCalledWith({ publicKey: { parsed: options } });
  const sent = fetch.mock.calls.find(([url]) => url === "/api/donor/me/passkeys/register");
  expect(JSON.parse(String(sent?.[1]?.body))).toEqual({
    ceremony: "ceremony-1",
    credential: { id: "new-passkey", type: "public-key" },
    name: "Work laptop",
  });
  expect(screen.getAllByRole("listitem")).toHaveLength(1);
  expect(screen.getByRole("textbox", { name: "Passkey name (optional)" })).toHaveValue("");
});

it("treats a cancelled prompt as a choice and says when the device already holds a passkey", async () => {
  let failure = new DOMException("The operation either timed out or was not allowed.", "NotAllowedError");
  const create = browser(async () => {
    throw failure;
  });
  const fetch = serve({
    "GET /api/donor/me/passkeys": () => json({ passkeys: [laptop] }),
    "POST /api/donor/me/passkeys/register/options": () => json({ ceremony: "ceremony-1", options }),
  });
  render(<Passkeys onSignedOut={vi.fn()} />);
  const button = await screen.findByRole("button", { name: "Add a passkey" });
  button.focus();
  fireEvent.click(button);
  await waitFor(() =>
    expect(screen.getByRole("alert")).toHaveTextContent("Adding a passkey was cancelled or timed out."),
  );
  expect(button).toHaveFocus();
  failure = new DOMException("", "InvalidStateError");
  fireEvent.click(button);
  await waitFor(() =>
    expect(screen.getByRole("alert")).toHaveTextContent(
      "This device or password manager already has a passkey for your account.",
    ),
  );
  expect(create).toHaveBeenCalledTimes(2);
  expect(fetch.mock.calls.some(([url]) => url === "/api/donor/me/passkeys/register")).toBe(false);
});

it("checks the session again when the API refuses a new passkey", async () => {
  browser();
  let listed = 0;
  const onSignedOut = vi.fn();
  serve({
    "GET /api/donor/me/passkeys": () => {
      listed += 1;
      return listed === 1 ? json({ passkeys: [] }) : json({ detail: "Sign in required" }, 401);
    },
    "POST /api/donor/me/passkeys/register/options": () => json({ ceremony: "ceremony-1", options }),
    "POST /api/donor/me/passkeys/register": () => json({ detail: "Passkey or ceremony invalid or expired" }, 401),
  });
  render(<Passkeys onSignedOut={onSignedOut} />);
  fireEvent.click(await screen.findByRole("button", { name: "Add a passkey" }));
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("That passkey wasn’t accepted."));
  await waitFor(() => expect(onSignedOut).toHaveBeenCalled());
});

it("removes a passkey and keeps keyboard focus in the list", async () => {
  browser();
  serve({
    "GET /api/donor/me/passkeys": () => json({ passkeys: [laptop, phone] }),
    "POST /api/donor/me/passkeys/k1/delete": () => json({ passkeys: [phone] }),
    "POST /api/donor/me/passkeys/k2/delete": () => json({ passkeys: [] }),
  });
  render(<Passkeys onSignedOut={vi.fn()} />);
  fireEvent.click(await screen.findByRole("button", { name: /^Remove Work laptop/ }));
  await waitFor(() =>
    expect(screen.getByRole("status")).toHaveTextContent(
      "Removed Work laptop. It can no longer sign in to your account.",
    ),
  );
  await waitFor(() => expect(screen.getByRole("button", { name: /^Remove Passkey/ })).toHaveFocus());
  fireEvent.click(screen.getByRole("button", { name: /^Remove Passkey/ }));
  await waitFor(() => expect(screen.getByRole("heading", { name: "Passkeys" })).toHaveFocus());
  expect(screen.getByText("You haven’t added a passkey yet.")).toBeVisible();
});

it("shows what is left when a passkey was already removed elsewhere", async () => {
  browser();
  let listed = 0;
  serve({
    "GET /api/donor/me/passkeys": () => {
      listed += 1;
      return json({ passkeys: listed === 1 ? [laptop, phone] : [phone] });
    },
    "POST /api/donor/me/passkeys/k1/delete": () => json({ detail: "Passkey not found" }, 404),
  });
  render(<Passkeys onSignedOut={vi.fn()} />);
  fireEvent.click(await screen.findByRole("button", { name: /^Remove Work laptop/ }));
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Removed Work laptop."));
  expect(screen.getAllByRole("listitem")).toHaveLength(1);
  expect(screen.getByRole("alert")).toBeEmptyDOMElement();
});

it("keeps the list and offers a refresh when a removal is not confirmed", async () => {
  browser();
  serve({
    "GET /api/donor/me/passkeys": () => json({ passkeys: [laptop] }),
    "POST /api/donor/me/passkeys/k1/delete": () => json({ detail: "Passkey sign-in is temporarily unavailable" }, 503),
  });
  render(<Passkeys onSignedOut={vi.fn()} />);
  fireEvent.click(await screen.findByRole("button", { name: /^Remove Work laptop/ }));
  await waitFor(() =>
    expect(screen.getByRole("alert")).toHaveTextContent(
      "Removal was not confirmed. Refresh the list to check before retrying.",
    ),
  );
  expect(screen.getByText("Work laptop")).toBeVisible();
  expect(screen.getByRole("button", { name: "Refresh passkeys" })).toBeVisible();
});

it("lists and removes passkeys in a browser that cannot add one", async () => {
  serve({ "GET /api/donor/me/passkeys": () => json({ passkeys: [laptop] }) });
  render(<Passkeys onSignedOut={vi.fn()} />);
  expect(await screen.findByText(/This browser can’t use passkeys/)).toBeVisible();
  expect(screen.queryByRole("button", { name: "Add a passkey" })).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: /^Remove Work laptop/ })).toBeVisible();
});

it("leaves the passkey alone when removal is not confirmed", async () => {
  browser();
  vi.spyOn(window, "confirm").mockReturnValue(false);
  const fetch = serve({ "GET /api/donor/me/passkeys": () => json({ passkeys: [laptop] }) });
  render(<Passkeys onSignedOut={vi.fn()} />);
  fireEvent.click(await screen.findByRole("button", { name: /^Remove Work laptop/ }));
  expect(window.confirm).toHaveBeenCalledWith(expect.stringContaining("Work laptop"));
  expect(fetch.mock.calls.some(([url]) => String(url).endsWith("/delete"))).toBe(false);
  expect(screen.getByText("Work laptop")).toBeVisible();
});
