#!/usr/bin/env bash
# Builds the image, starts commandcenter, then uses the real Conan clients
# (python:3.12-slim with pip's conan 2 and conan 1) to:
#   1. Conan 2: log in, create a small recipe with two binaries, upload it to
#      conan-hosted (a second upload is skipped), install it from an empty
#      cache, list, filter and search, carry metadata files, remove on the server
#   2. use a private repository: anonymous is refused, credentials from the
#      environment log in once and the stored token is enough afterwards, a
#      changed password ends it
#   3. install from conan-proxy of ConanCenter, again with the proxy blocked
#      (everything comes from its cache), and search it
#   4. install through the conan-all group; a recipe uploaded through the group
#      under a public name answers alone, the public revision is not listed
#   5. Conan 1 without revisions: log in, upload, install, filter a search, and
#      install from a proxy of center.conan.io; Conan 1 with revisions: the same
#      on the v2 API
# The server's request log then shows the routes the clients used.
# Finally prints the container's memory usage.
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-conan-$$"
net=commandcenter-conan-smoke-$$
name=commandcenter-conan-smoke-$$
. test/postgres.sh

cleanup() {
  docker rm -f -v "$name" "$name-pg" "$name-client" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup

# Built before the work directory exists: a new directory in the build
# context would compile the binary again on every run.
docker build -q -t commandcenter:smoke-conan . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i commandcenter:smoke-conan hash-password)"

mkdir -p "$work"
cat > "$work/config.yaml" <<EOF
anonymousRead: true
baseURL: "http://commandcenter:8081"
users:
  - { name: admin, passwordHash: "$hash", roles: [admin] }
  - { name: ci,    passwordHash: "$hash", roles: [deployer] }
  - { name: ci2,   passwordHash: "$hash", roles: [deployer] }
roles:
  deployer: { write: [conan-hosted, conan-private, conan-hosted1, conan-hosted1r] }
repositories:
  - { name: conan-proxy,    format: conan, type: proxy,  remote: "https://center2.conan.io/" }
  - { name: conan-center1,  format: conan, type: proxy,  remote: "https://center.conan.io/" }
  - { name: conan-hosted,   format: conan, type: hosted }
  - { name: conan-all,      format: conan, type: group,  members: [conan-hosted, conan-proxy], writeMember: conan-hosted }
  - { name: conan-private,  format: conan, type: hosted, anonymousRead: false }
  - { name: conan-hosted1,  format: conan, type: hosted }
  - { name: conan-hosted1r, format: conan, type: hosted }
EOF

cat > "$work/rest.py" <<'EOF'
# rest.py METHOD PATH [BODY] [CONTENT-TYPE]: the REST API as admin; prints the answer, fails on an error status.
import base64, sys, urllib.request
method, path = sys.argv[1:3]
body = sys.argv[3].encode() if len(sys.argv) > 3 else None
req = urllib.request.Request("http://commandcenter:8081" + path, data=body, method=method)
req.add_header("Authorization", "Basic " + base64.b64encode(b"admin:smoke-pass").decode())
if body is not None:
    req.add_header("Content-Type", sys.argv[4] if len(sys.argv) > 4 else "application/json")
print(urllib.request.urlopen(req).read().decode())
EOF

cat > "$work/run.sh" <<'EOF'
set -eu
export PIP_DISABLE_PIP_VERSION_CHECK=1 PIP_ROOT_USER_ACTION=ignore
REPO=http://commandcenter:8081/repository
REST="python /work/rest.py"
# says TEXT COMMAND...: the command succeeds and says TEXT; refused: it fails
# and says TEXT. The output goes to a file, not a pipe: a client cut off by
# grep -q stops in the middle of its work.
says() {
  want=$1; shift
  "$@" >/tmp/out.log 2>&1 || { echo "FAIL: $* exit $? (expected \"$want\")"; cat /tmp/out.log; return 1; }
  grep -q -- "$want" /tmp/out.log || { echo "FAIL: $*: no \"$want\" in the output"; cat /tmp/out.log; return 1; }
}
refused() {
  want=$1; shift
  if "$@" >/tmp/out.log 2>&1; then echo "FAIL: $*: succeeded (expected \"$want\")"; cat /tmp/out.log; return 1; fi
  grep -q -- "$want" /tmp/out.log || { echo "FAIL: $*: no \"$want\" in the output"; cat /tmp/out.log; return 1; }
}
lacks() {
  want=$1; shift
  "$@" >/tmp/out.log 2>&1 || { echo "FAIL: $* exit $?"; cat /tmp/out.log; return 1; }
  if grep -q -- "$want" /tmp/out.log; then echo "FAIL: $*: \"$want\" in the output"; cat /tmp/out.log; return 1; fi
}

pip install -q conan==2.34.0
python -m venv /tmp/c1venv && /tmp/c1venv/bin/pip install -q conan==1.66.0
conan --version
/tmp/c1venv/bin/conan --version

export CONAN_HOME=/tmp/c2
conan profile list >/dev/null 2>&1 || true
mkdir -p "$CONAN_HOME/profiles"
# A profile ConanCenter has binaries for; the test's own recipes compile nothing.
cat > "$CONAN_HOME/profiles/default" <<PROFILE
[settings]
arch=x86_64
build_type=Release
compiler=gcc
compiler.cppstd=gnu17
compiler.libcxx=libstdc++11
compiler.version=11
os=Linux
PROFILE
# A prompt would hang here: credentials are in the arguments or the environment, or the command fails.
echo "core:non_interactive=True" > "$CONAN_HOME/global.conf"
conan remote remove conancenter
conan remote add cc "$REPO/conan-hosted"
conan remote add ccproxy "$REPO/conan-proxy"
conan remote add ccgroup "$REPO/conan-all"
conan remote add ccprivate "$REPO/conan-private"

mkdir -p /tmp/hello/include /tmp/zlib/include
cat > /tmp/hello/conanfile.py <<'RECIPE'
import os
from conan import ConanFile
from conan.tools.files import copy


class Recipe(ConanFile):
    name = "hello"
    version = "1.0"
    license = "MIT"
    description = "Command Center smoke recipe"
    settings = "os", "arch"
    exports_sources = "include/*"

    def package(self):
        copy(self, "*.h", os.path.join(self.source_folder, "include"), os.path.join(self.package_folder, "include"))
RECIPE
echo '#define HELLO "hello from hosted"' > /tmp/hello/include/hello.h
sed -e 's/"hello"/"zlib"/' -e 's/"1.0"/"1.3.1"/' /tmp/hello/conanfile.py > /tmp/zlib/conanfile.py
echo '#define INTERNAL_ZLIB 1' > /tmp/zlib/include/zlib.h

echo "== Conan 2: login"
refused "Wrong user or password" conan remote login cc ci -p wrong
says "Authenticated in remote 'cc' with user 'ci'" conan remote login cc ci -p smoke-pass
says "ci" conan remote list-users

echo "== Conan 2: create and upload to hosted"
cd /tmp/hello
says "Package '" conan create .
says "Package '" conan create . -s os=Windows
says "(Uploaded)" conan upload "hello/1.0:*" -r cc -c
says "Skipped, already in server" conan upload "hello/1.0:*" -r cc -c

echo "== Conan 2: install from an empty cache"
conan remove "*" -c >/dev/null
cd /tmp
says "Downloaded package revision" conan install --requires=hello/1.0 -r cc --deployer=direct_deploy --deployer-folder=/tmp/dep
grep -q "hello from hosted" /tmp/dep/direct_deploy/hello/include/hello.h

echo "== Conan 2: list, filter, search"
says "os: Windows" conan list "hello/1.0#*:*" -r cc
grep -q "os: Linux" /tmp/out.log
lacks "os: Linux" conan list "hello/1.0:*" -p "os=Windows" -r cc
says "hello/1.0" conan search "hel*" -r cc

echo "== Conan 2: metadata files travel with the recipe"
meta="$(conan cache path hello/1.0 --folder=metadata)"
mkdir -p "$meta/logs" && echo "built by the smoke test" > "$meta/logs/build.log"
conan upload hello/1.0 -r cc --metadata="logs/*" -c >/dev/null
conan remove "*" -c >/dev/null
conan download hello/1.0 -r cc --only-recipe --metadata="logs/*" >/dev/null
grep -q "built by the smoke test" "$(conan cache path hello/1.0 --folder=metadata)/logs/build.log"

echo "== Conan 2: a private repository, logged in once"
says "Conan interactive mode disabled" conan list "hello/*" -r ccprivate  # conan list reports a remote's error in its output
cd /tmp/hello
conan create . >/dev/null 2>&1
CONAN_LOGIN_USERNAME_CCPRIVATE=ci2 CONAN_PASSWORD_CCPRIVATE=smoke-pass says "(Uploaded)" conan upload "hello/1.0:*" -r ccprivate -c
cd /tmp
says "hello/1.0" conan list "hello/*" -r ccprivate  # no credentials left: the stored token
days="$(python - <<'PY'
import base64, json, time, urllib.request
req = urllib.request.Request("http://commandcenter:8081/repository/conan-private/v2/users/authenticate")
req.add_header("Authorization", "Basic " + base64.b64encode(b"ci2:smoke-pass").decode())
body = urllib.request.urlopen(req).read().decode().split(".")[0]
print(int((json.loads(base64.urlsafe_b64decode(body + "==="))["exp"] - time.time()) / 86400))
PY
)"
test "$days" -ge 364 || { echo "FAIL: the login token lasts $days days"; exit 1; }
echo "the login token lasts $days days"
$REST PUT /service/rest/v1/security/users/ci2/change-password new-pass text/plain >/dev/null
says "Conan interactive mode disabled" conan list "hello/*" -r ccprivate  # the changed password ended the token
CONAN_LOGIN_USERNAME_CCPRIVATE=ci2 CONAN_PASSWORD_CCPRIVATE=new-pass says "hello/1.0" conan list "hello/*" -r ccprivate
says "hello/1.0" conan list "hello/*" -r ccprivate

echo "== Conan 2: proxy of ConanCenter"
conan remove "*" -c >/dev/null
says "Downloaded package revision" conan install --requires=zlib/1.3.1 -r ccproxy
says "zlib/1.3" conan search "zlib/1.3*" -r ccproxy
blocked() {
  $REST PUT /service/rest/v1/repositories/conan/proxy/conan-proxy \
    "{\"name\":\"conan-proxy\",\"online\":true,\"storage\":{\"blobStoreName\":\"default\",\"strictContentTypeValidation\":false},\"proxy\":{\"remoteUrl\":\"https://center2.conan.io/\",\"contentMaxAge\":-1,\"metadataMaxAge\":1440},\"negativeCache\":{\"enabled\":true,\"timeToLive\":10},\"httpClient\":{\"blocked\":$1,\"autoBlock\":true}}" >/dev/null
}
blocked true
conan remove "*" -c >/dev/null
says "Downloaded package revision" conan install --requires=zlib/1.3.1 -r ccproxy  # all of it from the cache
blocked false

echo "== Conan 2: group"
conan remove "*" -c >/dev/null
says "Authenticated in remote 'ccgroup'" conan remote login ccgroup ci -p smoke-pass
says "Downloaded package revision" conan install --requires=hello/1.0 --requires=zlib/1.3.1 -r ccgroup
public="$(conan list "zlib/1.3.1#*" -c --format=json | python -c 'import json,sys; d=json.load(sys.stdin); print(list(d["Local Cache"]["zlib/1.3.1"]["revisions"])[0])')"
conan remove "*" -c >/dev/null
cd /tmp/zlib
conan create . >/dev/null 2>&1
internal="$(conan list "zlib/1.3.1#*" -c --format=json | python -c 'import json,sys; d=json.load(sys.stdin); print(list(d["Local Cache"]["zlib/1.3.1"]["revisions"])[0])')"
says "(Uploaded)" conan upload "zlib/1.3.1:*" -r ccgroup -c
cd /tmp
conan remove "*" -c >/dev/null
says "$internal" conan list "zlib/1.3.1#*" -r ccgroup
lacks "$public" conan list "zlib/1.3.1#*" -r ccgroup
says "Downloaded package revision" conan install --requires=zlib/1.3.1 -r ccgroup --deployer=direct_deploy --deployer-folder=/tmp/dep
grep -q INTERNAL_ZLIB /tmp/dep/direct_deploy/zlib/include/zlib.h

echo "== Conan 2: remove on the server, and the REST API"
$REST GET "/service/rest/v1/search?repository=conan-hosted&name=hello" >/tmp/found.json
grep -q '"name":"hello"' /tmp/found.json
says "Remove summary" conan remove "hello/1.0#*:*" -r cc -c
says "Remove summary" conan remove "hello/1.0" -r cc -c
lacks "hello/1.0" conan search "hel*" -r cc

echo "== Conan 1, no revisions"
c1() { CONAN_USER_HOME=/tmp/c1 CONAN_NON_INTERACTIVE=1 /tmp/c1venv/bin/conan "$@"; }
mkdir -p /tmp/hi && cd /tmp/hi
cat > conanfile.py <<'RECIPE'
from conans import ConanFile


class Recipe(ConanFile):
    name = "hi"
    version = "1.0"
    settings = "os"
    exports_sources = "hi.h"

    def package(self):
        self.copy("hi.h", dst="include")
RECIPE
echo '#define HI 1' > hi.h
c1 config init >/dev/null 2>&1 || true
c1 remote clean
c1 remote add cc "$REPO/conan-hosted1"
c1 remote add ccenter "$REPO/conan-center1"
says "Changed user" c1 user -p smoke-pass -r cc ci
says "Created package" c1 create . demo/stable -s os=Linux
says "Created package" c1 create . demo/stable -s os=Windows
c1 upload "hi/1.0@demo/stable" --all -r cc -c >/dev/null
c1 remove "hi/*" -f
says "Downloaded package" c1 install hi/1.0@demo/stable -r cc -s os=Linux
says "os: Windows" c1 search hi/1.0@demo/stable -r cc -q "os=Windows"
lacks "os: Linux" c1 search hi/1.0@demo/stable -r cc -q "os=Windows"
says "Downloaded package" c1 install zlib/1.2.13@ -r ccenter -s os=Linux -s arch=x86_64 -s compiler=gcc -s compiler.version=11 -s compiler.libcxx=libstdc++11 -s build_type=Release

echo "== Conan 1 with revisions"
c1r() { CONAN_USER_HOME=/tmp/c1r CONAN_NON_INTERACTIVE=1 CONAN_REVISIONS_ENABLED=1 /tmp/c1venv/bin/conan "$@"; }
c1r config init >/dev/null 2>&1 || true
c1r remote clean
c1r remote add cc "$REPO/conan-hosted1r"
says "Changed user" c1r user -p smoke-pass -r cc ci
says "Created package" c1r create . demo/stable -s os=Linux
says "Created package" c1r create . demo/stable -s os=Windows
c1r upload "hi/1.0@demo/stable" --all -r cc -c >/dev/null
c1r remove "hi/*" -f
says "Downloaded package" c1r install hi/1.0@demo/stable -r cc -s os=Linux
says "os: Windows" c1r search hi/1.0@demo/stable -r cc -q "os=Windows"
lacks "os: Linux" c1r search hi/1.0@demo/stable -r cc -q "os=Windows"
echo "CONAN CLIENT OK"
EOF

docker network create "$net" >/dev/null
start_postgres
docker run -d --name "$name" --network "$net" -e COMMANDCENTER_DATABASE_URL="$pg_url" --network-alias commandcenter --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" commandcenter:smoke-conan >/dev/null

# No port is published: the image's own health check asks the server.
ready=0
for i in $(seq 1 30); do
  if docker exec "$name" commandcenter healthcheck 2>/dev/null; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "FAIL: commandcenter did not become ready"; docker logs "$name"; exit 1
fi

docker run --rm --name "$name-client" --network "$net" --memory 2g -v "$root/$work:/work:ro" \
  python:3.12-slim sh /work/run.sh

echo "== what the clients asked for"
docker logs "$name" > "$work/server.log" 2>&1
asked() {
  grep -q "\"method\":\"$1\",\"path\":\"/repository/$2\",\"status\":$3," "$work/server.log" ||
    { echo "FAIL: the client made no $1 $2 answered $3"; exit 1; }
}
asked GET conan-hosted/v2/users/authenticate 200                 # Conan 2 logs in under v2
asked GET conan-hosted/v2/users/authenticate 401
asked GET conan-hosted/v2/users/check_credentials 200
asked GET conan-hosted/v1/ping 200
asked GET conan-proxy/v2/conans/zlib/1.3.1/_/_/latest 200
asked GET conan-hosted1/v1/users/authenticate 200                # Conan 1 stays on v1
asked GET conan-hosted1/v1/conans/hi/1.0/demo/stable/search 200
asked POST conan-hosted1/v1/conans/hi/1.0/demo/stable/upload_urls 200
asked GET conan-hosted1r/v1/users/authenticate 200               # with revisions it logs in there, and moves to v2
asked GET conan-hosted1r/v2/users/check_credentials 200
asked GET conan-hosted1r/v2/conans/hi/1.0/demo/stable/search 200
asked GET conan-center1/v1/conans/zlib/1.2.13/_/_/download_urls 200

echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "CONAN SMOKE OK"
