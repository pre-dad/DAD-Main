import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { nativeRules } from "./native-ingress.mjs";

const source = JSON.parse(await readFile(new URL("../apps/donor-api/api/openapi.json", import.meta.url)));
const rules = nativeRules(source);
const permits = (method, path) => rules.some(({ pattern }) => new RegExp(pattern).test(`${method}:${path}`));
test("native ingress admits supported authentication and donor operations", () => {
  for (const [method, path] of [
    ["POST", "/donor/login"],
    ["GET", "/donor/me/profile"],
    ["GET", "/donor/partner/overview"],
    ["GET", "/health/live"],
  ])
    assert.equal(permits(method, path), true, `${method} ${path}`);
});
test("native ingress excludes staff, browser checkout and unlisted methods", () => {
  for (const [method, path] of [
    ["POST", "/donor/staff/sign-in"],
    ["GET", "/donor/staff/me"],
    ["POST", "/donor/checkouts"],
    ["GET", "/api/donor/me/profile"],
    ["DELETE", "/donor/login"],
    ["GET", "/health/ready"],
  ])
    assert.equal(permits(method, path), false, `${method} ${path}`);
});
test("native path parameters cannot cross a path segment or regex boundary", () => {
  assert.equal(permits("GET", "/events/example/calendar.ics"), true);
  for (const path of [
    "/events/example/calendarXics",
    "/events/example/extra/calendar.ics",
    "/events/example/calendar.ics/extra",
  ])
    assert.equal(permits("GET", path), false, path);
});

test("native HEAD inherits the admitted GET operation", () => {
  assert.equal(permits("HEAD", "/health/live"), true);
  assert.equal(permits("HEAD", "/donor/staff/me"), false);
});
