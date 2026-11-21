import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { PhoneSignIn } from "./PhoneSignIn";

afterEach(() => vi.unstubAllGlobals());

it("requests a code without disclosing whether the phone belongs to an account", async () => {
  const fetch = vi.fn(() => Promise.resolve(Response.json({ sent: true })));
  vi.stubGlobal("fetch", fetch);
  render(<PhoneSignIn />);
  fireEvent.click(screen.getByText("Sign in with your phone"));
  fireEvent.change(screen.getByLabelText("Phone number"), { target: { value: "+14155550123" } });
  fireEvent.click(screen.getByRole("button", { name: "Text me a code" }));
  expect(await screen.findByLabelText("Verification code")).toHaveAttribute("autocomplete", "one-time-code");
  // Focus is applied by an effect after the `sent` render commits, so retry until it lands
  // rather than asserting on the tick the input first appears (flaked under CI load).
  await waitFor(() => expect(screen.getByLabelText("Verification code")).toHaveFocus());
  expect(screen.getByRole("status")).toHaveTextContent("If this number is linked to an account");
  expect(fetch).toHaveBeenCalledWith(
    "/api/donor/phone/sign-in/options",
    expect.objectContaining({
      method: "POST",
      body: JSON.stringify({ phone: "+14155550123" }),
    }),
  );
  fireEvent.click(screen.getByRole("button", { name: "Change number or request a new code" }));
  expect(screen.queryByLabelText("Verification code")).not.toBeInTheDocument();
  expect(screen.getByLabelText("Phone number")).not.toHaveAttribute("readonly");
});

it("retains the challenge after an incorrect code and allows retry", async () => {
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(Response.json({ sent: true }))
    .mockResolvedValueOnce(Response.json({ detail: "That code is incorrect or has expired" }, { status: 401 }));
  vi.stubGlobal("fetch", fetch);
  render(<PhoneSignIn />);
  fireEvent.click(screen.getByText("Sign in with your phone"));
  fireEvent.change(screen.getByLabelText("Phone number"), { target: { value: "+14155550123" } });
  fireEvent.click(screen.getByRole("button", { name: "Text me a code" }));
  fireEvent.change(await screen.findByLabelText("Verification code"), { target: { value: "123456" } });
  fireEvent.click(screen.getByRole("button", { name: "Verify and sign in" }));
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("That code is incorrect or has expired"));
  await waitFor(() => expect(screen.getByRole("button", { name: "Verify and sign in" })).toBeEnabled());
  expect(fetch).toHaveBeenLastCalledWith(
    "/api/donor/phone/sign-in",
    expect.objectContaining({
      body: JSON.stringify({ phone: "+14155550123", code: "123456" }),
    }),
  );
});

it("explains delivery unavailability and permits another request", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(() => Promise.resolve(Response.json({ detail: "This sign-in method is not available" }, { status: 503 }))),
  );
  render(<PhoneSignIn />);
  fireEvent.click(screen.getByText("Sign in with your phone"));
  fireEvent.change(screen.getByLabelText("Phone number"), { target: { value: "+14155550123" } });
  fireEvent.click(screen.getByRole("button", { name: "Text me a code" }));
  await waitFor(() =>
    expect(screen.getByRole("alert")).toHaveTextContent(
      "Phone sign-in could not be completed. Please retry, or use an email link.",
    ),
  );
  expect(screen.queryByLabelText("Verification code")).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Text me a code" })).toBeEnabled();
});

it("opens the dashboard after a correct code", async () => {
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(Response.json({ sent: true }))
    .mockResolvedValueOnce(Response.json({}));
  const assign = vi.fn();
  vi.stubGlobal("fetch", fetch);
  vi.stubGlobal("location", { ...window.location, assign });
  render(<PhoneSignIn />);
  fireEvent.click(screen.getByText("Sign in with your phone"));
  fireEvent.change(screen.getByLabelText("Phone number"), { target: { value: "+14155550123" } });
  fireEvent.click(screen.getByRole("button", { name: "Text me a code" }));
  fireEvent.change(await screen.findByLabelText("Verification code"), { target: { value: "123456" } });
  fireEvent.click(screen.getByRole("button", { name: "Verify and sign in" }));
  await waitFor(() => expect(assign).toHaveBeenCalledWith("/account/dashboard"));
});
