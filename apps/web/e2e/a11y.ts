import AxeBuilder from "@axe-core/playwright";
import { expect, type Page } from "@playwright/test";

/** Runs axe against the WCAG 2.2 AA rule set the product commits to. */
export const axe = (page: Page) =>
  new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa", "wcag21aa", "wcag22aa"]).analyze();

export async function expectNoHorizontalScroll(page: Page) {
  expect(await page.locator("body").evaluate((body) => body.scrollWidth <= window.innerWidth + 1)).toBe(true);
}

export async function expectAccessible(page: Page) {
  expect((await axe(page)).violations).toEqual([]);
  await expectNoHorizontalScroll(page);
}

/** Uncaught page errors, collected as they happen. */
export function collectErrors(page: Page) {
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  return errors;
}
