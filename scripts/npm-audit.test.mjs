import assert from "node:assert/strict";
import test from "node:test";
import { problems } from "./npm-audit.mjs";

const url = "https://github.com/advisories/GHSA-test";
const report = (...vias) => ({ vulnerabilities: { pkg: { via: vias } } });
const advisory = { url, title: "ReDoS", severity: "high" };
const clean = report();
const exception = { advisory: url, packages: ["dad-web"], reason: "dev only", expires: "2027-02-18" };
const today = "2026-11-18";

test("fails on a blocking advisory nobody excepted, and ignores moderate ones and transitive mentions", () => {
  assert.deepEqual(problems({ full: report(advisory), production: clean, exceptions: [], today }), [`${url} (ReDoS)`]);
  const moderate = report({ ...advisory, severity: "moderate" }, "other-package");
  assert.deepEqual(problems({ full: moderate, production: clean, exceptions: [], today }), []);
});

test("an exception covers a development-only advisory until it expires", () => {
  assert.deepEqual(problems({ full: report(advisory), production: clean, exceptions: [exception], today }), []);
  assert.deepEqual(
    problems({ full: report(advisory), production: clean, exceptions: [exception], today: "2027-02-18" }),
    [],
  );
  assert.match(
    problems({ full: report(advisory), production: clean, exceptions: [exception], today: "2027-02-19" })[0],
    /expired on 2027-02-18/,
  );
});

test("an exception never covers an advisory in production dependencies", () => {
  const found = problems({ full: report(advisory), production: report(advisory), exceptions: [exception], today });
  assert.match(found[0], /reaches production dependencies/);
});

test("an exception whose advisory is gone fails until it is removed", () => {
  assert.deepEqual(problems({ full: clean, production: clean, exceptions: [exception], today }), [
    `${url} no longer appears; remove its exception`,
  ]);
});
