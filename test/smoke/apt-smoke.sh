#!/usr/bin/env bash
# Builds the image, starts commandcenter, then uses real apt-get, dpkg-deb
# and gpg (Debian 13) to:
#   1. apt-get update and install curl and gpg through the apt-all group:
#      Debian main comes from deb.debian.org via apt-proxy (one suite, one
#      component, one architecture), verified with Debian's archive keyring
#   2. build tiny packages with dpkg-deb (xz, zstd, gzip and uncompressed)
#      and upload them the four documented ways: PUT, POST of the bare file
#      as Nexus documents it, a multipart POST, the component upload API
#   3. install from the unsigned repository through the group with
#      [trusted=yes] (a dependency comes via the proxy); without it apt
#      refuses the repository
#   4. create two signed repositories through the Nexus REST API with keys
#      made here (Ed25519; RSA with a passphrase), apt-get update of the
#      still empty repository with signed-by (the armored public.key, and
#      the gpg --dearmor keyring), install, snapshot, upload a second
#      version, upgrade, install the first version from the snapshot again,
#      delete the second version
# The proxy downloads about 18 MB from deb.debian.org.
# Finally prints the container's memory usage.
#   SMOKE_APT_IMAGE  the client image (default debian:trixie-slim; any Debian release)
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-apt-$$"
net=commandcenter-apt-smoke-$$
name=commandcenter-apt-smoke-$$
. test/postgres.sh

cleanup() {
  docker rm -f -v "$name" "$name-pg" "$name-c" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup
rm -rf "$work" && mkdir -p "$work"

docker build -q -t commandcenter:smoke-apt . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i commandcenter:smoke-apt hash-password)"

cat > "$work/config.yaml" <<EOF
anonymousRead: true
baseURL: "http://commandcenter:8081"
users:
  - { name: admin, passwordHash: "$hash", roles: [admin] }
  - { name: ci, passwordHash: "$hash", roles: [deployer] }
roles:
  deployer: { write: ["*"] }
repositories:
  - { name: apt-proxy,  format: apt, type: proxy,  remote: "https://deb.debian.org/debian/" }
  - { name: apt-hosted, format: apt, type: hosted }
  - { name: apt-all,    format: apt, type: group,  members: [apt-hosted, apt-proxy], writeMember: apt-hosted }
EOF

cat > "$work/run.sh" <<'EOF'
set -eu
export DEBIAN_FRONTEND=noninteractive
CC=http://commandcenter:8081
R=$CC/repository
. /etc/os-release
suite=$VERSION_CODENAME
arch=$(dpkg --print-architecture)
fail() { echo "FAIL: $*"; exit 1; }
status() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
# apt-get update fails on any warning; no translations, no recommended packages.
printf 'APT::Update::Error-Mode "any";\nAcquire::Languages "none";\nAPT::Install-Recommends "false";\n' > /etc/apt/apt.conf.d/99smoke
# source <name> <line>: this source only, beside Debian's.
source_is() { rm -f /etc/apt/sources.list.d/cc-*.list; echo "$2" > "/etc/apt/sources.list.d/cc-$1.list"; }
# offered <package>: the versions the sources offer, like "1.0 2.0".
offered() { apt-cache madison "$1" | awk -F'|' '{gsub(/ /, "", $2); print $2}' | sort -u | xargs; }

echo "== apt-get update and install through the group ($ID $suite main via the proxy, the archive keyring)"
rm -f /etc/apt/sources.list /etc/apt/sources.list.d/*
echo "deb $R/apt-all $suite main" > /etc/apt/sources.list.d/debian.list
apt-get update -qq
apt-get install -y -qq curl gpg gpg-agent >/dev/null
curl --version | head -1

echo "== build packages with dpkg-deb"
mkdeb() { # mkdeb <package> <version> <architecture> <compression> [depends]
  d="/tmp/deb/$1_$2_$3"
  mkdir -p "$d/DEBIAN" "$d/usr/bin"
  printf '#!/bin/sh\necho "%s %s"\n' "$1" "$2" > "$d/usr/bin/$1"
  chmod 755 "$d/usr/bin/$1"
  {
    printf 'Package: %s\nVersion: %s\nArchitecture: %s\nMaintainer: Smoke Test <smoke@example.com>\n' "$1" "$2" "$3"
    [ -z "${5:-}" ] || printf 'Depends: %s\n' "$5"
    printf 'Description: Command Center apt smoke package\n A tiny package for the apt smoke test.\n'
  } > "$d/DEBIAN/control"
  dpkg-deb --root-owner-group "-Z$4" --build "$d" /tmp/deb >/dev/null
}
mkdeb smoke-hello 1.0 all xz bc
mkdeb smoke-hello 2.0 all zstd bc
mkdeb smoke-tool 1.0 "$arch" gzip
mkdeb smoke-plain 1.0 all none
mkdeb smoke-form 1:1.0-1 all xz
cd /tmp/deb

echo "== upload to the unsigned repository: PUT, Nexus POST, multipart POST, component API"
put="$(curl -fsS -u ci:smoke-pass -T smoke-tool_1.0_${arch}.deb "$R/apt-hosted/pool/smoke-tool_1.0_${arch}.deb")"
echo "$put" | grep -q "\"filename\":\"pool/main/s/smoke-tool/smoke-tool_1.0_${arch}.deb\"" || fail "PUT answer: $put"
post="$(curl -fsS -u ci:smoke-pass -H 'Content-Type: multipart/form-data' --data-binary @smoke-hello_1.0_all.deb "$R/apt-all/")"
echo "$post" | grep -q '"filename":"pool/main/s/smoke-hello/smoke-hello_1.0_all.deb"' || fail "Nexus POST through the group: $post"
form="$(curl -fsS -u ci:smoke-pass -F file=@smoke-form_1.0-1_all.deb "$R/apt-hosted/")"
echo "$form" | grep -q '"filename":"pool/main/s/smoke-form/smoke-form_1%3a1.0-1_all.deb"' || fail "multipart POST: $form"
test "$(status -u ci:smoke-pass -F apt.asset=@smoke-plain_1.0_all.deb "$CC/service/rest/v1/components?repository=apt-hosted")" = 204 || fail "component upload API"
test "$(status -u ci:smoke-pass -T smoke-hello_1.0_all.deb "$R/apt-hosted/pool/x.deb")" = 409 || fail "re-upload is not 409"
test "$(status -T smoke-hello_2.0_all.deb "$R/apt-hosted/pool/x.deb")" = 401 || fail "anonymous upload is not 401"
test "$(status -u ci:smoke-pass -T /etc/os-release "$R/apt-hosted/pool/x.deb")" = 400 || fail "a file that is no package is not 400"
test "$(status "$R/apt-hosted/pool/s/smoke-hello/smoke-hello_1.0_all.deb")" = 200 || fail "the Nexus pool path"
test "$(status "$R/apt-hosted/public.key")" = 404 || fail "public.key of a repository without a key"

echo "== unsigned repository: refused without [trusted=yes], installs with it (the dependency via the proxy)"
source_is hosted "deb $R/apt-all stable main"
if apt-get update -qq >/tmp/unsigned.log 2>&1; then fail "apt accepted an unsigned repository"; fi
grep -q 'not signed' /tmp/unsigned.log || { cat /tmp/unsigned.log; fail "no 'not signed' error"; }
source_is hosted "deb [trusted=yes] $R/apt-all stable main"
apt-get update -qq
apt-get install -y -qq smoke-hello smoke-tool smoke-plain smoke-form >/dev/null
test "$(smoke-hello)" = "smoke-hello 1.0" && test "$(smoke-tool)" = "smoke-tool 1.0" && test "$(smoke-plain)" = "smoke-plain 1.0" || fail "installed packages do not run"
test "$(dpkg-query -W -f '${Version}' smoke-form)" = "1:1.0-1" || fail "the package with an epoch"
dpkg-query -W bc >/dev/null || fail "the dependency was not installed"
apt-cache show smoke-hello | grep -q '^Filename: pool/main/s/smoke-hello/smoke-hello_1.0_all.deb$' || fail "Packages stanza"
apt-get purge -y -qq smoke-hello smoke-tool smoke-plain smoke-form >/dev/null

echo "== signed repositories through the Nexus REST API: Ed25519, and RSA with a passphrase"
export GNUPGHOME=/tmp/gnupg
mkdir -m 700 "$GNUPGHOME"
gpg --batch --quiet --pinentry-mode loopback --passphrase '' --quick-gen-key 'Smoke Ed25519 <ed@example.com>' ed25519 sign never
gpg --batch --quiet --pinentry-mode loopback --passphrase 'key-pass' --quick-gen-key 'Smoke RSA <rsa@example.com>' rsa3072 sign never
create() { # create <repository> <key uid> <passphrase>
  key="$(gpg --batch --pinentry-mode loopback --passphrase "$3" --armor --export-secret-keys "$2" | awk '{printf "%s\\n", $0}')"
  code="$(status -u admin:smoke-pass -H 'Content-Type: application/json' "$CC/service/rest/v1/repositories/apt/hosted" -d '{
    "name": "'"$1"'", "online": true,
    "storage": {"blobStoreName": "default", "strictContentTypeValidation": true, "writePolicy": "allow_once"},
    "apt": {"distribution": "stable"},
    "aptSigning": {"keypair": "'"$key"'", "passphrase": "'"$3"'"}}')"
  test "$code" = 201 || fail "create $1: $code"
}
create apt-ed ed@example.com ''
create apt-rsa rsa@example.com key-pass
curl -fsS -u admin:smoke-pass "$CC/service/rest/v1/repositories/apt/hosted/apt-rsa" | grep -q 'BEGIN PGP PRIVATE' && fail "the API returns the private key"

echo "== Ed25519: signed-by the armored public.key, install, snapshot, second version, upgrade"
mkdir -p /etc/apt/keyrings
curl -fsS "$R/apt-ed/public.key" -o /etc/apt/keyrings/cc-ed.asc
grep -q 'BEGIN PGP PUBLIC KEY BLOCK' /etc/apt/keyrings/cc-ed.asc || fail "public.key"
source_is ed "deb [signed-by=/etc/apt/keyrings/cc-ed.asc] $R/apt-ed stable main"
apt-get update -qq    # nothing uploaded yet: the empty signed Release
curl -fsS -u ci:smoke-pass -T smoke-hello_1.0_all.deb "$R/apt-ed/pool/smoke-hello_1.0_all.deb" >/dev/null
apt-get update -qq
apt-get install -y -qq smoke-hello >/dev/null
test "$(smoke-hello)" = "smoke-hello 1.0" || fail "smoke-hello 1.0 from the signed repository"
test "$(status -u ci:smoke-pass -X MKCOL "$R/apt-ed/snapshots/rel-1")" = 201 || fail "snapshot"
curl -fsS -u ci:smoke-pass -T smoke-hello_2.0_all.deb "$R/apt-ed/pool/smoke-hello_2.0_all.deb" >/dev/null
apt-get update -qq
apt-get install -y -qq --only-upgrade smoke-hello >/dev/null
test "$(smoke-hello)" = "smoke-hello 2.0" || fail "smoke-hello was not upgraded"

echo "== the snapshot still has the first version only"
source_is snap "deb [signed-by=/etc/apt/keyrings/cc-ed.asc] $R/apt-ed/snapshots/rel-1 stable main"
apt-get update -qq
test "$(offered smoke-hello)" = 1.0 || fail "the snapshot offers: $(offered smoke-hello)"
apt-get install -y -qq --allow-downgrades smoke-hello=1.0 >/dev/null
test "$(smoke-hello)" = "smoke-hello 1.0" || fail "smoke-hello 1.0 from the snapshot"
test "$(status -u ci:smoke-pass -X DELETE "$R/apt-ed/snapshots/rel-1")" = 204 || fail "delete the snapshot"
test "$(status "$R/apt-ed/snapshots/rel-1/dists/stable/InRelease")" = 404 || fail "the deleted snapshot is still served"

echo "== delete 2.0 (component delete API): dists/ follows"
source_is ed "deb [signed-by=/etc/apt/keyrings/cc-ed.asc] $R/apt-ed stable main"
apt-get update -qq
test "$(offered smoke-hello)" = "1.0 2.0" || fail "offered before the delete: $(offered smoke-hello)"
del="$(curl -fsS -u ci:smoke-pass -X DELETE -H 'X-Command-Center-UI: 1' "$CC/-/api/repositories/apt-ed/components?group=all&name=smoke-hello&version=2.0")"
echo "$del" | grep -q '"assets":1' || fail "component delete: $del"
apt-get update -qq
test "$(offered smoke-hello)" = 1.0 || fail "offered after the delete: $(offered smoke-hello)"

echo "== RSA with a passphrase: signed-by the gpg --dearmor keyring; another key is refused"
curl -fsS "$R/apt-rsa/public.key" | gpg --dearmor -o /usr/share/keyrings/cc-rsa.gpg
curl -fsS -u ci:smoke-pass -T "smoke-tool_1.0_${arch}.deb" "$R/apt-rsa/pool/smoke-tool_1.0_${arch}.deb" >/dev/null
source_is rsa "deb [signed-by=/usr/share/keyrings/cc-rsa.gpg] $R/apt-rsa stable main"
apt-get update -qq
apt-get install -y -qq smoke-tool >/dev/null
test "$(smoke-tool)" = "smoke-tool 1.0" || fail "smoke-tool from the RSA repository"
curl -fsS "$R/apt-rsa/dists/stable/Release" -o /tmp/Release
curl -fsS "$R/apt-rsa/dists/stable/Release.gpg" -o /tmp/Release.gpg
gpg --homedir "$(mktemp -d)" --batch --no-default-keyring --keyring /usr/share/keyrings/cc-rsa.gpg --verify /tmp/Release.gpg /tmp/Release 2>/tmp/verify.log ||
  { cat /tmp/verify.log; fail "Release.gpg does not verify"; }
source_is rsa "deb [signed-by=/etc/apt/keyrings/cc-ed.asc] $R/apt-rsa stable main"
if apt-get update -qq >/tmp/wrongkey.log 2>&1; then fail "apt accepted a signature of another key"; fi
echo "APT CLIENT OK"
EOF

docker network create "$net" >/dev/null
start_postgres
docker run -d --name "$name" --network "$net" -e COMMANDCENTER_DATABASE_URL="$pg_url" --network-alias commandcenter --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" commandcenter:smoke-apt >/dev/null

ready=0
for i in $(seq 1 30); do
  if docker exec "$name" commandcenter healthcheck >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "FAIL: commandcenter did not become ready"; docker logs "$name"; exit 1
fi

docker run --rm --name "$name-c" --network "$net" --memory 1g -v "$root/$work:/work" "${SMOKE_APT_IMAGE:-debian:trixie-slim}" sh /work/run.sh

echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "APT SMOKE OK"
