#!/usr/bin/env bash
# End-to-end test of `belay update` against real Docker: healthy update, crash->rollback, skip.
# Requires: docker, go. Run from the repo root: ./test/integration.sh
set -euo pipefail
cd "$(dirname "$0")/.."

bin=$(mktemp -d)/belay
go build -o "$bin" ./cmd/belay

d=$(mktemp -d)
printf 'FROM alpine:3.20\nCMD ["sleep","infinity"]\n' >"$d/good.Dockerfile"
printf 'FROM alpine:3.20\nCMD ["sh","-c","echo BOOM-bad-config; sleep 1; exit 1"]\n' >"$d/bad.Dockerfile"
docker build -q -t belay-test:good -f "$d/good.Dockerfile" "$d" >/dev/null
docker tag belay-test:good belay-test:good2
docker build -q -t belay-test:bad -f "$d/bad.Dockerfile" "$d" >/dev/null

proj=$(mktemp -d)
cat >"$proj/docker-compose.yml" <<'EOF'
services:
  demo:
    image: belay-test:good
    restart: "no"
EOF
cf="$proj/docker-compose.yml"
running_image() { docker inspect -f '{{.Config.Image}}' "$(docker compose -f "$cf" ps -aq demo | head -1)"; }
file_tag() { awk '/image:/{print $2}' "$cf"; }

cleanup() {
	docker compose -f "$cf" down >/dev/null 2>&1 || true
	docker rmi -f belay-test:good belay-test:good2 belay-test:bad >/dev/null 2>&1 || true
	rm -rf "$d" "$proj"
}
trap cleanup EXIT

fail() { echo "FAIL: $1"; exit 1; }

echo "# 1. healthy update (good -> good2)"
"$bin" update --min-uptime 3s --timeout 30s "$proj" demo belay-test:good2
[ "$(running_image)" = "belay-test:good2" ] || fail "not on good2"
[ "$(file_tag)" = "belay-test:good2" ] || fail "compose file not updated"

echo "# 2. failed update rolls back (good2 -> bad)"
if "$bin" update --min-uptime 3s --timeout 30s "$proj" demo belay-test:bad; then
	fail "bad update should have exited non-zero"
fi
[ "$(running_image)" = "belay-test:good2" ] || fail "not rolled back to good2"
[ "$(file_tag)" = "belay-test:good2" ] || fail "compose file not reverted"

echo "# 3. no-op when already on target"
"$bin" update "$proj" demo belay-test:good2 | grep -q "nothing to do" || fail "should skip"

echo "ALL PASS"
