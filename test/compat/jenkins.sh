#!/usr/bin/env bash
# Container, network and work-dir names carry the shell PID, so runs can overlap.
# Builds the image, starts commandcenter and Jenkins (the jenkins/jenkins LTS
# image, configured as code by test/compat/jenkins/) with the plugins
# nexus-artifact-uploader and nexus-jenkins-plugin (Nexus Platform), then runs
# real pipeline builds:
#   1. "Test connection" of Command Center as a Nexus Repository Manager 3.x
#      server, and the repository list of the publisher's form
#   2. nexusArtifactUploader (Nexus 3 mode): a jar and its pom to maven-releases;
#      again (refused: allow_once); twice to maven-redeploy (writePolicy allow);
#      a SNAPSHOT twice to maven-snapshots
#   3. nexusPublisher: jar, sources and pom through the component upload API;
#      again (refused); a jar with a generated pom; twice to maven-redeploy;
#      a SNAPSHOT (refused, as Nexus refuses it)
#   4. tags and staging: createTag, nexusPublisher with tagName, associateTag,
#      moveComponents and deleteComponents
# What a build published is downloaded, compared with its .sha1 and looked up
# with the search API. Finally prints the containers' memory usage.
#   COMPAT_PREFIX  start of the container and network names (default commandcenter-compat)
#   JENKINS_IMAGE  Jenkins image (default jenkins/jenkins:lts-jdk21)
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/compat/.work-jenkins-$$"
name="${COMPAT_PREFIX:-commandcenter-compat}-jenkins-$$"
net="$name"
. test/postgres.sh

cleanup() {
  docker rm -f -v "$name" "$name-pg" "$name-jenkins" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup
fail() { echo "FAIL: $*" >&2; exit 1; }

docker build -q -t commandcenter:compat . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i --name "$name-hash" --memory 128m commandcenter:compat hash-password)"
rm -rf "$work" && mkdir -p "$work"
cp test/compat/jenkins/* "$work/"

cat > "$work/config.yaml" <<EOF
anonymousRead: true
baseURL: "http://commandcenter:8081"
users:
  - { name: admin, passwordHash: "$hash", roles: [admin] }
  - { name: ci, passwordHash: "$hash", roles: [deployer] }
roles:
  deployer: { write: [maven-releases, maven-redeploy, maven-snapshots] }
repositories:
  - { name: maven-releases,  format: maven, type: hosted }
  - { name: maven-redeploy,  format: maven, type: hosted, writePolicy: allow }
  - { name: maven-snapshots, format: maven, type: hosted, versionPolicy: snapshot, writePolicy: allow }
  - { name: maven-staged,    format: maven, type: hosted }
EOF

docker network create "$net" >/dev/null
start_postgres
docker update --memory 512m --memory-swap 512m "$name-pg" >/dev/null
# The containers get the work dir read-only: no file in it is ever theirs, so
# cleanup can always remove it.
docker run -d --name "$name" --network "$net" -e COMMANDCENTER_DATABASE_URL="$pg_url" --network-alias commandcenter --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" -p 127.0.0.1::8081 commandcenter:compat >/dev/null
cc="http://$(docker port "$name" 8081/tcp | head -1)"

# The plugins are installed when the container starts, then Jenkins. Its CLI
# and curl read the admin's name and password from the container's environment.
docker run -d --name "$name-jenkins" --network "$net" --memory 2g \
  -e CASC_JENKINS_CONFIG=/work/jenkins.yaml -e JAVA_OPTS=-Djenkins.install.runSetupWizard=false \
  -e JENKINS_USER_ID=admin -e JENKINS_API_TOKEN="$(head -c 18 /dev/urandom | base64 | tr -d /+)" -e CC_PASSWORD=smoke-pass \
  -v "$root/$work:/work:ro" "${JENKINS_IMAGE:-jenkins/jenkins:lts-jdk21}" \
  sh -c 'jenkins-plugin-cli --plugin-file /work/plugins.txt >/dev/null && exec /usr/local/bin/jenkins.sh' >/dev/null

ready=0
for i in $(seq 1 30); do
  if curl -fs "$cc/service/rest/v1/status" >/dev/null; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "FAIL: commandcenter did not become ready"; docker logs "$name"; exit 1
fi
ready=0
for i in $(seq 1 150); do
  if docker exec "$name-jenkins" curl -fs -o /dev/null http://localhost:8080/login 2>/dev/null; then ready=1; break; fi
  [ "$(docker inspect -f '{{.State.Running}}' "$name-jenkins" 2>/dev/null)" = true ] || break
  sleep 2
done
if [ "$ready" != 1 ]; then
  echo "FAIL: Jenkins did not become ready"; docker logs --tail 50 "$name-jenkins"; exit 1
fi

jenkins() {
  docker exec "$name-jenkins" sh -c 'exec java -jar "$JENKINS_HOME"/war/WEB-INF/lib/cli-*.jar -s http://localhost:8080/ "$@"' sh "$@"
}
# build <job> <version> <repository> [pom]: runs the job and waits for it; its console is in $console.
build() { console="$(jenkins build "$1" -s -v -p VERSION="$2" -p REPO="$3" -p POM="${4:-true}" 2>&1)"; }
ok() { build "$@" || fail "build $*: $console"; }
# refused <text> <job> <version> <repository> [pom]: the build must fail and its console say text.
refused() {
  local why="$1"
  shift
  if build "$@"; then fail "build $* succeeded"; fi
  grep -q "$why" <<<"$console" || fail "build $*: no \"$why\" in: $console"
}
# form <descriptor>/<method> <curl data arguments>: a request of a configuration form, with its crumb.
form() {
  docker exec "$name-jenkins" sh -c 'cd /tmp && auth="admin:$JENKINS_API_TOKEN" path="$1" && shift &&
    crumb="$(curl -fsS -u "$auth" -c cookies http://localhost:8080/crumbIssuer/api/json | sed -E "s/.*\"crumb\":\"([^\"]+)\".*/\1/")" &&
    curl -fsS -u "$auth" -b cookies -H "Jenkins-Crumb: $crumb" "$@" "http://localhost:8080/descriptorByName/$path"' sh "$@"
}
# published <repository> <directory> <search parameters> <file>...: each file downloads with the checksum its
# .sha1 names, and the asset search finds it with the parameters.
published() {
  local found sum f
  found="$(curl -fsS "$cc/service/rest/v1/search/assets?repository=$1&$3")"
  for f in "${@:4}"; do
    sum="$(curl -fsS "$cc/repository/$1/$2/$f" | sha1sum | cut -d' ' -f1)"
    [ "$sum" = "$(curl -fsS "$cc/repository/$1/$2/$f.sha1")" ] || fail "$1/$2/$f is not what its .sha1 names"
    grep -q "\"path\":\"/$2/$f\"" <<<"$found" || fail "the search ($3) does not find $1/$2/$f: $found"
  done
}
sha1_of() { curl -fsS "$cc/repository/$1.sha1"; }

echo "== Jenkins $(jenkins version) with"
jenkins list-plugins | grep -E '^(nexus-artifact-uploader|nexus-jenkins-plugin) '

echo "== Test connection of the Nexus Repository Manager 3.x server; the repositories of the publisher's form"
verdict="$(form org.sonatype.nexus.ci.config.Nxrm3Configuration/verifyCredentials -d serverUrl=http://commandcenter:8081 -d credentialsId=commandcenter)"
grep -q 'connection succeeded (4 hosted maven2 repositories)' <<<"$verdict" || fail "Test connection: $verdict"
if grep -q 'Some operations require' <<<"$verdict"; then fail "Test connection warns about the server: $verdict"; fi
form org.sonatype.nexus.ci.nxrm.NexusPublisherWorkflowStep/fillNexusRepositoryIdItems -d nexusInstanceId=commandcenter |
  grep -q '"value":"maven-releases"' || fail "the publisher's form does not list maven-releases"

echo "== nexusArtifactUploader: release, again (refused), redeploy twice, SNAPSHOT twice"
up=com/example/uploader/hello
ok uploader 1.0.0 maven-releases
published maven-releases $up/1.0.0 'group=com.example.uploader&name=hello&version=1.0.0' hello-1.0.0.jar hello-1.0.0.pom
curl -fsS "$cc/repository/maven-releases/$up/maven-metadata.xml" | grep -q '<version>1.0.0</version>' || fail "no 1.0.0 in maven-metadata.xml"
refused 'status: 400' uploader 1.0.0 maven-releases
ok uploader 2.0.0 maven-redeploy
first="$(sha1_of maven-redeploy/$up/2.0.0/hello-2.0.0.jar)"
ok uploader 2.0.0 maven-redeploy
published maven-redeploy $up/2.0.0 'group=com.example.uploader&name=hello&version=2.0.0' hello-2.0.0.jar hello-2.0.0.pom
[ "$first" != "$(sha1_of maven-redeploy/$up/2.0.0/hello-2.0.0.jar)" ] || fail "the second publish did not replace the jar"
ok uploader 1.1.0-SNAPSHOT maven-snapshots
sleep 1 # a distinct timestamp for the second build
ok uploader 1.1.0-SNAPSHOT maven-snapshots
vmeta="$(curl -fsS "$cc/repository/maven-snapshots/$up/1.1.0-SNAPSHOT/maven-metadata.xml")"
grep -q '<buildNumber>2</buildNumber>' <<<"$vmeta" || fail "SNAPSHOT version metadata is not at build 2: $vmeta"
stamp="$(sed -n 's:.*<timestamp>\(.*\)</timestamp>.*:\1:p' <<<"$vmeta")"
published maven-snapshots $up/1.1.0-SNAPSHOT 'maven.groupId=com.example.uploader&maven.artifactId=hello&maven.baseVersion=1.1.0-SNAPSHOT' \
  "hello-1.1.0-$stamp-2.jar" "hello-1.1.0-$stamp-2.pom"

echo "== nexusPublisher: release with its pom, again (refused), generated pom, redeploy twice, SNAPSHOT (refused)"
pub=com/example/publisher
ok publisher 1.0.0 maven-releases
published maven-releases $pub/hello/1.0.0 'group=com.example.publisher&name=hello&version=1.0.0' hello-1.0.0.jar hello-1.0.0-sources.jar hello-1.0.0.pom
curl -fsS "$cc/repository/maven-releases/$pub/hello/maven-metadata.xml" | grep -q '<version>1.0.0</version>' || fail "no 1.0.0 in maven-metadata.xml"
refused '400 response from server' publisher 1.0.0 maven-releases
ok publisher 1.0.0 maven-releases false
published maven-releases $pub/hello-nopom/1.0.0 'group=com.example.publisher&name=hello-nopom&version=1.0.0' \
  hello-nopom-1.0.0.jar hello-nopom-1.0.0-sources.jar hello-nopom-1.0.0.pom
curl -fsS "$cc/repository/maven-releases/$pub/hello-nopom/1.0.0/hello-nopom-1.0.0.pom" | grep -q '<packaging>jar</packaging>' || fail "the generated pom has no packaging"
ok publisher 2.0.0 maven-redeploy
first="$(sha1_of maven-redeploy/$pub/hello/2.0.0/hello-2.0.0.jar)"
ok publisher 2.0.0 maven-redeploy
[ "$first" != "$(sha1_of maven-redeploy/$pub/hello/2.0.0/hello-2.0.0.jar)" ] || fail "the second publish did not replace the jar"
refused '400 response from server' publisher 1.1.0-SNAPSHOT maven-snapshots

echo "== tags and staging: createTag, nexusPublisher with tagName, associateTag, moveComponents, deleteComponents"
ok staging 3.0.0 maven-releases
for step in "Successfully created tag" "Associate to tag 'build-3.0.0' successful" "Move successful" "Delete successful"; do
  grep -q "$step" <<<"$console" || fail "staging: no \"$step\" in: $console"
done
for a in tagged found; do # moved (the job downloads both from maven-staged), then deleted
  grep -q "name: $a, version: 3.0.0" <<<"$console" || fail "staging: $a was not moved or deleted: $console"
  for r in maven-releases maven-staged; do
    [ "$(curl -s -o /dev/null -w '%{http_code}' "$cc/repository/$r/com/example/staging/$a/3.0.0/$a-3.0.0.jar")" = 404 ] || fail "$a 3.0.0 is still in $r"
  done
done

echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name" "$name-jenkins"
echo "JENKINS COMPAT OK"
