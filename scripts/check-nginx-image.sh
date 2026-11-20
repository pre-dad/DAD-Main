#!/usr/bin/env bash
#
# check-nginx-image.sh — build the ECS ingress image (deploy/nginx/Dockerfile) and check it.
#
# Usage: scripts/check-nginx-image.sh
#
# docker-compose.prod.yml bind-mounts deploy/nginx into the stock nginx image;
# scripts/check-ingress.sh checks that path. Fargate cannot bind-mount, so ECS runs the
# image built here instead. This checks the differences, with a throwaway certificate:
#   - the base image is exactly the one Compose pins;
#   - the certificate and key arrive from the environment (ECS secrets) and are written
#     0600 for uid 101;
#   - the Docker-only resolver is gone and `web` and `native_api` use static upstreams;
#   - nginx starts as uid 101, passes `nginx -t`, and answers /health on 127.0.0.1:8081;
#   - a half-configured certificate is refused.
# `web` must resolve when nginx starts; on ECS, Service Connect provides it. Here an
# --add-host entry stands in for it.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

image="dad/nginx:check-$$"
name="dad-nginx-check-$$"
work="$(mktemp -d "${TMPDIR:-/tmp}/nginx-image-check.XXXXXX")"
cleanup() {
  docker rm -f "$name" >/dev/null 2>&1 || true
  docker rmi "$image" >/dev/null 2>&1 || true
  rm -rf -- "$work"
}
trap cleanup EXIT

fail() {
  echo "check-nginx-image: $*" >&2
  docker logs "$name" 2>&1 | tail -n 20 >&2 || true
  exit 1
}

# 1. One nginx pin, not two that can drift.
compose_image="$(sed -n 's/^ *image: \(nginx:.*\)$/\1/p' docker-compose.prod.yml)"
dockerfile_image="$(sed -n 's/^FROM \(nginx:.*\)$/\1/p' deploy/nginx/Dockerfile)"
[[ -n "$compose_image" && "$compose_image" == "$dockerfile_image" ]] ||
  fail "base image differs: compose='$compose_image' Dockerfile='$dockerfile_image'"

docker build -q -t "$image" deploy/nginx >/dev/null

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 \
  -subj /CN=ingress-check -addext 'subjectAltName=DNS:public.test,DNS:staff.test,DNS:native.test' \
  -keyout "$work/privkey.pem" -out "$work/fullchain.pem" 2>/dev/null

common=(
  -e INGRESS_MODE=cloudflare
  -e DONOR_PUBLIC_ORIGIN=https://public.test
  -e DONOR_STAFF_ORIGIN=https://staff.test
  -e DONOR_NATIVE_API_ORIGIN=https://native.test
  -e 'NGINX_ENVSUBST_FILTER=^INGRESS_'
  -e NGINX_ENTRYPOINT_QUIET_LOGS=1
  --add-host web:127.0.0.1
  --add-host api:127.0.0.1
)

# 3. Start with the certificate in the environment, as ECS provides it.
docker run -d --name "$name" "${common[@]}" \
  -e "INGRESS_TLS_FULLCHAIN=$(cat "$work/fullchain.pem")" \
  -e "INGRESS_TLS_PRIVKEY=$(cat "$work/privkey.pem")" \
  "$image" >/dev/null

healthy=false
for _ in $(seq 1 30); do
  if docker exec "$name" wget -q -O /dev/null http://127.0.0.1:8081/health 2>/dev/null; then
    healthy=true
    break
  fi
  [[ "$(docker inspect -f '{{.State.Running}}' "$name")" == "true" ]] || fail "nginx exited during start"
  sleep 1
done
$healthy || fail "nginx never answered /health on 127.0.0.1:8081"

docker exec "$name" nginx -t >/dev/null 2>&1 || fail "nginx -t failed"

[[ "$(docker exec "$name" id -u)" == "101" ]] || fail "nginx must not run as root"

for f in fullchain.pem privkey.pem; do
  [[ "$(docker exec "$name" stat -c '%a %u' "/etc/nginx/tls/$f")" == "600 101" ]] ||
    fail "/etc/nginx/tls/$f must be mode 600 owned by uid 101"
done
docker exec "$name" cat /etc/nginx/tls/privkey.pem | cmp -s - "$work/privkey.pem" ||
  fail "the key on disk does not match the one supplied"

docker exec "$name" test ! -e /etc/nginx/conf.d/default.conf || fail "default.conf must be removed"
docker exec "$name" sh -c "! grep -q 'resolver' /etc/nginx/nginx.conf" ||
  fail "nginx.conf still has a resolver (Docker's 127.0.0.11 does not exist on Fargate)"
docker exec "$name" grep -q 'server web:3000;' /etc/nginx/nginx.conf ||
  fail "upstream must be the static 'server web:3000;'"

docker exec "$name" grep -q 'server api:8002;' /etc/nginx/conf.d/ingress.conf ||
  fail "native upstream must resolve api:8002 at start through Service Connect"
docker exec "$name" sh -c "! grep -q ' resolve;' /etc/nginx/conf.d/ingress.conf" ||
  fail "native upstream must not require Docker's runtime resolver"

# 5. Half a certificate must be refused, not silently ignored.
if timeout 30 docker run --rm "${common[@]}" -e "INGRESS_TLS_FULLCHAIN=x" "$image" >"$work/half.log" 2>&1; then
  fail "starting with only INGRESS_TLS_FULLCHAIN must fail"
fi
grep -q 'set both INGRESS_TLS_FULLCHAIN and INGRESS_TLS_PRIVKEY' "$work/half.log" ||
  fail "the half-configured certificate was refused for the wrong reason: $(cat "$work/half.log")"

echo "check-nginx-image: ok"
