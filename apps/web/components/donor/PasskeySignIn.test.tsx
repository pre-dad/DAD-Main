import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { PasskeySignIn } from "./PasskeySignIn";

const options = { challenge: "AQID", rpId: "example.org", userVerification: "required" };

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
    url === "/api/donor/passkeys/sign-in/options"
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

it("signs in with a passkey and opens the dashboard", async () => {
  const get = browser(async () => {});
  const fetch = api(new Response(JSON.stringify({ signed_in: true }), { status: 200 }));
  const assign = vi.fn();
  vi.stubGlobal("location", { ...window.location, assign });
  render(<PasskeySignIn />);
  fireEvent.click(screen.getByRole("button", { name: "Sign in with a passkey" }));
  await vi.waitFor(() => expect(assign).toHaveBeenCalledWith("/account/dashboard"));
  expect(get).toHaveBeenCalledWith({ publicKey: { parsed: options } });
  expect(fetch.mock.calls[1][0]).toBe("/api/donor/passkeys/sign-in");
  expect(JSON.parse(fetch.mock.calls[1][1]?.body as string)).toEqual({
    ceremony: "ceremony-1",
    credential: { id: "passkey", type: "public-key" },
  });
});

it("treats a cancelled prompt as a choice and lets the donor retry from the same button", async () => {
  browser(async () => {
    throw new DOMException("The operation either timed out or was not allowed.", "NotAllowedError");
  });
  const fetch = api(new Response("{}", { status: 500 }));
  render(<PasskeySignIn />);
  const button = screen.getByRole("button", { name: "Sign in with a passkey" });
  button.focus();
  fireEvent.click(button);
  await waitFor(() =>
    expect(screen.getByRole("alert")).toHaveTextContent(
      "Passkey sign-in was cancelled or timed out. Try again, or use an email link.",
    ),
  );
  expect(fetch).toHaveBeenCalledTimes(1);
  expect(button).not.toHaveAttribute("aria-disabled");
  expect(button).toHaveFocus();
});

it("points a donor whose passkey is refused to the email link", async () => {
  browser(async () => {});
  api(new Response(JSON.stringify({ detail: "Passkey or ceremony invalid or expired" }), { status: 401 }));
  render(<PasskeySignIn />);
  fireEvent.click(screen.getByRole("button", { name: "Sign in with a passkey" }));
  await waitFor(() =>
    expect(screen.getByRole("alert")).toHaveTextContent(
      "That passkey isn’t linked to a donor account. Sign in with an email link",
    ),
  );
});

it("is not offered in a browser that cannot use passkeys", () => {
  render(<PasskeySignIn />);
  expect(screen.queryByRole("button")).not.toBeInTheDocument();
});
