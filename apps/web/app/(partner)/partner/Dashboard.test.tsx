import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import Dashboard from "./Dashboard";

afterEach(() => vi.unstubAllGlobals());

it("hides unverified overview values instead of presenting fabricated zeros", async () => {
  stub({
    committed_minor: 0,
    deployed_minor: 0,
    people_reached: 0,
    committed_verified: false,
    deployed_verified: false,
    people_reached_verified: false,
  });
  render(<Dashboard />);
  const tiles = within(await screen.findByRole("region", { name: "Overview" }));
  expect(tiles.getAllByText("Awaiting verification")).toHaveLength(3);
  expect(tiles.queryByText("$0.00")).not.toBeInTheDocument();
  expect(tiles.queryByText("0")).not.toBeInTheDocument();
});

it("keeps a failed sign-out visible and lets the partner retry", async () => {
  stub();
  const base = vi.mocked(fetch).getMockImplementation();
  let resolveSignOut: ((response: Response) => void) | undefined;
  vi.mocked(fetch).mockImplementation((input, init) => {
    if (String(input).endsWith("/sign-out"))
      return new Promise<Response>((resolve) => {
        resolveSignOut = resolve;
      });
    if (!base) throw new Error("Missing fixture fetch");
    return base(input, init);
  });
  render(<Dashboard />);
  fireEvent.click(await screen.findByRole("button", { name: "Sign out" }));
  expect(screen.getByRole("button", { name: "Signing out…" })).toBeDisabled();
  resolveSignOut?.(Response.json({ detail: "Unavailable" }, { status: 503 }));
  expect(await screen.findByText(/Sign-out wasn’t confirmed/)).toBeVisible();
  expect(screen.getByRole("button", { name: "Sign out" })).toBeEnabled();
  expect(screen.getByRole("region", { name: "Overview" })).toBeVisible();
});

it("links retained reports and explains unavailable historical files", async () => {
  stub();
  const base = vi.mocked(fetch).getMockImplementation();
  vi.mocked(fetch).mockImplementation((input, init) => {
    if (String(input).endsWith("/exports"))
      return Promise.resolve(
        Response.json({
          exports: [
            {
              id: "retained",
              created_at: "2026-10-20T15:00:00Z",
              exported_by: "Ana",
              committed_minor: 150000,
              people_reached: 200,
              people_reached_verified: true,
              downloadable: true,
            },
            {
              id: "legacy",
              created_at: "2026-10-19T15:00:00Z",
              exported_by: "Ana",
              committed_minor: 0,
              people_reached: 0,
              people_reached_verified: false,
              downloadable: false,
            },
          ],
        }),
      );
    if (!base) throw new Error("Missing fixture fetch");
    return base(input, init);
  });
  render(<Dashboard />);
  expect(await screen.findByRole("link", { name: "Download report" })).toHaveAttribute(
    "href",
    "/api/donor/partner/reports/retained/report.pdf",
  );
  expect(screen.getByText("File was not retained")).toBeInTheDocument();
  expect(screen.getByText(/People reached awaiting verification/)).toBeInTheDocument();
});

const overview = {
  has_logo: false,
  committed_verified: true,
  deployed_verified: true,
  people_reached_verified: true,
  organization: "Delta Inc",
  description: "Meals and water across northern Nevada.",
  role: "ADMIN",
  company_code: "DELTA-7KQ4MX",
  committed_minor: 150000,
  deployed_minor: 50000,
  deployed_percent: 33,
  people_reached: 200,
  employees_enrolled: null,
  workforce_size: 400,
  enrolled_percent: null,
  updated_at: "2026-10-20T15:00:00Z",
};

const initiative = {
  id: "i1",
  title: "Reno Meals",
  summary: "Hot meals delivered daily.",
  status: "ACTIVE",
  region: "Northern Nevada",
  county_count: 3,
  committed_minor: 100000,
  deployed_minor: 40000,
  metrics: [
    { label: "Households served", value: 150, unit: "PEOPLE", verified_on: "2026-10-18" },
    { label: "Cost per delivery", value: 425, unit: "USD_CENTS", verified_on: "2026-10-18" },
  ],
  spend: [{ category: "Direct program delivery", amount_minor: 40000 }],
  updated_at: "2026-10-20T15:00:00Z",
};

function stub(overrides: Record<string, unknown> = {}, calls: { url: string; init?: RequestInit }[] = []) {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      calls.push({ url, init });
      if (url.endsWith("/overview")) return Promise.resolve(Response.json({ ...overview, ...overrides }));
      if (url.endsWith("/initiatives")) return Promise.resolve(Response.json({ initiatives: [initiative] }));
      if (url.endsWith("/spend"))
        return Promise.resolve(Response.json({ categories: [{ category: "Meals", amount_minor: 50000 }] }));
      if ((url.endsWith("/profile") || url.endsWith("/workforce")) && init?.method === "POST")
        return Promise.resolve(Response.json({ saved: true }));
      if (url.endsWith("/users") && init?.method === "POST") return Promise.resolve(Response.json({ invited: true }));
      if (url.endsWith("/users"))
        return Promise.resolve(
          Response.json({ users: [{ id: "u1", email: "a@delta.test", name: "Ana", role: "ADMIN" }] }),
        );
      if (url.endsWith("/exports")) return Promise.resolve(Response.json({ exports: [] }));
      if (url.endsWith("/our-people"))
        return Promise.resolve(
          Response.json({
            enrolled: 0,
            workforce_size: 400,
            given_minor: 0,
            matched_minor: 0,
            avg_streak_days: 0,
            volunteer_minutes: 0,
            teams: 0,
            suppressed: true,
          }),
        );
      if (url.endsWith("/teams")) return Promise.resolve(Response.json({ teams: [] }));
      return Promise.resolve(Response.json({}));
    }),
  );
}

it("shows the four headline tiles, suppressing enrollment under five employees", async () => {
  stub();
  render(<Dashboard />);
  const tiles = within(await screen.findByRole("region", { name: "Overview" }));
  expect(tiles.getByText("$1,500.00")).toBeInTheDocument();
  expect(tiles.getByText("33% of committed")).toBeInTheDocument();
  expect(tiles.getByText("200")).toBeInTheDocument();
  expect(tiles.getByText("Fewer than 5")).toBeInTheDocument();
  expect(screen.getByText(/Figures last updated/)).toBeInTheDocument();
});

it("shows each initiative's status, progress, headline metrics and detail", async () => {
  stub();
  render(<Dashboard />);
  const card = within(await screen.findByRole("article", { name: "Reno Meals" }));
  expect(card.getByText("Active")).toBeInTheDocument();
  expect(card.getByText("Northern Nevada · 3 counties")).toBeInTheDocument();
  expect(card.getByText("$400.00 deployed of $1,000.00 committed (40%)")).toBeInTheDocument();
  expect(card.getAllByText("$4.25").length).toBeGreaterThan(0);
  expect(card.getByText("Direct program delivery")).toBeInTheDocument();
});

it("gives a partner admin export and user management, and emails invitations", async () => {
  const calls: { url: string; init?: RequestInit }[] = [];
  stub({}, calls);
  render(<Dashboard />);
  expect(await screen.findByRole("button", { name: "Export verified report (PDF)" })).toBeInTheDocument();
  fireEvent.change(await screen.findByLabelText("Work email"), { target: { value: "val@delta.test" } });
  fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Val" } });
  fireEvent.click(screen.getByRole("button", { name: "Email invitation" }));
  expect(await screen.findByText("We emailed val@delta.test an invitation to sign in.")).toBeInTheDocument();
  const sent = calls.find((c) => c.url.endsWith("/users") && c.init?.method === "POST");
  expect(JSON.parse(String(sent?.init?.body))).toEqual({ email: "val@delta.test", name: "Val", role: "VIEWER" });
  expect(screen.getByText("DELTA-7KQ4MX")).toBeInTheDocument();
});

it("tells a partner admin why an export was refused instead of asking them to retry", async () => {
  stub();
  const base = vi.mocked(fetch).getMockImplementation();
  vi.mocked(fetch).mockImplementation((input, init) => {
    if (String(input).endsWith("/report.pdf"))
      return Promise.resolve(Response.json({ detail: "Too many requests" }, { status: 429 }));
    if (!base) throw new Error("Missing fixture fetch");
    return base(input, init);
  });
  render(<Dashboard />);
  fireEvent.click(await screen.findByRole("button", { name: "Export verified report (PDF)" }));
  expect(await screen.findByText("Too many requests from this connection. Wait a minute, then retry.")).toBeVisible();
});

it("keeps a partner viewer read-only", async () => {
  stub({ role: "VIEWER" });
  render(<Dashboard />);
  expect(
    await screen.findByText("Your organization’s partner administrators can export the report."),
  ).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /Export verified report/ })).not.toBeInTheDocument();
  expect(screen.queryByRole("region", { name: "Users" })).not.toBeInTheDocument();
  expect(screen.queryByRole("region", { name: "Organization" })).not.toBeInTheDocument();
  expect(screen.getByText("Meals and water across northern Nevada.")).toBeInTheDocument();
  expect(screen.getByText("DELTA-7KQ4MX")).toBeInTheDocument();
});

it("sends a signed-out partner to request a sign-in link", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(() => Promise.resolve(Response.json({ detail: "Partner sign-in required" }, { status: 401 }))),
  );
  render(<Dashboard />);
  await waitFor(() =>
    expect(screen.getByRole("link", { name: "Email me a sign-in link" })).toHaveAttribute("href", "/partner/sign-in"),
  );
});

it("lets a partner admin edit the organization profile and workforce size", async () => {
  const calls: { url: string; init?: RequestInit }[] = [];
  stub({}, calls);
  render(<Dashboard />);
  const organization = within(await screen.findByRole("region", { name: "Organization" }));
  expect(organization.getByLabelText("Organization name")).toHaveValue("Delta Inc");
  fireEvent.change(organization.getByLabelText("Organization name"), { target: { value: "Delta Group" } });
  fireEvent.change(organization.getByLabelText("Description"), { target: { value: "Water first." } });
  fireEvent.change(organization.getByLabelText("Workforce size"), { target: { value: "450" } });
  fireEvent.click(organization.getByRole("button", { name: "Save profile" }));
  expect(await organization.findByText("Organization profile saved.")).toBeInTheDocument();
  const body = (path: string) =>
    JSON.parse(String(calls.find((c) => c.url.endsWith(path) && c.init?.method === "POST")?.init?.body));
  expect(body("/profile")).toEqual({ organization: "Delta Group", description: "Water first." });
  expect(body("/workforce")).toEqual({ workforce_size: 450 });
});

it("leaves the workforce size alone when only the profile changes", async () => {
  const calls: { url: string; init?: RequestInit }[] = [];
  stub({}, calls);
  render(<Dashboard />);
  const organization = within(await screen.findByRole("region", { name: "Organization" }));
  fireEvent.click(organization.getByRole("button", { name: "Save profile" }));
  expect(await organization.findByText("Organization profile saved.")).toBeInTheDocument();
  expect(calls.some((c) => c.url.endsWith("/workforce"))).toBe(false);
});
