#!/usr/bin/env bash
# High availability smoke test: PostgreSQL and two Command Center nodes on one
# shared data volume, asked in turn by the client in test/ha (no load balancer:
# the client alternates, and retries a request on the other node as a load
# balancer would). Docker only; it pulls nothing when the images below are
# local.
#   1. check: maven deploy and maven-metadata.xml, npm publish and install
#      metadata, a Docker chunked push with each step on the other node, a
#      session started on one node and used and ended on the other, a task
#      due once that runs once, and webhook deliveries received once each.
#   2. kill -9: under a request loop, the node running a long task
#      (vulnerability.sync against a dump the receiver holds for 20 s) is
#      killed; the other keeps serving, runs the task again and finishes it
#      (at least once), and every webhook delivery is received at least once.
#   3. rolling restart: under a request loop, each node is stopped (SIGTERM)
#      and started again in turn; no request fails beyond client retries.
# Environment:
#   HA_BASE_IMAGE  image the nodes and the client run in, with the binaries
#                  built from this tree mounted (default postgres:17-alpine:
#                  the PostgreSQL image, so nothing more to pull)
#   HA_PG_IMAGE    PostgreSQL image (default postgres:17-alpine)
#   HA_GOCACHE     a Go cache directory with build/ and mod/: build in
#                  HA_GO_IMAGE (default golang:1.27.3) offline instead of
#                  with the go command on the host
set -euo pipefail
cd "$(dirname "$0")/../.."
root="$(pwd)"
id=$$
net=cc-ha-$id pg=cc-ha-pg-$id a=cc-ha-a-$id b=cc-ha-b-$id recv=cc-ha-recv-$id vol=cc-ha-data-$id
load1=cc-ha-load1-$id load2=cc-ha-load2-$id
base="${HA_BASE_IMAGE:-postgres:17-alpine}"
pgimage="${HA_PG_IMAGE:-postgres:17-alpine}"
work="$(mktemp -d)"
pass="ha-pass-$id"
pgpw="pg-$id"
dburl="postgres://postgres:$pgpw@ha-pg:5432/cc?sslmode=disable"

cleanup() {
  docker rm -f -v "$a" "$b" "$recv" "$pg" "$load1" "$load2" >/dev/null 2>&1 || true
  docker volume rm "$vol" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work"
}
fail() {
  echo "FAIL: $*"
  for c in "$a" "$b"; do echo "---- $c (last 30 lines)"; docker logs --tail 30 "$c" 2>&1 || true; done
  exit 1
}
trap cleanup EXIT

gobuild() { # gobuild OUT PACKAGE: a static linux binary in $work
  if [ -n "${HA_GOCACHE:-}" ]; then
    docker run --rm --network none --user "$(id -u):$(id -g)" -e HOME=/tmp -e GOCACHE=/cache/build -e GOMODCACHE=/cache/mod \
      -e GOFLAGS=-buildvcs=false -e GOPROXY=off -e CGO_ENABLED=0 -v "$HA_GOCACHE:/cache" -v "$root:/src:ro" -v "$work:/out" \
      -w /src "${HA_GO_IMAGE:-golang:1.27.3}" go build -o "/out/$1" "$2"
  else
    CGO_ENABLED=0 GOOS=linux go build -o "$work/$1" "$2"
  fi
}
echo "== build"
gobuild commandcenter ./cmd/commandcenter
gobuild ha-client ./test/ha
chmod 755 "$work" "$work"/*

docker network create "$net" >/dev/null
docker volume create "$vol" >/dev/null
docker run --rm -v "$vol:/data" --entrypoint chown "$base" 65532:65532 /data
docker run -d --name "$pg" --network "$net" --network-alias ha-pg -e POSTGRES_PASSWORD="$pgpw" -e POSTGRES_DB=cc "$pgimage" >/dev/null
for _ in $(seq 60); do docker exec "$pg" pg_isready -q -U postgres -d cc 2>/dev/null && break; sleep 1; done

hash="$(printf '%s\n' "$pass" | docker run --rm -i -v "$work:/w:ro" --entrypoint /w/commandcenter "$base" hash-password)"
key="$(head -c 32 /dev/urandom | base64)"
cat >"$work/config.yaml" <<EOF
listen: ":8081"
dataDir: /data
baseURL: "http://commandcenter.example:8081"
users:
  - { name: admin, passwordHash: "$hash", roles: [admin] }
repositories:
  - { name: maven-releases, format: maven, type: hosted }
  - { name: npm-hosted, format: npm, type: hosted }
  - { name: docker-hosted, format: docker, type: hosted }
  - { name: raw-load, format: raw, type: hosted, writePolicy: allow }
EOF
chmod 644 "$work/config.yaml"

start_node() { # start_node NAME: a node, or the stopped container again
  if docker container inspect "$1" >/dev/null 2>&1; then docker start "$1" >/dev/null; return; fi
  docker run -d --name "$1" --hostname "$1" --network "$net" --user 65532:65532 --memory 256m \
    -e COMMANDCENTER_DATABASE_URL="$dburl" -e COMMANDCENTER_SECRET_KEY="$key" -e GOMEMLIMIT=160MiB \
    -v "$vol:/data" -v "$work/config.yaml:/etc/commandcenter/config.yaml:ro" -v "$work/commandcenter:/usr/local/bin/commandcenter:ro" \
    --entrypoint /usr/local/bin/commandcenter "$base" serve >/dev/null
}
client() { docker run --rm --network "$net" -e HA_PASS="$pass" -v "$work:/w:ro" --entrypoint /w/ha-client "$base" "$@"; }
load_start() { # load_start NAME SECONDS: the request loop in the background
  docker run -d --name "$1" --network "$net" -e HA_PASS="$pass" -v "$work:/w:ro" --entrypoint /w/ha-client "$base" \
    load -a "http://$a:8081" -b "http://$b:8081" -for "$2s" >/dev/null
}
load_end() { # load_end NAME: waits for the loop and fails on a request no node served
  local rc
  rc="$(docker wait "$1")"
  docker logs "$1" 2>&1 | tail -5
  [ "$rc" = 0 ] || fail "request loop $1 exited $rc"
}

echo "== start PostgreSQL, node A, node B and the receiver"
docker run -d --name "$recv" --network "$net" --network-alias ha-recv -v "$work:/w:ro" --entrypoint /w/ha-client "$base" receive >/dev/null
start_node "$a"
start_node "$b"
client wait -a "http://$a:8081" -b "http://$b:8081" || fail "the nodes did not get ready"

echo "== 1. check: formats, sessions, Docker chunks, a task once, webhooks once"
client check -a "http://$a:8081" -b "http://$b:8081" -recv http://ha-recv:9999 -db "$dburl" || fail "check"

echo "== 2. kill -9 the node running a task, under a request loop"
load_start "$load1" 120
out="$(client sync-start -a "http://$a:8081" -b "http://$b:8081" -recv http://ha-recv:9999)" || fail "sync-start: $out"
echo "$out"
victim="$(awk '/^RUNNING/ {print $2}' <<<"$out")" task="$(awk '/^RUNNING/ {print $3}' <<<"$out")"
case "$victim" in "$a") other=$b ;; "$b") other=$a ;; *) fail "unknown node $victim" ;; esac
docker kill -s KILL "$victim" >/dev/null
echo "killed $victim; $other carries on"
client sync-check -a "http://$other:8081" -recv http://ha-recv:9999 -db "$dburl" -task "$task" || fail "sync-check"
start_node "$victim"
client wait -a "http://$victim:8081" || fail "$victim did not come back"
load_end "$load1"

echo "== 3. rolling restart under a request loop"
load_start "$load2" 75
sleep 5
for n in "$a" "$b"; do
  docker stop -t 60 "$n" >/dev/null
  start_node "$n"
  client wait -a "http://$n:8081" || fail "$n did not come back"
  echo "restarted $n"
done
load_end "$load2"

docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$a" "$b"
echo "OK: high availability smoke test passed"
