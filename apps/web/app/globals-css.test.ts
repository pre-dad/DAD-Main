import { readFileSync } from "node:fs";
import { join } from "node:path";
import { expect, it } from "vitest";

const css = readFileSync(join(process.cwd(), "app/globals.css"), "utf8");

it("styles metric tile headings at both h2 and h3 so partner tiles match the cards", () => {
  expect(css).toContain(".metric :is(h2, h3) {");
  expect(css).toContain(".metric :is(h2, h3).initiativeTitle {");
});

it("keeps the interest list rules ahead of the media blocks, as the header says", () => {
  expect(css.indexOf(".interestList {")).toBeLessThan(css.indexOf("@media"));
});

it("carries no rules for classes no component uses", () => {
  expect(css).not.toContain(".segmented");
});
