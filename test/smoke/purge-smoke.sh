#!/usr/bin/env bash
# repository.purge-unused on proxies of the real registries. Builds the image,
# starts commandcenter, and fills proxies of registry.npmjs.org, pypi.org and
# proxy.golang.org with real npm (node:22), pip (python:3.12) and go, then:
#   1. a purge right after that removes nothing
#   2. the rows' times move 30 days back (nothing else can age them), a package
#      is used again, and a purge with lastUsed 7 runs: what no one used goes, the
#      cached documents and indexes with their components; the package just used
#      stays; a proxy ahead of another member of a group keeps its documents
#   3. blobstore.compact gives the disk space back
#   4. the same clients again, with empty caches: everything is fetched again
# It prints what each proxy held and how much went. Finally prints the
# container's memory usage.
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-purge-$$"
net=commandcenter-purge-smoke-$$
name=commandcenter-purge-smoke-$$
. test/postgres.sh

cleanup() {
  docker rm -f -v "$name" "$name-pg" "$name-client" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup
fail() { echo "FAIL: $*" >&2; docker logs --tail 30 "$name" >&2 || true; exit 1; }
rm -rf "$work" && mkdir -p "$work"

docker build -q -t commandcenter:smoke-purge . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i --name "$name-hash" --memory 256m commandcenter:smoke-purge hash-password)"

cat > "$work/config.yaml" <<EOF
anonymousRead: true
baseURL: "http://commandcenter:8081"
users:
  - { name: root, passwordHash: "$hash", roles: [admin] }
repositories:
  - { name: npm-proxy,  format: npm,  type: proxy,  remote: "https://registry.npmjs.org/" }
  - { name: npm-hosted, format: npm,  type: hosted }
  - { name: npm-all,    format: npm,  type: group,  members: [npm-hosted, npm-proxy], writeMember: npm-hosted }
  - { name: npm-ahead,  format: npm,  type: proxy,  remote: "https://registry.npmjs.org/" }
  - { name: npm-first,  format: npm,  type: group,  members: [npm-ahead, npm-hosted] }
  - { name: pypi-proxy, format: pypi, type: proxy,  remote: "https://pypi.org/" }
  - { name: go-proxy,   format: go,   type: proxy,  remote: "https://proxy.golang.org/" }
EOF

docker network create "$net" >/dev/null
start_postgres
docker run -d --name "$name" --network "$net" -e COMMANDCENTER_DATABASE_URL="$pg_url" --network-alias commandcenter --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" commandcenter:smoke-purge >/dev/null
ready=0
for i in $(seq 1 30); do
  if docker exec "$name" commandcenter healthcheck >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
[ "$ready" = 1 ] || fail "commandcenter did not become ready within 30s"

R=http://commandcenter:8081/repository
client() { # client <image> <shell command>
  docker run --rm --name "$name-client" --network "$net" --memory 1g -e HOME=/tmp -e R="$R" "$1" sh -c "$2"
}
npm_in() { # npm_in <registry> <package...>: npm install in a new project, with an empty npm cache
  reg=$1; shift
  client node:22 "cd /tmp && npm init -y >/dev/null && npm install $* --registry \$R/$reg/ --cache /tmp/c --no-audit --no-fund --update-notifier=false --loglevel=error >/dev/null"
}
fill() {
  npm_in npm-all express
  npm_in npm-first left-pad
  client python:3.12-slim "pip download -q --no-cache-dir --disable-pip-version-check -d /tmp/d --index-url \$R/pypi-proxy/simple/ --trusted-host commandcenter requests"
  client golang:1.27.3 "mkdir /tmp/m && cd /tmp/m && go mod init x 2>/dev/null && GOPROXY=\$R/go-proxy GOSUMDB=off GOFLAGS=-mod=mod go get rsc.io/quote"
}
sql() { docker exec "$name-pg" psql -U postgres -d cc -At -c "$1"; }
has() { [ "$(sql "SELECT count(*) FROM assets WHERE repo = '$1' AND path LIKE '$2'")" != 0 ]; }
# What each proxy holds: rows and bytes of components, and of files without one.
held() {
  sql "SELECT repo, count(*) FILTER (WHERE $comp), COALESCE(sum(size) FILTER (WHERE $comp), 0), count(*) FILTER (WHERE NOT $comp), COALESCE(sum(size) FILTER (WHERE NOT $comp), 0)
FROM assets WHERE repo <> 'npm-hosted' GROUP BY repo ORDER BY repo" | awk -F'|' '{printf "  %-11s components: %4d files %8.1f MiB; without a component: %4d files %8.1f MiB\n", $1, $2, $3/1048576, $4, $5/1048576}'
}
comp="(path LIKE '%/-/%' OR path LIKE 'packages/%' OR path ~ '[.](zip|mod|info)$')"
API=http://commandcenter:8081/service/rest/v1
api() { docker run --rm --name "$name-client" --network "$net" --memory 256m node:22 curl -fsS -u root:smoke-pass "$@"; }
# task <json>: creates a task of the type and properties, prints its id
task() { api -H 'Content-Type: application/json' -d "$1" "$API/tasks" | sed 's/.*"id":"\([^"]*\)".*/\1/'; }
# run <id>: runs the task and waits for its result
run() {
  api -X POST "$API/tasks/$1/run"
  for _ in $(seq 60); do
    result="$(api "$API/tasks/$1" | sed -n 's/.*"lastRunResult":"\([A-Z]*\)".*/\1/p')"
    state="$(api "$API/tasks/$1" | sed -n 's/.*"currentState":"\([A-Z]*\)".*/\1/p')"
    if [ -n "$result" ] && [ "$state" != RUNNING ]; then
      [ "$result" = OK ] || fail "task $1: $result"
      return
    fi
    sleep 1
  done
  fail "task $1 did not finish"
}
log() { api "http://commandcenter:8081/-/api/tasks/$1" | sed 's/.*"lastLog":"//; s/\\n/\n/g; s/\\"/"/g'; }

echo "== real clients fill the proxies"
fill
held
rows="$(sql 'SELECT count(*) FROM assets')"

echo "== a purge right after that removes nothing"
purge="$(task '{"type":"repository.purge-unused","name":"purge","frequency":{"schedule":"manual"},"properties":{"repositoryName":"*","lastUsed":"7"}}')"
run "$purge"
[ "$(sql 'SELECT count(*) FROM assets')" = "$rows" ] || fail "the purge took recently used files: $(log "$purge")"

echo "== 30 days later, with one package used again"
sql "UPDATE assets SET created_at = created_at - 2592000000, last_modified = last_modified - 2592000000,
  checked_at = CASE WHEN checked_at > 0 THEN checked_at - 2592000000 ELSE 0 END,
  last_downloaded = CASE WHEN last_downloaded > 0 THEN last_downloaded - 2592000000 ELSE 0 END" >/dev/null
npm_in npm-all ms
run "$purge" # writes the pending counts first
held
log "$purge" | grep -E 'keeps its cached files|lastUsed-7' | sed 's/^/  /'
has npm-proxy 'ms/package.json' || fail "the document of a package just used went"
has npm-proxy 'ms/-/%' || fail "the tarball of a package just used went"
! has npm-proxy 'express/package.json' || fail "the document of express stayed"
! has npm-proxy 'express/-/%' || fail "the tarball of express stayed"
has npm-ahead 'left-pad/package.json' || fail "a proxy ahead of another member lost a document"
! has npm-ahead 'left-pad/-/%' || fail "its tarball is a component like any other"
! has pypi-proxy 'simple/requests/index' || fail "the page of requests stayed"
! has pypi-proxy 'packages/requests/%' || fail "the files of requests stayed"
! has go-proxy 'rsc.io/quote/@v/list' || fail "the list of rsc.io/quote stayed"
! has go-proxy 'rsc.io/quote/@v/%.zip' || fail "the zip of rsc.io/quote stayed"
log "$purge" | grep -q 'npm-ahead: keeps its cached files' || fail "no word on what npm-ahead keeps: $(log "$purge")"

echo "== blobstore.compact gives the space back"
# Blobs younger than a day stay, for writes in flight.
old="$(date -u -d '2 days ago' '+%Y-%m-%d %H:%M:%S')"
disk() { docker run --rm --name "$name-client" --memory 256m --volumes-from "$name" alpine:3 sh -c "du -sm /data/blobs | cut -f1"; }
before="$(disk)"
docker run --rm --name "$name-client" --memory 256m --volumes-from "$name" alpine:3 sh -c "find /data/blobs -type f -exec touch -d '$old' {} +"
compact="$(task '{"type":"blobstore.compact","name":"compact","frequency":{"schedule":"manual"},"properties":{"blobstoreName":"default"}}')"
run "$compact"
echo "  blobs on disk: $before MiB, after compact $(disk) MiB"

echo "== the same clients with empty caches: everything comes back"
fill
has npm-proxy 'express/package.json' || fail "the document of express did not come back"
has pypi-proxy 'simple/requests/index' || fail "the page of requests did not come back"
has go-proxy 'rsc.io/quote/@v/list' || fail "the list of rsc.io/quote did not come back"
held

echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "PURGE SMOKE OK"
