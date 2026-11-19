import { readFile, writeFile } from "node:fs/promises";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { nativeContract } from "./mobile-foundation.mjs";

const root = new URL("../", import.meta.url);
const target = new URL("deploy/nginx/conf/native-routes.conf", root);
const methods = new Set(["get", "post", "put", "patch", "delete"]);
const categories = new Set(["read", "auth", "visitor", "account", "analytics"]);
const escapePattern = (text) => text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

export function nativeRules(source) {
  const rules = [];
  for (const [path, item] of Object.entries(nativeContract(source).paths)) {
    if (path.startsWith("/donor/staff")) throw new Error("Staff operations cannot use native ingress");
    for (const [method, operation] of Object.entries(item)) {
      if (!methods.has(method)) continue;
      const category = operation["x-rate-limit"] ?? "read";
      if (!categories.has(category)) throw new Error(`Unsupported native rate-limit class: ${category}`);
      const pattern = path
        .split(/(\{[^{}]+\})/)
        .map((part) => (part.startsWith("{") ? "[^/]+" : escapePattern(part)))
        .join("");
      rules.push({ pattern: `^${method.toUpperCase()}:${pattern}$`, category });
      if (method === "get") rules.push({ pattern: `^HEAD:${pattern}$`, category });
    }
  }
  return rules;
}

export function render(source) {
  const rules = nativeRules(source);
  const lines = rules.map(({ pattern, category }) => `    ${JSON.stringify(`~${pattern}`)} ${category};`);
  const paths = new Map();
  for (const { pattern } of rules) {
    const [, method, path] = pattern.match(/^\^([A-Z]+):(.*)$/);
    if (!paths.has(path)) paths.set(path, []);
    paths.get(path).push(method);
  }
  const allowed = [...paths].map(([path, verbs]) => `    ${JSON.stringify(`~^${path}`)} "${verbs.join(", ")}";`);
  return (
    "# Generated from the canonical native contract; run make generate.\n" +
    'map "$request_method:$uri" $native_operation_class {\n    default forbidden;\n' +
    lines.join("\n") +
    "\n}\n" +
    'map $uri $native_allowed_methods {\n    default "";\n' +
    allowed.join("\n") +
    "\n}\n"
  );
}

if (resolve(process.argv[1] ?? "") === fileURLToPath(import.meta.url)) {
  const source = JSON.parse(await readFile(new URL("apps/donor-api/api/openapi.json", root), "utf8"));
  const expected = render(source);
  if (process.argv[2] === "--check") {
    if ((await readFile(target, "utf8")) !== expected)
      throw new Error("Native ingress routes differ from the canonical contract; run make generate");
  } else if (process.argv.length === 2) {
    await writeFile(target, expected);
  } else {
    throw new Error("Usage: node scripts/native-ingress.mjs [--check]");
  }
}
