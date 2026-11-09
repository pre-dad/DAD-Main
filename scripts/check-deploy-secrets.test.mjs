import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("../", import.meta.url));
const source = readFileSync(path.join(root, "docker-compose.prod.yml"), "utf8");

function render(overrides = {}) {
  const work = mkdtempSync(path.join(os.tmpdir(), "dad-secret-boundary-"));
  const values = Object.fromEntries([...source.matchAll(/\$\{([A-Z][A-Z0-9_]*)/g)].map((m) => [m[1], ""]));
  for (const name of Object.keys(values)) {
    if (name.endsWith("DATABASE_URL")) values[name] = `postgresql://${name.toLowerCase()}@postgres/check`;
  }
  Object.assign(values, {
    IMAGE_TAG: "secrets-check",
    POSTGRES_USER: "check",
    POSTGRES_DB: "check",
    POSTGRES_PASSWORD: "canary-database",
    DONOR_PUBLIC_ORIGIN: "https://public.test",
    DONOR_STAFF_ORIGIN: "https://staff.test",
    INGRESS_MODE: "direct",
    STRIPE_SECRET_KEY: "canary-stripe",
    STRIPE_WEBHOOK_SECRET: "canary-webhook",
    DONOR_CONTENT_ENCRYPTION_KEY: "canary-content",
    DONOR_EMAIL_ENCRYPTION_KEY: "canary-email",
    DONOR_SMTP_HOST: "smtp.fixture",
    DONOR_SMTP_USERNAME: "canary-mail-user",
    DONOR_SMTP_PASSWORD: "canary-mail-password",
    DONOR_EMAIL_FROM: "mail@example.test",
    DONOR_PUSH_APNS_KEY_FILE: "/run/private/apns.p8",
    DONOR_PUSH_APNS_KEY_ID: "fixture-key",
    DONOR_PUSH_APNS_TEAM_ID: "fixture-team",
    DONOR_PUSH_APNS_TOPIC: "org.example.fixture",
    DONOR_PUSH_FCM_KEY_FILE: "/run/private/fcm.json",
    DONOR_EVENT_CHECKIN_SECRET: "canary-checkin",
    DONOR_FOUNDING_CLAIM_KEY: "canary-founding",
    DONOR_SMS_AUTH_TOKEN: "canary-sms",
    DONOR_OAUTH_APPLE_CLIENT_ID: "fixture-apple-web",
    DONOR_OAUTH_APPLE_CLIENT_SECRET: "canary-apple-oauth",
    DONOR_OAUTH_GOOGLE_CLIENT_ID: "fixture-google-web",
    DONOR_OAUTH_GOOGLE_CLIENT_SECRET: "canary-google-oauth",
    DONOR_OIDC_REQUIRE_NONCE: "true",
    UNRELATED_SECRET: "must-never-reach-a-process",
    ...overrides,
  });
  const envFile = path.join(work, "fixture.env");
  writeFileSync(
    envFile,
    Object.entries(values)
      .map(([k, v]) => `${k}=${v}`)
      .join("\n"),
  );
  try {
    return JSON.parse(
      execFileSync(
        "docker",
        [
          "compose",
          "--env-file",
          envFile,
          "-f",
          "docker-compose.prod.yml",
          "--profile",
          "workers",
          "--profile",
          "maintenance",
          "config",
          "--format",
          "json",
        ],
        { cwd: root, env: { ...process.env, ...values }, encoding: "utf8", timeout: 30_000 },
      ),
    ).services;
  } finally {
    rmSync(work, { recursive: true, force: true });
  }
}

test("rendered deployment gives each process only its credential capabilities", () => {
  const services = render();
  for (const service of Object.values(services)) assert.equal(service.environment?.UNRELATED_SECRET, undefined);
  for (const [name, service] of Object.entries(services)) {
    for (const [key, value] of Object.entries({
      DONOR_OAUTH_APPLE_CLIENT_ID: "fixture-apple-web",
      DONOR_OAUTH_APPLE_CLIENT_SECRET: "canary-apple-oauth",
      DONOR_OAUTH_GOOGLE_CLIENT_ID: "fixture-google-web",
      DONOR_OAUTH_GOOGLE_CLIENT_SECRET: "canary-google-oauth",
    }))
      assert.equal(service.environment?.[key], name === "web" ? value : undefined, `${name}:${key}`);
  }
  const api = services.api.environment;
  assert.equal(api.DONOR_OIDC_REQUIRE_NONCE, "true");
  assert.equal(api.STRIPE_SECRET_KEY, "canary-stripe");
  assert.equal(api.DONOR_EMAIL_ENCRYPTION_KEY, "canary-email");
  assert.equal(api.DONOR_FOUNDING_CLAIM_KEY, "canary-founding");
  assert.equal(api.DONOR_SMTP_PASSWORD, undefined);
  assert.equal(api.DONOR_SMTP_USERNAME, undefined);
  assert.equal(api.DONOR_PUSH_APNS_KEY_FILE, undefined);
  assert.equal(api.DONOR_PUSH_FCM_KEY_FILE, undefined);
  assert.equal(api.DONOR_PUSH_PLATFORMS, "IOS,ANDROID");
  for (const name of ["monitor", "projection-worker", "staff", "migrate", "provision", "web", "nginx"]) {
    const env = services[name].environment ?? {};
    for (const key of [
      "STRIPE_SECRET_KEY",
      "STRIPE_WEBHOOK_SECRET",
      "DONOR_SMTP_PASSWORD",
      "DONOR_EMAIL_ENCRYPTION_KEY",
      "DONOR_CONTENT_ENCRYPTION_KEY",
      "DONOR_SMS_AUTH_TOKEN",
      "DONOR_EVENT_CHECKIN_SECRET",
      "DONOR_FOUNDING_CLAIM_KEY",
      "DONOR_PUSH_APNS_KEY_FILE",
      "DONOR_PUSH_FCM_KEY_FILE",
    ])
      assert.equal(env[key], undefined, `${name} unexpectedly received ${key}`);
  }
  for (const name of ["login-email-worker", "email-change-worker", "message-worker"]) {
    const env = services[name].environment;
    assert.equal(env.DONOR_SMTP_PASSWORD, "canary-mail-password");
    assert.equal(env.STRIPE_SECRET_KEY, undefined);
    assert.equal(env.DONOR_CONTENT_ENCRYPTION_KEY, undefined);
  }
  assert.equal(services["push-worker"].environment.DONOR_PUSH_APNS_KEY_FILE, "/run/private/apns.p8");
  assert.equal(services["push-worker"].environment.DONOR_PUSH_FCM_KEY_FILE, "/run/private/fcm.json");
  assert.equal(services["push-worker"].environment.STRIPE_SECRET_KEY, undefined);
  assert.equal(services.reconcile.environment.STRIPE_SECRET_KEY, "canary-stripe");
  assert.equal(services.rekey.environment.DONOR_CONTENT_ENCRYPTION_KEY, "canary-content");
  assert.equal(services.rekey.environment.STRIPE_SECRET_KEY, undefined);
});

test("registration can be disabled independently of configured delivery credentials", () => {
  assert.equal(render({ DONOR_PUSH_PLATFORMS: "none" }).api.environment.DONOR_PUSH_PLATFORMS, "none");
});
