#!/usr/bin/env bash
# Builds the image, starts commandcenter with proxies of real Eclipse sites (a
# release site, a composite whose children have p2.mirrorsURL and lie partly
# outside it, and a simple site with .xml.xz documents) and a group, and uses
# the real p2 director (Eclipse Platform, in a JDK image) from a network that
# reaches only commandcenter, so a mirror or a direct download fails the test:
#   1. the artifacts documents come without p2.mirrorsURL in the .jar and the
#      .xml.xz form (read with jar and with xz)
#   2. director -installIU through the proxy: it selects no mirror, and every
#      bundle it installed is in the proxy's cache
#   3. the same through the group, whose children are relative: no baseURL
#      that works is configured, and they resolve against the name the client
#      used; its documents keep their Last-Modified
#   4. the install again with empty client caches after commandcenter lost its
#      internet access
#   5. an ALLOW routing rule written for repository paths, a mirrored child's
#      files included
#   6. changing a proxy's remote: the old site is gone from the cache and from
#      what the director sees
#   7. REST: search by bundle id and by p2.pluginName, every downloadUrl answers
#   8. a Tycho build of an eclipse-plugin project against the proxy of the
#      composite and against the group, with Tycho itself through a Maven proxy
#      on the same server, and again after commandcenter lost its internet access;
#      against a proxy that needs a login: refused, and passes with the
#      repository's credentials in settings.xml
# Finally prints the container's memory usage. Every run downloads the Eclipse
# Platform (about 150 MiB) and about 120 MiB through the proxies.
#   SMOKE_PREFIX   start of the container and network names (default commandcenter)
#   JDK_IMAGE      the client's image, with a JDK and Maven (default maven:3.9-eclipse-temurin-21)
#   ECLIPSE_TGZ    the URL of an Eclipse Platform tar.gz for Linux
#   TYCHO_VERSION  default 4.0.13
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-p2-$$"
name="${SMOKE_PREFIX:-commandcenter}-p2-smoke-$$"
net="$name"
closed="$name-closed"
. test/postgres.sh

cleanup() {
  docker rm -f -v "$name" "$name-pg" "$name-client" >/dev/null 2>&1 || true
  docker network rm "$net" "$closed" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup
rm -rf "$work" && mkdir -p "$work"
cp -r test/smoke/p2/tycho "$work/tycho"

docker build -q -t commandcenter:smoke-p2 . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i --name "$name-hash" --memory 128m commandcenter:smoke-p2 hash-password)"

cat > "$work/config.yaml" <<EOF
anonymousRead: true
baseURL: "http://nowhere.invalid:8081"
users:
  - { name: admin, passwordHash: "$hash", roles: [admin] }
repositories:
  - { name: eclipse, format: p2, type: proxy, remote: "https://download.eclipse.org/releases/2024-09/" }
  - { name: platform, format: p2, type: proxy, remote: "https://download.eclipse.org/eclipse/updates/4.33/R-4.33-202409030240/" }
  - { name: p2-all, format: p2, type: group, members: [platform, eclipse] }
  - { name: private, format: p2, type: proxy, remote: "https://download.eclipse.org/eclipse/updates/4.33/R-4.33-202409030240/", anonymousRead: false }
  - { name: central, format: maven, type: proxy, remote: "https://repo.maven.apache.org/maven2/" }
EOF

cat > "$work/settings.xml" <<'EOF'
<settings>
  <mirrors>
    <mirror><id>cc-central</id><mirrorOf>*,!cc-p2</mirrorOf><url>http://commandcenter:8081/repository/central/</url></mirror>
  </mirrors>
</settings>
EOF
sed 's|<settings>|<settings><servers><server><id>cc-p2</id><username>admin</username><password>smoke-pass</password></server></servers>|' "$work/settings.xml" > "$work/settings-login.xml"

cat > "$work/lib.sh" <<'EOF'
set -eu
CC=http://commandcenter:8081
REG=$CC/repository
API=$CC/service/rest/v1
fail() { echo "FAIL: $*" >&2; exit 1; }
api() { curl -fsS -u admin:smoke-pass -H 'Content-Type: application/json' "$@"; }
status() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
# director <arguments>: the p2 director, with empty caches of its own, its
# mirror selection traced; the log is /tmp/director.log.
director() {
  rm -rf /tmp/director
  printf 'org.eclipse.equinox.p2.core/debug=true\norg.eclipse.equinox.p2.core/artifacts/mirrors=true\n' > /tmp/options
  java -Dosgi.configuration.area=/tmp/director/config -Dp2.httpRule=allow -jar /opt/eclipse/plugins/org.eclipse.equinox.launcher_*.jar \
    -nosplash -consoleLog -debug /tmp/options -data /tmp/director/data -application org.eclipse.equinox.p2.director "$@" > /tmp/director.log 2>&1 ||
    { tail -30 /tmp/director.log; fail "director $*"; }
}
# install <repository URL> <IU>: installs into an empty directory, /tmp/inst.
install() {
  rm -rf /tmp/inst
  director -repository "$1" -installIU "$2" -destination /tmp/inst -profile T -bundlepool /tmp/inst -roaming -p2.os linux -p2.ws gtk -p2.arch x86_64
  if grep -q 'Selected mirror' /tmp/director.log; then
    grep 'Selected mirror' /tmp/director.log | head -5
    fail "the client selected a mirror"
  fi
  grep "Operation completed" /tmp/director.log
}
# tycho <repository>: builds the project against a p2 repository with Tycho's
# p2 caches empty.
tycho() {
  rm -rf /tmp/tycho /root/.m2/repository/p2 /root/.m2/repository/.cache
  cp -r /work/tycho /tmp/tycho
  (cd /tmp/tycho && mvn -B -s "/work/${2:-settings.xml}" -Dtycho.version="$TYCHO_VERSION" -Dp2.url="$REG/$1/" verify > /tmp/mvn.log 2>&1) || { tail -30 /tmp/mvn.log; fail "Tycho against $1"; }
  grep -E 'Tycho Version|BUILD SUCCESS' /tmp/mvn.log
  test -f /tmp/tycho/target/smoke.plugin-1.0.0-SNAPSHOT.jar
}
# paths <repository>: the asset paths of a repository, one page at a time.
paths() {
  token=
  while :; do
    curl -fsS "$API/assets?repository=$1${token:+&continuationToken=$token}" > /tmp/page.json
    grep -o '"path":"[^"]*"' /tmp/page.json | cut -d'"' -f4
    token=$(grep -o '"continuationToken":"[^"]*"' /tmp/page.json | cut -d'"' -f4 || true)
    [ -n "$token" ] || break
  done
}
EOF

cat > "$work/run1.sh" <<'EOF'
. /work/lib.sh
echo "== the client's network reaches only commandcenter"
if curl -fsS -m 5 -o /dev/null https://download.eclipse.org/ 2>/dev/null; then fail "download.eclipse.org is reachable"; fi
java -version 2>&1 | head -1
ls /opt/eclipse/plugins | grep -E '^org\.eclipse\.equinox\.p2\.director_'

echo "== artifacts documents without p2.mirrorsURL: .xml.xz, .jar"
curl -fsS -o /tmp/a.xz "$REG/eclipse/202409111000/artifacts.xml.xz"
xz --version | head -1
xz -t /tmp/a.xz
test "$(xz -dc /tmp/a.xz | grep -c 'p2.mirrorsURL' || true)" = 0
curl -fsS -o /tmp/a.jar "$REG/eclipse/202409111000/artifacts.jar"
mkdir -p /tmp/j && cd /tmp/j && jar xf /tmp/a.jar
test "$(grep -c 'p2.mirrorsURL' artifacts.xml || true)" = 0
test "$(sed -n 4p artifacts.xml | sed "s/.*size='\([0-9]*\)'.*/\1/")" = "$(sed -n '5,/<\/properties>/p' artifacts.xml | grep -c '<property ')"
cd /tmp

echo "== director -installIU through the proxy"
install "$REG/eclipse/" org.eclipse.jgit.feature.group
installed=$(find /tmp/inst/plugins -maxdepth 1 -name '*_*' | sed 's|.*/||' | sort)
echo "$installed" | wc -l
paths eclipse > /tmp/cached
for f in $installed; do
  grep -q "/plugins/$f" /tmp/cached || fail "$f is not in the proxy's cache"
done

echo "== the composite: a child under the remote and one elsewhere, a mirrored one"
curl -fsS -o /tmp/c.jar "$REG/eclipse/compositeContent.jar"
cd /tmp/j && rm -f * && jar xf /tmp/c.jar && cat compositeContent.xml | grep '<child' && cd /tmp
grep -q "<child location='../../technology" /tmp/j/compositeContent.xml && fail "a child outside the remote was left as it is"
grep -q "\.remote/" /tmp/j/compositeContent.xml || fail "no mirrored child"

echo "== through the group: children relative to the URL the client used"
curl -fsS "$REG/p2-all/compositeContent.xml" | grep '<child'
curl -fsS "$REG/p2-all/compositeContent.xml" | grep -q nowhere.invalid && fail "the group's children use baseURL"
lm=$(curl -fsSI "$REG/p2-all/compositeArtifacts.xml" | grep -i '^last-modified:')
test -n "$lm"
sleep 1
test "$(curl -fsSI "$REG/p2-all/compositeArtifacts.xml" | grep -i '^last-modified:')" = "$lm"
install "$REG/p2-all/" org.eclipse.jgit.feature.group
test -n "$(find /tmp/inst/plugins -name 'org.eclipse.jgit_*')"
director -repository "$REG/p2-all/" -list org.eclipse.equinox.p2.director.app
grep -q '^org.eclipse.equinox.p2.director.app=' /tmp/director.log

echo "== Tycho against the composite's proxy and the group"
mvn -v | head -1
tycho eclipse
tycho p2-all
echo "CLIENT PART 1 OK"
EOF

cat > "$work/run2.sh" <<'EOF'
. /work/lib.sh
echo "== the install again, with empty client caches: commandcenter has no internet now"
install "$REG/eclipse/" org.eclipse.jgit.feature.group
install "$REG/p2-all/" org.eclipse.jgit.feature.group
tycho p2-all
echo "CLIENT PART 2 OK"
EOF

cat > "$work/run3.sh" <<'EOF'
. /work/lib.sh
proxy() { # proxy <name> <remote> <routing rule>
  printf '{"name":"%s","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":true},"proxy":{"remoteUrl":"%s","contentMaxAge":-1,"metadataMaxAge":1440},"negativeCache":{"enabled":true,"timeToLive":10},"httpClient":{"blocked":false,"autoBlock":true},"routingRule":"%s"}' "$1" "$2" "$3"
}

echo "== an ALLOW routing rule for repository paths"
api -X POST "$API/routing-rules" -d '{"name":"p2-allow","mode":"ALLOW","matchers":["^/[^/]*$","^/202409111000/.*","^/\\.remote/.*"]}' >/dev/null
api -X POST "$API/repositories/p2/proxy" -d "$(proxy routed https://download.eclipse.org/releases/2024-09/ p2-allow)" >/dev/null
test "$(status "$REG/routed/other/content.jar")" = 403
install "$REG/routed/" org.eclipse.jgit.feature.group
paths routed > /tmp/cached
grep -q '^/202409111000/plugins/org.eclipse.jgit_' /tmp/cached
child=$(grep -o '^/\.remote/[0-9a-f]*/' /tmp/cached | head -1)
test -n "$child"
director -repository "$REG/routed/" -list org.eclipse.epp.package.common
grep -q '^org.eclipse.epp.package.common=' /tmp/director.log
v=$(grep '^org.eclipse.epp.package.common=' /tmp/director.log | cut -d= -f2)
test "$(status "$REG/routed$child"202409051200/plugins/org.eclipse.epp.package.common_"$v".jar)" = 200

echo "== a proxy's remote changes: its cached site goes"
director -repository "$REG/platform/" -list org.eclipse.jdt.core.compiler.batch
grep '^org.eclipse.jdt.core.compiler.batch=' /tmp/director.log
grep -q '^org.eclipse.jdt.core.compiler.batch=3.39' /tmp/director.log
test -n "$(paths platform)"
api -X PUT "$API/repositories/p2/proxy/platform" -d "$(proxy platform https://download.eclipse.org/eclipse/updates/4.34/R-4.34-202411201800/ '')" >/dev/null
test -z "$(paths platform)"
director -repository "$REG/platform/" -list org.eclipse.jdt.core.compiler.batch
grep '^org.eclipse.jdt.core.compiler.batch=' /tmp/director.log
grep -q '^org.eclipse.jdt.core.compiler.batch=3.40' /tmp/director.log
if grep -q '^org.eclipse.jdt.core.compiler.batch=3.39' /tmp/director.log; then fail "the old site is still seen"; fi

echo "== Tycho against a proxy that needs a login"
test "$(status "$REG/private/p2.index")" = 401
if (rm -rf /tmp/tycho /root/.m2/repository/p2 /root/.m2/repository/.cache; cp -r /work/tycho /tmp/tycho; cd /tmp/tycho && mvn -B -s /work/settings.xml -Dtycho.version="$TYCHO_VERSION" -Dp2.url="$REG/private/" verify > /tmp/mvn.log 2>&1); then
  fail "Tycho passed without credentials"
fi
grep -m1 -E '401|Unauthorized|authentication|Unable to|Cannot' /tmp/mvn.log | cut -c1-200
tycho private settings-login.xml

echo "== REST: by bundle id, by p2.pluginName; every downloadUrl answers"
curl -fsS "$API/search/assets?repository=eclipse&name=org.eclipse.jgit" > /tmp/assets.json
grep -o '"downloadUrl":"[^"]*"' /tmp/assets.json | cut -d'"' -f4 | sed 's|nowhere.invalid|commandcenter|' > /tmp/urls
test -s /tmp/urls
while read -r u; do test "$(status "$u")" = 200 || fail "$u"; done < /tmp/urls
curl -fsS -G "$API/search" --data-urlencode 'repository=eclipse' --data-urlencode 'p2.pluginName="JGit Core"' | grep -q '"name":"org.eclipse.jgit"'
test "$(curl -fsS -G "$API/search" --data-urlencode 'repository=eclipse' --data-urlencode 'p2.pluginName=NoSuchBundleName' | grep -c '"name"' || true)" = 0
echo "CLIENT PART 3 OK"
EOF

docker network create "$net" >/dev/null
docker network create --internal "$closed" >/dev/null
# The database is on the closed network alone: taking commandcenter off the
# other one must not cut a connection to it.
net="$closed" start_postgres
docker update --memory 512m --memory-swap 512m "$name-pg" >/dev/null
docker run -d --name "$name" --network "$closed" --network-alias commandcenter -e COMMANDCENTER_DATABASE_URL="$pg_url" --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" commandcenter:smoke-p2 >/dev/null
docker network connect "$net" "$name"

echo "== install the client"
docker run -d --name "$name-client" --network "$net" --memory 2g -v "$root/$work:/work:ro" "${JDK_IMAGE:-maven:3.9-eclipse-temurin-21}" sleep 3600 >/dev/null
docker exec "$name-client" sh -c "apt-get update -qq >/dev/null 2>&1 && apt-get install -y -qq xz-utils >/dev/null 2>&1 && curl -fsSL '${ECLIPSE_TGZ:-https://archive.eclipse.org/eclipse/downloads/drops4/R-4.33-202409030240/eclipse-platform-4.33-linux-gtk-x86_64.tar.gz}' | tar xz --warning=no-unknown-keyword -C /opt"
docker network connect "$closed" "$name-client"
docker network disconnect "$net" "$name-client"

ready=0
for i in $(seq 1 30); do
  if docker exec "$name-client" curl -fs http://commandcenter:8081/service/rest/v1/status >/dev/null; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "FAIL: commandcenter did not become ready"; docker logs "$name"; exit 1
fi

client() { docker exec -e TYCHO_VERSION="${TYCHO_VERSION:-4.0.13}" "$name-client" sh "/work/$1"; }
client run1.sh
docker network disconnect "$net" "$name"
client run2.sh
docker network connect "$net" "$name"
client run3.sh

echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "P2 SMOKE OK"
