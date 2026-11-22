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

it("lets an odd final tile in a paired metrics grid span the row", () => {
  expect(css).toMatch(/\.metrics\.pair > :last-child:nth-child\(odd\) \{\s*grid-column: 1 \/ -1;/);
});

it("shows the hero gift framing once on narrow screens", () => {
  expect(css).toMatch(/@media \(max-width: 879\.98px\) \{\s*\.heroGift \{\s*display: none;/);
});
