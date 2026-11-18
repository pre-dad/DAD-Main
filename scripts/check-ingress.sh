#!/usr/bin/env bash
# `config` renders the production nginx service in both modes and runs `nginx -t`;
# `smoke` runs it in front of a stub web service and checks its behaviour over HTTP;
# `web IMAGE` runs it in front of a web image built for https://public.test and
# https://staff.test, with no API. All use docker-compose.prod.yml itself, under a
# throwaway project, certificate, and env.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"
usage() { printf '%s\n' 'Usage: scripts/check-ingress.sh {config|smoke|web IMAGE}'; }
case "${1:-config}" in
  config | smoke) command="${1:-config}" ;;
  web) [[ -n "${2:-}" ]] || { usage >&2; exit 2; }; command=web web_image="$2" ;;
  -h | --help) usage; exit 0 ;;
  *) usage >&2; exit 2 ;;
esac

project="dad-ingress-check-$$"
work="$(mktemp -d "${TMPDIR:-/tmp}/ingress-check.XXXXXX")"
image="$(sed -n 's/^ *image: \(nginx:.*\)$/\1/p' docker-compose.prod.yml)"
files=(-f docker-compose.prod.yml)
compose() { docker compose -p "$project" --env-file "$work/env" "${files[@]}" "$@"; }
cleanup() {
  compose down -v --remove-orphans >/dev/null 2>&1 || true
  rm -rf -- "$work"
}
trap cleanup EXIT

# The container user (uid 101) must be able to read the throwaway key on Linux hosts.
chmod 755 "$work"
mkdir -m 755 "$work/tls" "$work/pki"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj /CN=ingress-check \
  -addext 'subjectAltName=DNS:public.test,DNS:staff.test' \
  -keyout "$work/tls/privkey.pem" -out "$work/tls/fullchain.pem" 2>/dev/null
chmod 644 "$work/tls/privkey.pem"
cat > "$work/env" <<EOF
IMAGE_TAG=ingress-check
POSTGRES_USER=check
POSTGRES_PASSWORD=check
DONOR_PUBLIC_ORIGIN=https://public.test
DONOR_STAFF_ORIGIN=https://staff.test
INGRESS_TLS_DIR=$work/tls
EOF
# No database is involved, but Compose requires every service's database URL.
grep -o 'DONOR_[A-Z_]*DATABASE_URL' docker-compose.prod.yml | sort -u | sed 's|$|=postgresql://check@postgres/check|' >> "$work/env"
export ENV_FILE="$work/env"
# Leaves the production subnet free for a real deployment on the same Docker host. The upper
# half of the fixed edge subnet stands in for Cloudflare's ranges in the smoke test.
cat > "$work/check.yml" <<EOF
networks:
  default:
    ipam:
      config: !reset []
  edge:
    ipam:
      config: !override
        - subnet: 172.31.243.0/24
          ip_range: 172.31.243.0/25
        - subnet: fd42:dad:c4ec::/64
EOF
files+=(-f "$work/check.yml")
failures=0
check() {
  if [[ "$3" == "$2" ]]; then
    printf 'ok    %s\n' "$1"
  else
    printf 'FAIL  %s: expected %q, got %q\n' "$1" "$2" "$3"
    failures=$((failures + 1))
  fi
}
header() { awk -v name="$1" -F': ' 'tolower($1) == name { print $2 }' <<< "$2"; }
finish() {
  if ((failures)); then
    printf '%d ingress check(s) failed.\n' "$failures" >&2
    exit 1
  fi
  printf 'All ingress checks passed.\n'
}

if [[ "$command" == config ]]; then
  for mode in cloudflare direct; do
    if ! output="$(INGRESS_MODE="$mode" compose run --rm --no-deps -T nginx nginx -t -q 2>&1)"; then
      printf '%s\nnginx -t failed in %s mode.\n' "$output" "$mode" >&2
      exit 1
    fi
    printf 'nginx -t (%s mode): ok\n' "$mode"
  done
  if INGRESS_MODE=bogus compose run --rm --no-deps -T nginx nginx -t -q >/dev/null 2>&1; then
    printf 'An unknown INGRESS_MODE was accepted.\n' >&2
    exit 1
  fi
  printf 'unknown INGRESS_MODE rejected: ok\n'
  exit 0
fi

if [[ "$command" == web ]]; then
  cat > "$work/web.yml" <<EOF
services:
  web:
    image: $web_image
    build: !reset null
    depends_on: !reset {}
    networks: !override [edge]
    healthcheck:
      interval: 1s
  nginx:
    ports: !reset []
    healthcheck:
      interval: 1s
EOF
  files+=(-f "$work/web.yml")
  if ! INGRESS_MODE=direct compose up -d --wait nginx >/dev/null 2>&1; then
    compose logs web nginx >&2
    exit 1
  fi
  fetch() { compose exec -T nginx curl -sS -k --connect-to :443:127.0.0.1:8443 "$@"; }
  headers() { fetch -o /dev/null -D - "$1" | tr -d '\r'; }
  page() { local h; h="$(headers "$1")"; printf '%s %s' "$(awk 'NR == 1 { print $2 }' <<< "$h")" "$(header content-type "$h")"; }
  check 'public home page is HTML' '200 text/html; charset=utf-8' "$(page https://public.test/)"
  check 'staff sign-in page is HTML' '200 text/html; charset=utf-8' "$(page https://staff.test/staff/sign-in)"
  for host in public.test staff.test; do
    check "$host serves the tab icon" '200 image/svg+xml' "$(page "https://$host/icon.svg")"
  done
  check 'public host hides the staff sign-in page' 404 "$(page https://public.test/staff/sign-in | cut -d' ' -f1)"
  check 'staff host hides public pages' 404 "$(page https://staff.test/how-it-works | cut -d' ' -f1)"
  check 'staff host sends / to the back office' https://staff.test/staff \
    "$(header location "$(headers https://staff.test/)")"
  check 'staff robots.txt disallows everything' 'Disallow: /' "$(fetch https://staff.test/robots.txt | tail -1)"
  check 'staff page carries one X-Robots-Tag' 'noindex, nofollow' \
    "$(header x-robots-tag "$(headers https://staff.test/staff/sign-in)")"
  finish
  exit
fi

# Smoke: a stub replaces the web service and echoes what nginx forwarded. "Cloudflare"
# presents a client certificate from a throwaway origin-pull CA.
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj /CN=origin-pull-check \
  -keyout "$work/pki/ca.key" -out "$work/pki/ca.pem" 2>/dev/null
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj /CN=edge-check \
  -keyout "$work/pki/edge.key" -out "$work/pki/edge.csr" 2>/dev/null
openssl x509 -req -days 1 -in "$work/pki/edge.csr" -CA "$work/pki/ca.pem" -CAkey "$work/pki/ca.key" \
  -CAcreateserial -out "$work/pki/edge.pem" 2>/dev/null
chmod 644 "$work"/pki/*
printf 'set_real_ip_from 172.31.243.128/25;\n' > "$work/edge-ranges.conf"
cat > "$work/stub.conf" <<'EOF'
pid /tmp/nginx.pid;
error_log stderr error;
events {}
http {
    client_body_temp_path /tmp/body;
    proxy_temp_path /tmp/proxy;
    fastcgi_temp_path /tmp/fastcgi;
    uwsgi_temp_path /tmp/uwsgi;
    scgi_temp_path /tmp/scgi;
    access_log off;
    server {
        listen 3000;
        default_type text/plain;
        add_header X-Robots-Tag "noindex, nofollow";
        return 200 "host=$host\nuri=$request_uri\nx-real-ip=$http_x_real_ip\nx-forwarded-for=$http_x_forwarded_for\ncf-connecting-ip=$http_cf_connecting_ip\n";
    }
}
EOF
cat > "$work/stub.yml" <<EOF
services:
  web:
    image: $image
    build: !reset null
    depends_on: !reset {}
    environment: !reset {}
    entrypoint: [nginx, -c, /stub/nginx.conf, -g, daemon off;]
    user: "101:101"
    volumes: ["$work/stub.conf:/stub/nginx.conf:ro", "$work/pki:/pki:ro"]
    tmpfs: !override ["/tmp:uid=101,gid=101"]
    networks: !override [edge]
    healthcheck:
      test: [CMD, wget, -q, -O, /dev/null, http://127.0.0.1:3000/]
      interval: 1s
      retries: 30
  nginx:
    ports: !reset []
    volumes: ["$work/edge-ranges.conf:/etc/nginx/ingress/cloudflare/edge-ranges.conf:ro"]
    healthcheck:
      interval: 1s
EOF
files+=(-f "$work/stub.yml")

# Requests come from the stub, a peer outside the trusted ranges.
client() { compose exec -T web curl -sS -k --http1.1 --connect-to :443:nginx:8443 --connect-to :80:nginx:8080 "${family:--4}" "$@"; }
status() { client -o /dev/null -w '%{http_code}' "$@" 2>/dev/null || true; }
exit_code() { client -o /dev/null "$@" 2>/dev/null && echo 0 || echo "$?"; }
field() { sed -n "s/^$1=//p" <<< "$2"; }
spoof=(-H 'X-Real-IP: 203.0.113.9' -H 'X-Forwarded-For: 203.0.113.9' -H 'CF-Connecting-IP: 203.0.113.9')
counts() { client -w '%{http_code}\n' "$@" | sort | uniq -c | awk '{printf "%s:%s ", $2, $1}'; }
repeat() { local i; urls=(); for ((i = 0; i < $2; i++)); do urls+=(-o /dev/null "$1"); done; }

printf -- '-- direct mode\n'
INGRESS_MODE=direct compose up -d --wait --quiet-pull nginx >/dev/null 2>&1
peer="$(docker inspect -f "{{(index .NetworkSettings.Networks \"${project}_edge\").IPAddress}}" "$(compose ps -q web)")"

check 'HTTP redirects to HTTPS' '301 https://public.test/donate?amount=1' \
  "$(client -o /dev/null -w '%{http_code} %{redirect_url}' 'http://public.test/donate?amount=1')"
check 'unknown host over HTTP gets no response' 000 "$(status http://other.test/)"
check 'unknown TLS server name is refused (curl 35)' 35 "$(exit_code https://other.test/)"
check 'mismatched Host header is misdirected' 421 "$(status -H 'Host: other.test' https://public.test/)"

body="$(client "${spoof[@]}" 'https://public.test/donate?amount=1')"
check 'public page reaches web with its query' '/donate?amount=1' "$(field uri "$body")"
check 'X-Real-IP is the peer, not the client claim' "$peer" "$(field x-real-ip "$body")"
check 'X-Forwarded-For is the peer only' "$peer" "$(field x-forwarded-for "$body")"
check 'CF-Connecting-IP is dropped' '' "$(field cf-connecting-ip "$body")"
peer6="$(docker inspect -f "{{(index .NetworkSettings.Networks \"${project}_edge\").GlobalIPv6Address}}" "$(compose ps -q web)")"
check 'an IPv6 client keeps its own address' "$peer6" "$(field x-real-ip "$(family=-6 client https://public.test/)")"

for path in /staff /staff/sign-in /STAFF/audit /api/donor/staff/me /api/donor/staff/sign-in; do
  check "public host hides $path" 404 "$(status "https://public.test$path")"
done
check 'public 404 is JSON' '{"detail":"Not found"}' "$(client https://public.test/staff)"
body="$(client https://staff.test/)"
check 'staff host serves /' staff.test "$(field host "$body")"
for path in /staff /staff/donors/1 /api/donor/staff/me /_next/static/app.js /robots.txt /icon.svg; do
  check "staff host serves $path" 200 "$(status "https://staff.test$path")"
done
for path in /donate /r/abc /account/sign-in /api/donor/me/profile /api/donor/stripe/webhook /api/donor/staffing /icon.svg/extra; do
  check "staff host hides $path" 404 "$(status "https://staff.test$path")"
done
check 'staff host is not indexed, with one header' 'noindex, nofollow' \
  "$(header x-robots-tag "$(client -o /dev/null -D - https://staff.test/staff | tr -d '\r')")"

post_bytes() { head -c "$1" /dev/zero | status -X POST --data-binary @- "$2"; }
check 'webhook accepts 256 KiB' 200 "$(post_bytes 262144 https://public.test/api/donor/stripe/webhook)"
check 'webhook refuses larger bodies' 413 "$(post_bytes 262145 https://public.test/api/donor/stripe/webhook)"
check 'public API refuses bodies over 256 KiB' 413 "$(post_bytes 262145 https://public.test/api/donor/checkouts)"
check 'staff source upload accepts 10 MiB' 200 "$(post_bytes 10485760 https://staff.test/api/donor/staff/content/sources)"
check 'staff source upload refuses more' 413 "$(post_bytes 10485761 https://staff.test/api/donor/staff/content/sources)"
for route in https://public.test/api/donor/partner/logo https://staff.test/api/donor/staff/partners/00000000-0000-4000-8000-000000000001/logo; do
  check "$route accepts the API logo limit" 200 "$(post_bytes 360000 "$route")"
  check "$route refuses larger logos" 413 "$(post_bytes 360001 "$route")"
done
check 'founding-claim imports accept 5 MiB' 200 "$(post_bytes 5242880 https://staff.test/api/donor/staff/founding-claims)"
check 'founding-claim imports refuse more' 413 "$(post_bytes 5242881 https://staff.test/api/donor/staff/founding-claims)"
check 'recognition artwork batches accept 10 MiB' 200 "$(post_bytes 10485760 https://staff.test/api/donor/staff/content/rules)"
check 'recognition artwork batches refuse more' 413 "$(post_bytes 10485761 https://staff.test/api/donor/staff/content/rules)"
check 'other staff API refuses bodies over 256 KiB' 413 "$(post_bytes 262145 https://staff.test/api/donor/staff/accounts)"
check 'webhook accepts only POST' 403 "$(status https://public.test/api/donor/stripe/webhook)"
repeat https://public.test/api/donor/stripe/webhook 1000
webhooks="$(counts -X POST "${urls[@]}")"
check 'webhook admits a burst of 500, then limits' yes \
  "$(awk -F'[: ]' '{ ok = $1 == 200 && $2 >= 500 && $3 == 429 } END { print ok ? "yes" : $0 }' <<< "$webhooks")"

repeat https://public.test/api/donor/login 30
check 'sign-in admits a burst of 21, then limits' '200:21 429:9 ' "$(counts -X POST "${urls[@]}")"
headers="$(client -D - -o "/tmp/limited" -X POST https://public.test/api/donor/login | tr -d '\r')"
check 'sign-in 429 has Retry-After' 60 "$(header retry-after "$headers")"
check 'sign-in 429 is not cacheable' no-store "$(header cache-control "$headers")"
check 'sign-in 429 body is JSON' '{"detail":"Too many requests"}' "$(compose exec -T web cat /tmp/limited)"
check 'passkey sign-in shares the sign-in limit' '429 429' \
  "$(status -X POST https://public.test/api/donor/passkeys/sign-in/options) $(status -X POST https://public.test/api/donor/passkeys/sign-in)"
headers="$(client -D - -o /dev/null -X POST https://staff.test/api/donor/staff/sign-in | tr -d '\r')"
check 'staff host 429 is not indexed' '429 noindex, nofollow' \
  "$(awk 'NR == 1 { print $2 }' <<< "$headers") $(header x-robots-tag "$headers")"
repeat https://public.test/r/abc 400
pages="$(counts "${urls[@]}")"
check 'pages admit a burst of 300, then limit' yes \
  "$(awk -F'[: ]' '{ ok = $1 == 200 && $2 >= 300 && $3 == 429 } END { print ok ? "yes" : $0 }' <<< "$pages")"

client -o /dev/null 'https://public.test/r/SECRETCODE?token=SECRETTOKEN' || true
client -o /dev/null 'https://staff.test/staff?q=private%40example.org' || true
log="$(compose logs --no-log-prefix nginx 2>/dev/null)"
check 'access log redacts referral codes' yes "$(grep -q '"path":"/r/:code","status":200' <<< "$log" && echo yes)"
check 'access log omits query strings and codes' no "$(grep -qE 'SECRET|private@|private%40|amount=' <<< "$log" && echo yes || echo no)"

printf -- '-- cloudflare mode\n'
INGRESS_MODE=cloudflare INGRESS_ORIGIN_PULL_CA="$work/pki/ca.pem" compose up -d --wait nginx >/dev/null 2>&1
check 'origin pull certificate is required' 400 "$(status https://public.test/)"
cert=(--cert /pki/edge.pem --key /pki/edge.key)
body="$(client "${cert[@]}" "${spoof[@]}" https://public.test/)"
check 'CF-Connecting-IP from outside Cloudflare is ignored' "$peer" "$(field x-real-ip "$body")"
body="$(docker run --rm --network "${project}_edge" --ip 172.31.243.200 -v "$work/pki:/pki:ro" "$image" \
  curl -sS -k --http1.1 -4 --connect-to :443:nginx:8443 "${cert[@]}" -H 'CF-Connecting-IP: 198.51.100.7' \
  -H 'X-Real-IP: 203.0.113.9' -H 'X-Forwarded-For: 203.0.113.9' https://public.test/)"
check 'X-Real-IP is CF-Connecting-IP from Cloudflare' 198.51.100.7 "$(field x-real-ip "$body")"
check 'X-Forwarded-For is CF-Connecting-IP from Cloudflare' 198.51.100.7 "$(field x-forwarded-for "$body")"
check 'CF-Connecting-IP is not forwarded' '' "$(field cf-connecting-ip "$body")"
check 'staff host serves Cloudflare connections' 200 "$(status "${cert[@]}" https://staff.test/staff)"

# One curl, so the auth zone (one request per 3 s) cannot refill between the groups.
as_client() {
  printf -- '%s\n' --next -sS -k --http1.1 -4 --connect-to :443:nginx:8443 --cert /pki/edge.pem --key /pki/edge.key \
    -X POST -w '%{http_code}\n' -H "CF-Connecting-IP: $1"
  local i
  for ((i = 0; i < $2; i++)); do printf '%s\n' -o /dev/null https://public.test/api/donor/login; done
}
mapfile -t args < <(as_client 2001:db8:a:b::1 21; as_client 2001:db8:a:b:ffff:ffff:ffff:fffe 1;
  as_client 2001:db8:a:c::1 1; as_client 2001:db8::1 21; as_client 2001:db8::8000:0:0:1 1; as_client 198.51.100.8 1)
codes="$(docker run --rm --network "${project}_edge" --ip 172.31.243.200 -v "$work/pki:/pki:ro" "$image" \
  curl "${args[@]:1}" | tr '\n' ' ')"
check 'IPv6 clients share limits per /64' \
  "$(printf '200 %.0s' {1..21})429 200 $(printf '200 %.0s' {1..21})429 200 " "$codes"

finish
