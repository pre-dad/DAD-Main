import { randomUUID } from "node:crypto";
import { expect, test } from "@playwright/test";
import { axe, expectNoHorizontalScroll } from "./a11y";

test("an application receipt persists through navigation and refresh without granting portal access", async ({
  page,
  browser,
}) => {
  const organization = `Synthetic receipt ${randomUUID()}`;
  await page.goto("/apply");
  await page.getByLabel("Organization name").fill(organization);
  await page.getByLabel("Website").fill("https://receipt.example.org");
  await page.getByLabel("Approximate workforce size").fill("12");
  await page.getByLabel("Your name").fill("Synthetic Applicant");
  await page.getByLabel("Your title").fill("Test Coordinator");
  await page.getByLabel("Work email").fill(`${randomUUID()}@receipt.example.org`);
  await page.getByLabel("Phone", { exact: true }).fill("+14155550123");
  await page.getByRole("button", { name: "Submit application" }).click();
  await expect(page.getByRole("heading", { name: "Application received — pending review" })).toBeVisible();
  const receipt = (await page.context().cookies()).find((cookie) => cookie.name === "dad_application_receipt");
  expect(receipt?.httpOnly).toBe(true);
  expect(receipt?.sameSite).toBe("Lax");
  expect(receipt?.value).toMatch(/^[A-Za-z0-9_-]{43}$/);
  await page.getByRole("link", { name: "Check application status", exact: true }).click();
  await expect(page.getByRole("heading", { name: organization })).toBeVisible();
  await page.reload();
  await expect(page.getByText(/Your application is pending review/)).toBeVisible();
  expect(new URL(page.url()).search).toBe("");
  expect(new URL(page.url()).hash).toBe("");
  const status = await page.request.get("/api/donor/partner/application-status");
  expect(status.headers()["cache-control"]).toBe("no-store");
  expect(await status.json()).toEqual({ organization, status: "PENDING" });
  expect((await page.request.get("/api/donor/partner/overview")).status()).toBe(401);
  expect((await page.request.get("/api/donor/me/profile")).status()).toBe(401);
  await expectNoHorizontalScroll(page);
  expect((await axe(page)).violations).toEqual([]);
  await page.evaluate(() => window.scrollTo(0, 0));
  await page.screenshot({ path: test.info().outputPath("application-status.png"), fullPage: true });
  const other = await browser.newContext();
  try {
    expect((await other.request.get("/api/donor/partner/application-status")).status()).toBe(401);
  } finally {
    await other.close();
  }
});

test("phone sign-in offers a retry after a rejected code", async ({ page }) => {
  await page.route("**/api/donor/phone/sign-in/options", (route) => route.fulfill({ json: { sent: true } }));
  await page.route("**/api/donor/phone/sign-in", (route) =>
    route.fulfill({ status: 401, json: { detail: "That code is incorrect or has expired" } }),
  );
  await page.goto("/account/sign-in");
  await page.getByText("Sign in with your phone", { exact: true }).click();
  await page.getByLabel("Phone number", { exact: true }).fill("+14155550123");
  await page.getByRole("button", { name: "Text me a code" }).click();
  await page.getByLabel("Verification code", { exact: true }).fill("123456");
  await page.getByRole("button", { name: "Verify and sign in" }).click();
  await expect(page.getByText("That code is incorrect or has expired", { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Verify and sign in" })).toBeEnabled();
  await page.getByRole("button", { name: "Change number or request a new code" }).click();
  await expect(page.getByLabel("Verification code", { exact: true })).toHaveCount(0);
  await expect(page.getByLabel("Phone number", { exact: true })).toBeEditable();
  await expectNoHorizontalScroll(page);
  expect((await axe(page)).violations).toEqual([]);
  await page.evaluate(() => window.scrollTo(0, 0));
  await page.screenshot({ path: test.info().outputPath("phone-sign-in.png"), fullPage: true });
});
