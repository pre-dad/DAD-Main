import { expect, test } from "@playwright/test";
import { axe, expectNoHorizontalScroll } from "./a11y";
import { staffURL } from "./staff";

for (const path of [
  "/",
  "/donate",
  "/donate/thanks",
  "/donate/thanks?session_id=cs_test&amount_minor=3000&frequency=MONTHLY",
  "/app/return?result=success&session_id=cs_test",
  "/app/return?result=cancel",
  "/initiatives",
  "/spend",
  "/how-it-works",
  "/about",
  "/legal",
  "/partners",
  "/apply",
  "/account/sign-in",
  "/staff/sign-in",
]) {
  test(`accessible public surface: ${path}`, async ({ page }) => {
    const runtimeErrors: string[] = [];
    page.on("pageerror", (error) => runtimeErrors.push(error.message));
    page.on("console", (message) => {
      if (message.type() === "error" && /hydrat|server rendered HTML/i.test(message.text()))
        runtimeErrors.push(message.text());
    });
    const response = await page.goto(path.startsWith("/staff") ? staffURL(path) : path);
    expect(response?.status()).toBe(200);
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
    const results = await axe(page);
    expect(results.violations).toEqual([]);
    expect(runtimeErrors).toEqual([]);
    await expectNoHorizontalScroll(page);
  });
}
