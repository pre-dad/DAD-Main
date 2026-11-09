import { expect, type Page, test } from "@playwright/test";

async function navigateByHeader(page: Page, name: string) {
  const menu = page.getByRole("button", { name: "Menu" });
  if (await menu.isVisible()) await menu.click();
  await page.getByRole("navigation", { name: "Primary navigation" }).getByRole("link", { name, exact: true }).click();
}

async function expectNoHorizontalScroll(page: Page) {
  expect(await page.locator("body").evaluate((body) => body.scrollWidth <= window.innerWidth + 1)).toBe(true);
}

test("giving requires email verification before checkout", async ({ page }) => {
  await page.goto("/");
  await expect(page.getByRole("heading", { level: 1 })).toContainText("A dollar a day");
  await expect(page.getByRole("complementary", { name: "DAD at a glance" })).toContainText("April 22, 2027");
  await expect(page.getByRole("complementary", { name: "DAD at a glance" })).toContainText("$30/month");
  await page.getByRole("link", { name: /Give \$1 a day/ }).click();
  await expect(page).toHaveURL(/\/donate$/);
  await expect(page.getByLabel("Email")).toBeVisible();
  await expect(page.getByRole("button", { name: "Email me a sign-in link", exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: /^Continue to secure checkout/ })).toHaveCount(0);
  await expect(page.getByRole("link", { name: "Create your donor account", exact: true })).toBeVisible();
  await expectNoHorizontalScroll(page);
});

test("every public header destination works at this viewport", async ({ page }) => {
  await page.goto("/");
  for (const [name, path] of [
    ["How it works", "/how-it-works"],
    ["Initiatives", "/initiatives"],
    ["Every dollar", "/spend"],
    ["My account", "/account/dashboard"],
  ]) {
    await navigateByHeader(page, name);
    await expect(page).toHaveURL(new RegExp(`${path}$`));
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
    await expectNoHorizontalScroll(page);
  }
});

test("the mobile menu announces its state and restores keyboard focus", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/");
  const menu = page.getByRole("button", { name: "Menu" });
  const navigation = page.getByRole("navigation", { name: "Primary navigation" });
  await expect(menu).toHaveAttribute("aria-expanded", "false");
  await expect(navigation).toBeHidden();
  await menu.click();
  await expect(menu).toHaveAttribute("aria-expanded", "true");
  await expect(navigation).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(navigation).toBeHidden();
  await expect(menu).toBeFocused();
});

test("a cold payment-recovery visit requests a sign-in link in place without exposing account data", async ({
  page,
}) => {
  let request: unknown;
  await page.route("**/api/donor/login", async (route) => {
    request = route.request().postDataJSON();
    await route.fulfill({
      status: 202,
      json: { message: "If this email matches a donor account, a sign-in link will arrive shortly." },
    });
  });
  await page.goto("/account/payment-method");
  await expect(page.getByRole("button", { name: "Continue to Stripe" })).toHaveCount(0);
  await page.getByLabel("Email").fill("donor@example.org");
  await page.getByRole("button", { name: "Email me a sign-in link", exact: true }).click();
  await expect(page.getByRole("main").getByRole("status")).toContainText("sign-in link will arrive");
  expect(request).toEqual({ email: "donor@example.org", next: "/account/payment-method" });
  await expectNoHorizontalScroll(page);
});

test("an expired claim link explains itself instead of reporting an outage", async ({ page }) => {
  await page.goto(`/account/claim#token=${"e".repeat(43)}`);
  await page.getByRole("button", { name: "Confirm and sign in" }).click();
  await expect(page.getByRole("main").getByRole("alert")).toHaveText(
    "This link is invalid or expired. Request a new link.",
  );
  await expect(page.getByRole("link", { name: "Request a new link" })).toBeVisible();
});

test("removed legacy surfaces and proxy paths return not found", async ({ page, request }) => {
  for (const path of ["/impactgraph", "/operator", "/verifier", "/proof/old"]) {
    const response = await page.goto(path);
    expect(response?.status()).toBe(404);
    await expect(page.getByRole("heading", { name: "We couldn’t find this page" })).toBeVisible();
  }
  expect((await request.get("/api/claims")).status()).toBe(404);
});
