import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { type Browser, expect, type Page, test } from "@playwright/test";
import { expectAccessible } from "./a11y";
import { addPasskeyDevice, administratorInvitation, staffURL } from "./staff";

// Real API and database, nothing mocked. The API runs with DONOR_INSIGHTS_ENABLED=true (playwright.config.ts).
const layout = [
  { type: "Heading", title: "Your impact" },
  { type: "Metric", bind: "/impact/given_minor", format: "usd_minor", caption: "given" },
  { type: "LinkButton", navigate: "/account/gifts" },
];

async function readyDonorLink(origin: string): Promise<string> {
  const { stdout } = await promisify(execFile)(
    "go",
    ["run", "./cmd/browserfixture", "-kind", "donor", "-origin", origin, "-ready"],
    { cwd: "../donor-api", env: process.env },
  );
  return stdout.trim();
}

async function activate(browser: Browser, invitation: string): Promise<Page> {
  const page = await (await browser.newContext()).newPage();
  await addPasskeyDevice(page);
  await page.goto(invitation);
  await page.getByRole("button", { name: "Create a passkey" }).click();
  await expect(page).toHaveURL(staffURL("/staff"));
  return page;
}

/** The first administrator is the operator's; every other member is invited from the back office. */
async function inviteApprover(author: Page): Promise<string> {
  const form = author.getByRole("group", { name: "Invite individual staff member" });
  await form.getByLabel("Name").fill("E2E Approver");
  await form.getByLabel("Email").fill(`e2e.insights.approver.${Date.now()}@example.org`);
  await form.getByLabel("Role").selectOption("SUPER_ADMIN");
  await form.getByRole("button", { name: "Create private invitation" }).click();
  const link = await author.locator("code", { hasText: "/staff/activate#token=" }).innerText();
  return link;
}

test("one staff member writes a layout, a different one approves it, and a donor sees it until it is retired", async ({
  browser,
  baseURL,
}) => {
  test.setTimeout(240_000);
  const author = await activate(browser, await administratorInvitation("e2e.administrator@example.org"));
  const approver = await activate(browser, await inviteApprover(author));
  const donor = await (await browser.newContext({ baseURL })).newPage();
  const errors: string[] = [];
  for (const page of [author, approver, donor]) page.on("pageerror", (error) => errors.push(error.message));

  await author.getByRole("link", { name: "Dashboard insights" }).click();
  await expect(author.getByRole("heading", { level: 1, name: "Dashboard insights" })).toBeVisible();
  await author.getByLabel("Layout JSON").fill(JSON.stringify([{ type: "LinkButton", navigate: "/partner" }]));
  await author.getByRole("button", { name: "Save as draft" }).click();
  const problems = author.getByRole("list", { name: "Catalog problems" });
  await expect(problems).toContainText("NAV_FORBIDDEN");
  await expect(problems).toContainText("/0/navigate");

  await author.getByLabel("Layout JSON").fill(JSON.stringify(layout));
  await expect(author.getByRole("region", { name: "Preview with sample figures" })).toContainText("$45.00");
  await author.getByRole("button", { name: "Save as draft" }).click();
  const detail = author.getByRole("region", { name: /Draft layout by .* \(you\)/ });
  await expect(detail).toBeVisible();
  await expectAccessible(author);
  await author.getByRole("button", { name: "Submit for approval" }).click();
  await expect(author.getByText("Submit done.")).toBeVisible();
  await expect(author.getByText(/a different staff member must approve or reject/)).toBeVisible();
  await expect(author.getByRole("button", { name: /^Approve/ })).toHaveCount(0);

  await approver.goto(staffURL("/staff/insights"));
  await approver
    .getByRole("button", { name: /Waiting for approval/ })
    .first()
    .click();
  const pending = approver.getByRole("region", { name: /Waiting for approval layout by/ });
  await expect(pending).toBeVisible();
  await expect(pending.getByLabel("Layout JSON")).toHaveJSProperty("readOnly", true);
  await expectAccessible(approver);
  await approver.getByRole("button", { name: "Approve this layout", exact: true }).click();
  await approver.getByRole("button", { name: /Approve and make live/ }).click();
  await expect(approver.getByText("Approve done.")).toBeVisible();

  await donor.goto(await readyDonorLink(baseURL ?? ""));
  await donor.getByRole("button", { name: "Confirm and sign in" }).click();
  await expect(donor.getByRole("heading", { level: 1, name: "You’re signed in" })).toBeVisible();
  await donor.goto("/account/dashboard");
  const panel = donor.getByRole("region", { name: "Your insights" });
  await expect(panel).toBeVisible();
  await expect(panel.getByRole("heading", { level: 3, name: "Your impact" })).toBeVisible();
  await expect(panel.getByText("$0.00")).toBeVisible();
  await expect(panel.getByText("Total given")).toBeVisible();
  await expect(panel.getByRole("link", { name: "Your gifts" })).toHaveAttribute("href", "/account/gifts");
  await expectAccessible(donor);

  await author.goto(staffURL("/staff/insights"));
  await author.getByRole("button", { name: /^Live · / }).click();
  await author.getByRole("button", { name: "Retire the live layout", exact: true }).click();
  await author.getByRole("button", { name: /Retire layout/ }).click();
  await expect(author.getByText("Retire done.")).toBeVisible();
  await donor.reload();
  await expect(donor.getByRole("heading", { name: "Link Referrals" })).toBeVisible();
  await expect(donor.getByRole("heading", { name: "Your insights" })).toHaveCount(0);
  expect(errors).toEqual([]);
});
