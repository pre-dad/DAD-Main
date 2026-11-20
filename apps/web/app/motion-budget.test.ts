import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { expect, it } from "vitest";

function stylesheets(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) =>
    entry.isDirectory()
      ? stylesheets(join(dir, entry.name))
      : entry.name.endsWith(".css")
        ? [join(dir, entry.name)]
        : [],
  );
}

const css = stylesheets(join(process.cwd(), "app"))
  .map((file) => readFileSync(file, "utf8"))
  .join("\n")
  .replace(/\/\*[\s\S]*?\*\//g, " ");

it("keeps R1 numbers and controls free of unsolicited motion", () => {
  expect(css.match(/(?:transition|animation)(?:-[a-z-]+)?\s*:[^;}]+/g) ?? []).toEqual([]);
  expect(css.match(/@keyframes\s+[a-zA-Z-]+/g) ?? []).toEqual([]);
  expect(css.match(/scroll-behavior\s*:\s*smooth/g) ?? []).toEqual([]);
});
