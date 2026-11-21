import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { chmodSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const script = fileURLToPath(new URL("./run-task.sh", import.meta.url));
const TASK_ARN = "arn:aws:ecs:us-east-2:111111111111:task/dad-staging/abc123";
const TD = (family) => `arn:aws:ecs:us-east-2:111111111111:task-definition/${family}:99`;

// A stub `aws` on PATH (never touches AWS). Each call is appended to calls.log.
//   ecs run-task        -> STUB_RUN_JSON
//   ecs wait ...        -> exit 0, or STUB_WAIT_FAIL=1 to fail like a waiter timeout
//   ecs describe-tasks  -> STUB_DESCRIBE_JSON
const stub = `#!/bin/sh
echo "$*" >> "$STUB_DIR/calls.log"
if [ -n "$STUB_CAPTURE_OVERRIDE" ] && [ "$2" = run-task ]; then
  while [ "$#" -gt 0 ]; do
    if [ "$1" = --overrides ]; then
      shift
      printf '%s' "$1" > "$STUB_DIR/override.json"
      break
    fi
    shift
  done
  printf '%s' "$STUB_RUN_JSON"
  exit 0
fi
case "$2" in
  run-task) printf '%s' "$STUB_RUN_JSON" ;;
  wait)
    if [ -n "$STUB_WAIT_FAIL" ]; then echo "Waiter TasksStopped failed: Max attempts exceeded" >&2; exit 255; fi
    ;;
  describe-tasks) printf '%s' "$STUB_DESCRIBE_JSON" ;;
esac
`;

const started = JSON.stringify({ tasks: [{ taskArn: TASK_ARN }], failures: [] });
const maint = (task, container) => ({ name: `maint-${task}-Container`, ...container });
const stoppedWith = (containers, extra = {}) => JSON.stringify({ tasks: [{ containers, ...extra }] });

const baseEnv = {
  DAD_CLUSTER: "dad-staging",
  DAD_SUBNETS: "subnet-a,subnet-b",
  DAD_SECURITY_GROUPS: "sg-1",
  AWS_REGION: "us-east-2",
  STUB_RUN_JSON: started,
  STUB_DESCRIBE_JSON: stoppedWith([maint("migrate", { exitCode: 0 })]),
};

function run(args, env = {}, taskdefs = null) {
  const dir = mkdtempSync(path.join(os.tmpdir(), "dad-run-task-"));
  try {
    const bin = path.join(dir, "aws");
    writeFileSync(bin, stub);
    chmodSync(bin, 0o755);
    const full = { PATH: `${dir}:${process.env.PATH}`, STUB_DIR: dir, ...baseEnv, ...env };
    if (taskdefs) {
      const file = path.join(dir, "taskdefs.txt");
      writeFileSync(file, `${taskdefs.join("\n")}\n`);
      full.DAD_TASKDEFS_FILE = file;
    }
    const result = spawnSync("bash", [script, ...args], { encoding: "utf8", env: full });
    let calls = "";
    try {
      calls = readFileSync(path.join(dir, "calls.log"), "utf8");
    } catch {}
    return {
      status: result.status,
      stdout: env.STUB_CAPTURE_OVERRIDE ? readFileSync(path.join(dir, "override.json"), "utf8") : result.stdout,
      stderr: result.stderr,
      calls,
    };
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

test("runs the task, waits for it to stop, and succeeds when the container exits 0", () => {
  const r = run(["staging", "migrate"]);
  assert.equal(r.status, 0, r.stderr);
  const order = r.calls
    .trim()
    .split("\n")
    .map((l) => l.split(" ").slice(0, 3).join(" "));
  assert.deepEqual(order, ["ecs run-task --cluster", "ecs wait tasks-stopped", "ecs describe-tasks --cluster"]);
  assert.match(r.calls, /--task-definition dad-maint-migrate /);
  assert.match(
    r.calls,
    /--network-configuration awsvpcConfiguration=\{subnets=\[subnet-a,subnet-b\],securityGroups=\[sg-1\],assignPublicIp=DISABLED\}/,
  );
  assert.match(r.calls, new RegExp(`wait tasks-stopped --cluster dad-staging --tasks ${TASK_ARN}`));
});

test("fails when the container exits non-zero, so the next step does not run", () => {
  const r = run(["staging", "migrate"], {
    STUB_DESCRIBE_JSON: stoppedWith([maint("migrate", { exitCode: 3 })], { stoppedReason: "Essential container in task exited" }),
  });
  assert.equal(r.status, 1);
  assert.match(r.stderr, /dad-maint-migrate: container exit code 3: Essential container in task exited/);
});

test("fails when no container ever ran (for example an image pull failure)", () => {
  const r = run(["staging", "migrate"], {
    STUB_DESCRIBE_JSON: stoppedWith([maint("migrate", { reason: "CannotPullContainerError: manifest unknown" })], {
      stoppedReason: "Task failed to start",
    }),
  });
  assert.equal(r.status, 1);
  assert.match(r.stderr, /container exit code none: Task failed to start; CannotPullContainerError: manifest unknown/);
});

test("judges the maint container, not whichever container is listed first", () => {
  const r = run(["staging", "migrate"], {
    STUB_DESCRIBE_JSON: stoppedWith([{ name: "sidecar", exitCode: 0 }, maint("migrate", { exitCode: 3 })], {
      stoppedReason: "Essential container in task exited",
    }),
  });
  assert.equal(r.status, 1);
  assert.match(r.stderr, /container exit code 3/);
});

test("fails when the maint container is absent from the stopped task", () => {
  const r = run(["staging", "migrate"], { STUB_DESCRIBE_JSON: stoppedWith([{ name: "sidecar", exitCode: 0 }]) });
  assert.equal(r.status, 1);
  assert.match(r.stderr, /container exit code none/);
});

test("fails with the AWS reason when run-task places no task, and never waits", () => {
  const r = run(["staging", "migrate"], {
    STUB_RUN_JSON: JSON.stringify({ tasks: [], failures: [{ reason: "RESOURCE:FARGATE" }] }),
  });
  assert.equal(r.status, 1);
  assert.match(r.stderr, /run-task started no task: .*RESOURCE:FARGATE/);
  assert.doesNotMatch(r.calls, /wait tasks-stopped|describe-tasks/);
});

test("fails when the waiter gives up, without judging a still-running task", () => {
  const r = run(["staging", "migrate"], { STUB_WAIT_FAIL: "1" });
  assert.notEqual(r.status, 0);
  assert.match(r.stderr, /Max attempts exceeded/);
  assert.doesNotMatch(r.calls, /describe-tasks/);
});

test("DAD_WAIT=false starts the task and returns without waiting", () => {
  const r = run(["staging", "staff"], { DAD_WAIT: "false" });
  assert.equal(r.status, 0, r.stderr);
  assert.match(r.calls, /run-task/);
  assert.doesNotMatch(r.calls, /wait tasks-stopped|describe-tasks/);
});

test("with a task-definitions file, runs the pinned revision of dad-maint-<task>", () => {
  const r = run(["staging", "provision-roles"], { STUB_DESCRIBE_JSON: stoppedWith([maint("provision-roles", { exitCode: 0 })]) }, [
    `dad-maint-migrate=${TD("dad-maint-migrate")}`,
    `dad-maint-provision-roles=${TD("dad-maint-provision-roles")}`,
  ]);
  assert.equal(r.status, 0, r.stderr);
  assert.match(r.calls, new RegExp(`--task-definition ${TD("dad-maint-provision-roles")} `));
  assert.doesNotMatch(r.calls, /dad-maint-migrate/);
});

test("a missing pinned entry fails before any task is started", () => {
  const r = run(["staging", "migrate"], {}, [`dad-maint-staff=${TD("dad-maint-staff")}`]);
  assert.equal(r.status, 1);
  assert.match(r.stderr, /no pinned task definition for 'dad-maint-migrate'/);
  assert.doesNotMatch(r.calls, /run-task/);
});

test("passes extra arguments as a single command override", () => {
  const r = run(["staging", "migrate", "--adopt-existing"]);
  assert.equal(r.status, 0, r.stderr);
  assert.match(
    r.calls,
    /--overrides \{"containerOverrides":\[\{"name":"maint-migrate-Container","command":\["migrate","--adopt-existing"\]\}\]\}/,
  );
});

test("accepts bootstrap-owner and runs family dad-maint-bootstrap-owner", () => {
  const r = run(["staging", "bootstrap-owner"], {
    DAD_SECURITY_GROUPS: "sg-bootstrap",
    STUB_DESCRIBE_JSON: stoppedWith([maint("bootstrap-owner", { exitCode: 0 })]),
  });
  assert.equal(r.status, 0, r.stderr);
  assert.match(r.calls, /--task-definition dad-maint-bootstrap-owner /);
  assert.match(r.calls, /securityGroups=\[sg-bootstrap\]/);
});

test("rejects an unknown environment or task, and a missing cluster", () => {
  assert.equal(run(["qa", "migrate"]).status, 2);
  assert.equal(run(["staging", "wipe"]).status, 2);
  const r = run(["staging", "migrate"], { DAD_CLUSTER: "" });
  assert.notEqual(r.status, 0);
  assert.match(r.stderr, /DAD_CLUSTER/);
  assert.doesNotMatch(r.calls, /run-task/);
});

test("preserves exact argument boundaries and JSON characters in command overrides", () => {
  const args = ['a"b', "c\\d", "line\nbreak", "", "--name", "Jane Doe"];
  const r = run(["staging", "staff", ...args], { DAD_WAIT: "false", STUB_CAPTURE_OVERRIDE: "1" });
  assert.equal(r.status, 0, r.stderr);
  assert.deepEqual(JSON.parse(r.stdout).containerOverrides, [
    { name: "maint-staff-Container", command: ["staff", ...args] },
  ]);
});

test("provision-roles overrides invoke the provision binary", () => {
  const r = run(["staging", "provision-roles", "--help"], { DAD_WAIT: "false", STUB_CAPTURE_OVERRIDE: "1" });
  assert.equal(r.status, 0, r.stderr);
  assert.deepEqual(JSON.parse(r.stdout).containerOverrides[0].command, ["provision", "--help"]);
});
