#!/usr/bin/env bash
# Builds the image, starts commandcenter with a proxy of the CocoaPods trunk
# CDN and uses the real pod command (the cocoapods gem in the ruby image, on
# Linux) from a network that reaches only commandcenter, so a clone or a
# download that goes around the proxy fails the test:
#   1. pod repo add-cdn, pod repo list, pod spec cat: a git source (Alamofire)
#      comes back as a tarball through the repository
#   2. pod install, with integrate_targets off: a pod with a GitHub git source,
#      one with an http source and its git dependencies are resolved, downloaded
#      through the repository and laid out with a generated Pods.xcodeproj
#   3. REST: only what was installed is a component, every downloadUrl answers
#   4. pod install with empty caches after commandcenter lost its internet
#      access: everything comes from the proxy's cache
#   5. a read that needs authentication: 401 without ~/.netrc, passes with it
#   6. an ALLOW routing rule written for repository paths lets the install pass;
#      a BLOCK rule on one pod's archives refuses them
#   7. hosted is refused (the format is proxy-only, as in Nexus)
#   8. a small trunk of our own, whose podspecs name real public repositories on
#      Bitbucket and on GitLab (a nested group, no ref): pod install through the
#      proxy; a git source on another host stays what it is; with metadataMaxAge
#      0 a version published later is seen at once
# What needs a Mac does not run here: integrating into an Xcode project and
# building (xcodebuild), pod lib lint, pod spec lint.
# Finally prints the container's memory usage. Every run installs a compiler
# and the gems (about 150 MiB) and downloads about 30 MiB through the proxy.
#   SMOKE_PREFIX  start of the container and network names (default commandcenter)
#   RUBY_IMAGE    the client's image (default ruby:3.4-slim)
#   POD_VERSION   the cocoapods gem (default 1.17.0)
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-cocoapods-$$"
name="${SMOKE_PREFIX:-commandcenter}-cocoapods-smoke-$$"
net="$name"
closed="$name-closed"
. test/postgres.sh

cleanup() {
  docker rm -f -v "$name" "$name-pg" "$name-client" "$name-trunk" >/dev/null 2>&1 || true
  docker network rm "$net" "$closed" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup
rm -rf "$work" && mkdir -p "$work"

docker build -q -t commandcenter:smoke-cocoapods . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i --name "$name-hash" --memory 128m commandcenter:smoke-cocoapods hash-password)"

cat > "$work/config.yaml" <<EOF
anonymousRead: true
baseURL: "http://commandcenter:8081"
users:
  - { name: admin, passwordHash: "$hash", roles: [admin] }
repositories:
  - { name: pods, format: cocoapods, type: proxy, remote: "https://cdn.cocoapods.org/" }
  - { name: pods-private, format: cocoapods, type: proxy, remote: "https://cdn.cocoapods.org/", anonymousRead: false }
EOF

cat > "$work/run.sh" <<'EOF'
set -eu
REG=http://commandcenter:8081/repository
API=http://commandcenter:8081/service/rest/v1
ADMIN=admin:smoke-pass
export LANG=C.UTF-8 COCOAPODS_DISABLE_STATS=1

fail() { echo "FAIL: $*" >&2; exit 1; }
# json <ruby expression on j>: reads a JSON document from stdin.
json() { ruby -rjson -e 'j = JSON.parse(STDIN.read); puts(eval(ARGV[0]))' "$1"; }

podfile() { # podfile <source repository> <pods...>
  src=$1; shift
  { echo "source '$REG/$src/'"
    echo "platform :ios, '15.0'"
    echo "install! 'cocoapods', :integrate_targets => false"
    echo "target 'App' do"
    for p in "$@"; do echo "  pod $p"; done
    echo "end"; } > Podfile
}
# fresh: nothing on the client but the Podfile.
fresh() {
  rm -rf ~/.cocoapods ~/Library Pods Podfile.lock
}

echo "== the client's network reaches only commandcenter"
if curl -fsS -m 5 -o /dev/null https://github.com/ 2>/dev/null; then fail "github.com is reachable"; fi
if curl -fsS -m 5 -o /dev/null https://cdn.cocoapods.org/CocoaPods-version.yml 2>/dev/null; then fail "the CDN is reachable"; fi
curl -fsS -m 30 "$REG/pods/CocoaPods-version.yml" | grep -q '^last:'
pod --version

echo "== pod repo add-cdn, list"
pod repo add-cdn cc "$REG/pods/" >/dev/null
pod repo list | grep -q "URL:  $REG/pods/"
pod repo list | grep -q "Type: CDN"

echo "== pod spec cat: a git source is a tarball through the repository"
pod spec cat Alamofire --version=5.9.1 > /tmp/spec.json
src=$(json 'j["source"].to_json' < /tmp/spec.json)
echo "$src"
case "$src" in
*"\"http\":\"$REG/pods/pods/Alamofire/5.9.1/"*".tar.gz\""*) ;;
*) fail "source of Alamofire: $src" ;;
esac
case "$src" in *git*|*tag*) fail "git keys left: $src" ;; esac
pod repo remove cc >/dev/null

echo "== pod install: git sources on GitHub and an http source with git dependencies"
mkdir -p ~/app && cd ~/app
podfile pods "'Alamofire', '5.9.1'" "'GoogleSignIn', '5.0.2'"
fresh
pod install --verbose > /tmp/install.log 2>&1 || { tail -40 /tmp/install.log; fail "pod install"; }
grep "Pod installation complete" /tmp/install.log
test -d Pods/Alamofire/Source
test -d Pods/GoogleSignIn/Frameworks
test -d Pods/AppAuth/Sources
test -f Pods/Pods.xcodeproj/project.pbxproj
grep -q "Alamofire (5.9.1)" Podfile.lock
if grep -E 'git clone|https://github.com/[^ ]*\.git' /tmp/install.log; then fail "the client cloned from git"; fi
grep -q "curl.*$REG/pods/pods/Alamofire/5.9.1/" /tmp/install.log || fail "no download of Alamofire through the repository"

echo "== REST: what was installed is a component, every downloadUrl answers"
curl -fsS "$API/components?repository=pods" > /tmp/components.json
json 'j["items"].map { |c| c["name"] + "@" + c["version"] }.sort.join(" ")' < /tmp/components.json
json 'j["items"].size' < /tmp/components.json > /tmp/n
test "$(cat /tmp/n)" -ge 5 || fail "components: $(cat /tmp/n)"
# The client read the podspecs of every Alamofire version it considered: none but the installed one is a component.
test "$(json 'j["items"].count { |c| c["name"] == "Alamofire" }' < /tmp/components.json)" = 1 || fail "Alamofire components"
curl -fsS "$API/search/assets?repository=pods&name=Alamofire" > /tmp/assets.json
json 'j["items"].map { |a| a["downloadUrl"] + " " + a["path"] + " " + a["checksum"]["sha1"] }.join("\n")' < /tmp/assets.json > /tmp/urls
test "$(wc -l < /tmp/urls)" = 1 || { cat /tmp/urls; fail "Alamofire assets"; }
while read -r url p sum; do
  case "$p" in /pods/Alamofire/5.9.1/*.tar.gz) ;; *) fail "asset path $p" ;; esac
  got=$(curl -fsS "$url" | sha1sum | cut -d' ' -f1)
  test "$got" = "$sum" || fail "$url: sha1 $got, want $sum"
done < /tmp/urls
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST -u "$ADMIN" -H 'Content-Type: application/json' \
  -d '{"name":"pods-h","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":true,"writePolicy":"ALLOW"}}' "$API/repositories/cocoapods/hosted")
test "$code" = 400 || fail "cocoapods/hosted: $code"
echo "CLIENT PART 1 OK"
EOF

cat > "$work/run2.sh" <<'EOF'
set -eu
REG=http://commandcenter:8081/repository
API=http://commandcenter:8081/service/rest/v1
ADMIN=admin:smoke-pass
export LANG=C.UTF-8 COCOAPODS_DISABLE_STATS=1
fail() { echo "FAIL: $*" >&2; exit 1; }
cd ~/app

echo "== pod install with empty caches: commandcenter has no internet now"
rm -rf ~/.cocoapods ~/Library Pods Podfile.lock
pod install --verbose > /tmp/install2.log 2>&1 || { tail -40 /tmp/install2.log; fail "second pod install"; }
grep "Pod installation complete" /tmp/install2.log
test -d Pods/Alamofire/Source
test -d Pods/GoogleSignIn/Frameworks
echo "CLIENT PART 2 OK"
EOF

cat > "$work/run3.sh" <<'EOF'
set -eu
REG=http://commandcenter:8081/repository
API=http://commandcenter:8081/service/rest/v1
ADMIN=admin:smoke-pass
export LANG=C.UTF-8 COCOAPODS_DISABLE_STATS=1
fail() { echo "FAIL: $*" >&2; exit 1; }
podfile() {
  src=$1; shift
  { echo "source '$REG/$src/'"
    echo "platform :ios, '15.0'"
    echo "install! 'cocoapods', :integrate_targets => false"
    echo "target 'App' do"
    for p in "$@"; do echo "  pod $p"; done
    echo "end"; } > Podfile
}
fresh() { rm -rf ~/.cocoapods ~/Library Pods Podfile.lock; }
mkdir -p ~/app3 && cd ~/app3

echo "== a read that needs authentication: refused without ~/.netrc"
podfile pods-private "'SwiftyJSON', '5.0.2'"
fresh; rm -f ~/.netrc
code=$(curl -s -o /dev/null -w '%{http_code}' "$REG/pods-private/CocoaPods-version.yml")
test "$code" = 401 || fail "anonymous index file: $code"
if pod install > /tmp/noauth.log 2>&1; then fail "pod install without credentials passed"; fi
tail -3 /tmp/noauth.log

echo "== passes with ~/.netrc"
printf 'machine commandcenter\n  login admin\n  password smoke-pass\n' > ~/.netrc
chmod 600 ~/.netrc
fresh
pod install > /tmp/auth.log 2>&1 || { tail -40 /tmp/auth.log; fail "pod install with ~/.netrc"; }
grep "Pod installation complete" /tmp/auth.log
test -d Pods/SwiftyJSON/Source
rm -f ~/.netrc

echo "== an ALLOW routing rule for repository paths"
api() { curl -fsS -u "$ADMIN" -H 'Content-Type: application/json' "$@"; }
api -X POST "$API/routing-rules" -d '{"name":"pods-allow","mode":"ALLOW","matchers":["^/[^/]+\\.(txt|yml)$","^/Specs/.*","^/pods/.*"]}' >/dev/null
proxy='"proxy":{"remoteUrl":"https://cdn.cocoapods.org/","contentMaxAge":-1,"metadataMaxAge":1440},"negativeCache":{"enabled":true,"timeToLive":10},"httpClient":{"blocked":false,"autoBlock":true},"storage":{"blobStoreName":"default","strictContentTypeValidation":true}'
api -X POST "$API/repositories/cocoapods/proxy" -d "{\"name\":\"pods-routed\",\"online\":true,$proxy,\"routingRule\":\"pods-allow\"}" >/dev/null
podfile pods-routed "'SwiftyJSON', '5.0.2'"
fresh
pod install > /tmp/routed.log 2>&1 || { tail -40 /tmp/routed.log; fail "pod install under the ALLOW rule"; }
grep "Pod installation complete" /tmp/routed.log
test -d Pods/SwiftyJSON/Source
code=$(curl -s -o /dev/null -w '%{http_code}' "$REG/pods-routed/other/thing")
test "$code" = 403 || fail "a path outside the ALLOW rule: $code"

echo "== a BLOCK routing rule on one pod's archives"
api -X POST "$API/routing-rules" -d '{"name":"pods-block","mode":"BLOCK","matchers":["^/pods/SwiftyJSON/.*"]}' >/dev/null
api -X POST "$API/repositories/cocoapods/proxy" -d "{\"name\":\"pods-blocked\",\"online\":true,$proxy,\"routingRule\":\"pods-block\"}" >/dev/null
podfile pods-blocked "'SwiftyJSON', '5.0.2'"
fresh
if pod install > /tmp/blocked.log 2>&1; then fail "pod install under the BLOCK rule passed"; fi
grep -q 403 /tmp/blocked.log || { tail -20 /tmp/blocked.log; fail "no 403 from the BLOCK rule"; }

echo "== a small trunk: git sources on Bitbucket and on GitLab"
api -X POST "$API/repositories/cocoapods/proxy" -d "{\"name\":\"pods-local\",\"online\":true,$(printf '%s' "$proxy" | sed 's|https://cdn.cocoapods.org/|http://trunk:8000/|; s|"metadataMaxAge":1440|"metadataMaxAge":0|')}" >/dev/null
podfile pods-local "'SmokeBitbucket', '1.0.0'" "'SmokeGitlab', '1.0.0'"
fresh
pod install --verbose > /tmp/local.log 2>&1 || { tail -40 /tmp/local.log; fail "pod install from the small trunk"; }
grep "Pod installation complete" /tmp/local.log
test -f Pods/SmokeBitbucket/README.md
test -f Pods/SmokeGitlab/README.md
if grep -E 'git clone' /tmp/local.log; then fail "the client cloned from git"; fi
grep -q "curl.*$REG/pods-local/pods/SmokeGitlab/1.0.0/" /tmp/local.log

echo "== a git source on another host stays what it is"
pod repo add-cdn local "$REG/pods-local/" >/dev/null
pod spec cat SmokeOtherHost --version=1.0.0 > /tmp/other.json || { tail -20 /tmp/other.json; fail "pod spec cat"; }
grep -q '"git":"https://git.example/o/r.git"' /tmp/other.json
grep -q '"tag":"1.0.0"' /tmp/other.json
pod repo remove local >/dev/null
echo "CLIENT PART 3 OK"
EOF

cat > "$work/run4.sh" <<'EOF'
set -eu
export LANG=C.UTF-8 COCOAPODS_DISABLE_STATS=1
cd ~/app3
echo "== with metadataMaxAge 0 a version published later is seen at once"
sed -i "s/pod 'SmokeGitlab', '1.0.0'/pod 'SmokeGitlab', '1.1.0'/" Podfile
pod install --repo-update > /tmp/local2.log 2>&1 || { tail -40 /tmp/local2.log; echo "FAIL: the new version"; exit 1; }
grep "Pod installation complete" /tmp/local2.log
grep -q "SmokeGitlab (1.1.0)" Podfile.lock
echo "CLIENT PART 4 OK"
EOF

# trunk_pod <pod> <version> <source> <file a source tree has>: publishes a podspec on the small trunk.
trunk="$work/trunk"
trunk_pod() {
  local sum shard
  sum="$(printf '%s' "$1" | md5sum)"
  shard="${sum:0:1}/${sum:1:1}/${sum:2:1}"
  mkdir -p "$trunk/Specs/$shard/$1/$2"
  printf '{"name":"%s","version":"%s","summary":"Smoke","homepage":"https://example.com","license":"MIT","authors":{"a":"a@example.com"},"platforms":{"ios":"13.0"},"source":%s,"source_files":"%s"}' \
    "$1" "$2" "$3" "$4" > "$trunk/Specs/$shard/$1/$2/$1.podspec.json"
  local shardfile="$trunk/all_pods_versions_${sum:0:1}_${sum:1:1}_${sum:2:1}.txt"
  if grep -qs "^$1/" "$shardfile"; then sed -i "s|^$1/.*|&/$2|" "$shardfile"; else echo "$1/$2" >> "$shardfile"; echo "$1" >> "$trunk/all_pods.txt"; fi
}
mkdir -p "$trunk"
printf -- '---\nmin: 1.0.0\nlast: 1.16.1\nprefix_lengths:\n- 1\n- 1\n- 1\n' > "$trunk/CocoaPods-version.yml"
: > "$trunk/deprecated_podspecs.txt"
trunk_pod SmokeBitbucket 1.0.0 '{"git":"https://bitbucket.org/tutorials/markdowndemo.git","branch":"master"}' 'README.md'
trunk_pod SmokeGitlab 1.0.0 '{"git":"https://gitlab.com/gitlab-org/project-templates/spring.git"}' 'README.md'
trunk_pod SmokeOtherHost 1.0.0 '{"git":"https://git.example/o/r.git","tag":"1.0.0"}' '*.md'

docker network create "$net" >/dev/null
docker network create --internal "$closed" >/dev/null
# The database is on the closed network alone: taking commandcenter off the
# other one must not cut a connection to it.
net="$closed" start_postgres
docker update --memory 512m --memory-swap 512m "$name-pg" >/dev/null
docker run -d --name "$name" --network "$closed" --network-alias commandcenter -e COMMANDCENTER_DATABASE_URL="$pg_url" --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" commandcenter:smoke-cocoapods >/dev/null
docker network connect "$net" "$name"
docker run -d --name "$name-trunk" --network "$closed" --network-alias trunk --memory 128m -v "$root/$trunk:/trunk:ro" python:3.12-slim \
  python -m http.server 8000 --directory /trunk >/dev/null

echo "== install the client"
docker run -d --name "$name-client" --network "$net" --memory 2g -v "$root/$work:/work:ro" "${RUBY_IMAGE:-ruby:3.4-slim}" sleep 3600 >/dev/null
docker exec "$name-client" sh -c "apt-get update -qq >/dev/null 2>&1 && apt-get install -y -qq curl git rsync libcurl4 build-essential >/dev/null 2>&1 && gem install cocoapods -v '${POD_VERSION:-1.17.0}' --no-document >/dev/null 2>&1 && useradd -m pod"
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

pod() { docker exec -u pod -e HOME=/home/pod "$name-client" sh "/work/$1"; }
pod run.sh
docker network disconnect "$net" "$name"
pod run2.sh
docker network connect "$net" "$name"
pod run3.sh
sleep 1 # a Last-Modified has seconds
trunk_pod SmokeGitlab 1.1.0 '{"git":"https://gitlab.com/gitlab-org/project-templates/spring.git"}' 'README.md'
pod run4.sh

echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "COCOAPODS SMOKE OK"
