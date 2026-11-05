#!/usr/bin/env bash
# Starts commandcenter with --memory 128m and sends it refused uploads on many
# connections that hold their sockets open (test/load/flood). The server must
# survive: the kernel's socket buffers count against the container's memory.
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/load/.work-flood-$$"
net=commandcenter-flood-$$
name=commandcenter-flood-$$
. test/postgres.sh

cleanup() {
  docker rm -f "$name" "$name-pg" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup
rm -rf "$work" && mkdir -p "$work"

docker build -q -t commandcenter:flood . >/dev/null
hash="$(printf 'flood-pass\n' | docker run --rm -i commandcenter:flood hash-password)"

cat > "$work/config.yaml" <<EOF
baseURL: "http://commandcenter:8081"
users:
  - { name: admin, passwordHash: "$hash", roles: [admin] }
repositories:
  - { name: raw-hosted, format: raw, type: hosted }
EOF

docker network create "$net" >/dev/null
start_postgres
docker run -d --name "$name" --network "$net" -e COMMANDCENTER_DATABASE_URL="$pg_url" --network-alias commandcenter \
  --memory 128m --memory-swap 128m -e GOMEMLIMIT=80MiB \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" commandcenter:flood >/dev/null
for _ in $(seq 1 60); do
  [ "$(docker inspect -f '{{.State.Health.Status}}' "$name" 2>/dev/null)" = healthy ] && break
  sleep 1
done

docker run --rm --network "$net" --ulimit nofile=65536:65536 -v "$root:/src" -w /src \
  -v commandcenter-gomod:/go/pkg/mod -v commandcenter-gocache:/root/.cache/go-build \
  golang:1.27 go run ./test/load/flood -addr commandcenter:8081 -workers 1400 -seconds 12

if [ "$(docker inspect -f '{{.State.OOMKilled}}' "$name" 2>/dev/null)" = "true" ]; then
  echo "FAIL: commandcenter was OOM-killed"; exit 1
fi
if [ "$(docker inspect -f '{{.State.Running}}' "$name" 2>/dev/null)" != "true" ]; then
  echo "FAIL: commandcenter stopped"; docker logs --tail 20 "$name"; exit 1
fi
docker run --rm --network "$net" golang:1.27 curl -fsS -o /dev/null --max-time 20 http://commandcenter:8081/service/rest/v1/status \
  || { echo "FAIL: commandcenter does not answer after the flood"; exit 1; }
echo "== container memory after the run"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name" || true
echo "OK: survived the refused uploads"
