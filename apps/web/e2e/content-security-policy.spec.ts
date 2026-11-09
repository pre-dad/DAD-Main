import { expect, type Page, test } from "@playwright/test";
import { staffOrigin, staffURL } from "./staff";

// Init scripts run outside the page's policy, so the listener sees every violation.
async function recordViolations(page: Page) {
  const violations: string[] = [];
  page.on("console", (message) => {
    if (/Content Security Policy|Refused to (execute|load|apply)/i.test(message.text()))
      violations.push(message.text());
  });
  await page.addInitScript(() => {
    document.addEventListener("securitypolicyviolation", (event) => {
      console.error(`Refused to execute (securitypolicyviolation): ${event.violatedDirective} ${event.blockedURI}`);
    });
  });
  return violations;
}

const hydrated = (page: Page, selector: string) =>
  page
    .locator(selector)
    .first()
    .evaluate((element) => Object.keys(element).some((key) => key.startsWith("__reactFiber")));

test("prefetch headers cannot remove a private document's security policy", async ({ request }) => {
  // The staff host is named by Host rather than resolved, so the runner's resolver cannot matter.
  for (const [path, host] of [
    ["/account/sign-in", ""],
    ["/partner/sign-in", ""],
    ["/staff/sign-in", new URL(staffOrigin).host],
  ]) {
    for (const [name, value] of [
      ["next-router-prefetch", "1"],
      ["purpose", "prefetch"],
    ]) {
      const response = await request.get(path, { headers: { ...(host ? { host } : {}), [name]: value } });
      expect(response.status()).toBe(200);
      expect(response.headers()["content-security-policy"]).toMatch(/'nonce-[^']+' 'strict-dynamic'/);
    }
  }
});

test("the donor dashboard runs under a per-request nonce policy", async ({ page }) => {
  const violations = await recordViolations(page);
  const response = await page.goto("/account/dashboard");
  const policy = response?.headers()["content-security-policy"] ?? "";
  expect(policy).toMatch(/script-src 'self' 'nonce-[^']+' 'strict-dynamic'/);
  expect(policy).not.toMatch(/script-src[^;]*'unsafe-inline'/);
  // Only rendered after the client has asked the API for a session.
  await expect(page.getByRole("link", { name: "Sign in to view your dashboard" })).toBeVisible();
  expect(violations).toEqual([]);

  const again = await page.reload();
  expect(again?.headers()["content-security-policy"]).not.toBe(policy);
});

test("staff sign-in hydrates under the nonce policy", async ({ page }) => {
  const violations = await recordViolations(page);
  const response = await page.goto(staffURL("/staff/sign-in"));
  expect(response?.headers()["content-security-policy"]).toMatch(/'nonce-[^']+' 'strict-dynamic'/);
  await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
  await expect.poll(() => hydrated(page, "main button")).toBe(true);
  expect(violations).toEqual([]);
});

test("public pages keep the static, cacheable policy", async ({ page }) => {
  const violations = await recordViolations(page);
  const response = await page.goto("/");
  const policy = response?.headers()["content-security-policy"] ?? "";
  expect(policy).toMatch(/script-src 'self' 'unsafe-inline'/);
  expect(policy).not.toContain("nonce-");
  await expect.poll(() => hydrated(page, "main")).toBe(true);
  expect(violations).toEqual([]);
});

test("the partner portal hydrates under a fresh nonce policy", async ({ page }) => {
  const violations = await recordViolations(page);
  const response = await page.goto("/partner");
  const policy = response?.headers()["content-security-policy"] ?? "";
  expect(policy).toMatch(/script-src 'self' 'nonce-[^']+' 'strict-dynamic'/);
  expect(policy).not.toMatch(/script-src[^;]*'unsafe-inline'/);
  await expect(page.getByRole("link", { name: "Email me a sign-in link" })).toBeVisible();
  const again = await page.reload();
  expect(again?.headers()["content-security-policy"]).not.toBe(policy);
  await expect(page.getByRole("link", { name: "Email me a sign-in link" })).toBeVisible();
  expect(violations).toEqual([]);
});

test("partner email proof waits for confirmation and recovers from an unavailable service", async ({ page }) => {
  let exchanges = 0;
  await page.route("**/api/donor/partner/accept-invite", (route) => {
    exchanges++;
    return route.fulfill({ status: 503, json: { detail: "Unavailable" } });
  });
  await page.goto("/partner/accept#token=fixture-proof");
  const confirm = page.getByRole("button", { name: "Confirm and sign in" });
  await expect(confirm).toBeVisible();
  await expect(page).toHaveURL(/\/partner\/accept$/);
  expect(exchanges).toBe(0);
  await confirm.click();
  await expect(page.getByRole("main").getByRole("alert")).toContainText("We couldn’t sign you in right now.");
  await expect(confirm).toBeEnabled();
  await confirm.click();
  await expect.poll(() => exchanges).toBe(2);
});

test("partner sign-in links to the real application page", async ({ page }) => {
  await page.goto("/partner/sign-in");
  await page.getByRole("link", { name: "Apply to partner with DAD" }).click();
  await expect(page).toHaveURL(/\/apply$/);
  await expect(page.getByRole("heading", { name: "Apply to partner with DAD" })).toBeVisible();
});

test("event links load account pages with their private document policy", async ({ page }) => {
  const event = {
    id: "00000000-0000-4000-8000-000000000001",
    title: "Community cleanup",
    type: "VOLUNTEER",
    description: "Meet at the park.",
    venue: "Park",
    address: "Main St",
    city: "Boston",
    starts_at: "2099-01-01T12:00:00Z",
    ends_at: "2099-01-01T14:00:00Z",
    capacity: 10,
    spots_left: 10,
    reservation_status: null,
    invitation_note: null,
    report_issue_url: null,
    cancelled: false,
    booking_opens_at: null,
    early_booking: false,
    time_zone: "America/Los_Angeles",
    checked_in_at: null,
  };
  let signedIn = false;
  await page.route("**/api/donor/me/events/*", (route) =>
    signedIn ? route.fulfill({ json: event }) : route.fulfill({ status: 401, json: { detail: "Sign in required" } }),
  );
  await page.route("**/api/events/*", (route) => route.fulfill({ json: event }));
  for (const [label, destination] of [
    ["Sign in to reserve a spot", "/account/sign-in"],
    ["All events near you", "/account/events"],
  ]) {
    await page.goto(`/events/${event.id}`);
    const response = page.waitForResponse(
      (response) =>
        new URL(response.url()).pathname === destination && response.request().resourceType() === "document",
    );
    await page.getByRole("link", { name: label }).click();
    expect((await response).headers()["content-security-policy"]).toMatch(/'nonce-[^']+' 'strict-dynamic'/);
    signedIn = true;
  }
});

test("public account links load a new document with the private CSP", async ({ page }) => {
  const violations = await recordViolations(page);
  for (const [source, label, destination] of [
    ["/", "My account", "/account/dashboard"],
    ["/donate/thanks", "dashboard", "/account/dashboard"],
    ["/donate/thanks", "invite someone to give with you", "/account/share"],
    ["/how-it-works", "Partner sign-in", "/partner/sign-in"],
  ]) {
    await page.goto(source);
    await expect.poll(() => hydrated(page, "main")).toBe(true);
    if (label === "My account" && (await page.getByRole("button", { name: "Menu", exact: true }).isVisible())) {
      await page.getByRole("button", { name: "Menu", exact: true }).click();
    }
    const documentResponse = page.waitForResponse(
      (response) =>
        new URL(response.url()).pathname === destination && response.request().resourceType() === "document",
    );
    await page.getByRole("link", { name: label, exact: true }).click();
    const response = await documentResponse;
    expect(response.headers()["content-security-policy"]).toMatch(/'nonce-[^']+' 'strict-dynamic'/);
    expect(response.headers()["content-security-policy"]).not.toMatch(/script-src[^;]*'unsafe-inline'/);
    await expect.poll(() => hydrated(page, "main")).toBe(true);
  }
  expect(violations).toEqual([]);
});
