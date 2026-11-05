#!/usr/bin/env bash
# Container, network and work-dir names carry the shell PID, so runs can overlap.
# Builds the image, starts commandcenter, then runs nexus3-cli (the PyPI
# package, in python:3.12-slim) against it as an administrator:
#   1. login
#   2. the commands that use the REST API: blobstore list, create, show and
#      delete; task list, show, run and stop; security realm available, active
#      and activate; script list
#   3. the commands that install and run Groovy scripts through Nexus's script
#      API, which Command Center does not have: repository list, show, create
#      and delete; list, upload, download and delete of files; cleanup-policy
#      list, show and create; script create, run and delete. Each must fail
#      and say why, and leave nothing behind.
# Finally prints the container's memory usage.
#   COMPAT_PREFIX  start of the container and network names (default commandcenter-compat)
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/compat/.work-nexus3-cli-$$"
name="${COMPAT_PREFIX:-commandcenter-compat}-nexus3-cli-$$"
net="$name"
. test/postgres.sh

cleanup() {
  docker rm -f -v "$name" "$name-pg" "$name-cli" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup

docker build -q -t commandcenter:compat . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i --name "$name-hash" --memory 128m commandcenter:compat hash-password)"
rm -rf "$work" && mkdir -p "$work"

cat > "$work/config.yaml" <<EOF
anonymousRead: true
baseURL: "http://commandcenter:8081"
users:
  - { name: admin, passwordHash: "$hash", roles: [admin] }
repositories:
  - { name: raw-hosted, format: raw, type: hosted }
EOF

cat > "$work/run.sh" <<'EOF'
set -eu
export PIP_DISABLE_PIP_VERSION_CHECK=1 PIP_NO_CACHE_DIR=1 PIP_ROOT_USER_ACTION=ignore
# The client's own warnings: pkg_resources, and no version check without a Server header.
export PYTHONWARNINGS=ignore

echo "== install"
# nexus3-cli imports pkg_resources, which setuptools 81 dropped.
pip install -q nexus3-cli 'setuptools<81'
echo "nexus3-cli $(nexus3 --version)"

fail() { echo "FAIL: $*" >&2; exit 1; }
# ok <arguments>: the command succeeds; its output is in $out.
ok() { out="$(nexus3 "$@" 2>&1)" || fail "nexus3 $*: $out"; }
# has <text>: $out holds text.
has() { case "$out" in *"$1"*) ;; *) fail "no \"$1\" in: $out" ;; esac; }
# scripted <arguments>: the command needs the script API, so it must fail and say that there is none.
scripted() {
  if out="$(nexus3 "$@" 2>&1)"; then fail "nexus3 $* succeeded: $out"; fi
  case "$out" in
  *Error:*"Command Center has no script API"*) ;;
  *) fail "nexus3 $* does not say why it failed: $out" ;;
  esac
}

echo "== login"
ok login -U http://commandcenter:8081 -u admin -p smoke-pass --no-x509_verify
has "Login successful"

echo "== blobstore: list, create, show, delete"
ok blobstore list
has default
ok blobstore create file -q spaceUsedQuota -l 1000000 extra /data/extra
ok blobstore show extra
has '"path": "/data/extra"'
has '"limit": 1000000'
ok blobstore list
has extra
ok blobstore delete --yes extra
ok blobstore list
case "$out" in *extra*) fail "the blob store was not deleted: $out" ;; esac
if out="$(nexus3 blobstore show extra 2>&1)"; then fail "blobstore show of a deleted store succeeded: $out"; fi
has "not found"

echo "== task: list, show, run, stop"
ok task list
has compact-blobstore
ok task show compact-blobstore
has '"type": "blobstore.compact"'
ok task run compact-blobstore
for i in $(seq 1 30); do
  ok task show compact-blobstore
  case "$out" in *'"lastRunResult": "OK"'*) break ;; esac
  sleep 1
done
has '"lastRunResult": "OK"'
if out="$(nexus3 task stop compact-blobstore 2>&1)"; then fail "stopping a task that does not run succeeded: $out"; fi
has "Unable to stop task"

echo "== security realm: available, active, activate"
ok security realm available
has NexusAuthenticatingRealm
ok security realm active
case "$out" in *User-Token-Realm*) fail "the user token realm is active from the start: $out" ;; esac
ok security realm activate User-Token-Realm
ok security realm active
has User-Token-Realm
if out="$(nexus3 security realm activate NoSuchRealm 2>&1)"; then fail "activating an unknown realm succeeded: $out"; fi
has "Unknown realmIds"

echo "== script list"
ok script list
case "$out" in *groovy*) fail "there is a script: $out" ;; esac

echo "== commands that need the script API: refused with the reason"
mkdir -p /tmp/files/sub
echo one > /tmp/files/one.txt
echo two > /tmp/files/sub/two.txt
echo 'log.info("hello")' > /tmp/hello.groovy
scripted repository list
scripted repository show raw-hosted
scripted repository create hosted raw made-raw
scripted repository create hosted maven made-maven
scripted repository create hosted npm made-npm
scripted repository create proxy pypi made-pypi https://pypi.org/
scripted repository create group raw made-group
scripted repository delete --yes raw-hosted
scripted list raw-hosted
scripted upload /tmp/files/one.txt raw-hosted/one.txt
scripted upload /tmp/files/ raw-hosted/files/
scripted download raw-hosted/one.txt /tmp/down/
scripted download raw-hosted/files/ /tmp/down/
scripted delete raw-hosted/one.txt
scripted cleanup-policy list
scripted cleanup-policy show any
scripted cleanup-policy create --format maven --downloaded 30 made-policy
scripted script create hello /tmp/hello.groovy
scripted script run hello
scripted script delete hello
echo "NEXUS3-CLI OK"
EOF

docker network create "$net" >/dev/null
start_postgres
docker update --memory 512m --memory-swap 512m "$name-pg" >/dev/null
docker run -d --name "$name" --network "$net" -e COMMANDCENTER_DATABASE_URL="$pg_url" --network-alias commandcenter --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" -p 127.0.0.1::8081 commandcenter:compat >/dev/null
cc="http://$(docker port "$name" 8081/tcp | head -1)"

ready=0
for i in $(seq 1 30); do
  if curl -fs "$cc/service/rest/v1/status" >/dev/null; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "FAIL: commandcenter did not become ready"; docker logs "$name"; exit 1
fi

# The work dir is mounted read-only: the client writes inside its container only.
docker run --rm --name "$name-cli" --network "$net" --memory 512m -v "$root/$work:/work:ro" python:3.12-slim sh /work/run.sh

echo "== the refused commands changed nothing"
repos="$(curl -fsS -u admin:smoke-pass "$cc/service/rest/v1/repositories")"
if [ "$(grep -o '"name":"[^"]*"' <<<"$repos")" != '"name":"raw-hosted"' ]; then
  echo "FAIL: the repositories are not as they were: $repos" >&2
  exit 1
fi
if ! curl -fsS "$cc/service/rest/v1/assets?repository=raw-hosted" | grep -q '"items":\[\]'; then
  echo "FAIL: raw-hosted holds files" >&2
  exit 1
fi

echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "NEXUS3-CLI COMPAT OK"
