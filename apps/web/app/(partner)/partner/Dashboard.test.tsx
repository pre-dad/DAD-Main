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

it("lets a partner admin remove a colleague but not themselves", async () => {
  stub({ user_id: "u1" });
  const base = vi.mocked(fetch).getMockImplementation();
  vi.mocked(fetch).mockImplementation((input, init) => {
    if (String(input).endsWith("/users") && init?.method !== "POST")
      return Promise.resolve(
        Response.json({
          users: [
            { id: "u1", email: "a@delta.test", name: "Ana", role: "ADMIN" },
            { id: "u2", email: "v@delta.test", name: "Val", role: "VIEWER" },
          ],
        }),
      );
    if (String(input).endsWith("/remove")) return Promise.resolve(Response.json({ removed: true }));
    if (!base) throw new Error("Missing fixture fetch");
    return base(input, init);
  });
  render(<Dashboard />);
  expect(await screen.findByRole("button", { name: "Remove v@delta.test" })).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Remove a@delta.test" })).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Remove v@delta.test" }));
  expect(vi.mocked(fetch).mock.calls.some(([input]) => String(input).endsWith("/remove"))).toBe(false);
  fireEvent.click(screen.getByRole("button", { name: "Keep v@delta.test" }));
  expect(screen.getByRole("button", { name: "Remove v@delta.test" })).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Remove v@delta.test" }));
  fireEvent.click(screen.getByRole("button", { name: "Confirm removal of v@delta.test" }));
  expect(screen.getByRole("button", { name: "Confirm removal of v@delta.test" })).toBeDisabled();
  expect(await screen.findByText("Removed v@delta.test.")).toBeInTheDocument();
  expect(vi.mocked(fetch)).toHaveBeenCalledWith(
    "/api/donor/partner/users/u2/remove",
    expect.objectContaining({ method: "POST" }),
  );
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

it.each([
  ["exports", "report history", "Reports"],
  ["users", "users", "Users"],
  ["teams", "teams", "Our People"],
])("shows and retries a failed %s read independently", async (endpoint, label, section) => {
  stub();
  const base = vi.mocked(fetch).getMockImplementation();
  let failed = false;
  vi.mocked(fetch).mockImplementation((input, init) => {
    if (String(input).endsWith(`/${endpoint}`) && !failed) {
      failed = true;
      return Promise.resolve(Response.json({ detail: "Unavailable" }, { status: 503 }));
    }
    if (!base) throw new Error("Missing fixture fetch");
    return base(input, init);
  });
  render(<Dashboard />);
  const region = within(await screen.findByRole("region", { name: section }));
  fireEvent.click(await region.findByRole("button", { name: `Retry ${label}` }));
  await waitFor(() => expect(region.queryByRole("button", { name: `Retry ${label}` })).not.toBeInTheDocument());
  expect(vi.mocked(fetch).mock.calls.filter(([input]) => String(input).endsWith(`/${endpoint}`))).toHaveLength(2);
  expect(vi.mocked(fetch).mock.calls.every(([, init]) => !init?.method)).toBe(true);
});

it("downloads the verified report through a temporary object URL and refreshes the history", async () => {
  stub();
  const createObjectURL = vi.fn(() => "blob:report");
  const revokeObjectURL = vi.fn();
  const original = { create: URL.createObjectURL, revoke: URL.revokeObjectURL };
  Object.assign(URL, { createObjectURL, revokeObjectURL });
  const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
  const base = vi.mocked(fetch).getMockImplementation();
  vi.mocked(fetch).mockImplementation((input, init) => {
    if (String(input).endsWith("/report.pdf")) return Promise.resolve(new Response(new Blob(["%PDF"])));
    if (!base) throw new Error("Missing fixture fetch");
    return base(input, init);
  });
  render(<Dashboard />);
  fireEvent.click(await screen.findByRole("button", { name: "Export verified report (PDF)" }));
  expect(await screen.findByText("Your verified impact report was downloaded.")).toBeVisible();
  expect(createObjectURL).toHaveBeenCalledTimes(1);
  expect(click).toHaveBeenCalledTimes(1);
  expect(revokeObjectURL).not.toHaveBeenCalled();
  await waitFor(() => expect(revokeObjectURL).toHaveBeenCalledWith("blob:report"), { timeout: 2500 });
  await waitFor(() =>
    expect(vi.mocked(fetch).mock.calls.filter(([input]) => String(input).endsWith("/exports"))).toHaveLength(2),
  );
  click.mockRestore();
  Object.assign(URL, { createObjectURL: original.create, revokeObjectURL: original.revoke });
});

it("sends the partner to sign-in once sign-out is confirmed", async () => {
  stub();
  const assign = vi.fn();
  vi.stubGlobal("location", { ...window.location, assign });
  const base = vi.mocked(fetch).getMockImplementation();
  vi.mocked(fetch).mockImplementation((input, init) => {
    if (String(input).endsWith("/sign-out")) return Promise.resolve(Response.json({}));
    if (!base) throw new Error("Missing fixture fetch");
    return base(input, init);
  });
  render(<Dashboard />);
  fireEvent.click(await screen.findByRole("button", { name: "Sign out" }));
  await waitFor(() => expect(assign).toHaveBeenCalledWith("/partner/sign-in"));
});

it("shows employee figures once enough employees have linked their giving", async () => {
  stub();
  const base = vi.mocked(fetch).getMockImplementation();
  vi.mocked(fetch).mockImplementation((input, init) => {
    if (String(input).endsWith("/our-people"))
      return Promise.resolve(
        Response.json({
          enrolled: 12,
          workforce_size: 400,
          given_minor: 120000,
          matched_minor: 60000,
          avg_streak_days: 9,
          volunteer_minutes: 180,
          teams: 2,
          suppressed: false,
          streak_verified: true,
        }),
      );
    if (!base) throw new Error("Missing fixture fetch");
    return base(input, init);
  });
  render(<Dashboard />);
  const people = within(await screen.findByRole("region", { name: "Our People" }));
  expect(await people.findByText("of 400 employees")).toBeVisible();
  expect(people.getByText("$1,200.00")).toBeVisible();
  expect(people.getByText("$600.00")).toBeVisible();
  expect(people.getByText("Average streak 9 days")).toBeVisible();
  expect(people.queryByText(/appear once at least 5 employees/)).not.toBeInTheDocument();
  expect(people.queryByRole("button", { name: "Refresh employee figures" })).not.toBeInTheDocument();
});

it("keeps the saved profile and says so when only the workforce size fails to save", async () => {
  const calls: { url: string; init?: RequestInit }[] = [];
  stub({}, calls);
  const base = vi.mocked(fetch).getMockImplementation();
  vi.mocked(fetch).mockImplementation((input, init) => {
    if (String(input).endsWith("/workforce"))
      return Promise.resolve(Response.json({ detail: "Down" }, { status: 503 }));
    if (!base) throw new Error("Missing fixture fetch");
    return base(input, init);
  });
  render(<Dashboard />);
  const organization = within(await screen.findByRole("region", { name: "Organization" }));
  fireEvent.change(organization.getByLabelText("Organization name"), { target: { value: "Delta Group" } });
  fireEvent.change(organization.getByLabelText("Workforce size"), { target: { value: "450" } });
  fireEvent.click(organization.getByRole("button", { name: "Save profile" }));
  expect(await organization.findByText(/Profile saved; workforce size was not/)).toBeInTheDocument();
  expect(organization.queryByText(/could not be saved right now/)).not.toBeInTheDocument();
  expect(calls.filter((c) => c.url.endsWith("/overview")).length).toBeGreaterThanOrEqual(2);
});

it("does not report a profile save when the profile request itself fails", async () => {
  stub();
  const base = vi.mocked(fetch).getMockImplementation();
  vi.mocked(fetch).mockImplementation((input, init) => {
    if (String(input).endsWith("/profile")) return Promise.resolve(Response.json({ detail: "Down" }, { status: 503 }));
    if (!base) throw new Error("Missing fixture fetch");
    return base(input, init);
  });
  render(<Dashboard />);
  const organization = within(await screen.findByRole("region", { name: "Organization" }));
  fireEvent.click(organization.getByRole("button", { name: "Save profile" }));
  expect(await organization.findByText(/The profile could not be saved right now/)).toBeInTheDocument();
  expect(organization.queryByText("Organization profile saved.")).not.toBeInTheDocument();
  expect(vi.mocked(fetch).mock.calls.some(([input]) => String(input).endsWith("/workforce"))).toBe(false);
});

it("lets keyboard users scroll the spend table", async () => {
  stub();
  render(<Dashboard />);
  const table = await screen.findByRole("region", { name: "Spend by category across all initiatives" });
  expect(table).toHaveAttribute("tabindex", "0");
});
