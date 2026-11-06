#!/usr/bin/env bash
# Container, network and work-dir names carry the shell PID, so runs can overlap.
# Builds the image, starts commandcenter, then uses real sbt (the
# sbtscala/scala-sbt image) to:
#   1. publish com.example.sbt:hello:1.0.0 to maven-releases (POM, jar, sources,
#      javadoc; sbt sends no maven-metadata.xml); a second publish is refused
#   2. publish 1.1.0-SNAPSHOT twice to maven-snapshots (sbt's snapshot files
#      carry no timestamp, so the second replaces the first)
#   3. resolve both from another build through the maven-public group and run it
#   4. fail to publish with credentials for Nexus's realm: sbt finds them by the
#      realm of the server's challenge, which is "commandcenter" here
# Then compares the stored files with their .sha1 and looks them up with the
# search API. Finally prints the container's memory usage.
#   COMPAT_PREFIX  start of the container and network names (default commandcenter-compat)
#   SBT_IMAGE      sbt image (default: sbt 1.13.1 on Temurin 21; test/compat/sbt/*/project/build.properties names the same sbt)
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/compat/.work-sbt-$$"
name="${COMPAT_PREFIX:-commandcenter-compat}-sbt-$$"
net="$name"
. test/postgres.sh

cleanup() {
  docker rm -f -v "$name" "$name-pg" "$name-sbt" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup
fail() { echo "FAIL: $*" >&2; exit 1; }

docker build -q -t commandcenter:compat . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i --name "$name-hash" --memory 128m commandcenter:compat hash-password)"
rm -rf "$work" && mkdir -p "$work"
cp -r test/compat/sbt/lib test/compat/sbt/app "$work/"

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
cp -r /work/lib /work/app /tmp/ # the work dir is read-only: sbt writes beside its copies
s() { (cd "/tmp/$1" && shift && sbt -batch -Dsbt.color=false "$@" 2>&1); }
fail() { echo "FAIL: $*" >&2; exit 1; }
# refused <text> <sbt arguments>: the publish must fail and say text.
refused() {
  why="$1"
  shift
  if out="$(s lib "$@" publish)"; then fail "sbt $* publish succeeded: $out"; fi
  echo "$out" | grep -qF "$why" || fail "sbt $* publish failed without \"$why\": $out"
}
# runs <version> <greeting>: the app gets the library through the group and runs with it.
runs() {
  out="$(s app -DlibVersion="$1" update run)" || fail "resolving $1: $out"
  echo "$out" | grep -qx "$2" || fail "$1: the app did not print \"$2\": $out"
}

echo "== sbt $(sed -n 's/^sbt.version=//p' /tmp/lib/project/build.properties)"
echo "== publish 1.0.0"
out="$(s lib -DlibVersion=1.0.0 publish)" || fail "publish 1.0.0: $out"

echo "== a second publish is refused"
refused "destination file exists and overwriting is disabled" -DlibVersion=1.0.0

echo "== publish 1.1.0-SNAPSHOT twice"
out="$(s lib -DlibVersion=1.1.0-SNAPSHOT publish)" || fail "publish 1.1.0-SNAPSHOT: $out"
sed -i 's/build 1/build 2/' /tmp/lib/src/main/java/com/example/sbt/Hello.java
out="$(s lib -DlibVersion=1.1.0-SNAPSHOT publish)" || fail "publish 1.1.0-SNAPSHOT again: $out"

echo "== resolve both through the group and run"
runs 1.0.0 'hello from sbt, build 1'
runs 1.1.0-SNAPSHOT 'hello from sbt, build 2'

echo "== credentials for another realm are not sent"
refused "Unable to find credentials for [commandcenter @ commandcenter]" -DlibVersion=1.0.1 "-Drealm=Sonatype Nexus Repository Manager"
echo "SBT CLIENT OK"
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

docker run --rm --name "$name-sbt" --network "$net" --memory 2g -v "$root/$work:/work:ro" \
  "${SBT_IMAGE:-sbtscala/scala-sbt:eclipse-temurin-21.0.13_9_1.13.1_2.13.18}" sh /work/run.sh

# stored <repository> <directory> <search parameters> <file>...: each file is what its .sha1 names, and the
# asset search finds it with the parameters.
stored() {
  local found f
  found="$(curl -fsS "$cc/service/rest/v1/search/assets?repository=$1&$3")"
  for f in "${@:4}"; do
    [ "$(curl -fsS "$cc/repository/$1/$2/$f" | sha1sum | cut -d' ' -f1)" = "$(curl -fsS "$cc/repository/$1/$2/$f.sha1")" ] ||
      fail "$1/$2/$f is not what its .sha1 names"
    grep -q "\"path\":\"/$2/$f\"" <<<"$found" || fail "the search ($3) does not find $1/$2/$f: $found"
  done
}

echo "== the stored files, their checksums, the search, the metadata sbt does not send"
lib=com/example/sbt/hello
stored maven-releases $lib/1.0.0 'group=com.example.sbt&name=hello&version=1.0.0' \
  hello-1.0.0.pom hello-1.0.0.jar hello-1.0.0-sources.jar hello-1.0.0-javadoc.jar
stored maven-snapshots $lib/1.1.0-SNAPSHOT 'maven.groupId=com.example.sbt&maven.artifactId=hello&maven.baseVersion=1.1.0-SNAPSHOT' \
  hello-1.1.0-SNAPSHOT.pom hello-1.1.0-SNAPSHOT.jar
curl -fsS "$cc/repository/maven-public/$lib/maven-metadata.xml" | grep -q '<version>1.0.0</version>' || fail "no 1.0.0 in maven-metadata.xml"
if curl -fs -o /dev/null "$cc/repository/maven-releases/$lib/1.0.1/hello-1.0.1.pom"; then fail "1.0.1 was published without credentials"; fi

echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "SBT COMPAT OK"
