#!/usr/bin/env bash
# shellcheck disable=SC2016 # Single-quoted variables expand inside the postgres container.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="${ENV_FILE:-$root/.env.production}"
[[ "$ENV_FILE" == /* ]] || ENV_FILE="$PWD/$ENV_FILE"
DATABASE_ENV_FILE="${DATABASE_ENV_FILE:-$root/.env.database}"
[[ "$DATABASE_ENV_FILE" == /* ]] || DATABASE_ENV_FILE="$PWD/$DATABASE_ENV_FILE"
export ENV_FILE
cd "$root"
compose() { docker compose --env-file "$ENV_FILE" --env-file "$DATABASE_ENV_FILE" -f docker-compose.prod.yml "$@"; }
if [[ $# != 1 || "$1" != /* || ! -f "$1" ]]; then
  printf '%s\n' 'Usage: scripts/verify-restore.sh /absolute/path/to/backup.dump' >&2
  exit 2
fi
# A fresh, server-generated name prevents restoring over an application database.
name="restore_check_$(compose exec -T postgres sh -c 'psql -X -At -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT replace(gen_random_uuid()::text,chr(45),chr(95))"')"
[[ "$name" =~ ^restore_check_[a-f0-9_]{36}$ ]] || { printf '%s\n' 'could not generate a disposable database name' >&2; exit 1; }
compose exec -T postgres sh -c 'createdb -U "$POSTGRES_USER" "$1"' sh "$name"
cleanup() { compose exec -T postgres sh -c 'dropdb -U "$POSTGRES_USER" "$1"' sh "$name"; }
trap cleanup EXIT
compose exec -T postgres sh -c 'pg_restore -U "$POSTGRES_USER" -d "$1" --no-owner --no-acl --exit-on-error' sh "$name" < "$1"
compose exec -T postgres sh -c 'psql -X -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$1" -c "SELECT count(*) AS donors FROM donors; SELECT count(*) AS gifts FROM donor_gifts; SELECT count(*) AS identity_events FROM donor_identity_events;"' sh "$name"
printf '%s\n' 'Restore succeeded in a disposable database; cleanup follows. Verify separately stored encryption keys in an isolated application rehearsal.'
