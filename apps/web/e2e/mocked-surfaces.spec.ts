import AxeBuilder from "@axe-core/playwright";
import { expect, type Page, test } from "@playwright/test";
import { staffURL } from "./staff";

// These isolate rendering of states that need a session or a live referral code; Go
// journey tests exercise the real authorization and attribution behind them.
async function expectAccessible(page: Page) {
  const results = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa", "wcag21aa", "wcag22aa"]).analyze();
  expect(results.violations).toEqual([]);
  expect(await page.locator("body").evaluate((body) => body.scrollWidth <= innerWidth + 1)).toBe(true);
}

function collectErrors(page: Page) {
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  return errors;
}

// Development StrictMode replays effects, so a dev server may send the capture twice;
// every request must still carry the same kind.
const kinds = (captures: unknown[]) => [...new Set(captures.map((capture) => JSON.stringify(capture)))];

const code = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><rect width="10" height="10" fill="#000"/></svg>`;

test("referral landing is accessible and captures an IRL visit from the browser", async ({ page }) => {
  const errors = collectErrors(page);
  const captures: unknown[] = [];
  await page.route("**/api/donor/referrals/*", async (route) => {
    expect(route.request().method()).toBe("POST");
    captures.push(route.request().postDataJSON());
    await route.fulfill({ json: { captured: true, public_name: null } });
  });
  await page.goto("/r/e2e-browser-code?via=irl");
  await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
  await expect.poll(() => captures.length).toBeGreaterThan(0);
  expect(kinds(captures)).toEqual([JSON.stringify({ kind: "IRL" })]);
  await expectAccessible(page);
  expect(errors).toEqual([]);
});

test("a shared link without the marker is captured as a Link Referral", async ({ page }) => {
  const captures: unknown[] = [];
  await page.route("**/api/donor/referrals/*", async (route) => {
    captures.push(route.request().postDataJSON());
    await route.fulfill({ json: { captured: true, public_name: null } });
  });
  await page.goto("/r/e2e-browser-code");
  await expect.poll(() => captures.length).toBeGreaterThan(0);
  expect(kinds(captures)).toEqual([JSON.stringify({ kind: "LINK" })]);
});

// The real API does not know this code, so the page leads with "no longer available".
test("an unavailable referral link offers giving without a referral and no capture error", async ({ page }) => {
  let attempts = 0;
  await page.route("**/api/donor/referrals/*", (route) => {
    attempts += 1;
    return route.fulfill({ status: 429, json: { detail: "Too many requests" } });
  });
  await page.goto("/r/e2e-browser-code");
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("This referral link is no longer available");
  await expect.poll(() => attempts).toBeGreaterThan(0);
  await expect(page.getByRole("link", { name: "Continue without a referral" })).toHaveAttribute("href", "/donate");
  await expect(page.getByRole("button", { name: "Retry referral" })).toHaveCount(0);
  await expect(page.getByText(/couldn’t confirm your referral/)).toHaveCount(0);
  await expectAccessible(page);
});

test("the referral code page is accessible and shows the code", async ({ page }) => {
  const errors = collectErrors(page);
  await page.route("**/api/donor/me/sharing", (route) =>
    route.fulfill({
      json: { public_name: "Alex", link: "https://dad.example/r/x", irl_link: "https://dad.example/r/x?via=irl" },
    }),
  );
  await page.route("**/api/donor/me/referral-code.svg*", (route) =>
    route.fulfill({ contentType: "image/svg+xml", body: code }),
  );
  await page.route("**/api/donor/me/wallet", (route) => route.fulfill({ json: { apple: false, google: false } }));
  await page.goto("/account/code");
  await expect(page.getByRole("img", { name: "Your scannable in-person DAD referral code" })).toBeVisible();
  await expect(page.getByText(/Increase your screen brightness/)).toBeVisible();
  await expectAccessible(page);
  expect(errors).toEqual([]);
});

test("a staff page past sign-in is accessible", async ({ page }) => {
  const errors = collectErrors(page);
  await page.route("**/api/donor/staff/me", (route) =>
    route.fulfill({
      json: {
        id: "staff-1",
        email: "admin@example.org",
        name: "Ada Admin",
        role: "SUPER_ADMIN",
        active: true,
        disabled: false,
        permissions: ["data.export", "content.publish", "audit.read", "gift.refund", "gift.cancel", "staff.manage"],
      },
    }),
  );
  await page.goto(staffURL("/staff"));
  await expect(page.getByText("Signed in as Ada Admin · SUPER_ADMIN")).toBeVisible();
  await expect(page.getByRole("button", { name: "Search donors" })).toBeVisible();
  await expectAccessible(page);
  expect(errors).toEqual([]);
});

const staffDetail = {
  id: "donor-1",
  email: "pat.donor@example.org",
  public_name: "Pat",
  streak: { current: 4, best: 6, at_risk: false, computed_at: "2027-01-01T00:00:00Z" },
  referrer: { id: "donor-0", email: "friend@example.org", type: "IRL" },
  claimed: true,
  founding_donor: true,
  subscription: {
    amount_minor: 3000,
    next_renewal: "2027-02-01T00:00:00Z",
    observed_at: "2027-01-01T00:00:00Z",
    status: "active",
    stops_at: null,
  },
  gifts: [
    {
      id: "4f1c2b9e-0d7a-4e55-9b0c-1a2b3c4d5e6f",
      amount_minor: 3000,
      currency: "USD",
      frequency: "MONTHLY",
      paid_at: "2027-01-01T00:00:00Z",
      refunded_minor: 0,
      net_minor: 3000,
    },
  ],
  referred: [{ id: "donor-2", email: "invitee@example.org", type: "LINK", net_minor: 3000 }],
  next_gifts_offset: null,
  next_referrals_offset: null,
};

async function mockStaffDonor(page: Page) {
  await page.route("**/api/donor/staff/**", (route) => {
    const url = new URL(route.request().url());
    const path = url.pathname.replace("/api", "");
    if (path === "/donor/staff/me")
      return route.fulfill({
        json: {
          id: "staff-1",
          email: "finance@example.org",
          name: "Fin Ance",
          role: "SUPER_ADMIN",
          active: true,
          disabled: false,
          permissions: ["gift.refund", "gift.cancel"],
        },
      });
    if (path === "/donor/staff/donors")
      return route.fulfill({
        json: {
          donors: [
            { id: "donor-1", email: "pat.donor@example.org", public_name: null, claimed: true, disabled: false },
          ],
          next_cursor: null,
        },
      });
    if (path === "/donor/staff/donors/donor-1") return route.fulfill({ json: staffDetail });
    if (path === "/donor/staff/donors/donor-1/actions")
      return route.fulfill({
        json:
          url.searchParams.get("cursor") === null
            ? {
                actions: [
                  {
                    id: "action-1",
                    actor_id: "staff-1",
                    kind: "REFUND",
                    target_id: staffDetail.gifts[0].id,
                    amount_minor: 1000,
                    period_end: null,
                    reason: "Donor asked for a partial refund",
                    status: "SUCCEEDED",
                    completed_at: "2027-01-02T00:00:00Z",
                  },
                ],
                next_cursor: "cursor-2",
              }
            : { actions: [], next_cursor: null },
      });
    return route.fulfill({ status: 404, json: { detail: "Not found" } });
  });
}

test("a donor record has its own accessible page and Back returns to the search", async ({ page }) => {
  const errors = collectErrors(page);
  await mockStaffDonor(page);
  await page.goto(staffURL("/staff"));
  await page.getByRole("searchbox", { name: "Find donor by email" }).fill("pat");
  await page.getByRole("button", { name: "Search donors" }).click();
  await expect(page).toHaveURL(staffURL("/staff?q=pat"));
  await page.getByRole("link", { name: "pat.donor@example.org" }).click();
  await expect(page).toHaveURL(staffURL("/staff/donors/donor-1"));
  await expect(page).toHaveTitle(/^Donor record · /);
  await expect(page.locator('meta[name="robots"]')).toHaveAttribute("content", /noindex/);
  await expect(page.getByRole("heading", { level: 1, name: "pat.donor@example.org" })).toBeVisible();
  await expect(page.getByText("Donor asked for a partial refund", { exact: false })).toBeVisible();
  await expect(page.getByRole("link", { name: "Refund gift" })).toBeVisible();
  await expect(
    page.getByText(/^Monthly gift: \$30\.00\/month · Active · Next renewal [A-Z][a-z]{2} \d{1,2}, \d{4} ·/),
  ).toBeVisible();
  await expect(page.locator("p.note").filter({ hasText: /^Last updated from Stripe / })).toBeVisible();
  await expectAccessible(page);
  await page.getByRole("button", { name: "Older actions" }).click();
  await expect(page.getByText("No refunds or cancellations recorded.")).toBeVisible();
  await page.goBack();
  await expect(page).toHaveURL(staffURL("/staff?q=pat"));
  await expect(page.getByRole("searchbox", { name: "Find donor by email" })).toHaveValue("pat");
  await expect(page.getByRole("link", { name: "pat.donor@example.org" })).toBeVisible();
  expect(errors).toEqual([]);
});

test("a donor record link that matches no donor says so accessibly", async ({ page }) => {
  await mockStaffDonor(page);
  await page.goto(staffURL("/staff/donors/missing"));
  await expect(page.getByRole("main").getByRole("alert")).toContainText("No donor record matches this link");
  await expectAccessible(page);
});

test("staff resume an uncertain refund with the saved key and amount", async ({ page }) => {
  const errors = collectErrors(page);
  const gift = staffDetail.gifts[0].id;
  const requests: string[] = [];
  let resumed = false;
  await page.route("**/api/donor/staff/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    requests.push(url.pathname);
    if (url.pathname.endsWith("/actions/pending")) {
      expect(url.searchParams.get("target_id")).toBe(gift);
      expect(url.searchParams.get("kind")).toBe("REFUND");
      return route.fulfill({
        json: {
          action: resumed
            ? null
            : {
                id: "saved-refund",
                kind: "REFUND",
                target_id: gift,
                amount_minor: 1000,
                period_end: null,
                reason: "Donor requested a partial refund",
                status: "AUTHORIZED",
              },
        },
      });
    }
    if (url.pathname.endsWith("/refund")) {
      if (request.method() === "POST") {
        expect(request.headers()["idempotency-key"]).toBe("saved-refund");
        expect(request.postDataJSON()).toEqual({ amount_minor: 1000, reason: "Donor requested a partial refund" });
        resumed = true;
        return route.fulfill({ json: { status: "SUCCEEDED" } });
      }
      return route.fulfill({
        json: { gift_id: gift, donor_id: "donor-1", amount_minor: 3000, available_minor: resumed ? 2000 : 3000 },
      });
    }
    return route.fulfill({ status: 404, json: { detail: "Unexpected staff lookup" } });
  });
  await page.goto(staffURL(`/staff/finance?donor=donor-1&gift=${gift}`));
  await expect(page.getByText("Refund $10.00 USD to the original payment method.")).toBeVisible();
  await expectAccessible(page);
  await page.getByRole("button", { name: "Retry original financial action" }).click();
  await expect(page.getByRole("status")).toHaveText(
    "Stripe refund status: SUCCEEDED. Only successful refunds reduce collected totals.",
  );
  await expect(page.getByText(/Available to refund: \$20.00/)).toBeVisible();
  expect(resumed).toBe(true);
  expect(requests.every((path) => path.endsWith("/actions/pending") || path.endsWith("/refund"))).toBe(true);
  expect(errors).toEqual([]);
});

test("staff pages separate a missing session from a missing permission", async ({ page }) => {
  await page.route("**/api/donor/staff/audit*", (route) =>
    route.fulfill({ status: 403, json: { detail: "Forbidden" } }),
  );
  await page.goto(staffURL("/staff/audit"));
  await expect(page.getByRole("main").getByRole("alert")).toContainText("requires administrator access");
  await expect(page.getByRole("link", { name: "Staff sign-in" })).toHaveCount(0);
  await page.unroute("**/api/donor/staff/audit*");
  await page.route("**/api/donor/staff/audit*", (route) => route.fulfill({ status: 401, json: { detail: "Sign in" } }));
  await page.reload();
  await expect(page.getByRole("link", { name: "Staff sign-in" })).toBeVisible();
});

test("public pages carry their own titles and private pages are not indexed", async ({ page }) => {
  await page.goto("/spend");
  await expect(page).toHaveTitle("Spend by category · Dynamic Aid Delivery");
  await page.goto("/account/sign-in");
  await expect(page).toHaveTitle(/^Claim your account or sign in · /);
  await expect(page.locator('meta[name="robots"]')).toHaveAttribute("content", /noindex/);
  await page.goto(staffURL("/staff/sign-in"));
  await expect(page.locator('meta[name="robots"]')).toHaveAttribute("content", /noindex/);
});

const failedPayment = (id: string, overrides: Record<string, unknown> = {}) => ({
  amount_minor: 3000,
  disabled: false,
  donor_id: `donor-${id}`,
  email: `${id}@example.org`,
  failed_at: "2026-10-20T12:00:00Z",
  grace_ends_at: "2026-10-27T12:00:00Z",
  period_id: `period-${id}`,
  public_name: null,
  state: "in_grace",
  ...overrides,
});

test("staff see failed payments, filter and page them, and reach the donor record", async ({ page }) => {
  const errors = collectErrors(page);
  const requests: string[] = [];
  await page.route("**/api/donor/staff/failed-payments*", (route) => {
    const url = new URL(route.request().url());
    requests.push(url.search);
    if (url.searchParams.get("state") === "lapsed")
      return route.fulfill({
        json: { payments: [failedPayment("gone", { state: "lapsed", amount_minor: null })], next_cursor: null },
      });
    return route.fulfill({
      json: url.searchParams.get("cursor")
        ? { payments: [failedPayment("older")], next_cursor: null }
        : { payments: [failedPayment("newer")], next_cursor: "page-2" },
    });
  });
  await page.route("**/api/donor/staff/donors/donor-older", (route) => route.fulfill({ status: 404, json: {} }));
  await page.goto(staffURL("/staff/failed-payments"));
  await expect(page.getByRole("heading", { level: 1, name: "Failed payments" })).toBeVisible();
  await expect(page.getByRole("link", { name: "newer@example.org" })).toBeVisible();
  await expectAccessible(page);
  await page.getByRole("button", { name: "Older failures" }).click();
  await expect(page.getByRole("link", { name: "older@example.org" })).toBeVisible();
  await expect(page.getByRole("link", { name: "older@example.org" })).toHaveAttribute(
    "href",
    "/staff/donors/donor-older",
  );
  await page.getByRole("button", { name: "Newer failures" }).click();
  await expect(page.getByRole("link", { name: "newer@example.org" })).toBeVisible();
  await page.getByLabel("Show").selectOption("lapsed");
  await expect(page.getByRole("link", { name: "gone@example.org" })).toBeVisible();
  await expect(page.getByText("Not recorded")).toBeVisible();
  // Development StrictMode replays the first load, so collapse back-to-back repeats.
  expect(requests.filter((search, index) => search !== requests[index - 1])).toEqual([
    "?state=all",
    "?state=all&cursor=page-2",
    "?state=all",
    "?state=lapsed",
  ]);
  await expectAccessible(page);
  expect(errors).toEqual([]);
});

test("staff see the account-deletion record, empty and with a missing permission", async ({ page }) => {
  const errors = collectErrors(page);
  await page.route("**/api/donor/staff/deletions*", (route) =>
    route.fulfill({
      json: {
        deletions: [{ deleted_at: "2026-10-20T12:00:00Z", donor_id: "donor-gone", retained_gifts: 4 }],
        next_cursor: null,
      },
    }),
  );
  await page.goto(staffURL("/staff/deletions"));
  await expect(page.getByRole("link", { name: "donor-gone" })).toHaveAttribute("href", "/staff/donors/donor-gone");
  await expect(page.getByRole("cell", { name: "4" })).toBeVisible();
  await expectAccessible(page);
  await page.unroute("**/api/donor/staff/deletions*");
  await page.route("**/api/donor/staff/deletions*", (route) =>
    route.fulfill({ json: { deletions: [], next_cursor: null } }),
  );
  await page.reload();
  await expect(page.getByText("No accounts have been deleted.")).toBeVisible();
  await expectAccessible(page);
  await page.unroute("**/api/donor/staff/deletions*");
  await page.route("**/api/donor/staff/deletions*", (route) =>
    route.fulfill({ status: 403, json: { detail: "Forbidden" } }),
  );
  await page.reload();
  await expect(page.getByRole("main").getByRole("alert")).toContainText("require donors and money access");
  await expect(page.getByRole("link", { name: "Staff sign-in" })).toHaveCount(0);
  expect(errors).toEqual([]);
});

const staffActor = {
  id: "staff-1",
  email: "admin@example.org",
  name: "Ada Admin",
  role: "SUPER_ADMIN",
  active: true,
  disabled: false,
  permissions: ["donor.read", "audit.read", "partner.manage"],
};

test("staff page the audit history by keyboard with a cursor and stay accessible", async ({ page }) => {
  const errors = collectErrors(page);
  const requests: string[] = [];
  const event = (id: string) => ({
    id,
    actor_id: "staff-1",
    action: `audit.${id}`,
    target: "donors",
    at: "2027-01-02T00:00:00Z",
    details: {},
  });
  await page.route("**/api/donor/staff/me", (route) => route.fulfill({ json: staffActor }));
  await page.route("**/api/donor/staff/audit*", (route) => {
    const cursor = new URL(route.request().url()).searchParams.get("cursor");
    requests.push(cursor ?? "");
    return route.fulfill({
      json: cursor
        ? { events: [event("older")], next_cursor: null }
        : { events: [event("newer")], next_cursor: "page-2" },
    });
  });
  await page.goto(staffURL("/staff/audit"));
  await expect(page.getByText("audit.newer")).toBeVisible();
  await expect(page.getByRole("button", { name: "Newer events" })).toHaveCount(0);
  await expectAccessible(page);
  await page.getByRole("button", { name: "Older events" }).focus();
  await page.keyboard.press("Enter");
  await expect(page.getByText("audit.older")).toBeVisible();
  await expect(page.getByRole("button", { name: "Older events" })).toHaveCount(0);
  await expectAccessible(page);
  await page.getByRole("button", { name: "Newer events" }).click();
  await expect(page.getByText("audit.newer")).toBeVisible();
  expect(requests.filter((cursor, index) => cursor !== requests[index - 1])).toEqual(["", "page-2", ""]);
  expect(errors).toEqual([]);
});

test("staff page donor search results and the pending partner applications by cursor", async ({ page }) => {
  const errors = collectErrors(page);
  const application = (id: string, organization: string) => ({
    id,
    organization,
    website: "https://example.org",
    type: "Corporate",
    contact_name: "Dana",
    title: "Head of Giving",
    work_email: `${id}@example.org`,
    phone: "+14155550123",
    workforce_size: 500,
    status: "PENDING",
    decision_reason: "",
    approved_partner_id: null,
  });
  await page.route("**/api/donor/staff/me", (route) => route.fulfill({ json: staffActor }));
  await page.route("**/api/donor/staff/donors?*", (route) => {
    const url = new URL(route.request().url());
    const donor = (email: string) => ({ id: email, email, public_name: null, claimed: true, disabled: false });
    return route.fulfill({
      json: url.searchParams.get("cursor")
        ? { donors: [donor("zed@example.org")], next_cursor: null }
        : { donors: [donor("amy@example.org")], next_cursor: "after-amy" },
    });
  });
  await page.route("**/api/donor/staff/partners/pending*", (route) =>
    route.fulfill({
      json: new URL(route.request().url()).searchParams.get("cursor")
        ? { applications: [application("late", "Late Co")], next_cursor: null }
        : { applications: [application("early", "Early Co")], next_cursor: "after-early" },
    }),
  );
  await page.goto(staffURL("/staff?q=e"));
  await expect(page.getByRole("link", { name: "amy@example.org" })).toBeVisible();
  await page.getByRole("button", { name: "Next donor page" }).click();
  await expect(page.getByRole("link", { name: "zed@example.org" })).toBeVisible();
  await page.getByRole("button", { name: "Previous donor page" }).click();
  await expect(page.getByRole("link", { name: "amy@example.org" })).toBeVisible();
  await expectAccessible(page);

  await page.goto(staffURL("/staff/partners/pending"));
  await expect(page.getByRole("heading", { name: "Early Co" })).toBeVisible();
  await page.getByRole("button", { name: "Later applications" }).click();
  await expect(page.getByRole("heading", { name: "Late Co" })).toBeVisible();
  await expectAccessible(page);
  await page.getByRole("button", { name: "Earlier applications" }).click();
  await expect(page.getByRole("heading", { name: "Early Co" })).toBeVisible();
  expect(errors).toEqual([]);
});
