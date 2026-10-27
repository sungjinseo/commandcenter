#!/usr/bin/env bash
# Builds the image, then proves that an online backup taken under write load restores:
#   1. start a server (named data and backup volumes) and seed an artifact in every format
#   2. start an upload loop, run an online backup through the API while it runs, stop the loop;
#      any non-2xx upload fails the smoke and at least MIN_DURING uploads must succeed during the backup
#   3. verify the backup, stop the server, restore into a FRESH volume with
#      `docker run ... restore --from /backup/<name> --data /data`
#   4. start a server on the restored volume, download every seeded artifact and compare sha256,
#      and check the loop's uploads: those finished before the backup began are present, the others
#      are present (same content) or absent, and the present ones form a prefix (a consistent snapshot)
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-backup-$$"
net=commandcenter-backup-smoke-$$
name=commandcenter-backup-smoke-$$
loop=commandcenter-backup-loop-$$
vol=commandcenter-backup-data-$$
bkvol=commandcenter-backup-store-$$
rvol=commandcenter-backup-restored-$$
img=commandcenter:smoke-backup
base=http://localhost:18094
MIN_DURING=3 # uploads that must succeed while the backup runs
py=(docker run --rm --network "$net" -v "$root/test/smoke/backup/seed.py:/seed.py:ro" -v "$root/$work:/work" python:3.12-slim python /seed.py)

cleanup() {
  docker rm -f "$name" "$loop" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  docker volume rm "$vol" "$bkvol" "$rvol" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup
rm -rf "$work" && mkdir -p "$work"

fail() { echo "FAIL: $*" >&2; exit 1; }
status() { curl -s -o /dev/null -w '%{http_code}' "$@" || true; }

docker build -q -t "$img" . >/dev/null
hash="$(printf 'backup-pass\n' | docker run --rm -i "$img" hash-password)"

cat > "$work/config.yaml" <<EOF2
anonymousRead: false
baseURL: "http://localhost:18094"
users:
  - { name: admin, passwordHash: "$hash", roles: [admin] }
repositories:
  - { name: maven-hosted, format: maven, type: hosted }
  - { name: npm-hosted,   format: npm,   type: hosted }
  - { name: pypi-hosted,  format: pypi,  type: hosted }
  - { name: go-hosted,    format: go,    type: hosted }
  - { name: cargo-hosted, format: cargo, type: hosted }
EOF2

docker network create "$net" >/dev/null
docker volume create "$vol" >/dev/null
docker volume create "$bkvol" >/dev/null
docker volume create "$rvol" >/dev/null

# start_server <data volume>: runs the server and waits until it answers.
start_server() {
  docker rm -f "$name" >/dev/null 2>&1 || true
  docker run -d --name "$name" --network "$net" --network-alias "$name" --memory 128m \
    -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" -v "$1:/data" -v "$bkvol:/backup" \
    -p 18094:8081 "$img" >/dev/null
  local ready=0 i
  for i in $(seq 1 30); do
    if curl -fs "$base/service/rest/v1/status" >/dev/null; then ready=1; break; fi
    sleep 1
  done
  [ "$ready" = 1 ] || { docker logs "$name" 2>&1 | tail -20; fail "server did not become ready"; }
}

echo "== start the server and seed every format"
start_server "$vol"
"${py[@]}" seed "http://$name:8081" admin backup-pass first /work/manifest.txt
[ "$(wc -l < "$work/manifest.txt")" -eq 5 ] || fail "manifest: $(cat "$work/manifest.txt")"

echo "== online backup while an upload loop runs"
"${py[@]}" bulk "http://$name:8081" admin backup-pass 100 >/dev/null # a bigger pool, so the backup takes a moment
docker run -d --name "$loop" --network "$net" -v "$root/test/smoke/backup/seed.py:/seed.py:ro" python:3.12-slim \
  python /seed.py loop "http://$name:8081" admin backup-pass >/dev/null
sleep 2
# Uploads logged before the backup starts finished before its snapshot.
logged() { docker logs "$loop" 2>&1 | grep -c '^UP ' || true; }
before=$(logged)
bk="$(curl -s -u admin:backup-pass -X POST "$base/-/api/backup")"
bkid="$(echo "$bk" | sed -nE 's/.*"id":"([^"]+)".*/\1/p')"
[ -n "$bkid" ] || fail "backup start: $bk"
bkrun=""
for i in $(seq 1 240); do
  bkrun="$(curl -s -u admin:backup-pass "$base/-/api/backup/runs/$bkid")"
  echo "$bkrun" | grep -q '"status":"Running"' || break
  sleep 0.5
done
echo "$bkrun" | grep -q '"status":"OK"' || { docker logs "$name" 2>&1 | tail -20; fail "backup run: $bkrun"; }
after=$(logged)
echo "loop uploads: $before before the backup, $((after - before)) during it"
[ $((after - before)) -ge "$MIN_DURING" ] || fail "only $((after - before)) uploads succeeded during the backup, want at least $MIN_DURING"
[ "$(docker inspect -f '{{.State.Running}}' "$loop")" = true ] || fail "the upload loop stopped by itself: $(docker logs "$loop" 2>&1 | tail -5)"
sleep 1
docker logs "$loop" > "$work/loop.log" 2>&1 || true
docker rm -f "$loop" >/dev/null 2>&1
if grep -q '^BAD ' "$work/loop.log"; then fail "uploads failed during the backup: $(grep '^BAD ' "$work/loop.log" | head -3)"; fi
[ "$before" -ge 1 ] || fail "no upload finished before the backup began"

bklist="$(curl -s -u admin:backup-pass "$base/-/api/backups")"
bkname="$(echo "$bklist" | sed -nE 's/.*"name":"([0-9]{8}-[0-9]{6}[^"]*)".*/\1/p')"
[ -n "$bkname" ] || fail "backup list: $bklist"
echo "backup: $bkname"

echo "== standard verify"
jar="$work/cookies.txt"
[ "$(status -c "$jar" -H 'X-Command-Center-UI: 1' -X POST -d '{"user":"admin","password":"backup-pass"}' "$base/-/api/login")" = 200 ] || fail "login"
bkv="$(curl -s -b "$jar" -H 'X-Command-Center-UI: 1' -X POST "$base/-/api/backups/$bkname/verify")"
echo "$bkv" | grep -q '"ok":true' || fail "backup verify: $bkv"

echo "== stop the server, restore into a fresh volume"
docker rm -f "$name" >/dev/null
docker run --rm -v "$bkvol:/backup:ro" -v "$rvol:/data" "$img" restore --from "/backup/$bkname" --data /data

echo "== start on the restored volume, download and compare every seeded artifact"
start_server "$rvol"
"${py[@]}" check "http://$name:8081" admin backup-pass /work/manifest.txt
"${py[@]}" verify-loop "http://$name:8081" admin backup-pass /work/loop.log "$before"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "BACKUP SMOKE OK"
