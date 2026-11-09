import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { expect, test } from "@playwright/test";

// These journeys run against the real API and database, with nothing mocked. Browser
// tests run without email, so browserfixture stands in for the inbox: it prepares the
// account and prints the single-use link the email would carry.
async function emailedLink(kind: "donor" | "partner", origin: string): Promise<string> {
  const { stdout } = await promisify(execFile)(
    "go",
    ["run", "./cmd/browserfixture", "-kind", kind, "-origin", origin],
    { cwd: "../donor-api", env: process.env },
  );
  return stdout.trim();
}

test("a donor signs in from their email link, names themselves, gets a referral link, and signs out", async ({
  page,
  baseURL,
}) => {
  await page.goto(await emailedLink("donor", baseURL ?? ""));
  await page.getByRole("button", { name: "Confirm and sign in" }).click();
  await expect(page.getByRole("heading", { level: 1, name: "You’re signed in" })).toBeVisible();
  expect(page.url()).not.toContain("token=");

  await page.getByRole("link", { name: "View my dashboard" }).click();
  await expect(page).toHaveURL(/\/account\/dashboard$/);
  await expect(page.getByRole("heading", { level: 1 })).toBeVisible();

  await page.goto("/donate");
  await expect(page.getByRole("button", { name: /^Continue to secure checkout/ })).toBeEnabled();
  await expect(page.getByLabel("Email")).toHaveAttribute("readonly", "");
  const profile = await (await page.request.get("/api/donor/me/profile")).json();
  await expect(page.getByLabel("Email")).toHaveValue(profile.email);

  await page.goto("/account/share");
  await expect(page.getByLabel("Your referral link")).toHaveCount(0);
  await page.getByLabel("Public first name or nickname").fill("Robin");
  await page.getByRole("button", { name: "Save public name" }).click();
  await expect(page.getByLabel("Your referral link")).toHaveValue(/\/r\/\S+/);

  await page.goto("/account/profile");
  await expect(page.getByText("Robin", { exact: true })).toBeVisible();
  // The suite's public origin is an IP address, which cannot be a passkey relying party.
  await expect(page.getByRole("heading", { name: "Passkeys" })).toHaveCount(0);
  await page.getByRole("button", { name: "Sign out of this device" }).click();
  await expect(page.getByText(/You’re signed out/)).toBeVisible();
  await page.goto("/donate");
  await expect(page.getByRole("button", { name: "Email me a sign-in link", exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: /^Continue to secure checkout/ })).toHaveCount(0);
  await page.goto("/account/share");
  await expect(page.getByLabel("Public first name or nickname")).toHaveCount(0);
  await expect(page.getByText(/You’re signed out/)).toBeVisible();
});

test("a partner administrator accepts their invitation, renames the organization, and signs out", async ({
  page,
  baseURL,
}) => {
  await page.goto(await emailedLink("partner", baseURL ?? ""));
  await page.getByRole("button", { name: "Confirm and sign in" }).click();
  await expect(page).toHaveURL(/\/partner$/);
  await expect(page.getByRole("heading", { level: 1, name: "Browser Journey Co" })).toBeVisible();
  await expect(page.getByRole("heading", { level: 2, name: "Users" })).toBeVisible();

  await page.getByLabel("Organization name").fill("Browser Journey Cooperative");
  await page.getByRole("button", { name: "Save profile" }).click();
  await expect(page.getByText("Organization profile saved.")).toBeVisible();
  await page.reload();
  await expect(page.getByRole("heading", { level: 1, name: "Browser Journey Cooperative" })).toBeVisible();

  await page.getByRole("button", { name: "Sign out", exact: true }).click();
  await expect(page).toHaveURL(/\/partner\/sign-in$/);
  await page.goto("/partner");
  await expect(page.getByRole("heading", { level: 1, name: "Browser Journey Cooperative" })).toHaveCount(0);
});
