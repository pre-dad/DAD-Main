import AxeBuilder from "@axe-core/playwright";
import { expect, test } from "@playwright/test";

const dashboard = {
  mode: "test",
  state: "ready",
  computed_at: "2026-10-04T12:00:00Z",
  founding_donor: true,
  referrals: { link: 12, irl: 7, total_minor: 57000, link_minor: 36000, irl_minor: 21000, currency: "USD" },
  streak: { current: 3, at_risk: false, grace_ends_at: null },
  impact: { given_minor: 9000, currency: "USD", initiatives_funded: 4, events_volunteered: 1 },
};

test("dashboard loading preserves the account navigation position", async ({ page }) => {
  let release!: () => void;
  const ready = new Promise<void>((resolve) => {
    release = resolve;
  });
  await page.route("**/api/donor/me/dashboard", async (route) => {
    await ready;
    await route.fulfill({ json: dashboard });
  });
  await page.goto("/account/dashboard");
  await page.evaluate(() => document.fonts.ready);
  await expect(page.getByRole("main").getByRole("status")).toContainText("Loading your dashboard");
  const navigation = page.getByRole("navigation", { name: "Account", exact: true });
  const before = await navigation.boundingBox();
  const placeholders = await page.locator(".metric").evaluateAll((cards) =>
    cards.map((card) => ({
      title: card.querySelector("h2")?.textContent,
      height: card.getBoundingClientRect().height,
    })),
  );
  release();
  await expect(page.getByText("$570.00", { exact: true })).toBeVisible();
  const after = await navigation.boundingBox();
  expect(before).not.toBeNull();
  expect(after).not.toBeNull();
  const metrics = await page.locator(".metric").evaluateAll((cards) =>
    cards.map((card) => ({
      title: card.querySelector("h2")?.textContent,
      height: card.getBoundingClientRect().height,
    })),
  );
  expect(Math.abs((after?.y ?? 0) - (before?.y ?? 0)), JSON.stringify({ placeholders, metrics })).toBeLessThanOrEqual(
    10,
  );
});

const profiles = {
  "/account/dashboard": { endpoint: "dashboard", data: dashboard },
  "/account/profile": {
    endpoint: "profile",
    data: {
      email: "browser-test@example.org",
      public_name: "Alex",
      version: 1,
      referral_emails: true,
      marketing_emails: false,
      push_referral_converted: true,
      push_streak_at_risk: true,
      push_streak_milestone: false,
      push_streak_lapsed: true,
      push_badge_earned: true,
      push_gift_processed: true,
      push_event_nearby: true,
      push_event_reminder: true,
      push_event_promoted: true,
      rankings_opted_out: false,
      network_visible: true,
      volunteer: true,
      volunteer_interests: ["mentoring"],
      zip: "94103",
      city: "San Francisco",
      phone: null,
    },
  },
  "/account/share": {
    endpoint: "sharing",
    data: {
      public_name: "Alex",
      link: "https://dad.example/r/browser-test",
      irl_link: "https://dad.example/r/browser-test?via=irl",
    },
  },
};

const thisBrowser = {
  id: "0b8e7c52-3f0e-4c8a-9d51-2f1e6a7b8c9d",
  client: "BROWSER",
  platform: null,
  label: "Web browser",
  created_at: "2026-10-01T12:00:00Z",
  last_used_at: "2026-10-08T12:00:00Z",
  current: true,
};
const phone = {
  id: "5d2c1b0a-9e8f-4a7b-8c6d-5e4f3a2b1c0d",
  client: "APP",
  platform: "IOS",
  label: "Alex’s iPhone",
  created_at: "2026-09-20T12:00:00Z",
  last_used_at: "2026-10-07T09:30:00Z",
  current: false,
};
const passkeys = {
  passkeys: [
    {
      id: "9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d",
      name: "Work laptop",
      created_at: "2026-09-21T12:00:00Z",
      last_used_at: null,
    },
  ],
};

// These isolate authenticated rendering; Go journey tests exercise real session authorization.
for (const [path, fixture] of Object.entries(profiles)) {
  test(`authenticated accessible surface: ${path}`, async ({ page }) => {
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    await page.route(`**/api/donor/me/${fixture.endpoint}`, (route) => route.fulfill({ json: fixture.data }));
    await page.route("**/api/donor/me/sessions", (route) =>
      route.fulfill({ json: { sessions: [thisBrowser, phone] } }),
    );
    await page.route("**/api/donor/me/passkeys", (route) => route.fulfill({ json: passkeys }));
    await page.goto(path);
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
    await expect(page.getByText(/Loading (your dashboard|profile|your sharing details)/)).toHaveCount(0);
    if (path === "/account/dashboard" && (page.viewportSize()?.width ?? 0) > 600) {
      const link = await page
        .getByRole("article")
        .filter({ has: page.getByRole("heading", { name: "Link Referrals", exact: true }) })
        .boundingBox();
      const irl = await page
        .getByRole("article")
        .filter({ has: page.getByRole("heading", { name: "IRL Referrals", exact: true }) })
        .boundingBox();
      expect(link).not.toBeNull();
      expect(irl).not.toBeNull();
      expect(link?.y).toBe(irl?.y);
      expect(link?.width).toBe(irl?.width);
    }
    if (path === "/account/profile") {
      await page.getByText("Change account email", { exact: true }).click();
      await expect(page.getByLabel("New email address")).toBeVisible();
      // Axe measures targets at the current scroll; the sticky header must not hide the one scrolled under it.
      await page.evaluate(() => scrollTo(0, 0));
    }
    const results = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa", "wcag21aa", "wcag22aa"]).analyze();
    expect(results.violations).toEqual([]);
    expect(errors).toEqual([]);
    expect(await page.locator("body").evaluate((body) => body.scrollWidth <= innerWidth + 1)).toBe(true);
  });
}

test("the profile lists where the donor is signed in and signs out another device by keyboard", async ({ page }) => {
  let revoked = 0;
  await page.route("**/api/donor/me/profile", (route) => route.fulfill({ json: profiles["/account/profile"].data }));
  await page.route("**/api/donor/me/sessions", (route) => route.fulfill({ json: { sessions: [thisBrowser, phone] } }));
  await page.route("**/api/donor/me/passkeys", (route) => route.fulfill({ json: passkeys }));
  await page.route(`**/api/donor/me/sessions/${phone.id}/revoke`, async (route) => {
    expect(route.request().method()).toBe("POST");
    revoked += 1;
    await route.fulfill({ json: { sessions: [thisBrowser] } });
  });
  await page.goto("/account/profile");
  const section = page.getByRole("region", { name: "Where you’re signed in" });
  await expect(section.getByRole("listitem")).toHaveCount(2);
  await expect(section.getByRole("listitem").first()).toContainText("This device");
  await expect(section.getByRole("listitem").nth(1)).toContainText(
    "iOS app · Signed in Sep 20, 2026 · Last active Oct 7, 2026",
  );
  const accessibility = () =>
    new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa", "wcag21aa", "wcag22aa"]).analyze();
  expect((await accessibility()).violations).toEqual([]);
  const signOut = section.getByRole("button", { name: /^Sign out Alex’s iPhone, last active / });
  await signOut.focus();
  await page.keyboard.press("Enter");
  await expect(section.getByRole("status")).toHaveText("Signed out of Alex’s iPhone.");
  await expect(section.getByRole("heading", { name: "Where you’re signed in" })).toBeFocused();
  await expect(section.getByRole("listitem")).toHaveCount(1);
  await expect(section.getByText("You’re not signed in anywhere else.")).toBeVisible();
  expect(revoked).toBe(1);
  expect((await accessibility()).violations).toEqual([]);
  expect(await page.locator("body").evaluate((body) => body.scrollWidth <= innerWidth + 1)).toBe(true);
});

test("volunteer interests are added, removed, saved and reloaded by keyboard alone", async ({ page }) => {
  let saved = { ...profiles["/account/profile"].data };
  const posts: { version: number; volunteer_interests: string[] }[] = [];
  await page.route("**/api/donor/me/profile", (route) => route.fulfill({ json: saved }));
  await page.route("**/api/donor/me/sessions", (route) => route.fulfill({ json: { sessions: [thisBrowser, phone] } }));
  await page.route("**/api/donor/me/passkeys", (route) => route.fulfill({ json: passkeys }));
  await page.route("**/api/donor/me/preferences", async (route) => {
    const body = route.request().postDataJSON();
    posts.push(body);
    saved = { ...saved, ...body, version: saved.version + 1 };
    await route.fulfill({ json: saved });
  });
  await page.goto("/account/profile");
  const interests = page.getByRole("group", { name: "What interests you?" });
  await expect(interests.getByRole("listitem")).toHaveText(["mentoringRemove"]);
  const accessibility = () =>
    new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa", "wcag21aa", "wcag22aa"]).analyze();
  expect((await accessibility()).violations).toEqual([]);

  const box = interests.getByRole("textbox", { name: "Add an interest" });
  await box.focus();
  await page.keyboard.type("  Dog walking ");
  await page.keyboard.press("Enter");
  await expect(interests.getByRole("status")).toHaveText("Added Dog walking. 2 of 20 interests.");
  await expect(box).toBeFocused();
  await page.keyboard.type("MENTORING");
  await page.keyboard.press("Enter");
  await expect(interests.getByRole("alert")).toHaveText("“MENTORING” is already on your list.");
  await expect(interests.getByRole("listitem")).toHaveCount(2);
  expect((await accessibility()).violations).toEqual([]);

  await interests.getByRole("button", { name: "Remove mentoring" }).focus();
  await page.keyboard.press("Enter");
  await expect(interests.getByRole("status")).toHaveText("Removed mentoring. 1 of 20 interests.");
  await expect(interests.getByRole("listitem")).toHaveText(["Dog walkingRemove"]);
  expect(posts).toEqual([]);

  await page.getByRole("button", { name: "Save preferences" }).focus();
  await page.keyboard.press("Enter");
  await expect(page.getByText("Preferences saved.")).toBeVisible();
  expect(posts).toHaveLength(1);
  expect(posts[0]).toMatchObject({ version: 1, volunteer_interests: ["Dog walking"] });

  await page.reload();
  await expect(interests.getByRole("listitem")).toHaveText(["Dog walkingRemove"]);
  await interests.getByRole("button", { name: "Remove Dog walking" }).click();
  await expect(interests.getByText("No interests added yet.")).toBeVisible();
  expect((await accessibility()).violations).toEqual([]);
  expect(await page.locator("body").evaluate((body) => body.scrollWidth <= innerWidth + 1)).toBe(true);
});

const subscription = {
  id: "6f1c2b9e-0d7a-4e55-9b0c-1a2b3c4d5e6f",
  amount_minor: 3000,
  currency: "USD",
  status: "active",
  period_end: "2026-11-01T14:00:00Z",
  stops_at: null,
};
const recurring = {
  active: {
    subscription,
    text: /^Next renewal: Nov 1, 2026\.$/,
    links: ["Update payment method", "Change your daily gift"],
  },
  ending: {
    subscription: { ...subscription, stops_at: subscription.period_end },
    text: /^Renewals cancelled\. Your last monthly gift period ends Nov 1, 2026; no further gifts will be charged\.$/,
    links: ["Update payment method"],
  },
};

for (const [name, fixture] of Object.entries(recurring)) {
  test(`monthly giving describes a ${name} gift accessibly`, async ({ page }) => {
    await page.route("**/api/donor/me/recurring", (route) =>
      route.fulfill({
        json: {
          mode: "test",
          state: "ready",
          observed_at: "2026-10-07T13:42:00Z",
          subscription: fixture.subscription,
        },
      }),
    );
    await page.goto("/account/recurring");
    await expect(page.getByText(fixture.text)).toBeVisible();
    await expect(page.getByText(/^Billing details last updated from Stripe /)).toBeVisible();
    const nav = page.getByRole("navigation", { name: "Manage monthly giving" });
    await expect(nav.getByRole("link")).toHaveText(fixture.links);
    const cancel = page.getByRole("button", { name: "Cancel monthly giving" });
    if (name === "ending") await expect(cancel).toHaveCount(0);
    else {
      const linkBox = await nav.getByRole("link").first().boundingBox();
      const textBox = await cancel.evaluate((button) => {
        const range = document.createRange();
        range.selectNodeContents(button);
        return range.getBoundingClientRect().left;
      });
      expect(Math.abs((linkBox?.x ?? 0) - textBox)).toBeLessThan(1);
    }
    const results = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa", "wcag21aa", "wcag22aa"]).analyze();
    expect(results.violations).toEqual([]);
  });
}

test("a new monthly gift shows its confirmation first and the sandbox note last", async ({ page }) => {
  await page.route("**/api/donor/me/recurring", (route) =>
    route.fulfill({ json: { mode: "test", state: "preparing", subscription: null } }),
  );
  await page.goto("/account/recurring");
  const callout = page.locator(".callout").filter({ hasText: "We’re confirming your new monthly gift with Stripe." });
  await expect(callout).toBeVisible();
  await expect(callout.getByRole("button", { name: "Refresh billing details" })).toBeVisible();
  await expect(page.getByRole("main").getByRole("status")).toHaveText(
    "We’re confirming your new monthly gift with Stripe. This usually takes a few seconds.",
  );
  const order = await page
    .getByRole("main")
    .locator(".callout, .note")
    .evaluateAll((nodes) => nodes.map((node) => node.className));
  expect(order).toEqual(["callout", "note"]);
  const results = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa", "wcag21aa", "wcag22aa"]).analyze();
  expect(results.violations).toEqual([]);
});

test("dashboard explains partial data and recovers from a connection failure", async ({ page }) => {
  let available = false;
  await page.route("**/api/donor/me/dashboard", (route) =>
    available
      ? route.fulfill({ json: { ...dashboard, referrals: null, streak: null } })
      : route.abort("internetdisconnected"),
  );
  await page.goto("/account/dashboard");
  await expect(page.getByRole("main").getByRole("alert")).toContainText("Check your connection");
  available = true;
  await page.getByRole("button", { name: "Refresh dashboard" }).click();
  await expect(page.getByRole("main").getByRole("alert")).toBeEmpty();
  await expect(page.getByText("Referral totals are updating. Please refresh shortly.")).toBeVisible();
  await expect(page.getByText("Your streak is temporarily unavailable.", { exact: false })).toBeVisible();
  await page.getByRole("link", { name: "Share and invite", exact: true }).focus();
  await expect(page.getByRole("link", { name: "Share and invite", exact: true })).toBeFocused();
});

test("email proof page requires an explicit keyboard confirmation", async ({ page }) => {
  const proof = "a".repeat(43);
  let confirmed = 0;
  await page.route("**/api/donor/email-change/confirm", async (route) => {
    expect(route.request().postDataJSON()).toEqual({ token: proof });
    confirmed += 1;
    await route.fulfill({ json: { stage: "COMPLETED" } });
  });
  await page.goto(`/account/email-change#token=${proof}`);
  await expect(page).toHaveURL(/\/account\/email-change$/);
  expect(confirmed).toBe(0);
  const confirm = page.getByRole("button", { name: /confirm/i });
  await confirm.focus();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("link", { name: /sign in/i })).toBeVisible();
  expect(confirmed).toBe(1);
  expect(
    (await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa", "wcag21aa", "wcag22aa"]).analyze()).violations,
  ).toEqual([]);
});

// Employee giving: the donor links an employer by company code and
// joins a team. An already-linked donor sees their org and team as text, not a
// colour-only state.
test("authenticated accessible surface: /account/employer", async ({ page }) => {
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.route("**/api/donor/me/team", (route) => {
    if (route.request().method() !== "GET") return route.fulfill({ json: {} });
    return route.fulfill({
      json: {
        partner_id: "9c7e1f20-0a1b-4c3d-8e5f-6a7b8c9d0e1f",
        organization: "Delta Inc",
        has_logo: false,
        committed_verified: true,
        deployed_verified: true,
        people_reached_verified: true,
        team_name: "Team A",
        leaderboard: [],
      },
    });
  });
  await page.goto("/account/employer");
  await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
  // The enrolled state is conveyed in words, not by colour alone.
  await expect(page.getByText("Delta Inc", { exact: true })).toBeVisible();
  await expect(page.getByText("Team A")).toBeVisible();
  const results = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa", "wcag21aa", "wcag22aa"]).analyze();
  expect(results.violations).toEqual([]);
  expect(errors).toEqual([]);
  expect(await page.locator("body").evaluate((body) => body.scrollWidth <= innerWidth + 1)).toBe(true);
});

// The partner portal dashboard: every section, for an administrator, with
// an initiative, a team leaderboard, and unsuppressed employee figures.
const partnerPortal: Record<string, unknown> = {
  "/donor/partner/overview": {
    organization: "Delta Inc",
    has_logo: false,
    committed_verified: true,
    deployed_verified: true,
    people_reached_verified: true,
    description: "Meals and water across northern Nevada.",
    role: "ADMIN",
    company_code: "DELTA-7KQ4MX",
    committed_minor: 150000,
    deployed_minor: 50000,
    deployed_percent: 33,
    people_reached: 200,
    employees_enrolled: 8,
    workforce_size: 40,
    enrolled_percent: 20,
    updated_at: "2026-10-20T15:00:00Z",
  },
  "/donor/partner/initiatives": {
    initiatives: [
      {
        id: "i1",
        title: "Reno Meals",
        summary: "Hot meals delivered daily.",
        status: "ACTIVE",
        region: "Northern Nevada",
        county_count: 3,
        committed_minor: 100000,
        deployed_minor: 40000,
        metrics: [{ label: "Households served", value: 150, unit: "PEOPLE", verified_on: "2026-10-18" }],
        spend: [{ category: "Meals", amount_minor: 40000 }],
        updated_at: "2026-10-20T15:00:00Z",
      },
    ],
  },
  "/donor/partner/spend": { categories: [{ category: "Meals", amount_minor: 50000 }] },
  "/donor/partner/users": { users: [{ id: "u1", email: "a@delta.test", name: "Ana", role: "ADMIN" }] },
  "/donor/partner/exports": { exports: [] },
  "/donor/partner/our-people": {
    enrolled: 8,
    workforce_size: 40,
    given_minor: 80000,
    matched_minor: 8000,
    avg_streak_days: 40,
    volunteer_minutes: 300,
    teams: 1,
    suppressed: false,
  },
  "/donor/partner/teams": {
    teams: [
      {
        team_id: "t1",
        name: "Team A",
        active_givers: 6,
        annualized_minor: 216000,
        avg_streak_days: 40,
        volunteer_minutes: 300,
      },
    ],
  },
};

test("authenticated accessible surface: /partner", async ({ page }) => {
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  for (const [path, json] of Object.entries(partnerPortal)) {
    await page.route(`**/api${path}`, (route) => route.fulfill({ json: json as object }));
  }
  await page.goto("/partner");
  await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
  await expect(page.getByText("$1,500.00")).toBeVisible();
  // Every portal panel is a laboured region a screen reader can navigate to.
  for (const name of ["Overview", "Initiatives", "Spend", "Our People", "Reports", "Users"]) {
    await expect(page.getByRole("region", { name })).toBeVisible();
  }
  const results = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa", "wcag21aa", "wcag22aa"]).analyze();
  expect(results.violations).toEqual([]);
  expect(errors).toEqual([]);
  expect(await page.locator("body").evaluate((body) => body.scrollWidth <= innerWidth + 1)).toBe(true);
});

test("partner sign-out stays on the dashboard after failure and navigates only after a successful retry", async ({
  page,
}) => {
  for (const [path, json] of Object.entries(partnerPortal)) {
    await page.route(`**/api${path}`, (route) => route.fulfill({ json: json as object }));
  }
  let attempts = 0;
  await page.route("**/api/donor/partner/sign-out", (route) => {
    attempts++;
    return route.fulfill(
      attempts === 1 ? { status: 503, json: { detail: "Unavailable" } } : { json: { signed_out: true } },
    );
  });
  await page.goto("/partner");
  await page.getByRole("button", { name: "Sign out", exact: true }).click();
  await expect(page.getByRole("alert").filter({ hasText: "Sign-out wasn’t confirmed." })).toBeVisible();
  await expect(page).toHaveURL(/\/partner$/);
  await page.getByRole("button", { name: "Sign out", exact: true }).click();
  await expect(page).toHaveURL(/\/partner\/sign-in$/);
  await expect(page.getByRole("heading", { name: "Sign in to your partner portal" })).toBeVisible();
  expect(attempts).toBe(2);
});
