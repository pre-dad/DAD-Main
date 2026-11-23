import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import {
  catalogData,
  catalogTypes,
  checkCatalog,
  checkVectors,
  labelProblem,
  loadSources,
  parsePointer,
  validateSpec,
} from "./ui-catalog.mjs";

const sources = await loadSources();
const { vectors } = JSON.parse(
  await readFile(new URL("../contracts/mobile/ui-catalog-vectors.json", import.meta.url), "utf8"),
);

for (const vector of vectors) {
  test(`vector: ${vector.name}`, () => {
    const result = validateSpec(vector.spec, sources);
    assert.equal(result.ok, vector.valid, JSON.stringify(result.errors));
    assert.deepEqual(
      result.errors.map((error) => error.code),
      vector.valid ? [] : vector.codes,
    );
    for (const error of result.errors) assert.equal(typeof error.path, "string");
  });
}

test("vector suite covers both outcomes and every error code", () => {
  assert.ok(vectors.filter((vector) => vector.valid).length >= 20);
  assert.ok(vectors.filter((vector) => !vector.valid).length >= 25);
  assert.equal(new Set(vectors.map((vector) => vector.name)).size, vectors.length);
  const seen = new Set(vectors.flatMap((vector) => vector.codes ?? []));
  for (const code of [
    "SPEC_NOT_ARRAY",
    "SPEC_EMPTY",
    "NODE_INVALID",
    "MISSING_TYPE",
    "UNKNOWN_PROP",
    "MISSING_PROP",
    "ONE_OF",
    "BAD_TYPE",
    "BAD_ENUM",
    "BAD_FORMAT",
    "BAD_CAPTION",
    "LABEL_EMPTY",
    "LABEL_TOO_LONG",
    "LABEL_CONTENT",
    "BIND_SYNTAX",
    "BIND_UNRESOLVED",
    "BIND_NOT_SCALAR",
    "BIND_NOT_ARRAY",
    "BIND_NOT_INTEGER",
    "NAV_FORBIDDEN",
    "CHILD_NOT_ALLOWED",
    "CHILDREN_EMPTY",
    "GRID_TOO_LARGE",
    "TOO_DEEP",
    "TOO_MANY_NODES",
  ])
    assert.ok(seen.has(code), `no vector exercises ${code}`);
});

test("the catalog is consistent with openapi.json and links.json", () => {
  assert.deepEqual(checkCatalog(sources), []);
});

test("every vector passes the catalog self-check runner", async () => {
  assert.deepEqual(await checkVectors(sources, { vectors }), []);
});

test("renaming a bound DonorDashboard field breaks the check", async () => {
  const openapi = structuredClone(sources.openapi);
  const ready = openapi.components.schemas.DonorDashboard.oneOf[1].properties;
  const impact = openapi.components.schemas.ImpactSummary.properties;
  impact.total_given_minor = impact.given_minor;
  delete impact.given_minor;
  assert.ok(ready.impact);
  const problems = await checkVectors({ ...sources, openapi }, { vectors });
  assert.ok(
    problems.some((problem) => problem.includes("metric usd_minor")),
    problems.join("\n"),
  );
});

test("removing the bound operation or changing its response schema breaks the check", () => {
  const moved = structuredClone(sources.openapi);
  moved.paths["/donor/me/dashboard"].get.responses["200"].content["application/json"].schema = {
    $ref: "#/components/schemas/Profile",
  };
  assert.ok(checkCatalog({ ...sources, openapi: moved }).some((problem) => problem.includes("no longer returns")));
  const gone = structuredClone(sources.openapi);
  delete gone.paths["/donor/me/dashboard"];
  assert.ok(checkCatalog({ ...sources, openapi: gone }).some((problem) => problem.includes("no GET operation")));
});

test("dropping a navigate path from links.json breaks the check and the vectors", async () => {
  const links = structuredClone(sources.links);
  links.account_paths = links.account_paths.filter((path) => path !== "/account/profile");
  assert.ok(checkCatalog({ ...sources, links }).some((problem) => problem.includes("/account/profile")));
  assert.ok((await checkVectors({ ...sources, links }, { vectors })).length > 0);
});

test("a new links.json path without a catalog label breaks the check", () => {
  const links = structuredClone(sources.links);
  links.account_paths.push("/account/new");
  assert.ok(checkCatalog({ ...sources, links }).some((problem) => problem.includes("/account/new has no label")));
});

test("catalog structural mistakes are reported", () => {
  const catalog = structuredClone(sources.catalog);
  catalog.components.Text.props.title = { kind: "label", max: "max_title_chars" };
  catalog.components.MetricGrid.props.children.allowed.push("Missing");
  catalog.components.Callout.props.body.max = "max_nothing";
  catalog.metric_captions.bad = "<b>Bold</b>";
  const problems = checkCatalog({ ...sources, catalog });
  for (const fragment of [
    "Text carries literal text",
    "unknown component Missing",
    "unknown limit max_nothing",
    "Caption bad",
  ])
    assert.ok(
      problems.some((problem) => problem.includes(fragment)),
      fragment,
    );
});

test("pointers follow RFC 6901 escaping", () => {
  assert.deepEqual(parsePointer("/a~1b/c~0d"), ["a/b", "c~d"]);
  assert.equal(parsePointer(""), null);
  assert.equal(parsePointer("/a//b"), null);
  assert.equal(parsePointer("/a~"), null);
});

test("text policy rejects markup, schemes, markdown links and invisible characters", () => {
  for (const bad of [
    "<i>",
    "a > b",
    "http://x.y",
    "ftp://x",
    "mailto:a@b.c",
    "www.example.com",
    "[a](b)",
    "a\tb",
    "a\u202eb",
  ])
    assert.equal(labelProblem(bad), "LABEL_CONTENT", JSON.stringify(bad));
  for (const good of ["Thank you, Alex!", "Gifts: 3 of 5", "Q&A about time: 10:30", "Give $25 today"])
    assert.equal(labelProblem(good), null, good);
});

test("length is counted in characters, not UTF-16 units", () => {
  const title = "\u{1F600}".repeat(60);
  assert.deepEqual(validateSpec([{ type: "Heading", title }], sources), { ok: true, errors: [] });
});

test("binds resolve through nullable unions and oneOf branches", () => {
  const spec = [
    { type: "Text", bind: "/mode" },
    { type: "Text", bind: "/state" },
    { type: "Metric", bind: "/streak/ladder/next_milestone", format: "days" },
  ];
  assert.deepEqual(validateSpec(spec, sources), { ok: true, errors: [] });
});

test("generated TypeScript types describe every component and the unknown-node escape hatch", () => {
  const types = catalogTypes(sources.catalog, sources.links);
  for (const name of Object.keys(sources.catalog.components))
    assert.ok(types.includes(`export type Ui${name} =`), name);
  assert.ok(types.includes("uiCatalogVersion = 1"));
  assert.ok(types.includes("UiUnknownNode"));
  assert.ok(types.includes('"/account/gifts"'));
});

test("generated web data carries the id tables and the supported catalog version", () => {
  const data = catalogData(sources.catalog);
  assert.ok(data.includes(`supportedCatalogVersion = ${sources.catalog.catalog_version}`));
  for (const [id, caption] of Object.entries(sources.catalog.metric_captions))
    assert.ok(data.includes(`${JSON.stringify(id)}: ${JSON.stringify(caption)}`), id);
  for (const [path, label] of Object.entries(sources.catalog.navigate.labels))
    assert.ok(data.includes(`${JSON.stringify(path)}: ${JSON.stringify(label)}`), path);
  assert.ok(!data.includes("/partner"));
});

test("a bind under /insights never resolves", () => {
  for (const bind of ["/insights", "/insights/catalog_version", "/insights/spec/0/title"])
    assert.deepEqual(
      validateSpec([{ type: "Text", bind }], sources).errors.map((e) => e.code),
      ["BIND_UNRESOLVED"],
    );
});
