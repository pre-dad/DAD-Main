import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { SignInForm } from "@/components/donor/SignInForm";
import ClaimPage from "./claim/Claim";
import GivingPage from "./giving/Giving";
import { AlreadySignedIn } from "./sign-in/AlreadySignedIn";

afterEach(() => {
  vi.unstubAllGlobals();
  window.history.replaceState(null, "", "/");
});

it("removes the token from the address and requires a click before redeeming", async () => {
  window.history.replaceState(null, "", "/account/claim#token=secret-link");
  const fetch = vi.fn().mockResolvedValue({ ok: true, json: async () => ({ claimed: true }) });
  vi.stubGlobal("fetch", fetch);
  render(<ClaimPage />);
  expect(window.location.hash).toBe("");
  expect(fetch).not.toHaveBeenCalled();
  fireEvent.click(await screen.findByRole("button", { name: "Confirm and sign in" }));
  expect(await screen.findByText("You’re signed in")).toBeVisible();
  expect(fetch.mock.calls[0][1].body).toBe(JSON.stringify({ token: "secret-link" }));
});

it("shows an expired link error and replacement path", async () => {
  window.history.replaceState(null, "", "/account/claim#token=expired-link");
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue({ ok: false, status: 401, json: async () => ({ detail: "Link expired" }) }),
  );
  render(<ClaimPage />);
  fireEvent.click(await screen.findByRole("button", { name: "Confirm and sign in" }));
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("Link expired"));
  expect(screen.getByRole("link", { name: "Request a new link" })).toHaveAttribute("href", "/account/sign-in");
});

it("requests email without granting access", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({ message: "If this email matches a donor account, a link will arrive." }),
    }),
  );
  render(<SignInForm />);
  fireEvent.change(screen.getByLabelText("Email"), { target: { value: "donor@example.org" } });
  fireEvent.click(screen.getByRole("button", { name: "Email me a sign-in link" }));
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("If this email matches"));
  expect(screen.queryByText("My giving")).not.toBeInTheDocument();
});

it("prefills and clears a handed-off email, then locks resend with a quiet countdown", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  sessionStorage.setItem("dad.signin.email", "donor@example.org");
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue({ ok: true, json: async () => ({ message: "A link will arrive." }) }),
  );
  render(<SignInForm />);
  expect(screen.getByLabelText("Email")).toHaveValue("donor@example.org");
  expect(sessionStorage.getItem("dad.signin.email")).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Email me a sign-in link" }));
  const inbox = await screen.findByRole("heading", { name: "Check your inbox" });
  await waitFor(() => expect(inbox).toHaveFocus());
  expect(screen.getByRole("status")).toHaveTextContent("A link will arrive. You can resend in 30 seconds.");
  const resend = screen.getByRole("button", { name: "Resend the link" });
  expect(resend).toHaveAttribute("aria-disabled", "true");
  expect(screen.getByRole("status")).not.toHaveTextContent("You can resend in 30 s.");
  // Each second re-arms the timer after a render, so advance one tick at a time.
  for (let second = 0; second < 30; second++) {
    await act(async () => {
      vi.advanceTimersByTime(1000);
    });
  }
  expect(screen.getByRole("status")).toHaveTextContent("You can resend now.");
  expect(screen.getByRole("button", { name: "Resend the link" })).toHaveAttribute("aria-disabled", "false");
  vi.useRealTimers();
});

it("renders protected empty history, and removes it after logout", async () => {
  const fetch = vi
    .fn()
    .mockResolvedValue({ ok: true, json: async () => ({ email: "donor@example.org", gifts: [], next_offset: null }) });
  vi.stubGlobal("fetch", fetch);
  render(<GivingPage />);
  expect(await screen.findByText(/No confirmed test gifts/)).toBeVisible();
  fireEvent.click(screen.getByRole("button", { name: "Sign out" }));
  expect(await screen.findByRole("link", { name: "Sign in to view your giving" })).toBeVisible();
  expect(screen.queryByText("donor@example.org")).not.toBeInTheDocument();
  await waitFor(() => expect(fetch).toHaveBeenCalledTimes(2));
});

it("ignores a redirect target outside the account pages after claiming", async () => {
  window.history.replaceState(null, "", "/account/claim#token=secret-link&next=https://evil.example/account/dashboard");
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, json: async () => ({ claimed: true }) }));
  render(<ClaimPage />);
  fireEvent.click(await screen.findByRole("button", { name: "Confirm and sign in" }));
  expect(await screen.findByRole("heading", { name: "You’re signed in" })).toBeVisible();
  expect(window.location.href).toBe(`${window.location.origin}/account/claim`);
});

it("tells a donor who is already signed in, and stays silent for a visitor", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, json: async () => ({ email: "donor@example.org" }) }));
  const { unmount } = render(<AlreadySignedIn />);
  expect(await screen.findByText(/already signed in as/)).toHaveTextContent("donor@example.org");
  expect(screen.getByRole("link", { name: "Go to my dashboard" })).toHaveAttribute("href", "/account/dashboard");
  unmount();
  let refused = () => {};
  const answered = new Promise<void>((resolve) => {
    refused = resolve;
  });
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue({
      ok: false,
      status: 401,
      json: async () => {
        refused();
        return { detail: "Sign in required" };
      },
    }),
  );
  const { container } = render(<AlreadySignedIn />);
  await answered;
  await act(async () => {});
  expect(container).toBeEmptyDOMElement();
});

it("moves focus to the start of the new page of gifts", async () => {
  const gift = (id: string) => ({
    id,
    amount_minor: 3000,
    currency: "USD",
    frequency: "MONTHLY",
    paid_at: "2026-10-01T12:00:00Z",
    refunded_minor: id === "b" ? 1000 : 0,
    net_minor: 3000,
  });
  const fetch = vi
    .fn()
    .mockResolvedValueOnce({
      ok: true,
      json: async () => ({ email: "donor@example.org", gifts: [gift("a")], next_offset: 50 }),
    })
    .mockResolvedValueOnce({
      ok: true,
      json: async () => ({ email: "donor@example.org", gifts: [gift("b")], next_offset: null }),
    });
  vi.stubGlobal("fetch", fetch);
  render(<GivingPage />);
  fireEvent.click(await screen.findByRole("button", { name: "Older gifts" }));
  expect(await screen.findByRole("button", { name: "Newer gifts" })).toBeVisible();
  await waitFor(() => expect(screen.getByRole("heading", { name: "Gifts" })).toHaveFocus());
  expect(screen.getByText(/Refunded \$10\.00/)).toBeVisible();
  expect(fetch.mock.calls[1][0]).toBe("/api/donor/me/gifts?offset=50");
});
