#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
: "${TEST_DATABASE_URL:?set the migrated synthetic donor_go test database}"
ios="${DAD_IOS_DIR:-../DAD-iOS}"
android="${DAD_ANDROID_DIR:-../DAD-Android}"
[ -d "$ios" ] || { echo "set DAD_IOS_DIR to the DAD-iOS checkout" >&2; exit 1; }
[ -d "$android" ] || { echo "set DAD_ANDROID_DIR to the DAD-Android checkout" >&2; exit 1; }
ios="$(cd "$ios" && pwd)"
android="$(cd "$android" && pwd)"
work="$(mktemp -d "${TMPDIR:-/tmp}/dad-native-acceptance.XXXXXX")"
server=""
cleanup() {
  touch "$work/done"
  if [ -n "$server" ]; then wait "$server" || cat "$work/server.log"; fi
  rm -rf "$work"
}
trap cleanup EXIT
(
  cd apps/donor-api
  DONOR_NATIVE_ACCEPTANCE_DIR="$work" go test -race ./internal/httpapi -run '^TestAppSessionLifecycle$' -count=1 -timeout=10m
) > "$work/server.log" 2>&1 &
server=$!
ready=false
for _ in $(seq 1 120); do
  if [ -f "$work/ready" ]; then ready=true; break; fi
  if ! kill -0 "$server" 2>/dev/null; then cat "$work/server.log"; exit 1; fi
  sleep 1
done
if [ "$ready" != true ]; then echo "Native test API did not start" >&2; exit 1; fi
(
  cd "$ios"
  node scripts/prepare-client.mjs
  DONOR_NATIVE_FIXTURE="$work/ios.json" swift test --scratch-path "${DAD_SWIFT_BUILD:-$work/swift-build}" \
    --force-resolved-versions -Xswiftc -DDAD_LIVE_API_TEST --filter LiveAPIIntegrationTests
)
(
  cd "$android"
  ./gradlew :app:testDebugUnitTest -PdadIntegrationFixture="$work/android.json" \
    --tests org.dad.foundation.LiveAPIIntegrationTest --warning-mode all
)
touch "$work/done"
wait "$server"
server=""
echo "Both native clients passed the real HTTPS API session lifecycle."
