import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { render, screen } from "@testing-library/react";
import axe from "axe-core";
import { describe, expect, it } from "vitest";
import { Insights } from "./Insights";
import { canRender } from "./panel";
import { linkLabel, metricText, resolvePointer, scalarText } from "./resolve";
import { sampleDashboard } from "./sampleDashboard";

type Vector = { name: string; valid: boolean; spec: unknown[] };
const vectors: Vector[] = JSON.parse(
  readFileSync(resolve(process.cwd(), "../../contracts/mobile/ui-catalog-vectors.json"), "utf8"),
).vectors;
const valid = vectors.filter((vector) => vector.valid);

const draw = (spec: unknown, dashboard: unknown = sampleDashboard) =>
  render(<Insights spec={spec} dashboard={dashboard} />);

describe("every valid shared vector", () => {
  it.each(valid.map((vector) => [vector.name, vector.spec] as const))("renders without throwing: %s", (_name, spec) => {
    expect(() => draw(spec)).not.toThrow();
  });

  it.each(vectors.filter((vector) => !vector.valid).map((vector) => [vector.name, vector.spec] as const))(
    "does not throw on an invalid layout either: %s",
    (_name, spec) => {
      expect(() => draw(spec)).not.toThrow();
    },
  );

  it("has vectors to run", () => {
    expect(valid.length).toBeGreaterThan(20);
  });
});

describe("renders the catalog", () => {
  it("draws headings, bound text, metrics with their captions, callouts and links", () => {
    const { container } = draw([
      { type: "Heading", title: "Your impact" },
      { type: "Heading", bind: "/impact/currency" },
      { type: "Text", bind: "/founding_donor" },
      { type: "Text", bind: "/computed_at" },
      {
        type: "MetricGrid",
        children: [
          { type: "Metric", bind: "/impact/given_minor", format: "usd_minor", caption: "given" },
          { type: "Metric", bind: "/streak/ladder/days_to_next", format: "days", caption: "days_to_next" },
          { type: "Metric", bind: "/rank/national_percentile", format: "percent", caption: "national_percentile" },
          { type: "Metric", bind: "/network/active_count", format: "integer" },
        ],
      },
      { type: "Callout", tone: "success", title: "Thank you", body: "Every gift counts." },
      { type: "Callout", tone: "warning", title: "Founding", bind: "/streak/current" },
      {
        type: "List",
        bind: "/streak/ladder/bar",
        children: [
          { type: "Text", bind: "/streak/current" },
          { type: "LinkButton", navigate: "/account/gifts" },
        ],
      },
    ]);
    expect(screen.getByRole("heading", { level: 3, name: "Your impact" })).toBeVisible();
    expect(screen.getByRole("heading", { level: 3, name: "USD" })).toBeVisible();
    expect(screen.getByText("Yes")).toBeVisible();
    expect(screen.getByText("$45.00")).toBeVisible();
    expect(screen.getByText("Total given")).toBeVisible();
    expect(screen.getByText("6 days")).toBeVisible();
    expect(screen.getByText("2%")).toBeVisible();
    expect(screen.getByText("3")).toBeVisible();
    expect(screen.getByText("Every gift counts.")).toBeVisible();
    expect(container.querySelector(".callout.highlight")).not.toBeNull();
    expect(container.querySelector(".callout.warn")).not.toBeNull();
    expect(screen.getByRole("link", { name: "Your gifts" })).toHaveAttribute("href", "/account/gifts");
  });
});

describe("draws nothing for what it cannot trust", () => {
  const nothing = (spec: unknown, dashboard: unknown = sampleDashboard) => {
    const { container } = draw(spec, dashboard);
    expect(container).toBeEmptyDOMElement();
  };

  it("an unknown node type, however nested", () => {
    nothing([{ type: "Carousel", title: "Hello", children: [{ type: "Text", bind: "/mode" }] }]);
    nothing([{ type: "MetricGrid", children: [{ type: "Chart", bind: "/impact/given_minor" }] }]);
    nothing([{ type: "List", bind: "/streak/ladder/bar", children: [{ type: "Heading", title: "No" }] }]);
  });

  it("a bind that does not resolve, or reaches a missing branch", () => {
    nothing([{ type: "Text", bind: "/nowhere" }]);
    nothing([{ type: "Text", bind: "/streak/ladder/bar/0" }]);
    nothing([{ type: "Text", bind: "streak" }]);
    nothing([{ type: "Text", bind: "/streak/grace_ends_at" }]);
    nothing([{ type: "Metric", bind: "/impact/given_minor", format: "usd_minor" }], { ...sampleDashboard, impact: {} });
    nothing([{ type: "Text", bind: "/__proto__/constructor" }]);
  });

  it("a value of the wrong type", () => {
    nothing([{ type: "Metric", bind: "/mode", format: "integer" }]);
    nothing([{ type: "Metric", bind: "/impact/given_minor", format: "bogus" }]);
    nothing([{ type: "Metric", bind: "/rank/city", format: "integer" }]);
    nothing([{ type: "Text", bind: "/rank" }]);
    nothing([{ type: "List", bind: "/streak/current", children: [{ type: "Text", bind: "/mode" }] }]);
    nothing([{ type: "List", bind: "/streak/ladder/bar", children: "no" }]);
    nothing([{ type: "Callout", title: "Hi", body: 3 }]);
  });

  it("a link to a path outside the allowed set, or one it cannot label", () => {
    nothing([{ type: "LinkButton", navigate: "/partner" }]);
    nothing([{ type: "LinkButton", navigate: "https://example.com/" }]);
    nothing([{ type: "LinkButton", navigate: "//example.com" }]);
    nothing([{ type: "LinkButton", navigate: "javascript:alert(1)" }]);
    nothing([{ type: "LinkButton", navigate: 4 }]);
    nothing([{ type: "LinkButton", navigate: "constructor" }]);
  });

  it("never reads the layout's own field", () => {
    const dashboard = {
      ...sampleDashboard,
      insights: { catalog_version: 1, spec: [{ type: "Heading", title: "Secret" }] },
    };
    nothing([{ type: "Text", bind: "/insights/spec/0/title" }], dashboard);
    expect(resolvePointer(dashboard, "/insights/catalog_version")).toBeUndefined();
  });

  it("malformed input of every shape", () => {
    for (const spec of [null, undefined, 3, "x", {}, [null], [3], ["x"], [[]], [{}], [{ type: 4 }], [{ type: null }]])
      expect(() => draw(spec)).not.toThrow();
    nothing([null, 3, "x", [], {}, { type: 4 }]);
    nothing([{ type: "Heading" }, { type: "Callout" }, { type: "Callout", title: "No body" }, { type: "Text" }]);
  });

  it("never draws literal text a Text node was not given by a bind", () => {
    nothing([{ type: "Text", title: "Hello", body: "Hi", text: "Hi" }]);
  });
});

describe("a layout that is not drawn at all", () => {
  it("is skipped when absent, empty, or written for a newer catalog", () => {
    expect(canRender(undefined)).toBe(false);
    expect(canRender(null)).toBe(false);
    expect(canRender({ catalog_version: 1, spec: [] })).toBe(false);
    expect(canRender({ catalog_version: 2, spec: [{ type: "Heading", title: "Hi" }] })).toBe(false);
    expect(canRender({ catalog_version: 1, spec: "x" })).toBe(false);
    expect(canRender({ catalog_version: 1, spec: [{ type: "Heading", title: "Hi" }] })).toBe(true);
  });
});

describe("resolution helpers", () => {
  it("resolves RFC 6901 escapes and refuses inherited properties", () => {
    const root = { "a/b": { "c~d": 5 }, list: [1] };
    expect(resolvePointer(root, "/a~1b/c~0d")).toBe(5);
    expect(resolvePointer(root, "/list/0")).toBeUndefined();
    expect(resolvePointer(root, "/toString")).toBeUndefined();
    expect(resolvePointer(root, "")).toBeUndefined();
    expect(resolvePointer(root, "/a~1b//c~0d")).toBeUndefined();
  });

  it("formats each metric format and refuses non-integers", () => {
    expect(metricText(1234567, "integer")).toBe("1,234,567");
    expect(metricText(12345, "usd_minor")).toBe("$123.45");
    expect(metricText(1, "days")).toBe("1 day");
    expect(metricText(2, "days")).toBe("2 days");
    expect(metricText(40, "percent")).toBe("40%");
    for (const value of [1.5, "3", null, Number.NaN, 2 ** 60]) expect(metricText(value, "integer")).toBeNull();
  });

  it("formats scalars and refuses everything else", () => {
    expect(scalarText(true)).toBe("Yes");
    expect(scalarText(false)).toBe("No");
    expect(scalarText(1500)).toBe("1,500");
    expect(scalarText("plain")).toBe("plain");
    expect(scalarText("2027-01-01T00:00:00Z")).toMatch(/2026|2027/);
    for (const value of [null, undefined, {}, [], Number.POSITIVE_INFINITY]) expect(scalarText(value)).toBeNull();
  });

  it("labels only catalog paths", () => {
    expect(linkLabel("/account/profile")).toEqual({ href: "/account/profile", label: "Your profile" });
    expect(linkLabel("/partner")).toBeUndefined();
    expect(linkLabel("hasOwnProperty")).toBeUndefined();
  });
});

describe("accessibility", () => {
  it("has no axe violations for a layout using every component", async () => {
    const { container } = render(
      <main>
        <section aria-labelledby="h">
          <h2 id="h">Your insights</h2>
          <Insights
            spec={[
              { type: "Heading", title: "Your impact" },
              { type: "Text", bind: "/impact/currency" },
              {
                type: "MetricGrid",
                children: [
                  { type: "Metric", bind: "/impact/given_minor", format: "usd_minor", caption: "given" },
                  { type: "Metric", bind: "/network/active_count", format: "integer", caption: "active_network" },
                ],
              },
              { type: "Metric", bind: "/streak/current", format: "days" },
              { type: "Callout", tone: "info", title: "Note", body: "Thanks for giving." },
              { type: "Callout", tone: "warning", title: "Careful", bind: "/mode" },
              {
                type: "List",
                bind: "/streak/ladder/bar",
                children: [
                  { type: "Text", bind: "/streak/current" },
                  { type: "LinkButton", navigate: "/account/dashboard" },
                ],
              },
            ]}
            dashboard={sampleDashboard}
          />
        </section>
      </main>,
    );
    const result = await axe.run(container, {
      runOnly: ["wcag2a", "wcag2aa", "wcag21aa", "wcag22aa", "best-practice"],
      rules: { "color-contrast": { enabled: false }, region: { enabled: false } },
    });
    expect(result.violations.map((violation) => violation.id)).toEqual([]);
  });
});
