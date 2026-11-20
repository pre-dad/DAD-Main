import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { chmodSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const script = fileURLToPath(new URL("./ecs-rollout.sh", import.meta.url));

// A stub `aws` on PATH. It never touches AWS.
//   update-service   -> prints the id `dep-<service>` (or STUB_UPDATE_OUTPUT if set)
//   describe-services-> rolloutState from STATES_<service> (comma list, consumed one per
//                       call, last value repeats); the rolloutStateReason query returns
//                       STUB_REASON. STUB_DENY=1 makes describe-services fail.
const stub = `#!/bin/sh
echo "$*" >> "$STUB_DIR/calls.log"
cmd="$2"
svc=""
prev=""
for a in "$@"; do
  if [ "$prev" = "--service" ] || [ "$prev" = "--services" ]; then svc="$a"; fi
  prev="$a"
done
key=$(echo "$svc" | tr '-' '_')
case "$cmd" in
  update-service)
    if [ -n "$STUB_UPDATE_OUTPUT" ]; then echo "$STUB_UPDATE_OUTPUT"; else echo "dep-$svc"; fi
    ;;
  describe-services)
    if [ -n "$STUB_DENY" ]; then
      echo "An error occurred (AccessDenied) when calling the DescribeServices operation" >&2
      exit 254
    fi
    case "$*" in
      *rolloutStateReason*) echo "$STUB_REASON"; exit 0 ;;
      *desiredCount*) eval "d=\\\${DESIRED_$key:-1}"; echo "$d"; exit 0 ;;
    esac
    seqfile="$STUB_DIR/seq_$key"
    if [ ! -f "$seqfile" ]; then
      eval "init=\\$STATES_$key"
      echo "$init" | tr ',' '\\n' > "$seqfile"
    fi
    first=$(head -n 1 "$seqfile")
    rest=$(tail -n +2 "$seqfile")
    if [ -n "$rest" ]; then echo "$rest" > "$seqfile"; fi
    echo "$first"
    ;;
esac
`;

function run(args, env = {}) {
  const dir = mkdtempSync(path.join(os.tmpdir(), "dad-ecs-rollout-"));
  try {
    const bin = path.join(dir, "aws");
    writeFileSync(bin, stub);
    chmodSync(bin, 0o755);
    const result = spawnSync("bash", [script, ...args], {
      encoding: "utf8",
      env: {
        PATH: `${dir}:${process.env.PATH}`,
        STUB_DIR: dir,
        ROLLOUT_POLL_SECONDS: "0",
        ROLLOUT_TIMEOUT_SECONDS: "5",
        ...env,
      },
    });
    let calls = "";
    try {
      calls = readFileSync(path.join(dir, "calls.log"), "utf8");
    } catch (error) {
      if (error.code !== "ENOENT") throw error;
    }
    return { status: result.status, stdout: result.stdout, stderr: result.stderr, calls };
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

test("forces a new deployment of each service and succeeds when all complete", () => {
  const r = run(["dad-staging", "api", "web", "nginx"], {
    STATES_api: "COMPLETED",
    STATES_web: "COMPLETED",
    STATES_nginx: "COMPLETED",
  });
  assert.equal(r.status, 0, r.stderr);
  for (const svc of ["api", "web", "nginx"]) {
    assert.match(r.calls, new RegExp(`update-service --cluster dad-staging --service ${svc} --force-new-deployment`));
    assert.match(r.stdout, new RegExp(`${svc}: deployment dep-${svc} COMPLETED`));
  }
});

test("keeps polling while IN_PROGRESS or not yet listed, then succeeds", () => {
  const r = run(["dad-staging", "api"], { STATES_api: "None,IN_PROGRESS,IN_PROGRESS,COMPLETED" });
  assert.equal(r.status, 0, r.stderr);
  assert.match(r.stdout, /api: deployment dep-api COMPLETED/);
});

test("fails fast when a listed deployment disappears", () => {
  const r = run(["dad-staging", "api"], { STATES_api: "IN_PROGRESS,None", ROLLOUT_TIMEOUT_SECONDS: "600" });
  assert.equal(r.status, 1);
  assert.match(r.stderr, /api: deployment dep-api is no longer listed; another deployment superseded it/);
});

test("fails when the task definitions file cannot be read", () => {
  const r = run(["dad-staging", "api"], { DAD_TASKDEFS_FILE: "/nonexistent/taskdefs" });
  assert.equal(r.status, 1);
  assert.match(r.stderr, /cannot read \/nonexistent\/taskdefs/);
});

test("fails when OUR deployment ends FAILED, even though a rollback deployment exists", () => {
  // The circuit breaker rolled back: our id (dep-api) is FAILED. The script must key on
  // that id; the rollback deployment being COMPLETED must not make this pass.
  const r = run(["dad-staging", "api"], {
    STATES_api: "IN_PROGRESS,FAILED",
    STUB_REASON: "ECS deployment circuit breaker: tasks failed to start.",
  });
  assert.equal(r.status, 1);
  assert.match(
    r.stderr,
    /api: deployment dep-api FAILED and was rolled back: ECS deployment circuit breaker: tasks failed to start\./,
  );
  // The rolloutState query itself (not just the later rolloutStateReason lookup) must
  // filter on the id this run started, never on "whichever deployment is PRIMARY".
  assert.match(
    r.calls,
    /deployments\[\?id=='dep-api'\] \| \[0\]\.rolloutState --output text/,
    "state query must be keyed on the deployment id it started",
  );
  assert.doesNotMatch(
    r.calls,
    /deployments\[\?status=='PRIMARY'\] \| \[0\]\.rolloutState/,
    "must not ask about the current primary deployment",
  );
});

test("a failure on one service still waits for and reports the others, then exits 1", () => {
  const r = run(["dad-staging", "api", "web"], {
    STATES_api: "FAILED",
    STATES_web: "COMPLETED",
    STUB_REASON: "unhealthy",
  });
  assert.equal(r.status, 1);
  assert.match(r.stderr, /api: deployment dep-api FAILED/);
  assert.match(r.stdout, /web: deployment dep-web COMPLETED/);
});

test("fails with a clear message when the deployment never finishes", () => {
  const r = run(["dad-staging", "api"], { STATES_api: "IN_PROGRESS", ROLLOUT_TIMEOUT_SECONDS: "0" });
  assert.equal(r.status, 1);
  assert.match(r.stderr, /api: deployment dep-api still 'IN_PROGRESS' after 0s/);
});

test("fails when update-service returns no deployment id", () => {
  const r = run(["dad-staging", "api"], { STUB_UPDATE_OUTPUT: "None", STATES_api: "COMPLETED" });
  assert.equal(r.status, 1);
  assert.match(r.stderr, /returned no PRIMARY deployment id/);
});

test("surfaces AWS errors instead of swallowing them", () => {
  const r = run(["dad-staging", "api"], { STUB_DENY: "1" });
  assert.notEqual(r.status, 0);
  assert.match(r.stderr, /AccessDenied/);
});

test("requires a cluster and at least one service", () => {
  assert.equal(run([]).status, 2);
  assert.equal(run(["dad-staging"]).status, 2);
});

/** Run with a DAD_TASKDEFS_FILE containing `lines`, cleaning the file up afterwards. */
function runPinned(args, lines, env = {}) {
  const dir = mkdtempSync(path.join(os.tmpdir(), "dad-taskdefs-"));
  try {
    const file = path.join(dir, "taskdefs.txt");
    writeFileSync(file, `${lines.join("\n")}\n`);
    return run(args, { DAD_TASKDEFS_FILE: file, ...env });
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

const TD = (family) => `arn:aws:ecs:us-east-2:111111111111:task-definition/${family}:99`;

test("with a task-definitions file, deploys each service on its pinned revision", () => {
  const r = runPinned(
    ["dad-staging", "api", "stripe-worker"],
    [`dad-api=${TD("dad-api")}`, `dad-web=${TD("dad-web")}`, `dad-stripe-worker=${TD("dad-stripe-worker")}`],
    { STATES_api: "COMPLETED", STATES_stripe_worker: "COMPLETED" },
  );
  assert.equal(r.status, 0, r.stderr);
  assert.match(r.calls, new RegExp(`--service api --force-new-deployment --task-definition ${TD("dad-api")}`));
  assert.match(
    r.calls,
    new RegExp(`--service stripe-worker --force-new-deployment --task-definition ${TD("dad-stripe-worker")}`),
  );
  // Never the web revision, which belongs to a service we did not roll.
  assert.doesNotMatch(r.calls, /dad-web/);
});

test("a service with no pinned entry fails before any deployment is started", () => {
  const r = runPinned(["dad-staging", "api", "web"], [`dad-api=${TD("dad-api")}`], {
    STATES_api: "COMPLETED",
    STATES_web: "COMPLETED",
  });
  assert.equal(r.status, 1);
  assert.match(r.stderr, /no pinned task definition for 'dad-web'/);
  // api HAS an entry, but nothing may be started when any service is unresolved.
  assert.doesNotMatch(r.calls, /update-service/, "no deployment may start before every revision resolves");
});

test("does not mistake a longer family for the service (dad-api vs dad-api-extra)", () => {
  const r = runPinned(["dad-staging", "api"], [`dad-api-extra=${TD("dad-api-extra")}`], { STATES_api: "COMPLETED" });
  assert.equal(r.status, 1);
  assert.match(r.stderr, /no pinned task definition for 'dad-api'/);
});

test("without a task-definitions file, redeploys the current revision (no --task-definition)", () => {
  const r = run(["dad-staging", "api"], { STATES_api: "COMPLETED" });
  assert.equal(r.status, 0, r.stderr);
  assert.doesNotMatch(r.calls, /--task-definition/);
});

// ROLLOUT_SKIP_IDLE: services at a desired count of 0 are switched off in DAD-infra's
// config (the one owner of that switch); deploy.yml must skip them rather than keep its
// own flag.
test("ROLLOUT_SKIP_IDLE skips services at desired count 0 and rolls the rest", () => {
  const r = run(["dad-staging", "api", "stripe-worker"], {
    ROLLOUT_SKIP_IDLE: "true",
    DESIRED_api: "1",
    DESIRED_stripe_worker: "0",
    STATES_api: "COMPLETED",
  });
  assert.equal(r.status, 0, r.stderr);
  assert.match(r.stdout, /stripe-worker: desired count 0 \(switched off\); skipping/);
  assert.match(r.calls, /update-service --cluster dad-staging --service api /);
  assert.doesNotMatch(r.calls, /update-service .*--service stripe-worker/);
});

test("ROLLOUT_SKIP_IDLE with every service off rolls nothing and succeeds", () => {
  const r = run(["dad-staging", "stripe-worker", "push-worker"], {
    ROLLOUT_SKIP_IDLE: "true",
    DESIRED_stripe_worker: "0",
    DESIRED_push_worker: "0",
  });
  assert.equal(r.status, 0, r.stderr);
  assert.match(r.stdout, /every service is switched off; nothing to roll/);
  assert.doesNotMatch(r.calls, /update-service/);
});

test("an idle service needs no pinned revision", () => {
  const r = runPinned(["dad-staging", "api", "push-worker"], [`dad-api=${TD("dad-api")}`], {
    ROLLOUT_SKIP_IDLE: "true",
    DESIRED_push_worker: "0",
    STATES_api: "COMPLETED",
  });
  assert.equal(r.status, 0, r.stderr);
});

test("ROLLOUT_SKIP_IDLE fails on a service that does not exist", () => {
  const r = run(["dad-staging", "api"], { ROLLOUT_SKIP_IDLE: "true", DESIRED_api: "None" });
  assert.equal(r.status, 1);
  assert.match(r.stderr, /api: no such service in dad-staging/);
  assert.doesNotMatch(r.calls, /update-service/);
});

test("without ROLLOUT_SKIP_IDLE an idle service is still rolled (no desired-count lookup)", () => {
  const r = run(["dad-staging", "api"], { DESIRED_api: "0", STATES_api: "COMPLETED" });
  assert.equal(r.status, 0, r.stderr);
  assert.doesNotMatch(r.calls, /desiredCount/);
  assert.match(r.calls, /update-service/);
});
