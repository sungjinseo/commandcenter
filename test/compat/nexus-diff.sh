#!/usr/bin/env bash
# Compares a real Nexus Repository 3 with a Command Center built from this tree:
# the same requests to both, the differences listed (test/compat/nexusdiff).
#
# This script never starts or configures a Nexus: running one is the owner's
# decision. Start it first, on a network of its own (no published port):
#
#   docker network create --internal cc-nexusref
#   docker run -d --name nexus-ref --network cc-nexusref --memory 2500m --memory-swap 2500m --cpus 3 \
#     -e INSTALL4J_ADD_VM_PARAMS="-Xms512m -Xmx1100m -XX:MaxDirectMemorySize=1100m -Djava.util.prefs.userRoot=/nexus-data/javaprefs" \
#     -v nexus-ref-data:/nexus-data sonatype/nexus3:3.76.1
#
# 3.76.1 is the last release before the Community Edition's licence step. On a
# later one the script stops at the first upload the licence refuses: accepting
# it is the owner's decision too.
#
# Usage: test/compat/nexus-diff.sh [nexus container] [network]
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
ref="${1:-nexus-ref}"
net="${2:-cc-nexusref}"
work="test/compat/.work-diff-$$"
name=commandcenter-nexusdiff-$$
. test/postgres.sh

cleanup() {
  docker rm -f -v "$name" "$name-pg" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
rm -rf "$work" && mkdir -p "$work"
[ "$(docker inspect -f '{{.State.Running}}' "$ref" 2>/dev/null)" = true ] || { echo "start the Nexus first: see the top of $0"; exit 2; }

echo "== waiting for $ref (a first start takes minutes)"
probe() { docker run --rm --network "$net" golang:1.27 curl -fsS -o /dev/null --max-time 5 "$@"; }
for i in $(seq 1 90); do
  probe "http://$ref:8081/service/rest/v1/status/writable" 2>/dev/null && break
  [ "$i" = 90 ] && { echo "the Nexus did not become ready"; exit 2; }
  sleep 5
done
if ! docker exec "$ref" cat /nexus-data/admin.password > "$work/nexus.pw" 2>/dev/null; then
  [ -n "${NEXUS_PASSWORD_FILE:-}" ] || { echo "no /nexus-data/admin.password: give the admin password in NEXUS_PASSWORD_FILE"; exit 2; }
  cp "$NEXUS_PASSWORD_FILE" "$work/nexus.pw"
fi

docker build -q -t commandcenter:nexusdiff . >/dev/null
printf 'Diff-admin-1\n' > "$work/cc.pw"
hash="$(docker run --rm -i commandcenter:nexusdiff hash-password < "$work/cc.pw")"
cat > "$work/config.yaml" <<EOF
baseURL: "http://$name:8081"
users:
  - { name: admin, passwordHash: "$hash", roles: [admin] }
EOF
start_postgres
docker run -d --name "$name" --network "$net" -e COMMANDCENTER_DATABASE_URL="$pg_url" \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" commandcenter:nexusdiff >/dev/null
for _ in $(seq 1 60); do
  [ "$(docker inspect -f '{{.State.Health.Status}}' "$name" 2>/dev/null)" = healthy ] && break
  sleep 1
done

echo "== the same requests to both"
rc=0
docker run --rm --network "$net" -v "$root:/src" -w /src -e GOFLAGS=-buildvcs=false \
  -v commandcenter-gomod:/go/pkg/mod -v commandcenter-gocache:/root/.cache/go-build \
  golang:1.27 go run ./test/compat/nexusdiff -nexus "http://$ref:8081" -cc "http://$name:8081" \
  -nexus-password-file "$work/nexus.pw" -cc-password-file "$work/cc.pw" | tee "$work/diff.txt" || rc=$?
out="${NEXUS_DIFF_OUT:-}"
[ -z "$out" ] || cp "$work/diff.txt" "$out"
exit "$rc"
