#!/usr/bin/env bash
#
# run-task.sh — thin wrapper around `aws ecs run-task` for the DAD maintenance
# task definitions created by DAD-infra apps/dad-main/lib/maintenance-stack.ts
# (https://github.com/pre-dad/DAD-infra).
#
# This script is DOCUMENTED, NOT AUTO-RUN by CDK. An operator invokes it by hand
# (or the app pipeline invokes `aws ecs run-task` directly in the same shape).
#
# Deploy ordering (mirrors scripts/deploy.sh): run `migrate` first, then
# `provision-roles`, before bringing services up. The reconcile task runs on an
# EventBridge Scheduler cron and should not normally be run by hand.
#
# Usage:
#   scripts/run-task.sh <env> <task> [extra container command args...]
#
#   <env>   staging | production
#   <task>  migrate | provision-roles | staff | rekey | monitor | reconcile | bootstrap-owner
#
# bootstrap-owner runs once per environment, before migrate, to create the database
# owner role as the RDS master. Pass DAD_SECURITY_GROUPS=<BootstrapSecurityGroupId>
# (dad-data-<env> output): it is the only security group the instance admits besides the
# RDS Proxy. See DAD-infra docs/bootstrap.md.
#
# Examples:
#   scripts/run-task.sh staging migrate
#   scripts/run-task.sh staging migrate --adopt-existing
#   scripts/run-task.sh staging provision-roles
#
# Required environment / resolved values (read from CDK outputs or SSM, not
# hardcoded here):
#   DAD_CLUSTER            ECS cluster name            (e.g. dad-staging)
#   DAD_SUBNETS            comma-separated subnet ids  (compute tier)
#   DAD_SECURITY_GROUPS    comma-separated SG ids      (workers SG)
#   AWS_REGION             us-east-2
#
# Optional environment:
#   DAD_TASKDEFS_FILE      file of `<family>=<task definition ARN>` lines, as printed by
#                          ecs-pin-image.sh. When set, the task runs that pinned revision
#                          of `dad-maint-<task>` (a missing entry is an error). When unset,
#                          the family is used, which ECS resolves to its latest ACTIVE
#                          revision.
#   DAD_WAIT               `true` (default) waits for the task to stop and exits non-zero
#                          unless its container exited 0, so `migrate` finishing is what
#                          lets `provision-roles` start. `false` starts the task and returns.
#
# Requires: aws CLI v2 and jq.
set -euo pipefail

env="${1:?usage: run-task.sh <env> <task> [args...]}"
task="${2:?usage: run-task.sh <env> <task> [args...]}"
shift 2 || true

case "$env" in
  staging | production) ;;
  *) printf 'Unknown env %q (expected staging|production)\n' "$env" >&2; exit 2 ;;
esac

case "$task" in
  migrate | provision-roles | staff | rekey | monitor | reconcile | bootstrap-owner) ;;
  *) printf 'Unknown task %q\n' "$task" >&2; exit 2 ;;
esac

: "${DAD_CLUSTER:?set DAD_CLUSTER (ECS cluster name)}"
: "${DAD_SUBNETS:?set DAD_SUBNETS (comma-separated compute subnet ids)}"
: "${DAD_SECURITY_GROUPS:?set DAD_SECURITY_GROUPS (comma-separated workers SG ids)}"
: "${AWS_REGION:?set AWS_REGION (us-east-2)}"

# Task definition family follows the CDK convention: dad-maint-<task>.
family="dad-maint-${task}"
task_definition="$family"
if [ -n "${DAD_TASKDEFS_FILE:-}" ]; then
  line="$(grep -E "^${family}=" "$DAD_TASKDEFS_FILE" | head -n 1 || true)"
  if [ -z "$line" ]; then
    echo "::error::no pinned task definition for '$family' in $DAD_TASKDEFS_FILE" >&2
    exit 1
  fi
  task_definition="${line#*=}"
fi

# Build an optional command override when extra args are supplied (e.g.
# `migrate --adopt-existing`). The container name follows the factory id.
overrides=()
if [ "$#" -gt 0 ]; then
  binary="$task"
  if [ "$task" = provision-roles ]; then binary=provision; fi
  override="$(jq -cn --arg name "maint-${task}-Container" --args \
    '{containerOverrides: [{name: $name, command: $ARGS.positional}]}' -- "$binary" "$@")"
  overrides=(--overrides "$override")
fi

# AWS CLI shorthand syntax; ids are comma-separated and need no quoting here.
network="awsvpcConfiguration={subnets=[${DAD_SUBNETS}],securityGroups=[${DAD_SECURITY_GROUPS}],assignPublicIp=DISABLED}"

echo "running $task_definition on $DAD_CLUSTER" >&2
# ${overrides[@]+...} keeps an empty array safe under `set -u` on bash 3.2 (macOS).
result="$(aws ecs run-task \
  --cluster "$DAD_CLUSTER" \
  --launch-type FARGATE \
  --task-definition "$task_definition" \
  --network-configuration "$network" \
  --region "$AWS_REGION" \
  --output json \
  ${overrides[@]+"${overrides[@]}"})"

task_arn="$(printf '%s' "$result" | jq -r '.tasks[0].taskArn // empty')"
if [ -z "$task_arn" ]; then
  echo "::error::run-task started no task: $(printf '%s' "$result" | jq -c '.failures')" >&2
  exit 1
fi
echo "started $task_arn" >&2

if [ "${DAD_WAIT:-true}" != "true" ]; then
  exit 0
fi

# Wait for the task to stop (the aws waiter polls for about ten minutes), then judge it
# by its container's exit code. A task that never started a container (for example an
# image pull failure) has no exit code and is a failure.
aws ecs wait tasks-stopped \
  --cluster "$DAD_CLUSTER" \
  --tasks "$task_arn" \
  --region "$AWS_REGION"

stopped="$(aws ecs describe-tasks \
  --cluster "$DAD_CLUSTER" \
  --tasks "$task_arn" \
  --region "$AWS_REGION" \
  --output json)"
exit_code="$(printf '%s' "$stopped" | jq -r '.tasks[0].containers[0].exitCode // "none"')"
if [ "$exit_code" = "0" ]; then
  echo "$task_definition: container exited 0" >&2
  exit 0
fi
reason="$(printf '%s' "$stopped" | jq -r '[.tasks[0].stoppedReason, .tasks[0].containers[0].reason] | map(select(. != null)) | join("; ")')"
echo "::error::$task_definition: container exit code ${exit_code}: ${reason:-no reason reported}" >&2
exit 1
