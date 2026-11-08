import AxeBuilder from "@axe-core/playwright";
import { expect, type Page, test } from "@playwright/test";
import { addPasskeyDevice, administratorInvitation, buildExports, staffURL } from "./staff";

async function expectAccessible(page: Page) {
  const results = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa", "wcag21aa", "wcag22aa"]).analyze();
  expect(results.violations).toEqual([]);
  expect(await page.locator("body").evaluate((body) => body.scrollWidth <= innerWidth + 1)).toBe(true);
}

// Runs against the real API and database: the invitation comes from the operator command.
test("an invited administrator creates a passkey, then signs out and back in with it", async ({ page }) => {
  test.setTimeout(150_000);
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  const invitation = await administratorInvitation("e2e.administrator@example.org");
  expect(invitation.startsWith(staffURL("/staff/activate#token="))).toBe(true);
  await addPasskeyDevice(page);

  await page.goto(invitation);
  await expect(page).toHaveURL(staffURL("/staff/activate"));
  await expect(page.getByText("E2E Administrator", { exact: true })).toBeVisible();
  await expect(page.getByText(/no password to remember/)).toBeVisible();
  await expectAccessible(page);
  await page.getByRole("button", { name: "Create a passkey" }).click();
  await expect(page).toHaveURL(staffURL("/staff"));
  await expect(page.getByText("Signed in as E2E Administrator · SUPER_ADMIN")).toBeVisible();

  await page.getByRole("button", { name: "Sign out" }).click();
  await expect(page.getByRole("link", { name: "Staff sign-in" })).toBeFocused();
  await expect(page.getByRole("main").getByRole("status")).toHaveText("You’ve signed out of DAD staff.");
  await page.getByRole("link", { name: "Staff sign-in" }).click();
  await expect(page).toHaveURL(staffURL("/staff/sign-in"));
  await expect(page.getByRole("textbox")).toHaveCount(0);
  await expectAccessible(page);
  await page.getByRole("button", { name: "Sign in with a passkey" }).click();
  await expect(page).toHaveURL(staffURL("/staff"));
  await expect(page.getByText("Signed in as E2E Administrator · SUPER_ADMIN")).toBeVisible();

  // The donor money queues are served by the real API to a super admin.
  await page.getByRole("link", { name: "Failed payments" }).click();
  await expect(page.getByRole("heading", { level: 1, name: "Failed payments" })).toBeVisible();
  await expect(page.getByRole("table").or(page.getByText(/^No failed payments/))).toBeVisible();
  await expectAccessible(page);
  await page.getByRole("link", { name: "Staff back office" }).click();
  await page.getByRole("link", { name: "Account deletions" }).click();
  await expect(page.getByRole("heading", { level: 1, name: "Account deletions" })).toBeVisible();
  await expect(page.getByRole("table").or(page.getByText("No accounts have been deleted."))).toBeVisible();
  await expectAccessible(page);
  await page.getByRole("link", { name: "Staff back office" }).click();

  // An export builds in the background; the audited download starts from the back office itself,
  // never from another host.
  await page.getByRole("link", { name: "Exports and enrolment queue" }).click();
  await page.getByRole("button", { name: "Request gifts CSV" }).click();
  // Earlier runs leave their own exports listed; the newest is the first row after the header.
  const newest = page.getByRole("row").nth(1);
  await expect(newest.getByRole("cell", { name: "Waiting to start" })).toBeVisible();
  await buildExports();
  const fileLink = newest.getByRole("link", { name: "Download gifts CSV" });
  await expect(fileLink).toBeVisible();
  await expectAccessible(page);
  const exportURL = staffURL((await fileLink.getAttribute("href")) ?? "");
  const download = page.waitForEvent("download");
  await fileLink.click();
  expect((await download).suggestedFilename()).toBe("dad-gifts.csv");
  await page.goto("/");
  const crossHost = page.waitForResponse(exportURL);
  await page.evaluate((url) => {
    window.location.href = url;
  }, exportURL);
  expect((await crossHost).status()).toBe(403);
  await page.waitForURL(exportURL);

  await page.goto(invitation);
  await expect(page.getByRole("main").getByRole("alert")).toContainText(
    "This invitation has already been used or has expired.",
  );
  await page.getByRole("link", { name: "Staff sign-in" }).click();
  await expect(page).toHaveURL(staffURL("/staff/sign-in"));
  await page.goto(staffURL("/staff/activate"));
  await expect(page.getByText(/Open this page from the private invitation link/)).toBeVisible();
  expect(errors).toEqual([]);
});

test("the staff host serves only the back office, and the public host none of it", async ({ page }) => {
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("console", (message) => {
    if (message.type() === "error" && /hydrat|server rendered HTML/i.test(message.text())) errors.push(message.text());
  });
  const signIn = await page.goto(staffURL("/staff/sign-in"));
  expect(signIn?.status()).toBe(200);
  expect(signIn?.headers()["x-robots-tag"]).toBe("noindex, nofollow");
  expect(signIn?.headers()["cache-control"]).toContain("no-store");
  await expect(page.locator('meta[name="robots"]')).toHaveAttribute("content", /noindex/);
  await expect(page.getByRole("contentinfo")).toHaveCount(0);

  await page.goto(staffURL("/"));
  await expect(page).toHaveURL(staffURL("/staff"));
  for (const path of ["/spend", "/account/dashboard", "/donate", "/Staff/sign-in", "/staff/nowhere"]) {
    expect((await page.goto(staffURL(path)))?.status()).toBe(404);
    await expect(page.getByText("This page isn’t part of the staff back office.")).toBeVisible();
    await expect(page.getByRole("link", { name: "Go to staff home" })).toHaveAttribute("href", "/staff");
    await expect(page.getByRole("link", { name: "DAD staff" })).toBeVisible();
    await expect(page.getByRole("navigation", { name: "Primary navigation" })).toHaveCount(0);
    await expect(page.getByRole("contentinfo")).toHaveCount(0);
  }
  await expectAccessible(page);
  for (const path of ["/api/donor/me/dashboard", "/api/donor/%6De/dashboard"]) {
    expect((await page.goto(staffURL(path)))?.status()).toBe(404);
  }
  await page.goto(staffURL("/robots.txt"));
  await expect(page.locator("body")).toContainText("Disallow: /");

  for (const path of ["/staff", "/staff/sign-in", "/staff/activate", "/%73taff/sign-in"]) {
    const response = await page.goto(path);
    expect(response?.status()).toBe(404);
    await expect(page.getByRole("heading", { name: "We couldn’t find this page" })).toBeVisible();
    await expect(page.getByRole("link", { name: "Dynamic Aid Delivery" })).toHaveAttribute("href", "/");
    // Behind the menu button on narrow screens.
    await expect(page.getByRole("navigation", { name: "Primary navigation", includeHidden: true })).toBeAttached();
    await expect(page.getByRole("contentinfo")).toBeVisible();
  }
  await expectAccessible(page);
  for (const path of ["/api/donor/staff/me", "/api/donor/%73taff/me", "/api/donor/sTaFf/me"]) {
    expect((await page.goto(path))?.status()).toBe(404);
  }
  expect(errors).toEqual([]);
});
