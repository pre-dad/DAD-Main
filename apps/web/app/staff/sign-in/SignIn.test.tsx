import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import SignIn from "./SignIn";

const options = { challenge: "AQID", rpId: "staff.example.org", userVerification: "required" };

function browser(answer: () => Promise<unknown>) {
  class PublicKeyCredential {
    static parseRequestOptionsFromJSON = (json: unknown) => ({ parsed: json });
    toJSON = () => ({ id: "passkey", type: "public-key" });
  }
  const get = vi.fn(async () => {
    await answer();
    return new PublicKeyCredential();
  });
  vi.stubGlobal("PublicKeyCredential", PublicKeyCredential);
  Object.defineProperty(navigator, "credentials", { value: { create: vi.fn(), get }, configurable: true });
  return get;
}

function api(finish: Response) {
  const fetch = vi.fn(async (url: string, _init?: RequestInit) =>
    url === "/api/donor/staff/sign-in/options"
      ? new Response(JSON.stringify({ ceremony: "ceremony-1", options }), { status: 200 })
      : finish,
  );
  vi.stubGlobal("fetch", fetch);
  return fetch;
}

afterEach(() => {
  vi.unstubAllGlobals();
  Reflect.deleteProperty(navigator, "credentials");
});

it("signs in with a passkey, without asking for an email or password", async () => {
  const assign = vi.fn();
  vi.stubGlobal("location", { ...window.location, assign });
  const get = browser(async () => {});
  const fetch = api(new Response(JSON.stringify({ signed_in: true }), { status: 200 }));
  render(<SignIn />);
  expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Sign in with a passkey" }));
  await vi.waitFor(() => expect(fetch).toHaveBeenCalledTimes(2));
  expect(get).toHaveBeenCalledWith({ publicKey: { parsed: options } });
  expect(fetch.mock.calls[1][0]).toBe("/api/donor/staff/sign-in");
  expect(JSON.parse(fetch.mock.calls[1][1]?.body as string)).toEqual({
    ceremony: "ceremony-1",
    credential: { id: "passkey", type: "public-key" },
  });
  expect(screen.getByRole("alert")).toBeEmptyDOMElement();
  await vi.waitFor(() => expect(assign).toHaveBeenCalledWith("/staff"));
});

it("treats a cancelled prompt as a choice, not a failure, and lets the person retry from the same button", async () => {
  let cancel: () => void = () => {};
  const get = browser(
    () =>
      new Promise((_, reject) => {
        cancel = () =>
          reject(new DOMException("The operation either timed out or was not allowed.", "NotAllowedError"));
      }),
  );
  const fetch = api(new Response("{}", { status: 500 }));
  render(<SignIn />);
  const button = screen.getByRole("button", { name: "Sign in with a passkey" });
  button.focus();
  fireEvent.click(button);
  await vi.waitFor(() =>
    expect(screen.getByRole("status")).toHaveTextContent(
      "Waiting for your passkey. Follow the prompt from your device or password manager.",
    ),
  );
  expect(button).toHaveAccessibleName("Sign in with a passkey");
  expect(button).toHaveAttribute("aria-disabled", "true");
  fireEvent.click(button);
  await vi.waitFor(() => expect(get).toHaveBeenCalledTimes(1));
  cancel();
  await vi.waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("Sign-in was cancelled or timed out."));
  expect(fetch).toHaveBeenCalledTimes(1);
  expect(button).not.toHaveAttribute("aria-disabled");
  expect(button).toHaveFocus();
  expect(screen.getByRole("status")).toBeEmptyDOMElement();
});

it("blames the device, not the browser, when no suitable passkey is available", async () => {
  browser(async () => {
    throw new DOMException("", "NotSupportedError");
  });
  api(new Response("{}", { status: 500 }));
  render(<SignIn />);
  fireEvent.click(screen.getByRole("button", { name: "Sign in with a passkey" }));
  await waitFor(() =>
    expect(screen.getByRole("alert")).toHaveTextContent(
      "This device can’t use a passkey here. Try another browser, your phone, or a security key.",
    ),
  );
});

it("explains a passkey the server refuses", async () => {
  browser(async () => {});
  api(new Response(JSON.stringify({ detail: "Passkey, invitation, or session invalid or expired" }), { status: 401 }));
  render(<SignIn />);
  fireEvent.click(screen.getByRole("button", { name: "Sign in with a passkey" }));
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("That passkey wasn’t accepted."));
});

it("says when the browser cannot use passkeys", () => {
  render(<SignIn />);
  expect(screen.getByText(/This browser can’t use passkeys/)).toBeVisible();
  expect(screen.queryByRole("button", { name: "Sign in with a passkey" })).not.toBeInTheDocument();
});
