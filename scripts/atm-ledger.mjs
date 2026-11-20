import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { pathToFileURL } from "node:url";

const SOURCE = "github:pre-dad/DAD-Main";
const json = (file) => readFile(file, "utf8").then(JSON.parse);
async function responseJSON(response) {
  const length = Number(response.headers.get("content-length"));
  if (length > 16 * 1024 * 1024) throw new Error("Ledger response exceeds capacity");
  const reader = response.body?.getReader();
  if (!reader) throw new Error("Ledger response is empty");
  let size = 0;
  const chunks = [];
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    size += value.byteLength;
    if (size > 16 * 1024 * 1024) {
      await reader.cancel();
      throw new Error("Ledger response exceeds capacity");
    }
    chunks.push(value);
  }
  return JSON.parse(Buffer.concat(chunks).toString("utf8"));
}

export async function token() {
  const url = new URL(process.env.ACTIONS_ID_TOKEN_REQUEST_URL);
  if (url.protocol !== "https:") throw new Error("HTTPS OIDC issuer endpoint required");
  url.searchParams.set("audience", `atm-ledger:${SOURCE}`);
  const response = await fetch(url, {
    headers: { Authorization: `Bearer ${process.env.ACTIONS_ID_TOKEN_REQUEST_TOKEN}` },
    signal: AbortSignal.timeout(10000),
    redirect: "error",
  });
  if (!response.ok) throw new Error(`OIDC request failed (${response.status})`);
  const data = await responseJSON(response);
  if (typeof data.value !== "string") throw new Error("Missing OIDC credential");
  return data.value;
}

export async function request(route, method = "GET", body, dependencies = {}) {
  const base = new URL(process.env.ATM_LEDGER_URL);
  if (base.protocol !== "https:" || base.username || base.password || base.search || base.hash || base.pathname !== "/")
    throw new Error("Configure an HTTPS ledger origin without credentials or a path");
  const credential = await (dependencies.token ?? token)();
  const send = dependencies.fetch ?? fetch;
  for (let attempt = 0; attempt < 3; attempt++) {
    const response = await send(new URL(route, base), {
      method,
      headers: { Authorization: `Bearer ${credential}`, "Content-Type": "application/json" },
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: AbortSignal.timeout(60000),
      redirect: "error",
    });
    let value = {};
    try {
      value = await responseJSON(response);
    } catch (error) {
      if (response.ok || error.message.includes("capacity")) throw error;
    }
    if (response.ok) return value;
    if (response.status >= 500 && attempt < 2) {
      await new Promise((resolve) => setTimeout(resolve, (dependencies.delay ?? 500) * (attempt + 1)));
      continue;
    }
    throw new Error(`Ledger operation failed (${response.status}): ${String(value?.detail ?? "Unavailable")}`);
  }
}

export function migrationHistory(repo, revision) {
  if (!/^[a-f0-9]{40}$/.test(revision)) throw new Error("A full legacy state commit is required");
  const git = (...args) =>
    execFileSync("git", ["-C", repo, ...args], { encoding: "utf8", maxBuffer: 16 * 1024 * 1024 }).trim();
  const commits = git("rev-list", "--reverse", revision, "--", "ledger.json").split("\n").filter(Boolean);
  if (commits.at(-1) !== revision) throw new Error("Legacy tip must be its last ledger revision");
  let parent = null;
  const history = commits.map((commit) => {
    const entry = {
      commit,
      parent,
      at: git("show", "-s", "--format=%cI", commit),
      ledger: JSON.parse(git("show", `${commit}:ledger.json`)),
    };
    parent = commit;
    return entry;
  });
  return { source: SOURCE, history };
}

async function main() {
  process.umask(0o077);
  const [action, stateDir, atmDir] = process.argv.slice(2);
  if (action === "triage-request") {
    const [, noteFile, finding, severity, disposition] = process.argv.slice(2);
    const note = (await readFile(noteFile, "utf8")).trim();
    if (
      note.length < 10 ||
      !/^[a-f0-9]{24}$/.test(finding) ||
      !["unassessed", "low", "medium", "high", "critical"].includes(severity) ||
      !["untriaged", "confirmed", "resolved", "dismissed"].includes(disposition)
    )
      throw new Error("A finding, classification and meaningful private review note are required");
    const hash = createHash("sha256").update(note).digest("hex");
    execFileSync(
      "gh",
      [
        "workflow",
        "run",
        "atm.yml",
        "--repo",
        "pre-dad/DAD-Main",
        "--ref",
        "dev",
        "-f",
        "ledger_operation=triage",
        "-f",
        `finding=${finding}`,
        "-f",
        `severity=${severity}`,
        "-f",
        `disposition=${disposition}`,
        "-f",
        `note_digest=${hash}`,
      ],
      { stdio: "inherit" },
    );
    return;
  }
  if (!stateDir)
    throw new Error(
      "Usage: node scripts/atm-ledger.mjs {snapshot|publish|migrate|triage|history|recover} STATE_DIR [ATM_DIR]",
    );
  await mkdir(stateDir, { recursive: true, mode: 0o700 });
  if (action === "snapshot") {
    const { validateLedger, hashPolicy } = await loadATM(atmDir);
    const snapshot = await request("/v1/ledger");
    validateLedger(snapshot.ledger, SOURCE);
    if (
      !Number.isSafeInteger(snapshot.version) ||
      snapshot.version < 1 ||
      snapshot.digest !== hashPolicy(snapshot.ledger)
    )
      throw new Error("Ledger snapshot integrity failure");
    await writeFile(path.join(stateDir, "snapshot.json"), JSON.stringify(snapshot), { mode: 0o600 });
    await writeFile(path.join(stateDir, "ledger.json"), JSON.stringify(snapshot.ledger), { mode: 0o600 });
  } else if (action === "publish") {
    const { publishProposal, hashPolicy } = await loadATM(atmDir);
    const snapshot = await json(path.join(stateDir, "snapshot.json"));
    const proposalDir = process.env.ATM_PROPOSAL_DIR;
    if (!proposalDir) throw new Error("ATM_PROPOSAL_DIR is required");
    const next = publishProposal({
      ledger: snapshot.ledger,
      proposal: await json(path.join(proposalDir, "proposal.json")),
      source: SOURCE,
      execution: `${process.env.GITHUB_RUN_ID}:${process.env.GITHUB_RUN_ATTEMPT}`,
      revision: process.env.CANDIDATE,
      artifactId: process.env.ARTIFACT_ID,
      artifactDigest: process.env.ARTIFACT_DIGEST,
      bundle: await readFile(path.join(proposalDir, "evidence.atm")),
    });
    const saved = await request("/v1/publications", "POST", {
      expectedVersion: snapshot.version,
      previousDigest: snapshot.digest,
      ledger: next,
    });
    if (saved.version !== snapshot.version + 1 || saved.digest !== hashPolicy(next))
      throw new Error("Publication changed concurrently; rerun validation");
    const receipt = next.receipts.at(-1);
    await output("verdict", receipt.eligible ? "eligible" : "blocked");
    console.log(`Ledger publication retained at version ${saved.version}.`);
  } else if (action === "migrate") {
    const manifest = migrationHistory(process.env.ATM_LEGACY_REPO ?? ".", process.env.ATM_LEGACY_REVISION);
    const { hashPolicy, validateLedger } = await loadATM(atmDir);
    for (const entry of manifest.history) validateLedger(entry.ledger, SOURCE);
    const saved = await request("/v1/migration", "POST", manifest);
    if (saved.digest !== hashPolicy(manifest.history.at(-1).ledger) || saved.version !== manifest.history.length)
      throw new Error("Migration verification failed");
    await output("legacy_revision", manifest.history.at(-1).commit);
    console.log(`Verified ${manifest.history.length} legacy revisions and ${saved.ledger.receipts.length} receipts.`);
  } else if (action === "triage") {
    const snapshot = await request("/v1/ledger");
    const change = {
      finding: process.env.ATM_FINDING,
      severity: process.env.ATM_SEVERITY,
      disposition: process.env.ATM_DISPOSITION,
      noteDigest: process.env.ATM_NOTE_DIGEST,
    };
    const saved = await request("/v1/triage", "POST", {
      expectedVersion: snapshot.version,
      previousDigest: snapshot.digest,
      change,
    });
    console.log(
      `Audited triage retained at version ${saved.version}. Keep the private review note matching its digest.`,
    );
  } else if (action === "recover") {
    const saved = await request("/v1/recovery", "POST", {});
    console.log(`Verified ledger and archive at version ${saved.version}.`);
  } else if (action === "history") {
    const events = [];
    const { hashPolicy } = await loadATM(atmDir);
    let cursor = 0,
      previous = "0".repeat(64);
    for (;;) {
      const page = await request(`/v1/history?after=${cursor}`);
      if (!Array.isArray(page.events) || page.events.length > 100 || (!page.events.length && page.more))
        throw new Error("Invalid history page");
      for (const entry of page.events) {
        if (
          entry.event.version !== cursor + 1 ||
          entry.event.previousHash !== previous ||
          hashPolicy(entry.event) !== entry.hash
        )
          throw new Error("History chain verification failed");
        events.push(entry);
        cursor++;
        previous = entry.hash;
        if (events.length > 100000) throw new Error("Export exceeds interactive capacity; use paginated API");
      }
      if (!page.more) break;
    }
    await writeFile(path.join(stateDir, "history.json"), JSON.stringify(events), { mode: 0o600 });
    console.log(`Verified ${events.length} append-only events.`);
  } else throw new Error("Unknown ledger operation");
}

async function loadATM(directory) {
  if (!directory) throw new Error("Pinned ATM checkout required");
  const ci = await import(pathToFileURL(path.resolve(directory, "src/ci.js")));
  const policy = await import(pathToFileURL(path.resolve(directory, "src/policy.js")));
  return { ...ci, hashPolicy: policy.hashPolicy };
}
async function output(name, value) {
  if (!process.env.GITHUB_OUTPUT) throw new Error("GitHub output file required");
  await writeFile(process.env.GITHUB_OUTPUT, `${name}=${value}\n`, { flag: "a" });
}

if (process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href) {
  main().catch((error) => {
    console.error(error.message);
    process.exitCode = 1;
  });
}
