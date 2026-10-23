#!/usr/bin/env bash
# Container, network and work-dir names carry the shell PID, so runs can overlap.
# Builds the image, starts commandcenter, then uses real Maven to:
#   1. pull plugins through maven-central (proxy) via maven-public (group)
#   2. deploy com.example:hello:1.0.0 to maven-releases (hosted)
#   3. resolve it again from an empty local repo through maven-public
#   4. deploy com.example:hello:1.1.0-SNAPSHOT twice to maven-snapshots and
#      resolve the latest timestamped build into an empty local repo
#   4. deploy com.example:hello:1.1.0-SNAPSHOT twice to maven-snapshots and
#      resolve the latest timestamped build into an empty local repo
# Finally prints the container's memory usage.
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-$$"
net=commandcenter-smoke-$$
name=commandcenter-smoke-$$

cleanup() {
  docker rm -f "$name" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup
rm -rf "$work" && mkdir -p "$work"

docker build -q -t commandcenter:smoke . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i commandcenter:smoke hash-password)"

cat > "$work/config.yaml" <<EOF
anonymousRead: true
baseURL: "http://commandcenter:8081"
users:
  - { name: ci, passwordHash: "$hash", roles: [deployer] }
roles:
  deployer: { write: [maven-releases, maven-snapshots] }
repositories:
  - { name: maven-central,  format: maven, type: proxy,  remote: "https://repo1.maven.org/maven2/" }
  - { name: maven-releases, format: maven, type: hosted }
  - { name: maven-snapshots, format: maven, type: hosted, versionPolicy: snapshot, writePolicy: allow }
  - { name: maven-public,   format: maven, type: group,  members: [maven-releases, maven-snapshots, maven-central] }
EOF

cat > "$work/settings.xml" <<'EOF'
<settings>
  <servers><server><id>commandcenter</id><username>ci</username><password>smoke-pass</password></server></servers>
  <mirrors><mirror><id>commandcenter-mirror</id><mirrorOf>*</mirrorOf>
    <url>http://commandcenter:8081/repository/maven-public/</url></mirror></mirrors>
  <!-- The usual mirror setup: enable SNAPSHOTs for the mirrored central. -->
  <profiles><profile><id>snapshots</id><repositories><repository><id>central</id><url>http://central</url>
    <snapshots><enabled>true</enabled></snapshots></repository></repositories></profile></profiles>
  <activeProfiles><activeProfile>snapshots</activeProfile></activeProfiles>
</settings>
EOF

docker network create "$net" >/dev/null
docker run -d --name "$name" --network "$net" --network-alias commandcenter --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" -p 18081:8081 commandcenter:smoke >/dev/null

for i in $(seq 1 30); do
  curl -fs http://localhost:18081/service/rest/v1/status >/dev/null && break
  sleep 1
done

mvn() {
  docker run --rm --network "$net" \
    -v "$root/${project:-test/smoke/project}:/project" -v "$root/$work:/work" -w /project \
    maven:3.9-eclipse-temurin-21 mvn -B -q -s /work/settings.xml -Dmaven.repo.local=/work/$1 "${@:2}"
}

echo "== deploy (plugins via proxy, artifact to hosted)"
mvn m2-deploy deploy

echo "== resolve from an empty local repository through the group"
mvn m2-fresh dependency:get -Dartifact=com.example:hello:1.0.0

echo "== redeploy must be rejected (allow_once)"
if mvn m2-deploy deploy 2>/dev/null; then
  echo "FAIL: redeploy succeeded" >&2
  exit 1
fi

echo "== metadata"
curl -fs http://localhost:18081/repository/maven-public/com/example/hello/maven-metadata.xml | grep -q '<version>1.0.0</version>'

echo "== SNAPSHOT: two timestamped deploys, then the latest build into an empty local repository"
mkdir -p "$work/snapshot-project"
cp -r test/smoke/project/pom.xml test/smoke/project/src "$work/snapshot-project/"
sed -i '0,/<version>1.0.0<\/version>/s//<version>1.1.0-SNAPSHOT<\/version>/' "$work/snapshot-project/pom.xml"
snapdeploy=-DaltDeploymentRepository=commandcenter::http://commandcenter:8081/repository/maven-snapshots/
project="$work/snapshot-project" mvn m2-deploy deploy "$snapdeploy"
sleep 1 # a distinct timestamp for the second build
project="$work/snapshot-project" mvn m2-deploy deploy "$snapdeploy"
vmeta="$(curl -fs http://localhost:18081/repository/maven-public/com/example/hello/1.1.0-SNAPSHOT/maven-metadata.xml)"
if ! grep -q '<buildNumber>2</buildNumber>' <<<"$vmeta"; then
  echo "FAIL: SNAPSHOT version metadata is not at build 2: $vmeta" >&2
  exit 1
fi
mvn m2-snap-fresh dependency:get -Dartifact=com.example:hello:1.1.0-SNAPSHOT
snapdir="$work/m2-snap-fresh/com/example/hello/1.1.0-SNAPSHOT"
if ! ls "$snapdir" | grep -qE '^hello-1\.1\.0-[0-9]{8}\.[0-9]{6}-2\.jar$'; then
  echo "FAIL: the build-2 jar was not fetched:" >&2
  ls "$snapdir" >&2 || true
  exit 1
fi

echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "SMOKE OK"
