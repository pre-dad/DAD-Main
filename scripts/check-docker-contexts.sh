#!/usr/bin/env bash
#
# check-docker-contexts.sh — every image build must use a context that holds the files
# its Dockerfile copies.
#
# Usage: scripts/check-docker-contexts.sh
#
# apps/donor-api/Dockerfile and apps/web/Dockerfile copy `apps/...` paths, so they only
# build with the repository root as the context. This reads every `docker build` in the
# Makefile, scripts and workflows, plus every compose `build:` block, and asserts each
# COPY/ADD source (except `--from=` stages) exists relative to the context it is built
# with. It needs no Docker daemon.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

status=0
checked=0

# check <where> <dockerfile> <context>
check() {
  local where="$1" dockerfile="$2" context="$3" line source
  if [ ! -f "$dockerfile" ]; then
    echo "FAIL $where: Dockerfile '$dockerfile' not found" >&2
    status=1
    return
  fi
  if [ ! -d "$context" ]; then
    echo "FAIL $where: build context '$context' not found" >&2
    status=1
    return
  fi
  checked=$((checked + 1))
  while IFS= read -r line; do
    set -f
    # shellcheck disable=SC2086
    set -- $line
    set +f
    shift
    while [[ "${1:-}" == --* ]]; do shift; done
    while [ "$#" -gt 1 ]; do
      source="$1"
      shift
      # shellcheck disable=SC2086
      if ! compgen -G "$context/$source" >/dev/null; then
        echo "FAIL $where: $dockerfile copies '$source', not found in context '$context'" >&2
        status=1
      fi
    done
  done < <(sed -e ':a' -e '/\\$/N; s/\\\n//; ta' "$dockerfile" | grep -E '^(COPY|ADD) ' | grep -v -- '--from=' || true)
}

# `docker build ... -f <dockerfile> ... <context>` (continuation lines joined).
while IFS='|' read -r where command; do
  dockerfile=""
  context=""
  set -f
  # shellcheck disable=SC2086
  set -- $command
  set +f
  while [ "$#" -gt 0 ]; do
    case "$1" in
      -f | --file) dockerfile="$2"; shift 2 ;;
      -t | --tag | --build-arg | --target | --platform) shift 2 ;;
      -*) shift ;;
      *) context="$1"; shift ;;
    esac
  done
  [ -n "$dockerfile" ] || dockerfile="$context/Dockerfile"
  # Parameterised builds (a shell function) are checked at their call sites below.
  case "$dockerfile$context" in *'$'*) continue ;; esac
  check "$where" "$dockerfile" "$context"
done < <(
  for file in Makefile scripts/*.sh .github/workflows/*.yml; do
    [ "$file" = "scripts/check-docker-contexts.sh" ] && continue
    sed -e ':a' -e '/\\$/N; s/\\\n//; ta' "$file" \
      | grep -E 'docker build ' \
      | grep -v '^[[:space:]]*#' \
      | sed -E "s#^.*docker build ##; s# *(>|&&|;|\\|\\|).*\$##; s#[\"'\`]##g; s#^#$file|#" || true
  done
)

# `build_push <name> <dockerfile> <context>` call sites in the deploy workflow.
while read -r _ name dockerfile context; do
  check "deploy.yml build_push $name" "$dockerfile" "$context"
done < <(grep -E '^[[:space:]]+build_push [a-z]+ [^ ]+ [^ ]+' .github/workflows/deploy.yml | sed -E 's/^[[:space:]]+//; s/ \\$//')

# compose `build:` blocks: context + dockerfile.
for file in docker-compose*.yml; do
  while read -r context dockerfile; do
    check "$file" "$dockerfile" "$context"
  done < <(awk '
    /^[[:space:]]+context:/ { context = $2 }
    /^[[:space:]]+dockerfile:/ { print context, $2; context = "" }
  ' "$file")
done

if [ "$checked" -eq 0 ]; then
  echo "FAIL no image builds found; the check is not reading anything" >&2
  exit 1
fi
[ "$status" -eq 0 ] && echo "docker build contexts ok ($checked builds checked)"
exit "$status"
