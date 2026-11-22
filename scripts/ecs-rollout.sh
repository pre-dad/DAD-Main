#!/usr/bin/env bash
#
# ecs-rollout.sh — force a new deployment of ECS services and wait for it to succeed.
#
# Usage:
#   scripts/ecs-rollout.sh <cluster> <service> [<service> ...]
#
# Environment (optional):
#   ROLLOUT_TIMEOUT_SECONDS  give up on a service after this long   (default 1200)
#   ROLLOUT_POLL_SECONDS     seconds between status checks          (default 15)
#   ROLLOUT_SKIP_IDLE        `true` skips services whose desired count is 0 (they are switched
#                            off in DAD-infra's config, the one owner of that switch) and
#                            rolls the rest. When every service is idle it exits 0.
#   DAD_TASKDEFS_FILE        file of `<family>=<task definition ARN>` lines, as printed by
#                            ecs-pin-image.sh. When set, each service is deployed on the
#                            pinned revision of family `dad-<service>` (--task-definition);
#                            a service with no entry is an error. When unset, the service's
#                            current task definition is redeployed as is.
#
# For each service this starts a new deployment, remembers that deployment's id, then
# polls until THAT deployment's rolloutState is COMPLETED. It fails if the deployment
# ends FAILED or does not finish in time.
#
# Why the specific id, not "the current deployment": the services use the ECS
# deployment circuit breaker with rollback (DAD-infra apps/dad-main). When a rollout
# cannot reach a steady state, ECS rolls back by creating a NEW deployment of the
# previous task definition, and that rollback deployment is the one that reports
# COMPLETED. Asking "is the primary deployment COMPLETED" would pass after a failed
# rollout. The deployment this script started ends FAILED, which is what it checks.
#
# The health that decides COMPLETED vs FAILED comes from the container health checks in
# the task definitions, so no shell into a running task is needed.
set -euo pipefail

usage() {
  echo "usage: ecs-rollout.sh <cluster> <service> [<service> ...]" >&2
  exit 2
}

[ "$#" -ge 2 ] || usage
cluster="$1"
shift
services=("$@")

timeout="${ROLLOUT_TIMEOUT_SECONDS:-1200}"
interval="${ROLLOUT_POLL_SECONDS:-15}"
taskdefs_file="${DAD_TASKDEFS_FILE:-}"
if [ -n "$taskdefs_file" ] && [ ! -r "$taskdefs_file" ]; then
  echo "::error::cannot read $taskdefs_file" >&2
  exit 1
fi

# Drop services that are switched off (desired count 0) when asked to. They are not
# deployed: when DAD-infra switches them on, CloudFormation starts them at the image tag
# recorded for the last successful release.
if [ "${ROLLOUT_SKIP_IDLE:-false}" = "true" ]; then
  active=()
  for svc in "${services[@]}"; do
    desired="$(aws ecs describe-services \
      --cluster "$cluster" \
      --services "$svc" \
      --query 'services[0].desiredCount' \
      --output text)"
    case "$desired" in
      0) echo "$svc: desired count 0 (switched off); skipping" ;;
      '' | None) echo "::error::$svc: no such service in $cluster" >&2; exit 1 ;;
      *) active+=("$svc") ;;
    esac
  done
  if [ "${#active[@]}" -eq 0 ]; then
    echo "every service is switched off; nothing to roll"
    exit 0
  fi
  services=("${active[@]}")
fi

# Print the pinned task definition ARN for the service's family (dad-<service>).
pinned_taskdef() {
  local family="dad-$1" line
  line="$(grep -E "^${family}=" "$taskdefs_file" | head -n 1 || true)"
  if [ -z "$line" ]; then
    echo "::error::no pinned task definition for '$family' in $taskdefs_file" >&2
    return 1
  fi
  printf '%s' "${line#*=}"
}

# Resolve every pinned revision before touching any service, so a missing entry cannot
# leave some services rolling and others not.
taskdefs=()
for svc in "${services[@]}"; do
  if [ -n "$taskdefs_file" ]; then
    taskdefs+=("$(pinned_taskdef "$svc")")
  else
    taskdefs+=("")
  fi
done

# Start every deployment first so they roll out in parallel, then wait on each.
deployment_ids=()
for i in "${!services[@]}"; do
  svc="${services[$i]}"
  update_args=(--cluster "$cluster" --service "$svc" --force-new-deployment)
  if [ -n "${taskdefs[$i]}" ]; then
    update_args+=(--task-definition "${taskdefs[$i]}")
  fi
  id="$(aws ecs update-service \
    "${update_args[@]}" \
    --query "service.deployments[?status=='PRIMARY'] | [0].id" \
    --output text)"
  if [ -z "$id" ] || [ "$id" = "None" ]; then
    echo "::error::update-service for '$svc' returned no PRIMARY deployment id" >&2
    exit 1
  fi
  echo "started $svc deployment $id"
  deployment_ids+=("$id")
done

failed=0
for i in "${!services[@]}"; do
  svc="${services[$i]}"
  id="${deployment_ids[$i]}"
  deadline=$(($(date +%s) + timeout))
  seen=false
  while :; do
    state="$(aws ecs describe-services \
      --cluster "$cluster" \
      --services "$svc" \
      --query "services[0].deployments[?id=='$id'] | [0].rolloutState" \
      --output text)"
    case "$state" in
      COMPLETED)
        echo "$svc: deployment $id COMPLETED"
        break
        ;;
      FAILED)
        reason="$(aws ecs describe-services \
          --cluster "$cluster" \
          --services "$svc" \
          --query "services[0].deployments[?id=='$id'] | [0].rolloutStateReason" \
          --output text)"
        echo "::error::$svc: deployment $id FAILED and was rolled back: $reason" >&2
        failed=1
        break
        ;;
      None)
        if [ "$seen" = true ]; then
          echo "::error::$svc: deployment $id is no longer listed; another deployment superseded it" >&2
          failed=1
          break
        fi
        if [ "$(date +%s)" -ge "$deadline" ]; then
          echo "::error::$svc: deployment $id still '$state' after ${timeout}s" >&2
          failed=1
          break
        fi
        sleep "$interval"
        ;;
      *)
        seen=true
        if [ "$(date +%s)" -ge "$deadline" ]; then
          echo "::error::$svc: deployment $id still '$state' after ${timeout}s" >&2
          failed=1
          break
        fi
        sleep "$interval"
        ;;
    esac
  done
done

exit "$failed"
