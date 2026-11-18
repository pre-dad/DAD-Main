#!/usr/bin/env node
// Fails on any high or critical npm advisory in the package in the current
// directory, except one listed in npm-audit-exceptions.json that has not
// expired and is absent from the production dependency tree. An exception
// whose advisory no longer appears also fails, so it is removed once a fix
// ships rather than lingering to excuse a later one.
import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const blocking = new Set(["high", "critical"]);

/** The blocking advisories in an `npm audit --json` report, by URL. */
export function advisories(report) {
  const found = new Map();
  for (const vulnerability of Object.values(report.vulnerabilities ?? {})) {
    for (const via of vulnerability.via) {
      if (typeof via === "object" && blocking.has(via.severity)) found.set(via.url, via.title);
    }
  }
  return found;
}

/** The reasons the audit fails; none means it passes. */
export function problems({ full, production, exceptions, today }) {
  const out = [];
  const shipped = advisories(production);
  const excepted = new Map(exceptions.map((e) => [e.advisory, e]));
  for (const [url, title] of advisories(full)) {
    const exception = excepted.get(url);
    if (shipped.has(url)) out.push(`${url} (${title}) reaches production dependencies; it cannot be excepted`);
    else if (!exception) out.push(`${url} (${title})`);
    else if (exception.expires < today) out.push(`${url} (${title}): its exception expired on ${exception.expires}`);
  }
  for (const exception of exceptions) {
    if (!advisories(full).has(exception.advisory))
      out.push(`${exception.advisory} no longer appears; remove its exception`);
  }
  return out;
}

function audit(...args) {
  try {
    return JSON.parse(execFileSync("npm", ["audit", "--json", ...args], { encoding: "utf8" }));
  } catch (error) {
    // npm audit exits non-zero whenever it finds anything, with the report on stdout.
    if (error.stdout) return JSON.parse(error.stdout);
    throw error;
  }
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const file = new URL("./npm-audit-exceptions.json", import.meta.url);
  const { name } = JSON.parse(readFileSync("package.json", "utf8"));
  const exceptions = JSON.parse(readFileSync(file, "utf8")).filter((e) => e.packages.includes(name));
  const found = problems({
    full: audit(),
    production: audit("--omit=dev"),
    exceptions,
    today: new Date().toISOString().slice(0, 10),
  });
  if (found.length) {
    console.error(`npm audit failed for ${name}:\n  ${found.join("\n  ")}`);
    process.exit(1);
  }
  console.log(
    `npm audit passed for ${name}${exceptions.length ? ` with ${exceptions.length} dated exception(s)` : ""}`,
  );
}
