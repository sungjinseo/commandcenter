#!/usr/bin/env bash
# Starts commandcenter with --memory 128m and runs the load program next to it.
# The load container is reachable as "load"; it serves a fake npm upstream on :8000.
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/load/.work-$$"
net=commandcenter-load-$$
name=commandcenter-load-$$
. test/postgres.sh

cleanup() {
  docker rm -f "$name" "$name-pg" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup
rm -rf "$work" && mkdir -p "$work"

docker build -q -t commandcenter:load . >/dev/null
hash="$(printf 'load-pass\n' | docker run --rm -i commandcenter:load hash-password)"

cat > "$work/config.yaml" <<EOF
anonymousRead: true
baseURL: "http://commandcenter:8081"
users:
  - { name: admin, passwordHash: "$hash", roles: [admin] }
repositories:
  - { name: maven-hosted, format: maven, type: hosted }
  - { name: npm-proxy,    format: npm,   type: proxy,  remote: "http://load:8000/" }
  - { name: npm-hosted,   format: npm,   type: hosted }
  - { name: npm-all,      format: npm,   type: group,  members: [npm-hosted, npm-proxy], writeMember: npm-hosted }
  - { name: go-hosted,    format: go,    type: hosted }
  - { name: cargo-hosted, format: cargo, type: hosted }
  - { name: pypi-hosted,  format: pypi,  type: hosted }
EOF

docker network create "$net" >/dev/null
start_postgres
docker run -d --name "$name" --network "$net" -e COMMANDCENTER_DATABASE_URL="$pg_url" --network-alias commandcenter --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" commandcenter:load >/dev/null

rc=0
docker run --rm --network "$net" --network-alias load \
  -v "$root:/src" -w /src -v commandcenter-gomod:/go/pkg/mod -v commandcenter-gocache:/root/.cache/go-build \
  golang:1.27 go run ./test/load -target http://commandcenter:8081 -user admin -pass load-pass || rc=$?

if [ "$(docker inspect -f '{{.State.OOMKilled}}' "$name" 2>/dev/null)" = "true" ]; then
  echo "FAIL: commandcenter was OOM-killed"; rc=1
fi
echo "== container memory after the run"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name" || true
if [ "$rc" -ne 0 ]; then
  echo "== last commandcenter log lines"
  docker logs --tail 50 "$name" || true
fi
exit "$rc"
