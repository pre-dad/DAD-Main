import { fireEvent, render, screen } from "@testing-library/react";
import { StrictMode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import StaffActivate from "./Activate";

const options = {
  challenge: "AQID",
  rp: { id: "staff.example.org", name: "DAD staff" },
  user: { id: "AAE", name: "staff@example.org", displayName: "Sam Staff" },
  pubKeyCredParams: [],
};
const invitation = (ceremony: string) =>
  new Response(JSON.stringify({ ceremony, options, email: "staff@example.org", name: "Sam Staff" }), { status: 200 });

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

afterEach(() => {
  vi.unstubAllGlobals();
  Reflect.deleteProperty(navigator, "credentials");
  sessionStorage.clear();
  window.history.replaceState(null, "", "/");
});

it("retains the private invitation through StrictMode effect replay, clears the URL, and names the invitee", async () => {
  window.history.replaceState(null, "", "/staff/activate#token=private-test-invitation");
  browser();
  const fetch = vi.fn(async () => invitation("ceremony-1"));
  vi.stubGlobal("fetch", fetch);
  render(
    <StrictMode>
      <StaffActivate />
    </StrictMode>,
  );
  expect(window.location.hash).toBe("");
  expect(await screen.findByText("Sam Staff")).toBeVisible();
  expect(screen.getByText(/staff@example\.org/)).toBeVisible();
  expect(screen.getByRole("button", { name: "Create a passkey" })).toBeEnabled();
  for (const [url, init] of fetch.mock.calls as unknown as [string, RequestInit][]) {
    expect(url).toBe("/api/donor/staff/activate/options");
    expect(JSON.parse(init.body as string)).toEqual({ token: "private-test-invitation" });
  }
});

it("creates a passkey and finishes activation with the same ceremony", async () => {
  window.history.replaceState(null, "", "/staff/activate#token=kept-invitation");
  const assign = vi.fn();
  vi.stubGlobal("location", { ...window.location, assign });
  const create = browser();
  const fetch = vi.fn(async (url: string) =>
    url.endsWith("/options")
      ? invitation("ceremony-1")
      : new Response(JSON.stringify({ activated: true }), { status: 200 }),
  );
  vi.stubGlobal("fetch", fetch);
  render(<StaffActivate />);
  fireEvent.click(await screen.findByRole("button", { name: "Create a passkey" }));
  await vi.waitFor(() => expect(fetch).toHaveBeenCalledTimes(2));
  expect(create).toHaveBeenCalledWith({ publicKey: { parsed: options } });
  expect(fetch.mock.calls[1][0]).toBe("/api/donor/staff/activate");
  expect(JSON.parse((fetch.mock.calls[1] as unknown as [string, RequestInit])[1].body as string)).toEqual({
    ceremony: "ceremony-1",
    credential: { id: "new-passkey", type: "public-key" },
  });
  expect(sessionStorage.length).toBe(0);
  await vi.waitFor(() => expect(assign).toHaveBeenCalledWith("/staff"));
  expect(screen.getByText("Account activated, redirecting…")).toBeVisible();
  expect(screen.queryByText(/Open this page from the private invitation link/)).not.toBeInTheDocument();
});

it("lets the person retry after cancelling, and replaces a ceremony the server refused", async () => {
  window.history.replaceState(null, "", "/staff/activate#token=kept-invitation");
  let cancel = true;
  browser(async () => {
    if (cancel) throw new DOMException("", "NotAllowedError");
  });
  let issued = 0;
  const fetch = vi.fn(async (url: string) =>
    url.endsWith("/options")
      ? invitation(`ceremony-${++issued}`)
      : new Response(JSON.stringify({ detail: "Passkey, invitation, or session invalid or expired" }), { status: 401 }),
  );
  vi.stubGlobal("fetch", fetch);
  render(<StaffActivate />);
  fireEvent.click(await screen.findByRole("button", { name: "Create a passkey" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("Passkey creation was cancelled or timed out.");
  expect(issued).toBe(1);
  cancel = false;
  fireEvent.click(screen.getByRole("button", { name: "Create a passkey" }));
  await vi.waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("The passkey wasn’t accepted."));
  await vi.waitFor(() => expect(issued).toBe(2));
  expect(screen.getByRole("button", { name: "Create a passkey" })).toBeEnabled();
});

it("points a used or expired invitation to sign-in, then forgets it", async () => {
  window.history.replaceState(null, "", "/staff/activate#token=used-invitation");
  browser();
  vi.stubGlobal(
    "fetch",
    vi.fn(
      async () =>
        new Response(JSON.stringify({ detail: "Passkey, invitation, or session invalid or expired" }), { status: 401 }),
    ),
  );
  const first = render(<StaffActivate />);
  expect(await screen.findByRole("alert")).toHaveTextContent(
    "This invitation has already been used or has expired. If you already created a passkey, sign in with it. Otherwise, ask a DAD administrator for a new invitation.",
  );
  expect(screen.getByRole("link", { name: "Staff sign-in" })).toHaveAttribute("href", "/staff/sign-in");
  expect(screen.queryByRole("button", { name: "Create a passkey" })).not.toBeInTheDocument();
  expect(screen.queryByText(/Open this page from the private invitation link/)).not.toBeInTheDocument();
  expect(sessionStorage.length).toBe(0);
  first.unmount();
  window.history.replaceState(null, "", "/staff/activate");
  render(<StaffActivate />);
  expect(await screen.findByText(/Open this page from the private invitation link/)).toBeVisible();
});

it("keeps focus on the button after a cancelled prompt and blames the device when no passkey suits", async () => {
  window.history.replaceState(null, "", "/staff/activate#token=kept-invitation");
  browser(async () => {
    throw new DOMException("", "NotSupportedError");
  });
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => invitation("ceremony-1")),
  );
  render(<StaffActivate />);
  const button = await screen.findByRole("button", { name: "Create a passkey" });
  button.focus();
  fireEvent.click(button);
  expect(await screen.findByRole("alert")).toHaveTextContent(
    "This device can’t use a passkey here. Try another browser, your phone, or a security key.",
  );
  expect(button).toHaveFocus();
  expect(button).not.toHaveAttribute("aria-disabled");
});

it("replaces a stale ceremony before making the passkey", async () => {
  window.history.replaceState(null, "", "/staff/activate#token=kept-invitation");
  vi.useFakeTimers({ toFake: ["Date"] });
  const create = browser();
  let issued = 0;
  const fetch = vi.fn(async (url: string) =>
    url.endsWith("/options") ? invitation(`ceremony-${++issued}`) : new Response("{}", { status: 200 }),
  );
  vi.stubGlobal("fetch", fetch);
  vi.stubGlobal("location", { ...window.location, assign: vi.fn() });
  try {
    render(<StaffActivate />);
    const button = await screen.findByRole("button", { name: "Create a passkey" });
    vi.setSystemTime(Date.now() + 4 * 60_000);
    fireEvent.click(button);
    await vi.waitFor(() => expect(fetch).toHaveBeenCalledTimes(3));
    expect(issued).toBe(2);
    expect(fetch.mock.calls.map(([url]) => url)).toEqual([
      "/api/donor/staff/activate/options",
      "/api/donor/staff/activate/options",
      "/api/donor/staff/activate",
    ]);
    expect(create).toHaveBeenCalledTimes(1);
    expect(JSON.parse((fetch.mock.calls[2] as unknown as [string, RequestInit])[1].body as string).ceremony).toBe(
      "ceremony-2",
    );
  } finally {
    vi.useRealTimers();
  }
});

it("reports a refused invitation, not a bad passkey, when the stale refresh returns 401", async () => {
  window.history.replaceState(null, "", "/staff/activate#token=kept-invitation");
  vi.useFakeTimers({ toFake: ["Date"] });
  const create = browser();
  let calls = 0;
  vi.stubGlobal(
    "fetch",
    vi.fn(async () =>
      ++calls === 1 ? invitation("ceremony-1") : new Response(JSON.stringify({ detail: "expired" }), { status: 401 }),
    ),
  );
  try {
    render(<StaffActivate />);
    const button = await screen.findByRole("button", { name: "Create a passkey" });
    vi.setSystemTime(Date.now() + 4 * 60_000);
    fireEvent.click(button);
    expect(await screen.findByRole("alert")).toHaveTextContent("This invitation has already been used or has expired.");
    expect(screen.getByRole("alert")).not.toHaveTextContent("The passkey wasn’t accepted");
    expect(create).not.toHaveBeenCalled();
    expect(screen.getByRole("link", { name: "Staff sign-in" })).toBeVisible();
  } finally {
    vi.useRealTimers();
  }
});

it("says the browser cannot use passkeys instead of offering the button", async () => {
  window.history.replaceState(null, "", "/staff/activate#token=kept-invitation");
  vi.stubGlobal("PublicKeyCredential", undefined);
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => invitation("ceremony-1")),
  );
  render(<StaffActivate />);
  expect(await screen.findByText(/This browser can’t use passkeys/)).toBeVisible();
  expect(screen.queryByRole("button", { name: "Create a passkey" })).not.toBeInTheDocument();
});

it("explains rate limiting when the invitation cannot be checked", async () => {
  window.history.replaceState(null, "", "/staff/activate#token=kept-invitation");
  browser();
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response(JSON.stringify({ detail: "Too many requests" }), { status: 429 })),
  );
  render(<StaffActivate />);
  expect(await screen.findByRole("alert")).toHaveTextContent("Wait a minute, then retry.");
  expect(screen.queryByRole("link", { name: "Staff sign-in" })).not.toBeInTheDocument();
});
