import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import DashboardPage from "./Dashboard";

const data = {
  mode: "test",
  state: "ready",
  computed_at: "2027-01-01T00:00:00Z",
  founding_donor: true,
  referrals: { link: 2, irl: 3, total_minor: 12345, link_minor: 10000, irl_minor: 2345, currency: "USD" },
  streak: {
    current: 2,
    at_risk: false,
    grace_ends_at: null,
    ladder: { days: 24, last_milestone: 7, next_milestone: 30, days_to_next: 6, bar: Array(30).fill(true) },
  },
  rank: { national_percentile: 2, city: "Reno", city_rank: 7, computed_at: "2027-01-01T00:00:00Z" },
  network: { active_count: 3, annualized_minor: 108000, currency: "USD" },
  impact: { given_minor: 4500, currency: "USD", initiatives_funded: 4, events_volunteered: 1 },
};
const today = {
  initiative_id: "i-1",
  slug: "reno-meals",
  title: "Reno meals",
  summary: "Hot meals.",
  region: "Northern Nevada",
  location: "Reno, NV",
  type: "Food",
  started_on: "2026-03-01",
  metrics: [{ label: "Meals served", value: 1200, unit: "PEOPLE", verified_on: "2026-09-30" }],
  distinct_shown: 4,
};
afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});
/** Stubs fetch with `dashboard` for the dashboard and `initiative` (default: none today) for today's initiative. */
function stubFetch(dashboard: (input: RequestInfo | URL) => Promise<unknown>, initiative?: unknown) {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes("today-initiative"))
        return Promise.resolve(
          initiative
            ? { ok: true, json: async () => initiative }
            : { ok: false, status: 404, json: async () => ({ detail: "Record not found" }) },
        );
      if (url.includes("/spend"))
        return Promise.resolve({
          ok: true,
          json: async () => ({
            initiative_id: "i-1",
            total_minor: 90000,
            categories: [{ category: "Food", amount_minor: 90000 }],
          }),
        });
      return dashboard(input);
    }),
  );
}
/** A totals-strip entry from the loaded layout, not the aria-hidden skeleton that precedes it. */
function loadedStat(label: string): HTMLElement {
  const stat = screen
    .getAllByText(label, { selector: "dt" })
    .map((term) => term.closest<HTMLElement>(".stat"))
    .find((entry) => entry && !entry.closest('[aria-hidden="true"]'));
  if (!stat) throw new Error(`Missing ${label} stat`);
  return stat;
}
function respond(value: unknown, initiative?: unknown) {
  stubFetch(() => Promise.resolve({ ok: true, json: async () => value }), initiative);
}

it("reserves the metric layout without announcing unverified results", async () => {
  let resolve!: (response: { ok: boolean; json: () => Promise<typeof data> }) => void;
  stubFetch(
    () =>
      new Promise((done) => {
        resolve = done;
      }),
  );
  const { container } = render(<DashboardPage />);
  expect(container.querySelectorAll('[aria-hidden="true"] .metricSkeleton')).toHaveLength(7);
  // The skeleton mirrors the loaded layout: the same titles, in the same grids.
  expect(container.querySelectorAll(".statStrip")).toHaveLength(1);
  expect(container.querySelectorAll(".metrics.pair .metric")).toHaveLength(2);
  expect(screen.getByRole("status")).toHaveTextContent("Loading your dashboard");
  expect(screen.queryByRole("heading", { name: "Link Referrals" })).not.toBeInTheDocument();
  expect(screen.queryByText("Earned")).not.toBeInTheDocument();
  resolve({ ok: true, json: async () => data });
  expect(await screen.findByText("$123.45")).toBeVisible();
  expect(container.querySelectorAll(".metricSkeleton")).toHaveLength(0);
});

it("shows the five elements with separate channels and collected-dollar breakdown", async () => {
  respond(data);
  render(<DashboardPage />);
  expect(await screen.findByRole("heading", { name: "Link Referrals" })).toBeVisible();
  // Link, IRL, raised, streak, network, Founding Donor, plus the three nav group titles.
  expect(screen.getAllByRole("heading", { level: 2 })).toHaveLength(9);
  const link = screen.getByRole("heading", { name: "Link Referrals" }).closest("article");
  const irl = screen.getByRole("heading", { name: "IRL Referrals" }).closest("article");
  if (!link || !irl) throw new Error("Missing referral cards");
  expect(within(link).getByText("2")).toBeVisible();
  expect(within(irl).getByText("3")).toBeVisible();
  expect(screen.getByText("$123.45")).toBeVisible();
  expect(screen.getByText("24 days")).toBeVisible();
  expect(screen.getAllByText("6 days to your 30-day milestone.")).toHaveLength(2);
  expect(screen.getByText("Top 2% nationally · #7 in Reno")).toBeVisible();
  expect(screen.getByText("$1,080.00 a year")).toBeVisible();
  expect(screen.getByRole("img", { name: "30 of the last 30 days in your current streak" })).toBeVisible();
  fireEvent.click(screen.getByText("View Link and IRL breakdown"));
  expect(screen.getByText("Link: $100.00")).toBeVisible();
  expect(screen.getByText("IRL: $23.45")).toBeVisible();
  const badge = screen.getByRole("heading", { name: "Founding Donor" }).closest("article");
  if (!badge) throw new Error("Missing badge card");
  expect(within(badge).getByText("Earned. Yours for good.")).toBeVisible();
  expect(within(badge).getByText("You joined before DAD launched. Thank you for starting this with us.")).toBeVisible();
  expect(screen.queryByText(/not yet earned/)).not.toBeInTheDocument();
  expect(screen.queryByText(/Combined referrals/i)).not.toBeInTheDocument();
});

it("counts today's initiative as funded once its card has loaded", async () => {
  respond(data, { ...today, distinct_shown: 6 });
  render(<DashboardPage />);
  await waitFor(() => expect(within(loadedStat("Initiatives funded")).getByText("6")).toBeVisible());
});

it("shows the badge only when earned, and otherwise explains how it is earned", async () => {
  respond({ ...data, founding_donor: false });
  render(<DashboardPage />);
  expect(await screen.findByRole("heading", { name: "Link Referrals" })).toBeVisible();
  expect(screen.queryByRole("heading", { name: "Founding Donor" })).not.toBeInTheDocument();
  expect(screen.getAllByRole("heading", { level: 2 })).toHaveLength(8);
  expect(screen.getByText(/Founding Donor badge: not yet earned/)).toHaveTextContent(
    "A first gift by December 31, 2026 (New York time)",
  );
});

it("does not substitute zeros for unavailable metrics", async () => {
  respond({ ...data, referrals: null, streak: null });
  render(<DashboardPage />);
  expect(await screen.findByText(/streak is temporarily unavailable/)).toBeVisible();
  expect(screen.getAllByText("Updating")).toHaveLength(4);
  expect(screen.queryByText("0 days")).not.toBeInTheDocument();
  expect(screen.queryByText("$0.00")).not.toBeInTheDocument();
});

it("can refresh an unavailable streak while keeping verified referral totals", async () => {
  stubFetch(
    vi
      .fn()
      .mockResolvedValueOnce({ ok: true, json: async () => ({ ...data, streak: null }) })
      .mockResolvedValueOnce({ ok: true, json: async () => data }),
  );
  render(<DashboardPage />);
  expect(await screen.findByText("$123.45")).toBeVisible();
  expect(screen.getByRole("status")).toHaveTextContent("Your streak is updating");
  expect(screen.queryByText("0 days")).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Refresh dashboard" }));
  expect(await screen.findByText("24 days")).toBeVisible();
  expect(screen.getByText("$123.45")).toBeVisible();
  expect(screen.queryByRole("button", { name: "Refresh dashboard" })).not.toBeInTheDocument();
});

it("explains the payment grace state", async () => {
  respond({
    ...data,
    streak: { ...data.streak, at_risk: true, grace_ends_at: "2027-02-15T00:00:00Z" },
  });
  render(<DashboardPage />);
  expect(await screen.findByText(/Your streak is at risk/)).toHaveTextContent(
    "grace period ends, to keep your 24-day streak",
  );
  expect(screen.getByRole("link", { name: "Update payment method" })).toHaveAttribute(
    "href",
    "/account/payment-method",
  );
});

it("handles a logged-out response", async () => {
  stubFetch(vi.fn().mockResolvedValue({ ok: false, status: 401, json: async () => ({ detail: "Sign in required" }) }));
  render(<DashboardPage />);
  expect(await screen.findByRole("link", { name: "Sign in to view your dashboard" })).toBeVisible();
  expect(screen.getByRole("heading", { level: 1, name: "Your dashboard" })).toBeVisible();
  expect(screen.queryByText("$0.00")).not.toBeInTheDocument();
});

it("supports connection failure and retry", async () => {
  stubFetch(
    vi
      .fn()
      .mockRejectedValueOnce(new Error("Offline"))
      .mockResolvedValueOnce({ ok: true, json: async () => data }),
  );
  render(<DashboardPage />);
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("Check your connection"));
  expect(screen.getByRole("alert")).not.toHaveTextContent("Offline");
  fireEvent.click(screen.getByRole("button", { name: "Refresh dashboard" }));
  expect(await screen.findByText("$123.45")).toBeVisible();
  expect(screen.getByRole("alert")).toBeEmptyDOMElement();
});

it("distinguishes preparing data from an empty account", async () => {
  respond({ mode: "test", state: "preparing" });
  render(<DashboardPage />);
  expect(await screen.findByText(/preparing your dashboard/)).toBeVisible();
  expect(screen.queryByText("0 days")).not.toBeInTheDocument();
});

it("shows the donor's own impact and today's initiative with its spending", async () => {
  respond(data, today);
  render(<DashboardPage />);
  expect(await screen.findByText("$45.00")).toBeVisible();
  const given = loadedStat("Your giving");
  expect(within(given).getByText("$45.00")).toBeVisible();
  const funded = loadedStat("Initiatives funded");
  const volunteered = loadedStat("Events volunteered");
  expect(within(funded).getByText("4")).toBeVisible();
  expect(within(volunteered).getByText("1")).toBeVisible();
  expect(await screen.findByRole("heading", { name: "Reno meals" })).toBeVisible();
  expect(screen.getByText("Food · Reno, NV · Since March 1, 2026")).toBeVisible();
  expect(screen.getByText("1,200 people")).toBeVisible();
  expect(screen.getByText("Meals served")).toBeVisible();
  expect(screen.getByText("Verified September 30, 2026")).toBeVisible();
  fireEvent.click(screen.getByText("See every dollar this initiative has spent"));
  expect(await screen.findByText("Food: $900.00")).toBeVisible();
});

it("shows giving as updating rather than zero while it is recomputed", async () => {
  respond({ ...data, impact: { ...data.impact, given_minor: null } });
  render(<DashboardPage />);
  await waitFor(() => expect(within(loadedStat("Your giving")).getByText("Updating")).toBeVisible());
});

const quiet = { ...data, streak: { ...data.streak, ladder: { ...data.streak.ladder, days_to_next: 8 } } };
it("greets without a name and names the single next step", async () => {
  respond(data);
  const { unmount } = render(<DashboardPage />);
  expect(screen.getByRole("heading", { level: 1, name: "Welcome back." })).toBeVisible();
  // 6 days to go is within the 7-day threshold.
  expect(await screen.findAllByText("6 days to your 30-day milestone.")).toHaveLength(2);
  unmount();
  respond(quiet);
  render(<DashboardPage />);
  expect(await screen.findByText("Here is what your giving has done so far.")).toBeVisible();
});
it("names a payment problem before anything else and leaves the callout the only primary button", async () => {
  respond({ ...data, referrals: { ...data.referrals, link: 0, irl: 0 }, streak: { ...data.streak, at_risk: true } });
  const { container } = render(<DashboardPage />);
  expect(await screen.findByText("Your latest monthly payment didn’t go through.")).toBeVisible();
  expect(container.querySelectorAll(".button")).toHaveLength(1);
  expect(screen.getByRole("link", { name: "Invite someone" })).toHaveClass("secondary");
});
it("invites a first referral only when referrals are known to be zero", async () => {
  respond({ ...data, referrals: { ...data.referrals, link: 0, irl: 0, total_minor: 0 } });
  const { container } = render(<DashboardPage />);
  expect(await screen.findByText("Your first invite is one tap away.")).toBeVisible();
  expect(screen.getByText("No one yet. Your link is ready to share.")).toBeVisible();
  expect(screen.getByText("No one yet. Your code is ready to scan.")).toBeVisible();
  expect(screen.getByText("Gifts from people you invite will add up here.")).toBeVisible();
  expect(container.querySelectorAll(".metricValue.zero")).toHaveLength(3);
  expect(container.querySelectorAll(".button")).toHaveLength(1);
});
it("treats unknown referrals as unknown, not as zero", async () => {
  respond({ ...quiet, referrals: null });
  render(<DashboardPage />);
  expect(await screen.findByText("Here is what your giving has done so far.")).toBeVisible();
  expect(screen.queryByText(/No one yet/)).not.toBeInTheDocument();
  expect(screen.queryByText("Your first invite is one tap away.")).not.toBeInTheDocument();
});
it("groups the account links and keeps a path to the monthly gift", async () => {
  respond(data);
  render(<DashboardPage />);
  const nav = await screen.findByRole("navigation", { name: "Account" });
  expect(
    within(nav)
      .getAllByRole("heading", { level: 2 })
      .map((h) => h.textContent),
  ).toEqual(["Your giving", "Your circle", "You"]);
  const hrefs = within(nav)
    .getAllByRole("link")
    .map((a) => a.getAttribute("href"));
  expect(hrefs).toEqual([
    "/account/giving",
    "/account/recurring",
    "/account/payment-method",
    "/account/share",
    "/account/network",
    "/account/badges",
    "/account/events",
    "/account/employer",
    "/account/profile",
  ]);
  expect(within(nav).getByRole("link", { name: "Share and invite" })).toBeVisible();
});
it("counts the Founding days left in New York time, on the last day and after", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-12-31T17:00:00Z"));
  respond({ ...data, founding_donor: false });
  const { unmount } = render(<DashboardPage />);
  expect(await screen.findByText(/Founding Donor badge: not yet earned/)).toHaveTextContent("1 day left.");
  unmount();
  vi.setSystemTime(new Date("2027-01-01T05:00:00Z"));
  render(<DashboardPage />);
  expect(await screen.findByText(/Founding Donor badge: not yet earned/)).toHaveTextContent(
    "The Founding window has closed.",
  );
});

it("groups thousands in the streak length and milestone, as the PRD writes them", async () => {
  respond({
    ...data,
    streak: { ...data.streak, ladder: { ...data.streak.ladder, days: 1730, next_milestone: 1825, days_to_next: 95 } },
  });
  render(<DashboardPage />);
  expect(await screen.findByText("1,730 days")).toBeVisible();
  expect(screen.getByText("95 days to your 1,825-day milestone.")).toBeVisible();
});

it("says '1 day' rather than '1 days' on the streak card and the next step", async () => {
  respond({ ...data, streak: { ...data.streak, ladder: { ...data.streak.ladder, days_to_next: 1 } } });
  render(<DashboardPage />);
  expect(await screen.findAllByText("1 day to your 30-day milestone.")).toHaveLength(2);
  expect(screen.queryByText(/1 days/)).not.toBeInTheDocument();
});

const layout = {
  catalog_version: 1,
  spec_id: "5b0c7a8e-1111-4222-8333-444455556666",
  spec: [
    { type: "Heading", title: "Your impact" },
    { type: "Metric", bind: "/impact/given_minor", format: "usd_minor", caption: "given" },
    { type: "LinkButton", navigate: "/account/gifts" },
    { type: "Carousel", bind: "/mode" },
  ],
};

it("draws the approved insights layout in its own section, from the dashboard's figures", async () => {
  respond({ ...data, insights: layout });
  const { container } = render(<DashboardPage />);
  const panel = (await screen.findByRole("heading", { level: 2, name: "Your insights" })).closest("section");
  if (!panel) throw new Error("Missing insights section");
  expect(within(panel).getByRole("heading", { level: 3, name: "Your impact" })).toBeVisible();
  expect(within(panel).getByText("$45.00")).toBeVisible();
  expect(within(panel).getByText("Total given")).toBeVisible();
  expect(within(panel).getByRole("link", { name: "Your gifts" })).toHaveAttribute("href", "/account/gifts");
  expect(panel.closest(".metrics")).toBeNull();
  expect(container.querySelectorAll(".metrics.pair .metric")).toHaveLength(2);
});

it.each([
  ["missing", {}],
  ["null", { insights: null }],
  ["for a newer catalog", { insights: { ...layout, catalog_version: 2 } }],
  ["empty", { insights: { ...layout, spec: [] } }],
])("draws no insights section when the field is %s", async (_name, extra) => {
  respond({ ...data, ...extra });
  render(<DashboardPage />);
  expect(await screen.findByRole("heading", { name: "Link Referrals" })).toBeVisible();
  expect(screen.queryByRole("heading", { name: "Your insights" })).not.toBeInTheDocument();
  expect(screen.getAllByRole("heading", { level: 2 })).toHaveLength(9);
});
