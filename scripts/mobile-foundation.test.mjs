import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdtemp, readdir, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { bundle, nativeContract, swiftContract, tokenSources } from "./mobile-foundation.mjs";

const original = JSON.parse(await readFile(new URL("../apps/donor-api/api/openapi.json", import.meta.url), "utf8"));

test("mobile contract preserves native operations and excludes privileged and browser-only writes", () => {
  const mobile = nativeContract(original);
  for (const path of [
    "/donor/claim",
    "/donor/refresh",
    "/donor/me/profile",
    "/donor/me/dashboard",
    "/donor/me/push-device",
    "/donor/partner/sign-in",
    "/donor/partner/accept-invite",
    "/donor/partner/overview",
    "/donor/partner/push-device",
    "/donor/partner/application-status",
    "/donor/partner/applications",
    "/donor/me/events/{event_id}/calendar.ics",
    "/donor/partner/users",
  ]) {
    assert.deepEqual(mobile.paths[path], original.paths[path]);
  }
  assert.equal(mobile.paths["/donor/public/partner-apply"], undefined);
  for (const path of Object.keys(mobile.paths)) {
    assert.ok(
      !path.startsWith("/donor/staff/") && !path.startsWith("/donor/checkouts") && !path.includes("stripe/webhook"),
      path,
    );
  }
  assert.equal(mobile.components.schemas.StaffView, undefined);
  assert.deepEqual(mobile.components.schemas.Profile, original.components.schemas.Profile);
  assert.deepEqual(mobile.components.schemas.NativeSession, original.components.schemas.NativeSession);
});

test("every mobile reference resolves, including transitive response schemas", () => {
  const mobile = nativeContract(original);
  let count = 0;
  const visit = (value) => {
    if (!value || typeof value !== "object") return;
    if (value.$ref) {
      const parts = value.$ref.slice(2).split("/");
      assert.ok(
        parts.reduce((node, part) => node?.[part], mobile),
        value.$ref,
      );
      count++;
    }
    for (const child of Object.values(value)) visit(child);
  };
  visit(mobile);
  assert.ok(count > 50);
  assert.deepEqual(
    mobile.paths["/donor/claim"].post.responses["200"],
    original.paths["/donor/claim"].post.responses["200"],
  );
});

test("mobile initiative spend and event journeys retain their public reads", () => {
  const mobile = nativeContract(original);
  for (const path of [
    "/initiatives",
    "/initiatives/{slug}",
    "/initiatives/{initiative_id}/spend",
    "/events/{event_id}",
    "/events/{event_id}/calendar.ics",
  ]) {
    assert.deepEqual(mobile.paths[path]?.get, original.paths[path].get, path);
  }
  assert.equal(mobile.paths["/health/ready"], undefined);
  assert.equal(mobile.paths["/donor/public/spend"], undefined);
});

test("token generators preserve semantic names and reject invalid numeric values", () => {
  const input = {
    schema_version: 1,
    color: { "brand-ink": "#c8340f" },
    radius: { card: 18 },
    space: { 1: 4.5 },
    text: { "2xl": 28 },
  };
  const output = tokenSources(input);
  assert.ok(output["DADTokens.swift"].includes('"brand-ink": "#c8340f"'));
  assert.ok(output["DADTokens.kt"].includes('"2xl" to 28.0'));
  assert.ok(output["tokens.ts"].includes('"1": 4.5'));
  assert.ok(output["DADTokens.kt"].includes('"1" to 4.5,'));
  assert.throws(() => tokenSources({ ...input, text: { md: -1 } }), /Invalid design token/);
});

test("token generators reject extra groups that are not valid identifiers or hold invalid values", () => {
  const input = { schema_version: 1, color: { ink: "#000" }, radius: { card: 1 }, space: { 1: 4 }, text: { md: 16 } };
  assert.throws(() => tokenSources({ ...input, 'bad"group': { a: 1 } }), /Invalid token group name/);
  assert.throws(() => tokenSources({ ...input, motion: { fast: -1 } }), /Invalid design token in motion/);
  assert.ok(tokenSources({ ...input, motion: { fast: 120 } })["DADTokens.kt"].includes("val motion:"));
});

test("mobile contract carries only the security schemes its operations use", () => {
  const mobile = nativeContract(original);
  const used = new Set(
    Object.values(mobile.paths).flatMap((item) =>
      Object.values(item)
        .filter((operation) => !Array.isArray(operation))
        .flatMap((operation) => (operation.security ?? []).flatMap((requirement) => Object.keys(requirement))),
    ),
  );
  assert.deepEqual(Object.keys(mobile.components.securitySchemes).sort(), [...used].sort());
  assert.equal("staffCookieAuth" in mobile.components.securitySchemes, false);
});

test("bundle writes a manifest whose digests match the emitted files", async () => {
  const directory = await mkdtemp(join(tmpdir(), "mobile-bundle-"));
  try {
    const manifest = await bundle(directory);
    assert.match(manifest.source_revision, /^[a-f0-9]{40}$/);
    const emitted = (await readdir(directory)).filter((name) => name !== "manifest.json");
    assert.deepEqual(Object.keys(manifest.files).sort(), emitted.sort());
    for (const name of ["ui-catalog.json", "ui-catalog-vectors.json"]) assert.ok(name in manifest.files, name);
    for (const [name, digest] of Object.entries(manifest.files)) {
      const contents = await readFile(join(directory, name));
      assert.equal(digest, createHash("sha256").update(contents).digest("hex"), name);
    }
    assert.deepEqual(JSON.parse(await readFile(join(directory, "manifest.json"), "utf8")), manifest);
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});

test("Swift nullable input preserves object constraints and admits null enum values", () => {
  const mobile = nativeContract(original);
  const before = structuredClone(mobile);
  const swift = swiftContract(mobile);
  assert.deepEqual(mobile, before);
  const rank = swift.components.schemas.DonorDashboard.oneOf[1].properties.rank;
  const { type: _, ...ranking } = mobile.components.schemas.Ranking;
  assert.deepEqual(rank, {
    ...ranking,
    description: mobile.components.schemas.DonorDashboard.oneOf[1].properties.rank.description,
    type: ["object", "null"],
  });
  const status = swift.components.schemas.EventView.properties.reservation_status;
  assert.deepEqual(status.type, ["string", "null"]);
  assert.deepEqual(status.enum, ["RESERVED", "WAITLISTED", null]);
  assert.deepEqual(swift.components.schemas.Profile, mobile.components.schemas.Profile);
  assert.deepEqual(swift.components.schemas.PaymentUpdate.oneOf[1].properties.url, {
    $ref: "#/components/schemas/DADLiteralNull",
  });
  assert.equal(swift.components.schemas.DADLiteralNull["x-dad-literal-null"], true);
  const visit = (value) => {
    if (!value || typeof value !== "object") return;
    assert.ok(!value.anyOf?.some((branch) => branch.type === "null"));
    Object.values(value).forEach(visit);
  };
  visit(swift);
  assert.throws(() => swiftContract({ anyOf: [{ type: "null" }, { type: "string" }, { type: "integer" }] }));
});
