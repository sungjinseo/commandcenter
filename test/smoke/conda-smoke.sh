#!/usr/bin/env bash
# Builds the image, starts commandcenter, then uses real micromamba and conda
# (mambaorg/micromamba, condaforge/miniforge3) and curl to:
#   1. install through conda-org, a proxy of https://conda.anaconda.org/ with
#      conda-forge a channel below the repository root; its packages are
#      components: REST search and a cleanup preview see them; a second install
#      with the proxy blocked comes from its cache
#   2. make packages with micromamba package compress (.conda and .tar.bz2),
#      upload them to conda-hosted, with real conda-build packages taken through
#      the proxy; install the newest and a pinned version from hosted
#   3. install a hosted and a proxied package through the conda-all group, whose
#      proxy member publishes sharded repodata the clients ask for first
#   4. read a channel's index.html through a proxy, delete a package with REST
#      and run a cleanup policy on the channel below the root
# Finally prints the container's memory usage.
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-conda-$$"
net=commandcenter-conda-smoke-$$
name=commandcenter-conda-smoke-$$
. test/postgres.sh

cleanup() {
  docker rm -f -v "$name" "$name-pg" "$name-mm" "$name-conda" "$name-curl" >/dev/null 2>&1 || true
  docker volume rm "$name-share" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup

# Built before the work directory exists: a new directory in the build
# context would compile the binary again on every run.
docker build -q -t commandcenter:smoke-conda . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i commandcenter:smoke-conda hash-password)"

mkdir -p "$work"
cat > "$work/config.yaml" <<EOF
anonymousRead: true
baseURL: "http://commandcenter:8081"
users:
  - { name: admin, passwordHash: "$hash", roles: [admin] }
  - { name: ci, passwordHash: "$hash", roles: [deployer] }
roles:
  deployer: { write: [conda-hosted, conda-internal] }
repositories:
  - { name: conda-org,     format: conda, type: proxy,  remote: "https://conda.anaconda.org/" }
  - { name: conda-pytorch, format: conda, type: proxy,  remote: "https://conda.anaconda.org/pytorch/" }
  - { name: conda-hosted,  format: conda, type: hosted }
  - { name: conda-all,     format: conda, type: group,  members: [conda-hosted, conda-pytorch], writeMember: conda-hosted }
  - { name: conda-internal, format: conda, type: hosted }
  - { name: conda-first,    format: conda, type: group, members: [conda-internal, conda-pytorch] }
  - { name: conda-merged,   format: conda, type: group, members: [conda-internal, conda-pytorch], mergeMembers: true }
EOF

docker network create "$net" >/dev/null
docker volume create "$name-share" >/dev/null
start_postgres
docker run -d --name "$name" --network "$net" -e COMMANDCENTER_DATABASE_URL="$pg_url" --network-alias commandcenter --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" commandcenter:smoke-conda >/dev/null

# No port is published: the image's own health check asks the server.
ready=0
for i in $(seq 1 30); do
  if docker exec "$name" commandcenter healthcheck 2>/dev/null; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "FAIL: commandcenter did not become ready"; docker logs "$name"; exit 1
fi

# The clients keep running: uploads (curl) and installs take turns. Both write to the shared volume as root.
docker run -d --name "$name-mm" --network "$net" --memory 2g --user 0 -e MAMBA_ROOT_PREFIX=/tmp/mm -v "$name-share:/share" \
  --entrypoint sleep mambaorg/micromamba:2.10.0-ubuntu24.04 infinity >/dev/null
docker run -d --name "$name-conda" --network "$net" --memory 2g condaforge/miniforge3:26.9.4-0 sleep infinity >/dev/null
docker run -d --name "$name-curl" --network "$net" --memory 128m --user 0 -v "$name-share:/share" --entrypoint sleep curlimages/curl:8.23.0 infinity >/dev/null
mm() { docker exec "$name-mm" micromamba "$@"; }
cc() { docker exec "$name-conda" conda "$@"; }
BASE=http://commandcenter:8081
REPO=$BASE/repository
REST=$BASE/service/rest/v1
HOSTED=$REPO/conda-hosted
GROUP=$REPO/conda-all
FORGE=$REPO/conda-org/conda-forge
# want STATUS CURL-ARGS...: curl answers STATUS.
want() {
  local status=$1 got; shift
  got="$(docker exec "$name-curl" curl -s -o /tmp/out -w '%{http_code}' "$@")"
  if [ "$got" != "$status" ]; then
    echo "FAIL: curl $* answered $got, want $status:"; docker exec "$name-curl" cat /tmp/out; return 1
  fi
}
put() { want "$1" -u ci:smoke-pass -T "/share/$2" "$HOSTED/$3"; }
rest() { docker exec "$name-curl" curl -sS -u admin:smoke-pass "$@"; }
has() { case "$1" in *"$2"*) ;; *) echo "FAIL: no \"$2\" in: $1"; return 1;; esac; }
hasnt() { case "$1" in *"$2"*) echo "FAIL: \"$2\" in: $1"; return 1;; esac; }
blocked() {
  rest -o /dev/null -f -X PUT -H 'Content-Type: application/json' "$REST/repositories/conda/proxy/conda-org" \
    -d "{\"name\":\"conda-org\",\"online\":true,\"storage\":{\"blobStoreName\":\"default\",\"strictContentTypeValidation\":false},\"proxy\":{\"remoteUrl\":\"https://conda.anaconda.org/\",\"contentMaxAge\":-1,\"metadataMaxAge\":1440},\"negativeCache\":{\"enabled\":true,\"timeToLive\":10},\"httpClient\":{\"blocked\":$1,\"autoBlock\":true}}"
}
mm --version
docker exec "$name-conda" conda --version

echo "== proxy: a channel below the repository root"
mm create -y -q -n forge -c "$FORGE" --override-channels tzdata >/dev/null
docker exec "$name-mm" test -d /tmp/mm/envs/forge/share/zoneinfo
cc create -y -q -n forge -c "$FORGE" --override-channels tzdata >/dev/null
docker exec "$name-conda" sh -c 'test -d /opt/conda/envs/forge/share/zoneinfo'
found="$(rest "$REST/search?repository=conda-org&name=tzdata")"
has "$found" '"group":"noarch"'
has "$found" '/conda-forge/noarch/tzdata-'
preview="$(rest -X POST -H 'X-Command-Center-UI: 1' -H 'Content-Type: application/json' "$BASE/-/api/cleanup/preview" \
  -d '{"format":"conda","namePattern":"noarch:tzdata","pathPattern":"conda-forge/noarch/tzdata-.*","repository":"conda-org"}')"
has "$preview" '"name":"tzdata"'

echo "== proxy: the second install comes from the cache"
blocked true
mm clean -a -y >/dev/null
mm create -y -q -n forge2 -c "$FORGE" --override-channels tzdata >/dev/null
docker exec "$name-mm" test -d /tmp/mm/envs/forge2/share/zoneinfo
blocked false

echo "== hosted: packages made by micromamba, and real ones through the proxy"
docker exec -i "$name-mm" sh -s <<'EOF'
set -e
mk() { # name version build
  d=/tmp/pkg/$1-$2-$3; mkdir -p "$d/info" "$d/share/$1"
  echo "hello from $1 $2" > "$d/share/$1/hello.txt"
  printf 'share/%s/hello.txt\n' "$1" > "$d/info/files"
  size=$(wc -c < "$d/share/$1/hello.txt"); sum=$(sha256sum "$d/share/$1/hello.txt" | cut -d' ' -f1)
  printf '{"paths_version":1,"paths":[{"_path":"share/%s/hello.txt","path_type":"hardlink","sha256":"%s","size_in_bytes":%s}]}\n' "$1" "$sum" "$size" > "$d/info/paths.json"
  printf '{"name":"%s","version":"%s","build":"%s","build_number":0,"subdir":"noarch","noarch":"generic","depends":[],"license":"MIT","timestamp":1760000000000}\n' "$1" "$2" "$3" > "$d/info/index.json"
  printf '{"summary":"smoke package %s","license":"MIT","home":"https://example.com"}\n' "$1" > "$d/info/about.json"
}
mk ccsmoke 1.0 0; mk ccsmoke 1.1 0; mk ccgroup 1.0 0; mk ccrest 1.0 0; mk pytorch-mutex 0.1 0
cd /share
micromamba package compress /tmp/pkg/ccsmoke-1.0-0 ccsmoke-1.0-0.conda >/dev/null
micromamba package compress /tmp/pkg/ccsmoke-1.1-0 ccsmoke-1.1-0.tar.bz2 >/dev/null
micromamba package compress /tmp/pkg/ccgroup-1.0-0 ccgroup-1.0-0.conda >/dev/null
micromamba package compress /tmp/pkg/ccrest-1.0-0 ccrest-1.0-0.conda >/dev/null
micromamba package compress /tmp/pkg/pytorch-mutex-0.1-0 pytorch-mutex-0.1-0.conda >/dev/null
# not a package of what its name says
cp ccsmoke-1.0-0.conda other-1.0-0.conda
EOF
put 201 ccsmoke-1.0-0.conda noarch/
put 201 ccsmoke-1.1-0.tar.bz2 noarch/
want 401 -T /share/ccsmoke-1.0-0.conda "$HOSTED/noarch/"              # no credentials
put 409 ccsmoke-1.0-0.conda noarch/                                   # allow_once
put 400 other-1.0-0.conda noarch/                                     # index.json names another package
put 400 ccsmoke-1.0-0.conda linux-64/                                 # index.json names another subdir
put 400 ccsmoke-1.0-0.conda channel/noarch/                           # a hosted channel is the repository root
want 204 -u ci:smoke-pass -F conda.directory=noarch -F conda.asset=@/share/ccrest-1.0-0.conda "$REST/components?repository=conda-hosted"
# Real packages, as conda-build made them: a .conda of conda-forge and a .tar.bz2 of the pytorch channel.
tz="$(rest "$REST/search?repository=conda-org&name=tzdata" | sed -E 's|.*"path":"/conda-forge/noarch/(tzdata-[^"]*\.conda)".*|\1|')"
docker exec "$name-curl" curl -sSf -o "/share/$tz" "$FORGE/noarch/$tz"
docker exec "$name-curl" curl -sSf -o /share/pytorch-mutex-1.0-cpu.tar.bz2 "$REPO/conda-pytorch/noarch/pytorch-mutex-1.0-cpu.tar.bz2"
put 201 "$tz" noarch/
put 201 pytorch-mutex-1.0-cpu.tar.bz2 noarch/

echo "== hosted: install"
mm create -y -q -n h -c "$HOSTED" --override-channels ccsmoke >/dev/null
test "$(docker exec "$name-mm" cat /tmp/mm/envs/h/share/ccsmoke/hello.txt)" = "hello from ccsmoke 1.1"   # the .tar.bz2
mm create -y -q -n h2 -c "$HOSTED" --override-channels ccsmoke=1.0 ccrest tzdata pytorch-mutex >/dev/null
test "$(docker exec "$name-mm" cat /tmp/mm/envs/h2/share/ccsmoke/hello.txt)" = "hello from ccsmoke 1.0"   # the .conda
docker exec "$name-mm" test -d /tmp/mm/envs/h2/share/zoneinfo
cc create -y -q -n h -c "$HOSTED" --override-channels ccsmoke >/dev/null
test "$(docker exec "$name-conda" cat /opt/conda/envs/h/share/ccsmoke/hello.txt)" = "hello from ccsmoke 1.1"
listed="$(cc search -c "$HOSTED" --override-channels ccsmoke)"
has "$listed" 1.0
has "$listed" 1.1

echo "== group: a hosted and a proxied package, over a member with sharded repodata"
want 200 "$REPO/conda-pytorch/noarch/repodata_shards.msgpack.zst"     # the proxy passes the shards through
want 404 "$GROUP/noarch/repodata_shards.msgpack.zst"                  # alone they would hide the hosted packages
want 201 -u ci:smoke-pass -T /share/ccgroup-1.0-0.conda "$GROUP/noarch/"   # through the group, to its writeMember
mm clean -a -y >/dev/null
mm create -y -q -n g -c "$GROUP" --override-channels ccsmoke ccgroup pytorch-mutex=1.0=cpu >/dev/null
test "$(docker exec "$name-mm" cat /tmp/mm/envs/g/share/ccgroup/hello.txt)" = "hello from ccgroup 1.0"
docker exec "$name-mm" sh -c 'ls /tmp/mm/envs/g/conda-meta | grep -q pytorch-mutex'
cc clean -a -y >/dev/null
cc create -y -q -n g -c "$GROUP" --override-channels ccsmoke ccgroup pytorch-mutex=1.0=cpu >/dev/null
test "$(docker exec "$name-conda" cat /opt/conda/envs/g/share/ccgroup/hello.txt)" = "hello from ccgroup 1.0"

echo "== group: a newer public build does not beat an internal package of that name"
want 201 -u ci:smoke-pass -T /share/pytorch-mutex-0.1-0.conda "$REPO/conda-internal/noarch/"
mm clean -a -y >/dev/null
mm create -y -q -n first -c "$REPO/conda-first" --override-channels pytorch-mutex >/dev/null
test "$(docker exec "$name-mm" cat /tmp/mm/envs/first/share/pytorch-mutex/hello.txt)" = "hello from pytorch-mutex 0.1"
cc clean -a -y >/dev/null
cc create -y -q -n first -c "$REPO/conda-first" --override-channels pytorch-mutex >/dev/null
test "$(docker exec "$name-conda" cat /opt/conda/envs/first/share/pytorch-mutex/hello.txt)" = "hello from pytorch-mutex 0.1"
echo "== group with mergeMembers: every member's builds"
mm clean -a -y >/dev/null
mm create -y -q -n merged -c "$REPO/conda-merged" --override-channels pytorch-mutex >/dev/null
docker exec "$name-mm" sh -c 'ls /tmp/mm/envs/merged/conda-meta | grep -q pytorch-mutex-1.0'

echo "== a channel's index.html through the proxy"
page="$(docker exec "$name-curl" curl -sS -D - -o /dev/null "$REPO/conda-pytorch/noarch/index.html")"
has "$page" ' 200'
has "$page" 'text/html'

echo "== REST: delete from hosted"
found="$(rest "$REST/search?repository=conda-hosted&name=ccrest")"
id="$(printf '%s' "$found" | sed -E 's/.*"id":"([^"]*)","name":"ccrest".*/\1/')"
want 204 -u admin:smoke-pass -X DELETE "$REST/components/$id"
hasnt "$(docker exec "$name-curl" curl -s "$HOSTED/noarch/repodata.json")" ccrest-1.0-0

echo "== cleanup runs on the channel below the root"
rest -o /dev/null -f -X POST -H 'X-Command-Center-UI: 1' -H 'Content-Type: application/json' "$BASE/-/api/cleanup/policies" \
  -d '{"name":"forge-tzdata","format":"conda","pathPattern":"conda-forge/noarch/tzdata-.*"}'
rest -o /dev/null -f -X PUT -H 'Content-Type: application/json' "$REST/repositories/conda/proxy/conda-org" \
  -d '{"name":"conda-org","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":false},"cleanup":{"policyNames":["forge-tzdata"]},"proxy":{"remoteUrl":"https://conda.anaconda.org/","contentMaxAge":-1,"metadataMaxAge":1440},"negativeCache":{"enabled":true,"timeToLive":10},"httpClient":{"blocked":false,"autoBlock":true}}'
task="$(rest -X POST -H 'Content-Type: application/json' "$REST/tasks" \
  -d '{"type":"repository.cleanup","name":"cleanup-forge","enabled":true,"alertEmail":"","notificationCondition":"FAILURE","frequency":{"schedule":"manual"},"properties":{}}')"
tid="$(printf '%s' "$task" | sed -E 's/.*"id":"([^"]*)".*/\1/')"
want 204 -u admin:smoke-pass -X POST "$REST/tasks/$tid/run"
result=""
for i in $(seq 1 30); do
  state="$(rest "$REST/tasks/$tid")"
  case "$state" in *'"lastRunResult":"OK"'*) result=ok; break;; *'"lastRunResult":"FAILED"'*) result=failed; break;; esac
  sleep 1
done
[ "$result" = ok ] || { echo "FAIL: the cleanup task: $state"; exit 1; }
hasnt "$(rest "$REST/search?repository=conda-org&name=tzdata")" '"name":"tzdata"'

echo "== what the clients asked for"
docker logs "$name" > "$work/server.log" 2>&1
asked() {
  grep -q "\"method\":\"$1\",\"path\":\"/repository/$2\",\"status\":$3," "$work/server.log" ||
    { echo "FAIL: the client made no $1 $2 answered $3"; exit 1; }
}
asked GET conda-org/conda-forge/noarch/repodata_shards.msgpack.zst 200   # both clients read shards where they can
asked GET conda-all/noarch/repodata_shards.msgpack.zst 404               # conda too
asked HEAD conda-hosted/noarch/repodata_shards.msgpack.zst 404
asked GET conda-hosted/noarch/repodata.json 200
asked GET conda-hosted/linux-64/repodata.json 200
asked HEAD conda-all/noarch/repodata_shards.msgpack.zst 404
asked GET conda-all/noarch/repodata.json 200
asked GET conda-all/linux-64/repodata.json 200

echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "CONDA SMOKE OK"
