#!/usr/bin/env bash
# A large database: one Maven hosted repository of 1,000,000 components
# (3,000,000 asset rows, every one its own blob, written as rows; no blob
# exists), PostgreSQL 17 with its defaults in 2 GiB and on disk, commandcenter
# in 128 MiB with GOMEMLIMIT=80MiB. Times each page the dashboard, browse and
# the REST lists ask for (twice: cold, then warm), then one run of
# repository.cleanup with a policy that selects every component, and reports
# the server's memory.peak. About 5 minutes, a minute of it writing the rows
# (LARGE_COMPONENTS to change the size, in thousands of artifacts of 100
# versions: 10 is 1,000,000 components; LARGE_KEEP=1 leaves the containers
# and the database volume for a look, removed by the next run of the same
# LARGE_NAME).
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/load/.work-large-$$"
net=commandcenter-large-$$
name=${LARGE_NAME:-commandcenter-large-$$}
artifacts=$(( ${LARGE_COMPONENTS:-10} * 1000 ))

cleanup() {
  [ "${1:-}" = exit ] && [ -n "${LARGE_KEEP:-}" ] && { echo "kept: $name, $name-pg, volume $name-pgdata"; return; }
  docker rm -f -v "$name" "$name-pg" >/dev/null 2>&1 || true
  docker volume rm "$name-pgdata" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap 'cleanup exit' EXIT
cleanup
rm -rf "$work" && mkdir -p "$work"

docker build -q -t commandcenter:large . >/dev/null
hash="$(printf 'large-pass\n' | docker run --rm -i commandcenter:large hash-password)"
docker run --rm --memory 2g -v "$root:/src" -w /src -v commandcenter-gomod:/go/pkg/mod -v commandcenter-gocache:/root/.cache/go-build \
  -e CGO_ENABLED=0 golang:1.27 go build -buildvcs=false -o "$work/readers" ./test/load/readers

cat > "$work/config.yaml" <<EOF
baseURL: "http://commandcenter:8081"
users:
  - { name: admin, passwordHash: "$hash", roles: [admin] }
repositories:
  - { name: maven-hosted, format: maven, type: hosted }
EOF

docker network create "$net" >/dev/null
docker run -d --name "$name-pg" --network "$net" --network-alias postgres --memory 2g --memory-swap 2g \
  -v "$name-pgdata:/var/lib/postgresql/data" -e POSTGRES_PASSWORD=large-pg -e POSTGRES_DB=cc postgres:17-alpine >/dev/null
for _ in $(seq 60); do docker exec "$name-pg" pg_isready -q -h 127.0.0.1 -U postgres -d cc 2>/dev/null && break; sleep 1; done
start() {
  docker rm -f "$name" >/dev/null 2>&1 || true
  docker run -d --name "$name" --network "$net" --network-alias commandcenter --memory 128m --memory-swap 128m -e GOMEMLIMIT=80MiB \
    -e COMMANDCENTER_DATABASE_URL="postgres://postgres:large-pg@postgres:5432/cc?sslmode=disable" \
    -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" commandcenter:large >/dev/null
  docker run --rm --network "$net" --memory 256m -v "$root/$work:/w:ro" golang:1.27 /w/readers -target http://commandcenter:8081 -wait \
    || { echo "FAIL: commandcenter does not answer"; docker logs --tail 20 "$name"; exit 1; }
}
start
# memory.peak and the largest anon (heap, stacks) of memory.stat of the
# server's cgroup while a phase runs (systemd or cgroupfs driver).
sample() {
  local id d a anon=0; id="$(docker inspect -f '{{.Id}}' "$name")"
  for d in "/sys/fs/cgroup/system.slice/docker-$id.scope" "/sys/fs/cgroup/docker/$id"; do
    [ -e "$d/memory.peak" ] && break
  done
  while a="$(awk '$1 == "anon" {print $2}' "$d/memory.stat" 2>/dev/null)" && [ -n "$a" ]; do
    [ "$a" -gt "$anon" ] && anon=$a
    echo "memory.peak $(( $(cat "$d/memory.peak") >> 20 )) MiB, anon at most $(( anon >> 20 )) MiB" > "$work/mem"
    sleep 0.2
  done
}
measure() { sample & sampler=$!; }
measured() { kill "$sampler" 2>/dev/null || true; wait "$sampler" 2>/dev/null || true; echo "== commandcenter $(cat "$work/mem")"; }
psql() { docker exec -i "$name-pg" psql -q -v ON_ERROR_STOP=1 -U postgres -d cc "$@"; }

echo "== writing $((artifacts * 100)) components, $((artifacts * 300)) assets"
t0=$(date +%s)
# The per-row usage trigger is off while the rows go in; repo_usage is then
# counted as the schema's first install counts it.
psql <<EOF
ALTER TABLE assets DISABLE TRIGGER USER;
INSERT INTO assets(repo, path, blob_sha256, size, content_type, created_at, last_modified)
SELECT 'maven-hosted', 'com/acme/g' || (a % 100) || '/art' || a || '/1.' || v || '.0/art' || a || '-1.' || v || '.0' || s,
  md5(a || '/' || v || s) || md5(s || a || '/' || v), 10000 + a + v, 'application/java-archive', 1600000000000, 1600000000000
FROM generate_series(0, $artifacts - 1) a, generate_series(0, 99) v, (VALUES ('.jar'), ('.pom'), ('-sources.jar')) x(s);
ALTER TABLE assets ENABLE TRIGGER USER;
DELETE FROM repo_usage WHERE repo = 'maven-hosted';
INSERT INTO repo_usage(repo, assets, bytes, unique_bytes, last_upload, last_download)
  SELECT repo, SUM(n), SUM(b), SUM(sz), MAX(up), MAX(ld) FROM (SELECT repo, blob_sha256, COUNT(*) AS n, SUM(size) AS b, MAX(size) AS sz,
    MAX(last_modified) AS up, MAX(last_downloaded) AS ld FROM assets GROUP BY repo, blob_sha256) x GROUP BY repo;
VACUUM ANALYZE assets;
EOF
echo "   $(( $(date +%s) - t0 )) s; database $(psql -tAc "SELECT pg_size_pretty(pg_database_size('cc'))")"

cat > "$work/time.sh" <<'EOF'
set -u
t() { # label path [extra curl args]: status, seconds, bytes, cold then warm
  local label=$1 path=$2; shift 2
  local a b
  a=$(curl -s -o /dev/null -u admin:large-pass -H 'X-Command-Center-UI: 1' -w '%{http_code} %{time_total}s %{size_download}B' "$@" "http://commandcenter:8081$path")
  b=$(curl -s -o /dev/null -u admin:large-pass -H 'X-Command-Center-UI: 1' -w '%{http_code} %{time_total}s' "$@" "http://commandcenter:8081$path")
  printf '%-34s %-28s %s\n' "$label" "$a" "$b"
}
tok=$(curl -s -u admin:large-pass 'http://commandcenter:8081/service/rest/v1/components?repository=maven-hosted' | sed -n 's/.*"continuationToken":"\([^"]*\)".*/\1/p')
atok=$(curl -s -u admin:large-pass 'http://commandcenter:8081/service/rest/v1/assets?repository=maven-hosted' | sed -n 's/.*"continuationToken":"\([^"]*\)".*/\1/p')
t "UI blob stores"                  /-/api/blobstores
t "UI blob store default"           /-/api/blobstores/default
t "UI dashboard repositories"       /-/api/dashboard/repositories
t "UI repositories"                 /-/api/repositories
t "UI repository"                   /-/api/repositories/maven-hosted
t "UI repository stats"             /-/api/stats/repositories
t "UI status"                       /-/api/status
t "UI browse /"                     '/-/api/repositories/maven-hosted/browse?path='
t "UI browse com/acme/"             '/-/api/repositories/maven-hosted/browse?path=com/acme/'
t "UI browse com/acme/g7/art7/"  '/-/api/repositories/maven-hosted/browse?path=com/acme/g7/art7/'
t "UI search q=art7"             '/-/api/search?q=art7'
t "UI search components name"       '/-/api/search/components?name=art7'
t "REST repositories"               /service/rest/v1/repositories
t "REST components page 1"          '/service/rest/v1/components?repository=maven-hosted'
t "REST components page 2"          "/service/rest/v1/components?repository=maven-hosted&continuationToken=$tok"
t "REST assets page 1"              '/service/rest/v1/assets?repository=maven-hosted'
t "REST assets page 2"              "/service/rest/v1/assets?repository=maven-hosted&continuationToken=$atok"
t "REST search name"                '/service/rest/v1/search?repository=maven-hosted&name=art7'
t "REST search assets name"         '/service/rest/v1/search/assets?repository=maven-hosted&name=art7'
t "REST browse /"                   /service/rest/repository/browse/maven-hosted/
t "REST browse com/acme/g7/"        /service/rest/repository/browse/maven-hosted/com/acme/g7/
EOF
curl_box() { docker run --rm -i --network "$net" --memory 256m golang:1.27 bash -s; }

echo "== pages (status, time, size; then again)"
measure
curl_box < "$work/time.sh"
measured
if [ "$(docker inspect -f '{{.State.Running}}' "$name")" != true ]; then
  echo "FAIL: commandcenter stopped (OOM-killed: $(docker inspect -f '{{.State.OOMKilled}}' "$name"))"; exit 1
fi

echo "== repository.cleanup (policy: older than a day, every component)"
cat > "$work/cleanup.sh" <<'EOF'
set -eu
c() { curl -s -u admin:large-pass -H 'X-Command-Center-UI: 1' -H 'Content-Type: application/json' "$@"; }
c -o /dev/null -w 'create policy: %{http_code}\n' -X POST http://commandcenter:8081/service/rest/v1/cleanup-policies \
  -d '{"name":"old","format":"maven2","criteriaLastBlobUpdated":1}'
c -w ' set policy: %{http_code}\n' -X PUT http://commandcenter:8081/-/api/repositories/maven-hosted \
  -d '{"name":"maven-hosted","format":"maven","type":"hosted","cleanupPolicy":"old"}'
id=$(c -X POST http://commandcenter:8081/-/api/tasks -d '{"name":"cleanup","type":"repository.cleanup","schedule":"manual","params":{"repos":["maven-hosted"]}}' | sed -n 's/^{"id":"\([^"]*\)".*/\1/p')
[ -n "$id" ] || { echo "no task"; exit 1; }
start=$(date +%s%N)
c -X POST "http://commandcenter:8081/-/api/tasks/$id/run" >/dev/null
sleep 1
while c "http://commandcenter:8081/-/api/tasks/$id" | grep -q '"status":"Running"'; do sleep 0.5; done
echo "run: $(( ($(date +%s%N) - start) / 1000000 )) ms (polled)"
c "http://commandcenter:8081/-/api/tasks/$id" | grep -o '"lastResult":"[^"]*"\|"lastDurationMs":[0-9]*\|"lastRemoved":[0-9]*\|"lastMessage":"[^"]*"' | tr '\n' ' '; echo
EOF
docker restart "$name" >/dev/null # memory.peak of the cleanup run alone
docker run --rm --network "$net" --memory 256m -v "$root/$work:/w:ro" golang:1.27 /w/readers -target http://commandcenter:8081 -wait
measure
curl_box < "$work/cleanup.sh"
measured
echo "== components left: $(psql -tAc "SELECT COUNT(*) / 3 FROM assets WHERE repo = 'maven-hosted'")"

[ "$(docker inspect -f '{{.State.OOMKilled}}' "$name")" = true ] && { echo "FAIL: OOM-killed"; exit 1; }
echo "done"
