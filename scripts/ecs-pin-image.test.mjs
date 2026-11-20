import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { chmodSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const script = fileURLToPath(new URL("./ecs-pin-image.sh", import.meta.url));
const REGISTRY = "111111111111.dkr.ecr.us-east-2.amazonaws.com";

// A stub `aws` on PATH (never touches AWS).
//   describe-task-definition -> prints $STUB_DIR/def_<family>.json, or AccessDenied
//   register-task-definition -> saves its --cli-input-json to registered_<n>.json and
//                               prints an ARN for the family with revision 99
const stub = `#!/bin/sh
cmd="$2"
case "$cmd" in
  describe-task-definition)
    fam=""; prev=""
    for a in "$@"; do
      if [ "$prev" = "--task-definition" ]; then fam="$a"; fi
      prev="$a"
    done
    if [ -n "$STUB_DENY" ]; then
      echo "An error occurred (AccessDeniedException) when calling DescribeTaskDefinition" >&2
      exit 254
    fi
    cat "$STUB_DIR/def_$fam.json"
    ;;
  register-task-definition)
    json=""; prev=""
    for a in "$@"; do
      if [ "$prev" = "--cli-input-json" ]; then json="$a"; fi
      prev="$a"
    done
    n=$(ls "$STUB_DIR"/registered_*.json 2>/dev/null | wc -l | tr -d ' ')
    printf '%s' "$json" > "$STUB_DIR/registered_$n.json"
    fam=$(printf '%s' "$json" | jq -r .family)
    echo "arn:aws:ecs:us-east-2:111111111111:task-definition/$fam:99"
    ;;
esac
`;

/** A describe-task-definition payload shaped like the real API response. */
function definition(family, image, { tags = [{ key: "Project", value: "dad" }] } = {}) {
  return {
    taskDefinition: {
      taskDefinitionArn: `arn:aws:ecs:us-east-2:111111111111:task-definition/${family}:3`,
      family,
      revision: 3,
      status: "ACTIVE",
      registeredAt: "2026-10-29T12:00:00.000Z",
      registeredBy: "arn:aws:sts::111111111111:assumed-role/cdk/deploy",
      compatibilities: ["EC2", "FARGATE"],
      requiresAttributes: [{ name: "ecs.capability.execution-role-awslogs" }],
      taskRoleArn: "arn:aws:iam::111111111111:role/task",
      executionRoleArn: "arn:aws:iam::111111111111:role/exec",
      networkMode: "awsvpc",
      cpu: "512",
      memory: "1024",
      requiresCompatibilities: ["FARGATE"],
      volumes: [],
      placementConstraints: [],
      containerDefinitions: [
        {
          name: `${family}-Container`,
          image,
          essential: true,
          command: ["worker", "--kind", "stripe"],
          environment: [{ name: "DONOR_GO_LISTEN", value: "0.0.0.0:8002" }],
          secrets: [{ name: "STRIPE_SECRET_KEY", valueFrom: "arn:aws:secretsmanager:us-east-2:111111111111:secret:s" }],
          portMappings: [{ containerPort: 8002, name: "api" }],
          healthCheck: { command: ["CMD", "wget", "-q", "http://127.0.0.1:8002/health/live"], retries: 10 },
          logConfiguration: { logDriver: "awslogs", options: { "awslogs-group": "g" } },
        },
      ],
    },
    ...(tags ? { tags } : {}),
  };
}

function run(args, defs = {}, env = {}) {
  const dir = mkdtempSync(path.join(os.tmpdir(), "dad-ecs-pin-"));
  try {
    const bin = path.join(dir, "aws");
    writeFileSync(bin, stub);
    chmodSync(bin, 0o755);
    for (const [family, def] of Object.entries(defs)) {
      writeFileSync(path.join(dir, `def_${family}.json`), JSON.stringify(def));
    }
    const result = spawnSync("bash", [script, ...args], {
      encoding: "utf8",
      env: { PATH: `${dir}:${process.env.PATH}`, STUB_DIR: dir, ...env },
    });
    const registered = readdirSync(dir)
      .filter((f) => f.startsWith("registered_"))
      .sort()
      .map((f) => JSON.parse(readFileSync(path.join(dir, f), "utf8")));
    return { status: result.status, stdout: result.stdout, stderr: result.stderr, registered };
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

test("re-tags the image, keeps everything else, and prints family=ARN", () => {
  const r = run(["abc1234", "dad-api"], { "dad-api": definition("dad-api", `${REGISTRY}/dad/api:latest`) });
  assert.equal(r.status, 0, r.stderr);
  assert.equal(r.stdout.trim(), "dad-api=arn:aws:ecs:us-east-2:111111111111:task-definition/dad-api:99");
  assert.equal(r.registered.length, 1);
  const reg = r.registered[0];
  const c = reg.containerDefinitions[0];
  assert.equal(c.image, `${REGISTRY}/dad/api:abc1234`);
  // Untouched: command, environment, secrets, ports, health check, logging, roles, sizing.
  assert.deepEqual(c.command, ["worker", "--kind", "stripe"]);
  assert.deepEqual(c.environment, [{ name: "DONOR_GO_LISTEN", value: "0.0.0.0:8002" }]);
  assert.equal(c.secrets[0].name, "STRIPE_SECRET_KEY");
  assert.equal(c.healthCheck.retries, 10);
  assert.equal(c.logConfiguration.logDriver, "awslogs");
  assert.equal(reg.family, "dad-api");
  assert.equal(reg.taskRoleArn, "arn:aws:iam::111111111111:role/task");
  assert.equal(reg.executionRoleArn, "arn:aws:iam::111111111111:role/exec");
  assert.equal(reg.cpu, "512");
  assert.equal(reg.memory, "1024");
  assert.equal(reg.networkMode, "awsvpc");
  assert.deepEqual(reg.requiresCompatibilities, ["FARGATE"]);
});

test("strips the read-only fields RegisterTaskDefinition rejects, and carries tags over", () => {
  const r = run(["abc1234", "dad-api"], { "dad-api": definition("dad-api", `${REGISTRY}/dad/api:latest`) });
  assert.equal(r.status, 0, r.stderr);
  const reg = r.registered[0];
  for (const readOnly of [
    "taskDefinitionArn",
    "revision",
    "status",
    "registeredAt",
    "registeredBy",
    "compatibilities",
    "requiresAttributes",
  ]) {
    assert.ok(!(readOnly in reg), `${readOnly} must not be sent`);
  }
  assert.deepEqual(reg.tags, [{ key: "Project", value: "dad" }]);
});

test("omits tags when the source revision has none", () => {
  const r = run(["abc1234", "dad-api"], {
    "dad-api": definition("dad-api", `${REGISTRY}/dad/api:latest`, { tags: null }),
  });
  assert.equal(r.status, 0, r.stderr);
  assert.ok(!("tags" in r.registered[0]));
});

test("pins several families, one revision each, in order", () => {
  const r = run(["abc1234", "dad-api", "dad-web", "dad-maint-migrate"], {
    "dad-api": definition("dad-api", `${REGISTRY}/dad/api:latest`),
    "dad-web": definition("dad-web", `${REGISTRY}/dad/web:latest`),
    "dad-maint-migrate": definition("dad-maint-migrate", `${REGISTRY}/dad/api:latest`),
  });
  assert.equal(r.status, 0, r.stderr);
  assert.deepEqual(
    r.stdout
      .trim()
      .split("\n")
      .map((l) => l.split("=")[0]),
    ["dad-api", "dad-web", "dad-maint-migrate"],
  );
  assert.deepEqual(
    r.registered.map((d) => d.containerDefinitions[0].image),
    [`${REGISTRY}/dad/api:abc1234`, `${REGISTRY}/dad/web:abc1234`, `${REGISTRY}/dad/api:abc1234`],
  );
});

test("refuses a digest-pinned image instead of corrupting it, and registers nothing", () => {
  const r = run(["abc1234", "dad-api"], {
    "dad-api": definition("dad-api", `${REGISTRY}/dad/api@sha256:${"a".repeat(64)}`),
  });
  assert.notEqual(r.status, 0);
  assert.match(r.stderr, /cannot re-tag image reference/);
  assert.equal(r.registered.length, 0);
});

test("refuses an image reference that has no tag", () => {
  const r = run(["abc1234", "dad-api"], { "dad-api": definition("dad-api", `${REGISTRY}/dad/api`) });
  assert.notEqual(r.status, 0);
  assert.match(r.stderr, /cannot re-tag image reference/);
  assert.equal(r.registered.length, 0);
});

test("rejects an image tag that is not a valid Docker tag", () => {
  for (const bad of ["bad tag", "a/b", "-leading", "x:y", "", "a".repeat(129)]) {
    const r = run([bad, "dad-api"], { "dad-api": definition("dad-api", `${REGISTRY}/dad/api:latest`) });
    assert.equal(r.status, 2, `tag '${bad}' should be rejected`);
    assert.match(r.stderr, /invalid image tag/);
    assert.equal(r.registered.length, 0);
  }
});

test("surfaces AWS errors and registers nothing", () => {
  const r = run(
    ["abc1234", "dad-api"],
    { "dad-api": definition("dad-api", `${REGISTRY}/dad/api:latest`) },
    { STUB_DENY: "1" },
  );
  assert.notEqual(r.status, 0);
  assert.match(r.stderr, /AccessDeniedException/);
  assert.equal(r.registered.length, 0);
});

test("requires an image tag and at least one family", () => {
  assert.equal(run([]).status, 2);
  assert.equal(run(["abc1234"]).status, 2);
});
