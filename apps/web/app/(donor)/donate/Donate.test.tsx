import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import Donate from "./Donate";

const checkout = () => screen.getByRole("button", { name: /^Continue to secure checkout/ });
function openOther() {
  fireEvent.click(screen.getByRole("button", { name: /^Other/ }));
  return screen.getByLabelText("Dollars a day");
}

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  sessionStorage.clear();
});

async function expectAlert(text: string | RegExp) {
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent(text));
}

describe("sandbox giving", () => {
  it.each([
    "http://checkout.stripe.com/session",
    "https://checkout.stripe.com.attacker.example/session",
    "https://attacker@checkout.stripe.com/session",
    "https://checkout.stripe.com:444/session",
  ])("rejects an unsafe checkout destination: %s", async (url) => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({ url }), { status: 200 })));
    render(<Donate verifiedEmail="ben@example.org" />);
    fireEvent.change(screen.getByLabelText("Full name"), { target: { value: "Ben Donor" } });
    fireEvent.change(screen.getByLabelText("ZIP code"), { target: { value: "10001" } });
    fireEvent.change(screen.getByLabelText("City"), { target: { value: "New York" } });
    fireEvent.change(screen.getByLabelText("Email"), { target: { value: "ben@example.org" } });
    fireEvent.click(screen.getByRole("button", { name: /^Continue to secure checkout/ }));
    await expectAlert("unexpected destination");
    expect(screen.getByRole("button", { name: /^Continue to secure checkout/ })).toBeEnabled();
  });

  it("frames the gift in dollars a day beside the monthly charge", () => {
    render(<Donate verifiedEmail="ben@example.org" />);
    expect(screen.getByRole("button", { name: "$1 a day · $30/month" })).toBeVisible();
    expect(screen.getByRole("button", { name: "$1 a day · $30/month" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.queryByLabelText("Dollars a day")).not.toBeInTheDocument();
  });
  it("shows the daily and monthly figures in every preset tile, as separate equal elements", () => {
    render(<Donate verifiedEmail="ben@example.org" />);
    for (const [daily, monthly] of [
      ["$1 a day", "$30/month"],
      ["$3 a day", "$90/month"],
      ["$5 a day", "$150/month"],
    ]) {
      const tile = screen.getByRole("button", { name: `${daily} · ${monthly}` });
      const figures = [...tile.querySelectorAll("span")].map((span) => span.textContent);
      expect(figures).toEqual([daily, monthly]);
    }
  });
  it("reveals the amount input only for Other, focuses it, and states the cap", () => {
    render(<Donate verifiedEmail="ben@example.org" />);
    const other = screen.getByRole("button", { name: /^Other/ });
    expect(other).toHaveAttribute("aria-pressed", "false");
    const amount = openOther();
    expect(amount).toHaveFocus();
    expect(other).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("button", { name: "$1 a day · $30/month" })).toHaveAttribute("aria-pressed", "false");
    expect(amount).toHaveAccessibleDescription(/Up to \$30 a day/);
    fireEvent.click(screen.getByRole("button", { name: "$3 a day · $90/month" }));
    expect(screen.queryByLabelText("Dollars a day")).not.toBeInTheDocument();
  });
  it("mounts the amount input when a non-preset amount is restored, without taking focus", async () => {
    sessionStorage.setItem("dad.donate.choice", JSON.stringify({ amount: "7", email: "ben@example.org" }));
    render(<Donate verifiedEmail="ben@example.org" />);
    const amount = await screen.findByLabelText("Dollars a day");
    expect(amount).toHaveValue("7");
    expect(amount).not.toHaveFocus();
    expect(screen.getByRole("button", { name: /^Other/ })).toHaveAttribute("aria-pressed", "true");
  });
  it("keeps the amount input mounted and focused while typing a preset digit after a non-preset restore", async () => {
    sessionStorage.setItem("dad.donate.choice", JSON.stringify({ amount: "7", email: "ben@example.org" }));
    render(<Donate verifiedEmail="ben@example.org" />);
    const amount = await screen.findByLabelText("Dollars a day");
    amount.focus();
    fireEvent.change(amount, { target: { value: "1" } });
    expect(screen.getByLabelText("Dollars a day")).toBe(amount);
    expect(amount).toHaveFocus();
    fireEvent.change(amount, { target: { value: "12" } });
    expect(screen.getByLabelText("Dollars a day")).toHaveValue("12");
    expect(screen.getByRole("button", { name: /^Other/ })).toHaveAttribute("aria-pressed", "true");
  });
  it("mounts the amount input for a restored invalid amount so the rejection can focus it", async () => {
    sessionStorage.setItem("dad.donate.choice", JSON.stringify({ amount: "99", email: "ben@example.org" }));
    vi.stubGlobal("fetch", vi.fn());
    render(<Donate verifiedEmail="ben@example.org" />);
    const amount = await screen.findByLabelText("Dollars a day");
    fireEvent.change(screen.getByLabelText("Full name"), { target: { value: "Ben Donor" } });
    fireEvent.change(screen.getByLabelText("ZIP code"), { target: { value: "10001" } });
    fireEvent.change(screen.getByLabelText("City"), { target: { value: "New York" } });
    fireEvent.click(checkout());
    await expectAlert("from $1 to $30");
    expect(amount).toHaveFocus();
  });
  it("follows the chosen amount in the live summary and the submit label", () => {
    render(<Donate verifiedEmail="ben@example.org" />);
    expect(screen.getByText(/^You’ll give/)).toHaveTextContent("You’ll give $30/month · $1 a day");
    expect(checkout()).toHaveTextContent("Continue to secure checkout · $30/month");
    fireEvent.click(screen.getByRole("button", { name: "$5 a day · $150/month" }));
    expect(screen.getByText(/^You’ll give/)).toHaveTextContent("You’ll give $150/month · $5 a day");
    expect(checkout()).toHaveTextContent("· $150/month");
    fireEvent.change(openOther(), { target: { value: "12" } });
    expect(checkout()).toHaveTextContent("· $360/month");
    fireEvent.change(screen.getByLabelText("Dollars a day"), { target: { value: "x" } });
    expect(checkout()).toHaveAccessibleName("Continue to secure checkout");
  });
  it("orders the fields email, full name, then ZIP and city, with the receipt hint tied to email", () => {
    render(<Donate verifiedEmail="ben@example.org" />);
    const labels = ["Email", "Full name", "ZIP code", "City"].map((label) => screen.getByLabelText(label));
    for (let i = 1; i < labels.length; i++)
      expect(labels[i - 1].compareDocumentPosition(labels[i]) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(labels[0]).toHaveAccessibleDescription(
      "Your verified account email receives the receipt. Change it in your profile.",
    );
    expect(labels[2]).toHaveAttribute("inputmode", "numeric");
    expect(labels[2]).toHaveAttribute("pattern", "[0-9]{5}(-[0-9]{4})?");
    expect(screen.getByText("1 · Your daily gift")).toBeVisible();
    expect(screen.getByText("2 · Where we send your receipt")).toBeVisible();
  });
  it("sends exactly the same checkout request body and keys as before", async () => {
    const fetch = vi.fn().mockRejectedValue(new TypeError("Failed to fetch"));
    vi.stubGlobal("fetch", fetch);
    render(<Donate verifiedEmail="ben@example.org" />);
    fireEvent.change(screen.getByLabelText("Email"), { target: { value: "ben@example.org" } });
    fireEvent.change(screen.getByLabelText("Full name"), { target: { value: "Ben Donor" } });
    fireEvent.change(screen.getByLabelText("ZIP code"), { target: { value: "10001" } });
    fireEvent.change(screen.getByLabelText("City"), { target: { value: "New York" } });
    fireEvent.click(screen.getByRole("button", { name: "$3 a day · $90/month" }));
    fireEvent.click(checkout());
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(1));
    const [path, init] = fetch.mock.calls[0];
    expect(String(path)).toContain("/donor/checkouts");
    expect(init.method).toBe("POST");
    expect(Object.keys(init.headers)).toContain("Idempotency-Key");
    expect(init.body).toBe(
      '{"email":"ben@example.org","name":"Ben Donor","zip":"10001","city":"New York","amount_minor":9000,"frequency":"MONTHLY"}',
    );
    expect(JSON.parse(sessionStorage.getItem("dad.donate.choice") ?? "null")).toEqual({
      amount: "3",
      email: "ben@example.org",
      name: "Ben Donor",
      zip: "10001",
      city: "New York",
    });
  });
  it("keeps the card-handling and test-payment notes next to the submit", () => {
    render(<Donate verifiedEmail="ben@example.org" />);
    expect(screen.getByText(/Card details go to Stripe, never to DAD\./)).toBeVisible();
    expect(screen.getByText(/Preview: test payments only/)).toBeVisible();
  });

  it("charges a custom daily gift as the daily amount × 30", () => {
    render(<Donate verifiedEmail="ben@example.org" />);
    const amount = openOther();
    fireEvent.change(amount, { target: { value: "7" } });
    expect(screen.getByText(/^You’ll give/)).toHaveTextContent("You’ll give $210/month · $7 a day");
    expect(amount).toHaveAccessibleDescription(/\$210\/month/);
    fireEvent.change(amount, { target: { value: "31" } });
    expect(screen.queryByText(/^You’ll give/)).not.toBeInTheDocument();
  });

  it("marks exactly the chosen preset as pressed", () => {
    render(<Donate verifiedEmail="ben@example.org" />);
    const three = screen.getByRole("button", { name: "$3 a day · $90/month" });
    expect(screen.getByRole("button", { name: "$1 a day · $30/month" })).toHaveAttribute("aria-pressed", "true");
    fireEvent.click(three);
    expect(three).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("button", { name: "$1 a day · $30/month" })).toHaveAttribute("aria-pressed", "false");
    expect(screen.queryByLabelText("Dollars a day")).not.toBeInTheDocument();
  });

  it("keeps the idempotency key on retry and sends the monthly charge in integer cents", async () => {
    const fetch = vi.fn().mockRejectedValue(new TypeError("Failed to fetch"));
    vi.stubGlobal("fetch", fetch);
    render(<Donate verifiedEmail="ben@example.org" />);
    fireEvent.change(screen.getByLabelText("Full name"), { target: { value: "Ben Donor" } });
    fireEvent.change(screen.getByLabelText("ZIP code"), { target: { value: "10001" } });
    fireEvent.change(screen.getByLabelText("City"), { target: { value: "New York" } });
    fireEvent.change(screen.getByLabelText("Email"), { target: { value: "ben@example.org" } });
    fireEvent.change(openOther(), { target: { value: "30" } });
    fireEvent.click(screen.getByRole("button", { name: /^Continue to secure checkout/ }));
    await expectAlert("offline");
    expect(screen.getByRole("alert")).not.toHaveTextContent("Failed to fetch");
    fireEvent.click(screen.getByRole("button", { name: /^Continue to secure checkout/ }));
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(2));
    expect(fetch.mock.calls[0][1].headers["Idempotency-Key"]).toBe(fetch.mock.calls[1][1].headers["Idempotency-Key"]);
    expect(JSON.parse(fetch.mock.calls[0][1].body).amount_minor).toBe(90000);
  });

  it.each(["1.50", "31", "0"])(
    "refuses %s dollars a day without sending it, and ties the error to the amount",
    async (value) => {
      const fetch = vi.fn();
      vi.stubGlobal("fetch", fetch);
      render(<Donate verifiedEmail="ben@example.org" />);
      fireEvent.change(screen.getByLabelText("Full name"), { target: { value: "Ben Donor" } });
      fireEvent.change(screen.getByLabelText("ZIP code"), { target: { value: "10001" } });
      fireEvent.change(screen.getByLabelText("City"), { target: { value: "New York" } });
      fireEvent.change(screen.getByLabelText("Email"), { target: { value: "ben@example.org" } });
      const amount = openOther();
      fireEvent.change(amount, { target: { value } });
      fireEvent.click(screen.getByRole("button", { name: /^Continue to secure checkout/ }));
      await expectAlert("whole number of dollars a day from $1 to $30");
      expect(amount).toHaveAttribute("aria-invalid", "true");
      expect(amount).toHaveAccessibleDescription(/Larger gifts are arranged with DAD directly/);
      expect(amount).toHaveFocus();
      expect(fetch).not.toHaveBeenCalled();
      expect(screen.queryByLabelText(/card number/i)).not.toBeInTheDocument();
      fireEvent.change(amount, { target: { value: "2" } });
      expect(amount).not.toHaveAttribute("aria-invalid");
    },
  );

  it("returns from a cancelled checkout with the same choice, and submits what is shown", async () => {
    const fetch = vi.fn().mockRejectedValueOnce(new TypeError("Failed to fetch"));
    vi.stubGlobal("fetch", fetch);
    const { unmount } = render(<Donate verifiedEmail="ben@example.org" />);
    fireEvent.change(openOther(), { target: { value: "$10" } });
    fireEvent.change(screen.getByLabelText("Full name"), { target: { value: "Ben Donor" } });
    fireEvent.change(screen.getByLabelText("ZIP code"), { target: { value: "10001" } });
    fireEvent.change(screen.getByLabelText("City"), { target: { value: "New York" } });
    fireEvent.change(screen.getByLabelText("Email"), { target: { value: "ben@example.org" } });
    fireEvent.click(screen.getByRole("button", { name: /^Continue to secure checkout/ }));
    await expectAlert("offline");
    unmount();
    const retry = vi
      .fn()
      .mockResolvedValue(new Response(JSON.stringify({ url: "https://evil.example/" }), { status: 200 }));
    vi.stubGlobal("fetch", retry);
    render(<Donate verifiedEmail="ben@example.org" />);
    expect(await screen.findByLabelText("Dollars a day")).toHaveValue("$10");
    expect(screen.getByLabelText("Email")).toHaveValue("ben@example.org");
    fireEvent.click(screen.getByRole("button", { name: /^Continue to secure checkout/ }));
    await waitFor(() => expect(retry).toHaveBeenCalled());
    expect(JSON.parse(retry.mock.calls[0][1].body)).toEqual({
      email: "ben@example.org",
      name: "Ben Donor",
      zip: "10001",
      city: "New York",
      amount_minor: 30000,
      frequency: "MONTHLY",
    });
  });

  it("clears a stale error as soon as the donor edits the form", async () => {
    vi.stubGlobal("fetch", vi.fn());
    render(<Donate verifiedEmail="ben@example.org" />);
    fireEvent.change(screen.getByLabelText("Full name"), { target: { value: "Ben Donor" } });
    fireEvent.change(screen.getByLabelText("ZIP code"), { target: { value: "10001" } });
    fireEvent.change(screen.getByLabelText("City"), { target: { value: "New York" } });
    fireEvent.change(screen.getByLabelText("Email"), { target: { value: "ben@example.org" } });
    fireEvent.change(openOther(), { target: { value: "0.50" } });
    fireEvent.click(screen.getByRole("button", { name: /^Continue to secure checkout/ }));
    await expectAlert("from $1 to $30");
    fireEvent.click(screen.getByRole("button", { name: "$3 a day · $90/month" }));
    expect(screen.getByRole("alert")).toBeEmptyDOMElement();
  });

  it("leads with the short promise and states the Founding deadline at most once", async () => {
    // Fixed clock so the FoundingLine is certain to render its deadline before we count it.
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-06-15T16:00:00Z"));
    render(<Donate verifiedEmail="ben@example.org" />);
    await screen.findByText(/Founding Donor window/);
    expect(screen.getByText("A small monthly gift, securely linked to your verified donor account.")).toBeVisible();
    expect(screen.queryByText(/before January 2027/)).not.toBeInTheDocument();
    expect((document.body.textContent?.match(/December 31, 2026/g) ?? []).length).toBe(1);
  });
  it("is usable again when the browser restores it from the back/forward cache mid-redirect", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() => new Promise(() => {})),
    );
    render(<Donate verifiedEmail="ben@example.org" />);
    fireEvent.change(screen.getByLabelText("Full name"), { target: { value: "Ben Donor" } });
    fireEvent.change(screen.getByLabelText("ZIP code"), { target: { value: "10001" } });
    fireEvent.change(screen.getByLabelText("City"), { target: { value: "New York" } });
    fireEvent.change(screen.getByLabelText("Email"), { target: { value: "ben@example.org" } });
    fireEvent.click(screen.getByRole("button", { name: /^Continue to secure checkout/ }));
    expect(await screen.findByRole("button", { name: "Opening secure checkout…" })).toBeDisabled();
    const restored = Object.assign(new Event("pageshow"), { persisted: true });
    act(() => {
      window.dispatchEvent(restored);
    });
    expect(screen.getByRole("button", { name: /^Continue to secure checkout/ })).toBeEnabled();
  });
});

it("keeps the receipt email bound to the verified account despite DOM or stored-value changes", async () => {
  sessionStorage.setItem(
    "dad.donate.choice",
    JSON.stringify({ amount: "7", email: "other@example.org", name: "Other Person" }),
  );
  const fetch = vi.fn().mockRejectedValue(new TypeError("Failed to fetch"));
  vi.stubGlobal("fetch", fetch);
  render(<Donate verifiedEmail="ben@example.org" />);
  const email = screen.getByLabelText("Email");
  expect(email).toHaveValue("ben@example.org");
  expect(email).toHaveAttribute("readonly");
  fireEvent.change(email, { target: { value: "other@example.org" } });
  fireEvent.change(screen.getByLabelText("Full name"), { target: { value: "Ben Donor" } });
  fireEvent.change(screen.getByLabelText("ZIP code"), { target: { value: "10001" } });
  fireEvent.change(screen.getByLabelText("City"), { target: { value: "New York" } });
  fireEvent.click(checkout());
  await waitFor(() => expect(fetch).toHaveBeenCalledTimes(1));
  expect(JSON.parse(fetch.mock.calls[0][1].body).email).toBe("ben@example.org");
});
