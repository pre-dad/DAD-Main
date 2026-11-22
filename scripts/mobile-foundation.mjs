import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import { dirname, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { catalogTypes } from "./ui-catalog.mjs";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const json = (value) => `${JSON.stringify(value, null, 2)}\n`;
export const digest = (value) => createHash("sha256").update(value).digest("hex");

const bearer = (operation) =>
  operation.security?.some(
    (requirement) =>
      "bearerAuth" in requirement || "partnerBearerAuth" in requirement || "applicationBearerAuth" in requirement,
  );

const publicReads = new Set([
  "/health/live",
  "/initiatives",
  "/initiatives/{slug}",
  "/initiatives/{initiative_id}/spend",
  "/events/{event_id}",
  "/events/{event_id}/calendar.ics",
]);

export function nativeContract(source) {
  const paths = {};
  for (const [path, item] of Object.entries(source.paths)) {
    const operations = {};
    for (const [method, operation] of Object.entries(item)) {
      if (!["get", "post", "put", "patch", "delete"].includes(method)) continue;
      if (
        operation["x-clients"]?.includes("native") ||
        (method === "get" && (bearer(operation) || publicReads.has(path)))
      ) {
        operations[method] = structuredClone(operation);
      }
    }
    if (Object.keys(operations).length) {
      if (item.parameters) operations.parameters = structuredClone(item.parameters);
      paths[path] = operations;
    }
  }
  const components = { securitySchemes: {} };
  const visit = (value) => {
    if (!value || typeof value !== "object") return;
    if (value.$ref) {
      const match = value.$ref.match(/^#\/components\/([^/]+)\/([^/]+)$/);
      if (!match) throw new Error(`Unsupported contract reference: ${value.$ref}`);
      const [, group, name] = match;
      if (!components[group]?.[name]) {
        const target = source.components[group]?.[name];
        if (!target) throw new Error(`Missing contract reference: ${value.$ref}`);
        components[group] ??= {};
        components[group][name] = structuredClone(target);
        visit(target);
      }
    }
    for (const child of Object.values(value)) visit(child);
  };
  visit(paths);
  for (const item of Object.values(paths))
    for (const operation of Object.values(item).filter((value) => !Array.isArray(value))) {
      for (const requirement of operation.security ?? source.security ?? [])
        for (const name of Object.keys(requirement))
          components.securitySchemes[name] = structuredClone(source.components.securitySchemes[name]);
    }
  return {
    openapi: source.openapi,
    info: { ...source.info, title: "DAD donor mobile API" },
    servers: [source.servers[0]],
    paths,
    components,
  };
}

// Apple's generator understands nullable types, but skips standalone null
// branches. Keep the wire contract intact and adapt only its Swift input.
export function swiftContract(source) {
  const document = structuredClone(source);
  let literalNull = false;
  const visit = (value) => {
    if (!value || typeof value !== "object") return value;
    if (Array.isArray(value)) return value.map(visit);
    if (value.type === "null") {
      literalNull = true;
      return { $ref: "#/components/schemas/DADLiteralNull" };
    }
    if (value.anyOf?.some((branch) => branch.type === "null")) {
      const branches = value.anyOf.filter((branch) => branch.type !== "null");
      if (value.anyOf.length !== 2 || branches.length !== 1)
        throw new Error("Swift nullable adaptation requires exactly one value branch");
      let branch = structuredClone(branches[0]);
      if (branch.$ref) {
        const match = branch.$ref.match(/^#\/components\/schemas\/([^/]+)$/);
        if (!match || !document.components.schemas[match[1]]) throw new Error("Unknown Swift nullable reference");
        branch = structuredClone(document.components.schemas[match[1]]);
      }
      if (typeof branch.type !== "string" || branch.type === "null" || branch.const !== undefined)
        throw new Error("Unsupported Swift nullable value branch");
      const { anyOf: _, ...annotations } = value;
      value = { ...branch, ...annotations, type: [branch.type, "null"] };
      if (branch.enum) value.enum = [...branch.enum, null];
    }
    return Object.fromEntries(Object.entries(value).map(([key, child]) => [key, visit(child)]));
  };
  const result = visit(document);
  if (literalNull) {
    // A supported placeholder lets the pinned generator reach its type
    // override. DADNullValue's Codable implementation enforces literal null.
    result.components.schemas.DADLiteralNull = { type: "string", "x-dad-literal-null": true };
  }
  return result;
}

const header = "// Generated from DAD-Main design/tokens.json; do not edit.\n";

const validToken = (group, value) =>
  group === "color"
    ? typeof value === "string" && /^(#[0-9a-f]{3,8}|rgba\([\d.,]+\))$/i.test(value)
    : typeof value === "number" && Number.isFinite(value) && value >= 0;

// Each platform declares one map per token group, string-valued for colors and numeric otherwise.
function tokenMaps(tokens, declare, entry, close) {
  const groups = Object.entries(tokens).map(([group, values]) =>
    [declare(group, group === "color"), ...Object.entries(values).map(([key, value]) => entry(key, value)), close].join(
      "\n",
    ),
  );
  return groups.join("\n");
}

const swiftTokens = (tokens) =>
  `${header}public enum DADTokens {\n${tokenMaps(
    tokens,
    (group, color) => `    public static let ${group}: [String: ${color ? "String" : "Double"}] = [`,
    (key, value) => `        ${JSON.stringify(key)}: ${JSON.stringify(value)},`,
    "    ]",
  )}\n}\n`;

const kotlinNumber = (value) => (Number.isInteger(value) ? `${value}.0` : String(value));

const kotlinTokens = (tokens) =>
  `${header}package org.dad.foundation.generated\n\nobject DADTokens {\n${tokenMaps(
    tokens,
    (group, color) => `    val ${group}: Map<String, ${color ? "String" : "Double"}> = mapOf(`,
    (key, value) =>
      `        ${JSON.stringify(key)} to ${typeof value === "string" ? JSON.stringify(value) : kotlinNumber(value)},`,
    "    )",
  )}\n}\n`;

export function tokenSources(document) {
  if (document.schema_version !== 1) throw new Error("Unsupported design token schema");
  const { schema_version: _, ...tokens } = document;
  for (const group of ["color", "radius", "space", "text"]) {
    if (!tokens[group] || typeof tokens[group] !== "object") throw new Error(`Missing token group: ${group}`);
  }
  for (const [group, values] of Object.entries(tokens)) {
    if (!/^[a-z][A-Za-z0-9]*$/.test(group)) throw new Error(`Invalid token group name: ${group}`);
    if (!values || typeof values !== "object") throw new Error(`Invalid design token in ${group}`);
    if (!Object.values(values).every((value) => validToken(group, value))) {
      throw new Error(`Invalid design token in ${group}`);
    }
  }
  return {
    "tokens.ts": `${header}export const tokens = ${JSON.stringify(tokens, null, 2)} as const;\n`,
    "DADTokens.swift": swiftTokens(tokens),
    "DADTokens.kt": kotlinTokens(tokens),
  };
}

export async function generateWeb(check = false) {
  const tokens = tokenSources(JSON.parse(await readFile(resolve(root, "design/tokens.json"), "utf8")));
  const target = resolve(root, "apps/web/design/tokens.ts");
  if (check) {
    if ((await readFile(target, "utf8")) !== tokens["tokens.ts"])
      throw new Error("Web design tokens differ; run make generate-mobile");
  } else await writeFile(target, tokens["tokens.ts"]);

  const catalog = JSON.parse(await readFile(resolve(root, "contracts/mobile/ui-catalog.json"), "utf8"));
  const links = JSON.parse(await readFile(resolve(root, "contracts/mobile/links.json"), "utf8"));
  const types = catalogTypes(catalog, links);
  const typesTarget = resolve(root, "apps/web/lib/ui-catalog.d.ts");
  if (check) {
    if ((await readFile(typesTarget, "utf8")) !== types)
      throw new Error("UI catalog types differ; run make generate-mobile");
  } else await writeFile(typesTarget, types);
}

export async function bundle(output) {
  const version = JSON.parse(await readFile(resolve(root, "contracts/mobile/version.json"), "utf8")).version;
  if (!/^\d+\.\d+\.\d+$/.test(version)) throw new Error("Invalid mobile foundation version");
  const openapi = await readFile(resolve(root, "apps/donor-api/api/openapi.json"), "utf8");
  const tokens = await readFile(resolve(root, "design/tokens.json"), "utf8");
  const links = await readFile(resolve(root, "contracts/mobile/links.json"), "utf8");
  const catalog = await readFile(resolve(root, "contracts/mobile/ui-catalog.json"), "utf8");
  const vectors = await readFile(resolve(root, "contracts/mobile/ui-catalog-vectors.json"), "utf8");
  const mobile = nativeContract(JSON.parse(openapi));
  const files = {
    "openapi.json": openapi,
    "mobile.openapi.json": json(mobile),
    "swift.openapi.json": json(swiftContract(mobile)),
    "DADNullValue.swift": await readFile(
      resolve(root, "contracts/mobile/swift/Sources/DADAPI/DADNullValue.swift"),
      "utf8",
    ),
    "tokens.json": tokens,
    "links.json": links,
    "ui-catalog.json": catalog,
    "ui-catalog-vectors.json": vectors,
    ...tokenSources(JSON.parse(tokens)),
  };
  const revision = execFileSync("git", ["rev-parse", "HEAD"], { cwd: root, encoding: "utf8" }).trim();
  const manifest = {
    version,
    source_repository: "pre-dad/DAD-Main",
    source_revision: revision,
    files: Object.fromEntries(Object.entries(files).map(([name, contents]) => [name, digest(contents)])),
  };
  await mkdir(output, { recursive: true });
  for (const [name, contents] of Object.entries({ ...files, "manifest.json": json(manifest) }))
    await writeFile(resolve(output, name), contents);
  return manifest;
}

if (process.argv[1] && pathToFileURL(resolve(process.argv[1])).href === import.meta.url) {
  try {
    const [command, output] = process.argv.slice(2);
    if (command === "generate") await generateWeb();
    else if (command === "check") await generateWeb(true);
    else if (command === "bundle" && output) console.log(json(await bundle(resolve(output))));
    else throw new Error("Usage: node scripts/mobile-foundation.mjs {generate|check|bundle OUTPUT}");
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
