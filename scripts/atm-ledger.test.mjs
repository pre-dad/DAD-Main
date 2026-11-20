import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { afterEach, test } from "node:test";
import { migrationHistory, request, token } from "./atm-ledger.mjs";

const original = process.env.ATM_LEDGER_URL;
afterEach(() => {
  if (original === undefined) delete process.env.ATM_LEDGER_URL;
  else process.env.ATM_LEDGER_URL = original;
});

test("client refuses insecure or credential-bearing origins before requesting credentials", async () => {
  for (const url of [
    "http://localhost",
    "https://user:secret@example.org",
    "https://example.org/path",
    "https://example.org/?token=secret",
  ]) {
    process.env.ATM_LEDGER_URL = url;
    let called = false;
    await assert.rejects(
      request("/v1/ledger", "GET", undefined, {
        token: async () => {
          called = true;
          return "fixture";
        },
      }),
      /HTTPS ledger origin/,
    );
    assert.equal(called, false);
  }
});

test("client retries transient errors with the identical publication and refuses stale revisions", async () => {
  process.env.ATM_LEDGER_URL = "https://ledger.example.org";
  const bodies = [];
  const result = await request(
    "/v1/publications",
    "POST",
    { expectedVersion: 3 },
    {
      token: async () => "fixture",
      fetch: async (url, options) => {
        assert.equal(url.href, "https://ledger.example.org/v1/publications");
        assert.equal(options.redirect, "error");
        bodies.push(options.body);
        return Response.json(bodies.length === 1 ? { detail: "Archive unavailable" } : { version: 4 }, {
          status: bodies.length === 1 ? 503 : 200,
        });
      },
    },
  );
  assert.deepEqual(result, { version: 4 });
  assert.deepEqual(bodies, ['{"expectedVersion":3}', '{"expectedVersion":3}']);
  let calls = 0;
  await assert.rejects(
    request(
      "/v1/publications",
      "POST",
      {},
      {
        token: async () => "fixture",
        fetch: async () => {
          calls++;
          return Response.json({ detail: "Stale revision" }, { status: 409 });
        },
      },
    ),
    /409/,
  );
  assert.equal(calls, 1);
});

test("OIDC credential request refuses a non-HTTPS issuer endpoint", async () => {
  const saved = process.env.ACTIONS_ID_TOKEN_REQUEST_URL;
  process.env.ACTIONS_ID_TOKEN_REQUEST_URL = "http://issuer.example.org/token";
  try {
    await assert.rejects(token(), /HTTPS OIDC issuer endpoint required/);
  } finally {
    if (saved === undefined) delete process.env.ACTIONS_ID_TOKEN_REQUEST_URL;
    else process.env.ACTIONS_ID_TOKEN_REQUEST_URL = saved;
  }
});

const sequence = (...responses) => {
  let calls = 0;
  return {
    token: async () => "fixture",
    delay: 0,
    fetch: async () => responses[calls++](),
    calls: () => calls,
  };
};

test("client retries a gateway error with a non-JSON body and keeps the status", async () => {
  process.env.ATM_LEDGER_URL = "https://ledger.example.org";
  const html = () => new Response("<html>Bad Gateway</html>", { status: 502 });
  const retried = sequence(html, () => Response.json({ version: 5 }));
  assert.deepEqual(await request("/v1/ledger", "GET", undefined, retried), { version: 5 });
  assert.equal(retried.calls(), 2);
  const exhausted = sequence(html, html, html);
  await assert.rejects(request("/v1/ledger", "GET", undefined, exhausted), /502/);
  assert.equal(exhausted.calls(), 3);
});

test("client reports the status when the error body is JSON null", async () => {
  process.env.ATM_LEDGER_URL = "https://ledger.example.org";
  const client = sequence(() => new Response("null", { status: 400 }));
  await assert.rejects(request("/v1/ledger", "GET", undefined, client), /400.*Unavailable/);
});

test("client refuses an oversize response", async () => {
  process.env.ATM_LEDGER_URL = "https://ledger.example.org";
  const client = sequence(() => new Response("{}", { status: 200, headers: { "content-length": "20000000" } }));
  await assert.rejects(request("/v1/ledger", "GET", undefined, client), /capacity/);
});

test("migration refuses a revision that did not touch the ledger", () => {
  const repo = mkdtempSync(path.join(os.tmpdir(), "atm-ledger-migration-"));
  const git = (...args) => execFileSync("git", ["-C", repo, ...args], { encoding: "utf8" }).trim();
  try {
    git("init", "--quiet");
    git("config", "user.name", "Test");
    git("config", "user.email", "test@example.org");
    git("config", "commit.gpgsign", "false");
    writeFileSync(path.join(repo, "other.txt"), "x");
    git("add", ".");
    git("commit", "--quiet", "-m", "No ledger");
    assert.throws(() => migrationHistory(repo, git("rev-parse", "HEAD")), /last ledger revision/);
  } finally {
    rmSync(repo, { recursive: true, force: true });
  }
});

test("migration exports every committed ledger revision without copying application source", () => {
  const repo = mkdtempSync(path.join(os.tmpdir(), "atm-ledger-migration-"));
  const git = (...args) => execFileSync("git", ["-C", repo, ...args], { encoding: "utf8" }).trim();
  try {
    git("init", "--quiet");
    git("config", "user.name", "Test");
    git("config", "user.email", "test@example.org");
    git("config", "commit.gpgsign", "false");
    writeFileSync(path.join(repo, "ledger.json"), JSON.stringify({ sequence: 0 }));
    writeFileSync(path.join(repo, "private-source.txt"), "must not be exported");
    git("add", ".");
    git("commit", "--quiet", "-m", "Initialize fixture");
    const first = git("rev-parse", "HEAD");
    writeFileSync(path.join(repo, "ledger.json"), JSON.stringify({ sequence: 1 }));
    git("add", "ledger.json");
    git("commit", "--quiet", "-m", "Append fixture receipt");
    const second = git("rev-parse", "HEAD");
    const manifest = migrationHistory(repo, second);
    assert.deepEqual(
      manifest.history.map((e) => [e.commit, e.parent, e.ledger.sequence]),
      [
        [first, null, 0],
        [second, first, 1],
      ],
    );
    assert.equal(JSON.stringify(manifest).includes("must not be exported"), false);
  } finally {
    rmSync(repo, { recursive: true, force: true });
  }
});
