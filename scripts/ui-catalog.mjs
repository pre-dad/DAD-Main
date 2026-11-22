import { readFile } from "node:fs/promises";
import { dirname, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const scalarTypes = new Set(["string", "integer", "number", "boolean"]);
const own = (object, key) => Object.hasOwn(object, key);
const isObject = (value) => value !== null && typeof value === "object" && !Array.isArray(value);

const load = async (path) => JSON.parse(await readFile(resolve(root, path), "utf8"));

export const loadSources = async () => ({
  catalog: await load("contracts/mobile/ui-catalog.json"),
  openapi: await load("apps/donor-api/api/openapi.json"),
  links: await load("contracts/mobile/links.json"),
});

// ---- Schema resolution -------------------------------------------------------------------

const refTarget = (openapi, ref) => {
  const match = /^#\/components\/schemas\/([^/]+)$/.exec(ref);
  const target = match && openapi.components?.schemas?.[match[1]];
  if (!target) throw new Error(`Unresolvable schema reference: ${ref}`);
  return target;
};

// Flattens $ref, allOf, oneOf and anyOf into concrete alternatives. `nullable` is set when a
// null branch (or a type list containing null) is present.
function alternatives(openapi, schema, state = { nullable: false }, depth = 0) {
  if (depth > 32) throw new Error("Schema nesting too deep");
  if (!isObject(schema)) return { alts: [], ...state };
  if (schema.$ref) return alternatives(openapi, refTarget(openapi, schema.$ref), state, depth + 1);
  const branches = schema.oneOf ?? schema.anyOf;
  if (branches) {
    const alts = branches.flatMap((branch) => alternatives(openapi, branch, state, depth + 1).alts);
    return { alts, nullable: state.nullable };
  }
  if (schema.allOf) {
    const parts = schema.allOf.map((part) => alternatives(openapi, part, state, depth + 1).alts);
    const properties = Object.assign({}, ...parts.flat().map((part) => part.properties ?? {}));
    const merged = parts.flat().find((part) => part.type) ?? {};
    return {
      alts: [{ ...merged, ...(Object.keys(properties).length ? { properties } : {}) }],
      nullable: state.nullable,
    };
  }
  const types = Array.isArray(schema.type) ? schema.type : [schema.type];
  if (types.includes("null")) state.nullable = true;
  const concrete = types.filter((type) => type !== "null");
  if (!concrete.length) return { alts: [], nullable: state.nullable };
  return { alts: concrete.map((type) => ({ ...schema, type })), nullable: state.nullable };
}

export function parsePointer(pointer) {
  if (typeof pointer !== "string" || !pointer.startsWith("/")) return null;
  const segments = pointer.slice(1).split("/");
  for (const segment of segments) {
    if (!segment || /~(?![01])/.test(segment) || segment.endsWith("~")) return null;
  }
  return segments.map((segment) => segment.replaceAll("~1", "/").replaceAll("~0", "~"));
}

// Resolves a pointer against the bound response schema. A segment resolves when at least one
// oneOf branch carries it, so the `ready` fields of DonorDashboard are reachable while the
// `preparing` branch simply renders nothing for them.
export function resolveBind(openapi, schemaName, pointer) {
  const segments = parsePointer(pointer);
  if (!segments) return { error: "BIND_SYNTAX" };
  let current = alternatives(openapi, refTarget(openapi, `#/components/schemas/${schemaName}`)).alts;
  for (const segment of segments) {
    const next = [];
    for (const alt of current) {
      if (alt.type === "object" && alt.properties && own(alt.properties, segment))
        next.push(...alternatives(openapi, alt.properties[segment]).alts);
    }
    if (!next.length) return { error: "BIND_UNRESOLVED" };
    current = next;
  }
  const kinds = new Set(current.map((alt) => alt.type));
  const kind = kinds.size === 1 ? [...kinds][0] : "mixed";
  return { kind, scalar: [...kinds].every((type) => scalarTypes.has(type)) };
}

// ---- Text policy -------------------------------------------------------------------------

const schemeLike =
  /(?:^|[^a-z0-9])(?:[a-z][a-z0-9+.-]{1,30}:\/\/|(?:https?|javascript|data|mailto|tel|sms|file|ftp|blob|vbscript):|www\.)/i;
const markdownLink = /\]\s*\(|\]\s*\[|^\s*\[[^\]]*\]:\s/m;

export function labelProblem(value) {
  if (typeof value !== "string") return "BAD_TYPE";
  if (!value.trim()) return "LABEL_EMPTY";
  if (/[<>]/.test(value) || /[\p{Cc}\p{Cf}\p{Zl}\p{Zp}]/u.test(value)) return "LABEL_CONTENT";
  if (schemeLike.test(value) || markdownLink.test(value)) return "LABEL_CONTENT";
  return null;
}

// ---- Spec validation ---------------------------------------------------------------------

export function validateSpec(spec, { catalog, openapi, links }) {
  const errors = [];
  const fail = (path, code, message) => {
    errors.push({ path, code, message });
  };
  const limits = catalog.limits;
  const allowedPaths = new Set(catalog.navigate.path_lists.flatMap((name) => links[name] ?? []));
  const bound = catalog.bind;
  let nodes = 0;
  let nodeLimitReported = false;

  const checkBind = (path, value, target) => {
    if (typeof value !== "string") return fail(path, "BAD_TYPE", "bind must be a JSON Pointer string");
    const result = resolveBind(openapi, bound.schema, value);
    if (result.error === "BIND_SYNTAX") return fail(path, "BIND_SYNTAX", "bind must be an RFC 6901 pointer");
    if (result.error) return fail(path, "BIND_UNRESOLVED", `${value} is not a field of ${bound.schema}`);
    if (target === "array") {
      if (result.kind !== "array") fail(path, "BIND_NOT_ARRAY", `${value} must be an array`);
    } else if (!result.scalar) fail(path, "BIND_NOT_SCALAR", `${value} must be a scalar`);
    else if (target === "integer" && result.kind !== "integer")
      fail(path, "BIND_NOT_INTEGER", `${value} must be an integer`);
  };

  const checkProp = (path, name, definition, value, component) => {
    const at = `${path}/${name}`;
    switch (definition.kind) {
      case "label": {
        const problem = labelProblem(value);
        if (problem) return fail(at, problem, `${name} is not an acceptable plain label`);
        const max = limits[definition.max];
        if ([...value].length > max) fail(at, "LABEL_TOO_LONG", `${name} exceeds ${max} characters`);
        return;
      }
      case "bind":
        return checkBind(at, value, definition.target);
      case "enum":
        if (typeof value !== "string") return fail(at, "BAD_TYPE", `${name} must be a string`);
        if (!definition.values.includes(value))
          fail(at, "BAD_ENUM", `${name} must be one of ${definition.values.join(", ")}`);
        return;
      case "format":
        if (typeof value !== "string") return fail(at, "BAD_TYPE", "format must be a string");
        if (!own(catalog.formats, value)) fail(at, "BAD_FORMAT", `Unknown metric format: ${value}`);
        return;
      case "caption":
        if (typeof value !== "string") return fail(at, "BAD_TYPE", "caption must be a string");
        if (!own(catalog.metric_captions, value)) fail(at, "BAD_CAPTION", `Unknown metric caption: ${value}`);
        return;
      case "navigate":
        if (typeof value !== "string") return fail(at, "BAD_TYPE", "navigate must be a path string");
        if (!allowedPaths.has(value)) fail(at, "NAV_FORBIDDEN", "navigate must be an approved in-app path");
        return;
      case "children":
        return checkChildren(at, value, definition, component);
    }
  };

  const checkChildren = (at, value, definition, component) => {
    if (!Array.isArray(value)) return fail(at, "BAD_TYPE", "children must be an array");
    if (!value.length) return fail(at, "CHILDREN_EMPTY", "children must not be empty");
    const max = limits[definition.max];
    if (value.length > max)
      fail(
        at,
        component === "MetricGrid" ? "GRID_TOO_LARGE" : "LIST_TOO_LARGE",
        `${component} holds at most ${max} children`,
      );
    value.forEach((child, index) => {
      const childPath = `${at}/${index}`;
      if (isObject(child) && typeof child.type === "string" && own(catalog.components, child.type)) {
        countNode(childPath);
        if (!definition.allowed.includes(child.type)) {
          fail(childPath, "CHILD_NOT_ALLOWED", `${child.type} is not allowed inside ${component}`);
          if (Array.isArray(child.children))
            fail(`${childPath}/children`, "TOO_DEEP", "Nesting is limited to two levels");
          return;
        }
        validateNode(child, childPath, 2);
      } else validateNode(child, childPath, 2);
    });
  };

  // Children already counted by the parent skip the second count.
  const counted = new Set();
  const countNode = (path) => {
    counted.add(path);
    nodes++;
    if (nodes > limits.max_nodes && !nodeLimitReported) {
      nodeLimitReported = true;
      fail(path, "TOO_MANY_NODES", `A spec holds at most ${limits.max_nodes} nodes`);
    }
  };

  const validateNode = (node, path, depth) => {
    if (!counted.has(path)) countNode(path);
    if (!isObject(node)) return fail(path, "NODE_INVALID", "A node must be an object");
    if (typeof node.type !== "string" || !node.type) return fail(path, "MISSING_TYPE", "A node needs a string type");
    // Unknown types are valid and render nothing; their contents are never inspected.
    if (!own(catalog.components, node.type)) return;
    const definition = catalog.components[node.type];
    if (depth > limits.max_depth) return fail(path, "TOO_DEEP", "Nesting is limited to two levels");
    for (const key of Object.keys(node)) {
      if (key !== "type" && !own(definition.props, key))
        fail(`${path}/${key}`, "UNKNOWN_PROP", `${node.type} has no prop ${key}`);
    }
    for (const name of definition.required ?? []) {
      if (!own(node, name)) fail(`${path}/${name}`, "MISSING_PROP", `${node.type} requires ${name}`);
    }
    for (const group of [definition.exactly_one_of].filter(Boolean)) {
      const present = group.filter((name) => own(node, name));
      if (present.length !== 1) fail(path, "ONE_OF", `${node.type} needs exactly one of ${group.join(", ")}`);
    }
    for (const [name, prop] of Object.entries(definition.props)) {
      if (own(node, name)) {
        checkProp(path, name, prop, node[name], node.type);
      }
    }
  };

  if (!Array.isArray(spec)) fail("", "SPEC_NOT_ARRAY", "A spec is an ordered array of blocks");
  else if (!spec.length) fail("", "SPEC_EMPTY", "A spec needs at least one block");
  else for (const [index, block] of spec.entries()) validateNode(block, `/${index}`, 1);
  return { ok: errors.length === 0, errors };
}

// ---- Catalog self-check ------------------------------------------------------------------

export function checkCatalog({ catalog, openapi, links }) {
  const problems = [];
  const note = (message) => problems.push(message);
  if (catalog.schema_version !== 1) note("Unsupported catalog schema_version");
  if (!Number.isInteger(catalog.catalog_version) || catalog.catalog_version < 1)
    note("catalog_version must be a positive integer");
  const operation = openapi.paths?.[catalog.bind.path]?.get;
  if (!operation) note(`Bound path ${catalog.bind.path} has no GET operation`);
  else if (operation.operationId !== catalog.bind.operation_id)
    note(`${catalog.bind.path} operationId is not ${catalog.bind.operation_id}`);
  else {
    const ref = operation.responses?.["200"]?.content?.["application/json"]?.schema?.$ref;
    if (ref !== `#/components/schemas/${catalog.bind.schema}`)
      note(`${catalog.bind.path} no longer returns ${catalog.bind.schema}`);
  }
  for (const name of [
    "max_depth",
    "max_nodes",
    "max_metrics_per_grid",
    "max_list_items",
    "max_title_chars",
    "max_body_chars",
  ]) {
    if (!Number.isInteger(catalog.limits[name]) || catalog.limits[name] < 1) note(`Invalid limit ${name}`);
  }
  for (const [name, format] of Object.entries(catalog.formats)) {
    if (format.bind_type !== "integer") note(`Format ${name} has an unsupported bind_type`);
  }
  const listed = new Set();
  for (const list of catalog.navigate.path_lists) {
    if (!Array.isArray(links[list])) note(`links.json has no list ${list}`);
    else for (const path of links[list]) listed.add(path);
  }
  for (const path of listed) {
    if (!own(catalog.navigate.labels, path)) note(`Navigate path ${path} has no label`);
  }
  for (const [path, label] of Object.entries(catalog.navigate.labels)) {
    if (!listed.has(path)) note(`Navigate label for ${path} has no matching path in links.json`);
    if (labelProblem(label)) note(`Navigate label for ${path} breaks the text policy`);
  }
  for (const [id, caption] of Object.entries(catalog.metric_captions)) {
    if (labelProblem(caption)) note(`Caption ${id} breaks the text policy`);
    else if ([...caption].length > catalog.limits.max_title_chars) note(`Caption ${id} is too long`);
  }
  for (const [name, definition] of Object.entries(catalog.components)) {
    const props = definition.props;
    for (const prop of [...(definition.required ?? []), ...(definition.exactly_one_of ?? [])]) {
      if (!own(props, prop)) note(`${name} references unknown prop ${prop}`);
    }
    for (const [propName, prop] of Object.entries(props)) {
      if (prop.kind === "label" && !own(catalog.limits, prop.max))
        note(`${name}.${propName} names unknown limit ${prop.max}`);
      if (prop.kind === "children") {
        if (!own(catalog.limits, prop.max)) note(`${name}.${propName} names unknown limit ${prop.max}`);
        for (const allowed of prop.allowed)
          if (!own(catalog.components, allowed)) note(`${name} allows unknown component ${allowed}`);
        if (prop.allowed.some((child) => own(catalog.components[child]?.props ?? {}, "children")))
          note(`${name} would nest containers beyond max_depth`);
      }
      if (prop.kind === "bind") {
        if (!["scalar", "integer", "array"].includes(prop.target)) note(`${name}.${propName} has unknown bind target`);
      }
    }
  }
  for (const [name, definition] of Object.entries(catalog.components)) {
    const literal = Object.entries(definition.props)
      .filter(([, prop]) => prop.kind === "label")
      .map(([key]) => key);
    const allowedLiteral = { Heading: ["title"], Callout: ["title", "body"] }[name] ?? [];
    if (literal.sort().join() !== allowedLiteral.sort().join())
      note(`${name} carries literal text it must not: ${literal.join(", ")}`);
  }
  return problems;
}

export async function checkVectors(sources, vectors) {
  const problems = [];
  for (const vector of vectors.vectors) {
    const result = validateSpec(vector.spec, sources);
    const codes = result.errors.map((error) => error.code);
    if (result.ok !== vector.valid) problems.push(`${vector.name}: expected valid=${vector.valid}`);
    else if (!vector.valid && JSON.stringify(codes) !== JSON.stringify(vector.codes))
      problems.push(`${vector.name}: expected ${JSON.stringify(vector.codes)}, got ${JSON.stringify(codes)}`);
  }
  return problems;
}

// ---- TypeScript spec types ---------------------------------------------------------------

const literalUnion = (values) => values.map((value) => JSON.stringify(value)).join(" | ");

export function catalogTypes(catalog, links) {
  const paths = [...new Set(catalog.navigate.path_lists.flatMap((name) => links[name]))];
  const lines = [
    "// Generated from contracts/mobile/ui-catalog.json; run `make generate` after changing it.",
    "",
    `export const uiCatalogVersion = ${catalog.catalog_version};`,
    "",
    "/** RFC 6901 pointer into the DonorDashboard response. */",
    "export type UiBind = string;",
    `export type UiMetricFormat = ${literalUnion(Object.keys(catalog.formats))};`,
    `export type UiMetricCaption = ${literalUnion(Object.keys(catalog.metric_captions))};`,
    `export type UiNavigatePath = ${literalUnion(paths)};`,
    "",
  ];
  const typeOf = (prop) => {
    switch (prop.kind) {
      case "label":
      case "bind":
        return "string";
      case "enum":
        return literalUnion(prop.values);
      case "format":
        return "UiMetricFormat";
      case "caption":
        return "UiMetricCaption";
      case "navigate":
        return "UiNavigatePath";
      case "children":
        return `Array<${prop.allowed.map((name) => `Ui${name}`).join(" | ")} | UiUnknownNode>`;
      default:
        throw new Error(`Unknown prop kind ${prop.kind}`);
    }
  };
  for (const [name, definition] of Object.entries(catalog.components)) {
    const group = definition.exactly_one_of ?? [];
    const required = new Set(definition.required ?? []);
    const base = Object.entries(definition.props).filter(([key]) => !group.includes(key));
    const render = (entries, chosen) =>
      entries
        .map(([key, prop]) => `${key}${required.has(key) || key === chosen ? "" : "?"}: ${typeOf(prop)}`)
        .join("; ");
    const variants = group.length
      ? group.map((choice) => render([...base, [choice, definition.props[choice]]], choice))
      : [render(base)];
    const members = variants.map((fields) => `{ type: ${JSON.stringify(name)}; ${fields} }`);
    lines.push(`export type Ui${name} = ${members.join(" | ")};`);
  }
  lines.push(
    "",
    "/** A node whose type this catalog version does not know. Renderers draw nothing for it. */",
    "export interface UiUnknownNode {",
    "  type: string;",
    "  [key: string]: unknown;",
    "}",
    "",
    `export type UiBlock = ${[...Object.keys(catalog.components).map((name) => `Ui${name}`), "UiUnknownNode"].join(" | ")};`,
    "export type UiSpec = UiBlock[];",
    "",
  );
  return lines.join("\n");
}

if (process.argv[1] && pathToFileURL(resolve(process.argv[1])).href === import.meta.url) {
  try {
    if (process.argv[2] !== "check") throw new Error("Usage: node scripts/ui-catalog.mjs check");
    const sources = await loadSources();
    const vectors = await load("contracts/mobile/ui-catalog-vectors.json");
    const problems = [...checkCatalog(sources), ...(await checkVectors(sources, vectors))];
    if (problems.length) throw new Error(problems.join("\n"));
    console.log(`UI catalog v${sources.catalog.catalog_version}: ${vectors.vectors.length} vectors pass`);
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
