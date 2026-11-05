#!/usr/bin/env bash
# Container, network and work-dir names carry the shell PID, so runs can overlap.
# Builds the image, starts commandcenter, then uses real Gradle (the gradle
# image, its maven-publish plugin) to:
#   1. publish com.example.gradle:greeter:1.0.0 to maven-releases: jar, sources,
#      POM and Gradle module metadata (.module), each with its md5, sha1, sha256
#      and sha512 files; a second publish must be refused (allow_once)
#   2. publish 1.1.0-SNAPSHOT twice to maven-snapshots
#   3. resolve both from another build through the maven-public group, by
#      their module metadata, and run it: the SNAPSHOT is its second build
# Then compares the stored files with their checksum files and looks them up
# with the search API. Finally prints the container's memory usage.
#   COMPAT_PREFIX  start of the container and network names (default commandcenter-compat)
#   GRADLE_IMAGE   Gradle image (default gradle:jdk21)
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/compat/.work-gradle-$$"
name="${COMPAT_PREFIX:-commandcenter-compat}-gradle-$$"
net="$name"
. test/postgres.sh

cleanup() {
  docker rm -f -v "$name" "$name-pg" "$name-gradle" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup
fail() { echo "FAIL: $*" >&2; exit 1; }

docker build -q -t commandcenter:compat . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i --name "$name-hash" --memory 128m commandcenter:compat hash-password)"
rm -rf "$work" && mkdir -p "$work"
cp -r test/compat/gradle/lib test/compat/gradle/app "$work/"

cat > "$work/config.yaml" <<EOF
anonymousRead: true
baseURL: "http://commandcenter:8081"
users:
  - { name: ci, passwordHash: "$hash", roles: [deployer] }
roles:
  deployer: { write: [maven-releases, maven-snapshots] }
repositories:
  - { name: maven-releases,  format: maven, type: hosted }
  - { name: maven-snapshots, format: maven, type: hosted, versionPolicy: snapshot, writePolicy: allow }
  - { name: maven-public,    format: maven, type: group,  members: [maven-releases, maven-snapshots] }
EOF

cat > "$work/run.sh" <<'EOF'
set -eu
export CC_PASSWORD=smoke-pass
cp -r /work/lib /work/app /tmp/ # the work dir is read-only: Gradle writes beside its copies
g() { (cd "/tmp/$1" && shift && gradle -q --console=plain "$@"); }
fail() { echo "FAIL: $*" >&2; exit 1; }
# resolved <version> <component id pattern> <greeting>: the app gets the library through the group, from its
# module metadata, and runs with it.
resolved() {
  out="$(g app variants run -PlibVersion="$1")" || fail "resolving $1: $out"
  echo "$out" | grep -Eq "^VARIANT $2 runtimeElements\$" || fail "$1 was not resolved by its .module file: $out"
  echo "$out" | grep -qx "$3" || fail "$1: the app did not print \"$3\": $out"
}

echo "== $(gradle --version | grep '^Gradle')"
echo "== publish 1.0.0"
g lib publish -PlibVersion=1.0.0

echo "== a second publish must be refused (allow_once)"
if out="$(g lib publish -PlibVersion=1.0.0 2>&1)"; then fail "the second publish succeeded"; fi
echo "$out" | grep -q "Received status code 400 from server" || fail "the second publish failed for another reason: $out"

echo "== publish 1.1.0-SNAPSHOT twice"
g lib publish -PlibVersion=1.1.0-SNAPSHOT
sleep 1 # a distinct timestamp for the second build
sed -i 's/build 1/build 2/' /tmp/lib/src/main/java/com/example/gradle/Greeter.java
g lib publish -PlibVersion=1.1.0-SNAPSHOT

echo "== resolve both through the group and run"
resolved 1.0.0 'com\.example\.gradle:greeter:1\.0\.0' 'hello from greeter, build 1'
resolved 1.1.0-SNAPSHOT 'com\.example\.gradle:greeter:1\.1\.0-SNAPSHOT:[0-9.]+-2' 'hello from greeter, build 2'
echo "GRADLE CLIENT OK"
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

docker run --rm --name "$name-gradle" --network "$net" --memory 2g -v "$root/$work:/work:ro" "${GRADLE_IMAGE:-gradle:jdk21}" sh /work/run.sh

# stored <repository> <directory> <search parameters> <file>...: each file is what its sha1, sha256 and
# sha512 files name, and the asset search finds it with the parameters.
stored() {
  local found f alg
  found="$(curl -fsS "$cc/service/rest/v1/search/assets?repository=$1&$3")"
  for f in "${@:4}"; do
    for alg in sha1 sha256 sha512; do
      [ "$(curl -fsS "$cc/repository/$1/$2/$f" | ${alg}sum | cut -d' ' -f1)" = "$(curl -fsS "$cc/repository/$1/$2/$f.$alg")" ] ||
        fail "$1/$2/$f is not what its .$alg names"
    done
    grep -q "\"path\":\"/$2/$f\"" <<<"$found" || fail "the search ($3) does not find $1/$2/$f: $found"
  done
}

echo "== the stored files, their checksums, the search"
lib=com/example/gradle/greeter
stored maven-releases $lib/1.0.0 'group=com.example.gradle&name=greeter&version=1.0.0' \
  greeter-1.0.0.jar greeter-1.0.0-sources.jar greeter-1.0.0.pom greeter-1.0.0.module
curl -fsS "$cc/repository/maven-public/$lib/1.0.0/greeter-1.0.0.module" | grep -q '"formatVersion"' || fail "the .module file is not Gradle's"
vmeta="$(curl -fsS "$cc/repository/maven-public/$lib/1.1.0-SNAPSHOT/maven-metadata.xml")"
grep -q '<buildNumber>2</buildNumber>' <<<"$vmeta" || fail "SNAPSHOT version metadata is not at build 2: $vmeta"
grep -q '<extension>module</extension>' <<<"$vmeta" || fail "SNAPSHOT version metadata does not list the .module file: $vmeta"
stamp="$(sed -n 's:.*<timestamp>\(.*\)</timestamp>.*:\1:p' <<<"$vmeta")"
stored maven-snapshots $lib/1.1.0-SNAPSHOT 'maven.groupId=com.example.gradle&maven.artifactId=greeter&maven.baseVersion=1.1.0-SNAPSHOT' \
  "greeter-1.1.0-$stamp-2.jar" "greeter-1.1.0-$stamp-2.pom" "greeter-1.1.0-$stamp-2.module"

echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "GRADLE COMPAT OK"
