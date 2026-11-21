import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import PartnerDetail from "./PartnerDetail";

afterEach(() => {
  vi.unstubAllGlobals();
  sessionStorage.clear();
});

const detail = {
  id: "p-1",
  organization: "Delta Inc",
  type: "Corporate",
  company_code: "DELTAX7K2QP",
  workforce_size: 1200,
  match_rate_bp: 5000,
  match_cap_minor: 0,
  approved_at: "2026-09-01T12:00:00Z",
  user_count: 1,
  initiative_count: 1,
  website: "https://delta.example",
  has_logo: false,
  initiatives: [
    { id: "i-1", title: "Reno meals", status: "ACTIVE", publish_state: "PUBLISHED", committed_minor: 2_500_000 },
  ],
  users: [{ id: "u-1", email: "dana@delta.example", name: "Dana", role: "ADMIN", accepted: false }],
};

const match = {
  year: new Date().getFullYear(),
  match_rate_bp: 5000,
  match_cap_minor: 0,
  gifts_minor: 100_000,
  owed_minor: 50_000,
  paid_minor: 20_000,
  outstanding_minor: 30_000,
};

function stubApi() {
  const fetch = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    if (init?.method === "POST")
      return Promise.resolve(Response.json(url.endsWith("/match-payment") ? { recorded: true } : { saved: true }));
    if (url.includes("/match-status")) return Promise.resolve(Response.json(match));
    return Promise.resolve(Response.json(detail));
  });
  vi.stubGlobal("fetch", fetch);
  return fetch;
}

function posted(fetch: ReturnType<typeof stubApi>, suffix: string) {
  const call = fetch.mock.calls.find(([u, i]) => String(u).endsWith(suffix) && i?.method === "POST");
  return call ? JSON.parse(String(call[1]?.body ?? "null")) : undefined;
}

// A save reloads the page, which swaps the content for a loading state until the
// refetch lands; interacting before then acts on elements about to be replaced.
async function reloaded(fetch: ReturnType<typeof stubApi>, loads: number) {
  await waitFor(() =>
    expect(fetch.mock.calls.filter(([u, i]) => String(u).endsWith("/partners/p-1") && !i?.method)).toHaveLength(loads),
  );
}

it("shows the partner's match position, initiatives, and users", async () => {
  stubApi();
  render(<PartnerDetail partner="p-1" />);
  expect(await screen.findByText(/match owed \$500 · received \$200 · outstanding \$300/)).toBeInTheDocument();
  expect(screen.getByText(/Reno meals · ACTIVE · Committed \$25,000/)).toBeInTheDocument();
  expect(screen.getByText(/Invitation not yet used/)).toBeInTheDocument();
});

it("resends an unused invitation for a partner user", async () => {
  const fetch = stubApi();
  render(<PartnerDetail partner="p-1" />);
  fireEvent.click(await screen.findByRole("button", { name: "Resend invitation to dana@delta.example" }));
  await waitFor(() =>
    expect(screen.getByRole("status")).toHaveTextContent("Resent the invitation to dana@delta.example."),
  );
  expect(fetch.mock.calls.some(([u, i]) => String(u).endsWith("/p-1/users/u-1/reinvite") && i?.method === "POST")).toBe(
    true,
  );
});

it("saves match terms in basis points and cents", async () => {
  const fetch = stubApi();
  render(<PartnerDetail partner="p-1" />);
  fireEvent.change(await screen.findByLabelText("Match rate (%)"), { target: { value: "100" } });
  fireEvent.change(screen.getByLabelText(/Annual match cap/), { target: { value: "10,000" } });
  fireEvent.click(screen.getByRole("button", { name: "Save match terms" }));
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Match terms saved."));
  expect(posted(fetch, "/p-1/match")).toEqual({
    match_rate_bp: 10000,
    match_cap_minor: 1_000_000,
    workforce_size: 1200,
  });
});

it("records a match payment and removes a user", async () => {
  const fetch = stubApi();
  render(<PartnerDetail partner="p-1" />);
  fireEvent.change(await screen.findByLabelText(/Match payment received/), { target: { value: "250" } });
  fireEvent.click(screen.getByRole("button", { name: "Record payment" }));
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Recorded a $250.00 match payment"));
  expect(posted(fetch, "/p-1/match-payment")).toEqual({ year: match.year, amount_minor: 25_000 });
  await reloaded(fetch, 2);
  fireEvent.click(await screen.findByRole("button", { name: "Remove dana@delta.example" }));
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Removed dana@delta.example."));
  expect(fetch.mock.calls.some(([u, i]) => String(u).endsWith("/p-1/users/u-1/remove") && i?.method === "POST")).toBe(
    true,
  );
});

it("restores an uncertain match payment after navigation and retries the identical request", async () => {
  const fetch = stubApi();
  const base = fetch.getMockImplementation();
  let payments = 0;
  fetch.mockImplementation((input, init) => {
    if (String(input).endsWith("/match-payment") && init?.method === "POST" && payments++ === 0)
      return Promise.reject(new TypeError("Connection lost after commit"));
    if (!base) throw new Error("Missing fixture fetch");
    return base(input, init);
  });
  const page = render(<PartnerDetail partner="p-1" />);
  fireEvent.change(await screen.findByLabelText(/Match payment received/), { target: { value: "250" } });
  fireEvent.click(screen.getByRole("button", { name: "Record payment" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("The recording is not confirmed");
  page.unmount();
  render(<PartnerDetail partner="p-1" />);
  expect(await screen.findByLabelText(/Match payment received/)).toHaveValue("250.00");
  expect(screen.getByLabelText(/Match payment received/)).toHaveAttribute("readonly");
  fireEvent.click(screen.getByRole("button", { name: "Retry recorded payment" }));
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Recorded a $250.00 match payment"));
  const attempts = fetch.mock.calls.filter(([u, i]) => String(u).endsWith("/match-payment") && i?.method === "POST");
  expect(attempts).toHaveLength(2);
  expect(attempts[0][1]?.headers).toEqual(attempts[1][1]?.headers);
  expect(new Headers(attempts[0][1]?.headers).get("Idempotency-Key")).toMatch(/^[a-f0-9-]{36}$/i);
  expect(attempts[0][1]?.body).toBe(attempts[1][1]?.body);
  expect(sessionStorage.getItem("dad:match-payment:p-1")).toBeNull();
});

it("uploads the partner's logo while enrolling it", async () => {
  const fetch = stubApi();
  render(<PartnerDetail partner="p-1" />);
  expect(await screen.findByRole("heading", { level: 2, name: "Organization logo" })).toBeInTheDocument();
  const file = new File([new Uint8Array([137, 80, 78, 71])], "delta.png", { type: "image/png" });
  const input = screen.getByLabelText("Logo image");
  fireEvent.change(input, { target: { files: [file] } });
  // jsdom's constraint validation does not see files set by a test, so submit directly.
  fireEvent.submit(input.closest("form") as HTMLFormElement);
  expect(await screen.findByText("Organization logo saved.")).toBeInTheDocument();
  expect(posted(fetch, "/donor/staff/partners/p-1/logo").data_url).toMatch(/^data:image\/png;base64,/);
});
