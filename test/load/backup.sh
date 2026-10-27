#!/usr/bin/env bash
# Plan 14 backup load check: starts commandcenter with --memory 128m, fills it
# with about 1 GiB of distinct blobs, runs an online backup while uploads
# continue, deep-verifies it, and fails if the server's RSS (from /-/metrics)
# goes over 100 MiB. Usage: test/load/backup.sh [extra flags for test/load/backup]
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/load/.work-backup-$$"
net=commandcenter-bload-$$
name=commandcenter-bload-$$

cleanup() {
  docker rm -f "$name" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  # The backup files belong to the container's user; remove them from a container.
  docker run --rm -v "$root/test/load:/w" golang:1.26 rm -rf "/w/$(basename "$work")" >/dev/null 2>&1 || rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup
mkdir -p "$work/backup"
chmod 777 "$work/backup" 2>/dev/null || true

docker build -q -t commandcenter:load . >/dev/null
hash="$(printf 'load-pass\n' | docker run --rm -i commandcenter:load hash-password)"

cat > "$work/config.yaml" <<EOF
anonymousRead: true
baseURL: "http://commandcenter:8081"
users:
  - { name: admin, passwordHash: "$hash", roles: [admin] }
repositories:
  - { name: maven-hosted, format: maven, type: hosted, writePolicy: allow }
EOF

docker network create "$net" >/dev/null
docker run -d --name "$name" --network "$net" --network-alias commandcenter --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" \
  -v "$root/$work/backup:/backup" commandcenter:load >/dev/null

rc=0
docker run --rm --network "$net" \
  -v "$root:/src" -w /src -v commandcenter-gomod:/go/pkg/mod -v commandcenter-gocache:/root/.cache/go-build \
  golang:1.26 go run ./test/load/backup -target http://commandcenter:8081 -user admin -pass load-pass "$@" || rc=$?

if [ "$(docker inspect -f '{{.State.OOMKilled}}' "$name" 2>/dev/null)" = "true" ]; then
  echo "FAIL: commandcenter was OOM-killed"; rc=1
fi
echo "== container memory after the run"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name" || true
echo "== backup directory"
du -sh "$work/backup"/* 2>/dev/null || true
if [ "$rc" -ne 0 ]; then
  echo "== last commandcenter log lines"
  docker logs --tail 50 "$name" || true
fi
exit "$rc"
