import { describe, expect, it } from "vitest";
import { webTokenVariables } from "@/design/web";

function token(name: string): string {
  const value = (webTokenVariables as Record<string, unknown>)[`--${name}`];
  if (typeof value !== "string" || !/^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6})$/.test(value))
    throw new Error(`--${name} is not defined as a hex colour`);
  return value;
}

function luminance(hex: string): number {
  let h = hex.replace("#", "");
  if (h.length === 3) h = [...h].map((c) => c + c).join("");
  const channel = (pair: string) => {
    const v = parseInt(pair, 16) / 255;
    return v <= 0.04045 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * channel(h.slice(0, 2)) + 0.7152 * channel(h.slice(2, 4)) + 0.0722 * channel(h.slice(4, 6));
}

function contrast(foreground: string, background: string): number {
  const a = luminance(foreground);
  const b = luminance(background);
  return (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
}

describe("the palette meets accessible text contrast thresholds", () => {
  const paper = token("paper");

  it.each([
    ["--ink, body text", 4.5, "ink"],
    ["--ink-soft, the 9-11px labels that carry the detail", 4.5, "ink-soft"],
    ["--brand-ink, the brand hue taken to AA for small text", 4.5, "brand-ink"],
  ])("%s: reaches %s:1 on paper (--%s)", (_label, minimum, name) => {
    expect(contrast(token(name), paper)).toBeGreaterThanOrEqual(minimum);
  });

  it("--brand reaches large-text AA on paper, which is what it is reserved for", () => {
    // Fills, underlines and display sizes only; never small text, and never inside a
    // status chip where it would read as a verdict.
    expect(contrast(token("brand"), paper)).toBeGreaterThanOrEqual(3);
  });

  it("--warn is legible on its own wash", () => {
    expect(contrast(token("warn"), token("warn-wash"))).toBeGreaterThanOrEqual(4.5);
  });

  it("a verdict is never carried by the house colour", () => {
    // Orange is identity. Confirmation and failure have to be distinguishable from it, or
    // the palette says "verified" and "look at this" in the same voice.
    expect(token("ok")).not.toBe(token("brand"));
    expect(token("fail")).not.toBe(token("brand"));
  });

  it.each([
    ["--ok-ink, a confirmation", "ok-ink", "ok-wash"],
    ["--fail-ink, a failure", "fail-ink", "fail-wash"],
  ])("%s is legible on its own wash", (_label, ink, wash) => {
    expect(contrast(token(ink as string), token(wash as string))).toBeGreaterThanOrEqual(4.5);
  });

  it.each([
    ["--dkmut, secondary text on the dark header and hero", "dkmut", "ink", 4.5],
    ["--dkmut, footer text", "dkmut", "ink-deep", 4.5],
    ["--dkmut, the preview banner text", "dkmut", "ink-deep", 4.5],
    ["--paper, the preview banner link and label", "paper", "ink-deep", 4.5],
    ["--muted, notes inside cards", "muted", "card", 4.5],
    ["--ink, label text on a brand-filled primary button", "ink", "brand", 4.5],
    ["--paper, label text on a selected amount preset", "paper", "ink", 4.5],
    ["--fail, a destructive secondary action on a card", "fail", "card", 4.5],
    ["--ink-soft, badge guidance on paper", "ink-soft", "paper", 4.5],
    ["--dkmut, the Founding line on the dark hero", "dkmut", "ink", 4.5],
    ["--brand-ink, the step number on a step card", "brand-ink", "card-quiet", 4.5],
    ["--brand-ink, the step number on a step card below 600px", "brand-ink", "card", 4.5],
    ["--ink, a step title on a step card", "ink", "card-quiet", 4.5],
    ["--muted, a step description on a step card", "muted", "card-quiet", 4.5],
    ["--ok-ink, the verified line on the Today card", "ok-ink", "card-quiet", 4.5],
    ["--muted, a zero count on the dashboard", "muted", "card-quiet", 4.5],
    ["--brand-ink, the dashboard card links", "brand-ink", "card-quiet", 4.5],
    ["--dkmut, the Founding Donor card text", "dkmut", "ink-deep", 4.5],
    ["--paper, the Founding Donor card value", "paper", "ink-deep", 4.5],
    ["--ink, the live gift summary on its wash", "ink", "brand-wash", 4.5],
    ["--ink-soft, the Founding line on the Give card", "ink-soft", "card", 4.5],
    ["--muted, the field hints and notes on Give", "muted", "paper", 4.5],
    ["--paper, the selected amount tile label", "paper", "ink", 4.5],
    ["--brand, the selected amount tile check", "brand", "ink", 3],
  ])("%s: --%s on --%s reaches %s:1", (_label, foreground, background, minimum) => {
    expect(contrast(token(foreground as string), token(background as string))).toBeGreaterThanOrEqual(
      minimum as number,
    );
  });

  it.each([
    ["--brand-ink focus ring on the dark hero", "brand-ink", "ink"],
    ["--brand-ink focus ring on paper", "brand-ink", "paper"],
    ["--brand focus ring on the dark header", "brand", "ink"],
    ["--brand star on the dark hero", "brand", "ink"],
    ["--brand star on the Founding Donor card", "brand", "ink-deep"],
  ])("%s: --%s on --%s reaches the 3:1 non-text minimum", (_label, ring, background) => {
    expect(contrast(token(ring as string), token(background as string))).toBeGreaterThanOrEqual(3);
  });
});
