import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import ReferralLanding from "./ReferralLanding";

afterEach(() => {
  vi.unstubAllGlobals();
  vi.unstubAllEnvs();
  vi.useRealTimers();
});

function response(data: unknown, status = 200) {
  return { ok: status < 400, status, json: async () => data };
}

it("captures the IRL marker from the browser, names the code's owner, and offers giving at once", async () => {
  const fetch = vi.fn().mockResolvedValue(response({ captured: true, public_name: "Sam" }));
  vi.stubGlobal("fetch", fetch);
  render(<ReferralLanding code="code" kind="IRL" referrer={{ state: "found", name: "Sam" }} />);
  expect(screen.getByRole("heading", { name: "Sam invited you to DAD" })).toBeVisible();
  expect(screen.getByRole("link", { name: "Give $1 a day with Sam" })).toHaveAttribute("href", "/donate");
  expect(screen.getByText("$30/month")).toBeVisible();
  expect(screen.getByText(/April 22, 2027/)).toBeVisible();
  expect(screen.getByText(/December 31, 2026/)).toBeVisible();
  expect(screen.queryByText(/sandbox/i)).not.toBeInTheDocument();
  await waitFor(() => expect(fetch).toHaveBeenCalled());
  expect(fetch.mock.calls[0][0]).toBe("/api/donor/referrals/code");
  expect(JSON.parse(fetch.mock.calls[0][1].body)).toEqual({ kind: "IRL" });
});

it("holds a tap made before the referral is saved, rather than losing the attribution", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(() => new Promise(() => {})),
  );
  render(<ReferralLanding code="code" kind="LINK" referrer={{ state: "found", name: "Sam" }} />);
  const link = screen.getByRole("link", { name: "Give $1 a day with Sam" });
  expect(fireEvent.click(link)).toBe(false);
  expect(await screen.findByRole("link", { name: "Saving your referral…" })).toHaveAttribute("aria-disabled", "true");
  expect(screen.getByRole("status")).toHaveTextContent("Saving your referral…");
});

it("follows the give link after the capture window and ignores repeat taps while waiting", async () => {
  vi.useFakeTimers();
  const assign = vi.fn();
  vi.stubGlobal("location", { assign });
  vi.stubGlobal(
    "fetch",
    vi.fn(() => new Promise(() => {})),
  );
  try {
    render(<ReferralLanding code="code" kind="LINK" referrer={{ state: "found", name: "Sam" }} />);
    const link = screen.getByRole("link", { name: "Give $1 a day with Sam" });
    link.setAttribute("target", "_blank");
    expect(fireEvent.click(link, { altKey: true })).toBe(true);
    link.removeAttribute("target");
    expect(fireEvent.click(link)).toBe(false);
    expect(fireEvent.click(screen.getByRole("link", { name: "Saving your referral…" }))).toBe(false);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(3999);
    });
    expect(assign).not.toHaveBeenCalled();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1);
    });
    expect(assign).toHaveBeenCalledTimes(1);
    expect(assign).toHaveBeenCalledWith("/donate");
  } finally {
    vi.useRealTimers();
  }
});

it("does not let a capture response rename the server-rendered invitation", async () => {
  let captured = () => {};
  const applied = new Promise<void>((resolve) => {
    captured = resolve;
  });
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue({
      ok: true,
      json: async () => {
        captured();
        return { captured: true, public_name: "Unexpected Name" };
      },
    }),
  );
  render(<ReferralLanding code="code" kind="LINK" referrer={{ state: "found", name: "Sam" }} />);
  await applied;
  await act(async () => {});
  expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Sam invited you to DAD");
  expect(screen.queryByText(/Unexpected Name/)).not.toBeInTheDocument();
});

it("does not redirect a visitor who left while the referral was saving", async () => {
  vi.useFakeTimers();
  const assign = vi.fn();
  vi.stubGlobal("location", { assign });
  vi.stubGlobal(
    "fetch",
    vi.fn(() => new Promise(() => {})),
  );
  const view = render(<ReferralLanding code="code" kind="LINK" referrer={{ state: "found", name: "Sam" }} />);
  fireEvent.click(screen.getByRole("link", { name: "Give $1 a day with Sam" }));
  view.unmount();
  await act(async () => {
    await vi.advanceTimersByTimeAsync(4000);
  });
  expect(assign).not.toHaveBeenCalled();
  expect(vi.getTimerCount()).toBe(0);
});

it("cancels the capture deadline when saving finishes before it", async () => {
  vi.useFakeTimers();
  const assign = vi.fn();
  vi.stubGlobal("location", { assign });
  let finish: (value: ReturnType<typeof response>) => void = () => {};
  vi.stubGlobal(
    "fetch",
    vi.fn(
      () =>
        new Promise((resolve) => {
          finish = resolve;
        }),
    ),
  );
  render(<ReferralLanding code="code" kind="LINK" referrer={{ state: "found", name: "Sam" }} />);
  const existingTimers = vi.getTimerCount();
  fireEvent.click(screen.getByRole("link", { name: "Give $1 a day with Sam" }));
  await act(async () => {
    finish(response({ captured: true, public_name: "Sam" }));
    await vi.advanceTimersByTimeAsync(0);
  });
  expect(assign).toHaveBeenCalledExactlyOnceWith("/donate");
  expect(vi.getTimerCount()).toBe(existingTimers);
});

it("leads with the unavailable message for a replaced code", async () => {
  const fetch = vi.fn().mockResolvedValue(response({ captured: false, public_name: null }));
  vi.stubGlobal("fetch", fetch);
  render(<ReferralLanding code="old" kind="LINK" referrer={{ state: "unavailable" }} />);
  const heading = screen.getByRole("heading", { level: 1 });
  expect(heading).toHaveTextContent("This referral link is no longer available");
  expect(heading.parentElement?.querySelector("p")).toHaveTextContent("give without a referral");
  expect(screen.getByRole("link", { name: "Continue without a referral" })).toHaveAttribute("href", "/donate");
  await waitFor(() => expect(fetch).toHaveBeenCalled());
  expect(JSON.parse(fetch.mock.calls[0][1].body)).toEqual({ kind: "LINK" });
  expect(screen.queryByRole("link", { name: "Give $1 a day with Sam" })).not.toBeInTheDocument();
  expect(screen.queryByRole("alert")).not.toBeInTheDocument();
});

it("falls back to the capture when the server could not look the code up", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(response({ captured: false, public_name: null })));
  render(<ReferralLanding code="old" kind="LINK" />);
  expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Join the beginning");
  expect(screen.getByRole("link", { name: "Give $1 a day" })).toHaveAttribute("href", "/donate");
  expect(await screen.findByRole("heading", { name: "This referral link is no longer available" })).toBeVisible();
});

it("keeps an offline failure distinct from successful capture", async () => {
  vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new TypeError("Failed to fetch")));
  render(<ReferralLanding code="code" kind="LINK" />);
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("couldn’t confirm your referral"));
  expect(screen.getByRole("alert")).toHaveTextContent("offline");
  expect(screen.queryByRole("link", { name: "Give $1 a day with Sam" })).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Retry referral" })).toBeVisible();
});

it("explains rate limiting separately from being offline, and retries", async () => {
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(response({ detail: "Too many requests" }, 429))
    .mockResolvedValueOnce(response({ captured: true, public_name: "Sam" }));
  vi.stubGlobal("fetch", fetch);
  render(<ReferralLanding code="code" kind="LINK" referrer={{ state: "found", name: "Sam" }} />);
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("Wait a minute"));
  expect(screen.getByRole("alert")).not.toHaveTextContent("offline");
  fireEvent.click(screen.getByRole("button", { name: "Retry referral" }));
  expect(await screen.findByRole("link", { name: "Give $1 a day with Sam" })).toBeVisible();
  expect(screen.getByRole("alert")).toBeEmptyDOMElement();
});
