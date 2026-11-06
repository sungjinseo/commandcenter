#!/usr/bin/env bash
# Builds the image, starts commandcenter behind a TLS proxy (Caddy with its
# own CA: SwiftPM wants https, and `package-registry login` takes nothing
# else) and a second commandcenter as the proxy's remote (there is no public
# Swift registry), then uses real SwiftPM (the official swift image) to:
#   1. refuse without credentials and with a wrong password: a clean error,
#      not a hang (every SwiftPM command runs under a timeout)
#   2. package-registry login with a password, publish through the group,
#      refuse the second publish, and build and run a package that needs it
#   3. publish a second version with package-metadata.json and a
#      version-specific manifest: package update, identifiers, and a URL
#      dependency resolved with --replace-scm-with-registry
#   4. login with an API token, publish a signed release, PUT a zip that
#      `package archive-source` made with curl, delete a version
#   5. resolve a package of the remote through the proxy in the group, and
#      again from the proxy's cache with the remote stopped
# Takes the swift image (5 GB, the first time) and about 35 MB from Ubuntu's
# archive: zip, which publish calls and the image lacks, and curl.
# Finally prints the container's memory usage.
#   SMOKE_SWIFT_IMAGE  the client image (default swift:6.2)
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-swift-$$"
net=commandcenter-swift-smoke-$$
name=commandcenter-swift-smoke-$$
. test/postgres.sh

cleanup() {
  docker rm -f -v "$name" "$name-pg" "$name-up" "$name-tls" "$name-c" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup
rm -rf "$work" && mkdir -p "$work"

docker build -q -t commandcenter:smoke-swift . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i commandcenter:smoke-swift hash-password)"

cat > "$work/config.yaml" <<EOF
anonymousRead: false
baseURL: "https://registry.smoke.test"
users:
  - { name: ci, passwordHash: "$hash", roles: [deployer] }
roles:
  deployer: { write: [swift-hosted, swift-all] }
repositories:
  - { name: swift-proxy,  format: swift, type: proxy,  remote: "http://upstream:8081/repository/swift-hosted/" }
  - { name: swift-hosted, format: swift, type: hosted }
  - { name: swift-all,    format: swift, type: group,  members: [swift-hosted, swift-proxy], writeMember: swift-hosted }
EOF

cat > "$work/upstream.yaml" <<EOF
anonymousRead: true
baseURL: "https://upstream.smoke.test"
users:
  - { name: ci, passwordHash: "$hash", roles: [deployer] }
roles:
  deployer: { write: [swift-hosted] }
repositories:
  - { name: swift-hosted, format: swift, type: hosted }
EOF

cat > "$work/Caddyfile" <<'EOF'
registry.smoke.test {
	tls internal
	reverse_proxy commandcenter:8081
}
upstream.smoke.test {
	tls internal
	reverse_proxy upstream:8081
}
EOF

cat > "$work/lib.sh" <<'EOF'
R=https://registry.smoke.test/repository
U=https://upstream.smoke.test/repository
ACCEPT='Accept: application/vnd.swift.registry.v1+json'
fail() { echo "FAIL: $*"; exit 1; }
# A hung client (SwiftPM on a Basic challenge) must fail the test, not hold it.
t() { timeout -s KILL 300 "$@"; }
# mkapp <dir> <dependency lines> <products> <imports and print>: an executable package
mkapp() {
  rm -rf "$1" && mkdir -p "$1/Sources/App"
  cat > "$1/Package.swift" <<PKG
// swift-tools-version: 5.9
import PackageDescription

let package = Package(
    name: "App",
    dependencies: [$2],
    targets: [.executableTarget(name: "App", dependencies: [$3])]
)
PKG
  printf '%b\n' "$4" > "$1/Sources/App/main.swift"
}
fresh() { rm -rf .build Package.resolved "$HOME/.swiftpm/security" "$HOME/.cache/org.swift.swiftpm"; }
run() { t swift build -q "$@" >/tmp/build.log 2>&1 || { cat /tmp/build.log; fail "swift build $*"; }; .build/debug/App; }
GREET='import LinkedList\nprint(greet())'
LINKED='.package(id: "mona.LinkedList", from: "1.0.0")'
PRODUCT='.product(name: "LinkedList", package: "mona.LinkedList")'
EOF

cat > "$work/run.sh" <<'EOF'
set -eu
. /work/lib.sh
export DEBIAN_FRONTEND=noninteractive
(apt-get update -qq && apt-get install -y -qq zip curl) >/tmp/apt.log 2>&1 || { tail -20 /tmp/apt.log; fail "apt-get install zip curl"; }
cp /work/ca.crt /usr/local/share/ca-certificates/smoke.crt && update-ca-certificates >/dev/null 2>&1
for i in $(seq 30); do # a 404 (curl's 22) from the upstream: the TLS proxy has its certificates
  rc=0
  curl -fs -o /dev/null "$U/swift-hosted/smoke/none" || rc=$?
  [ "$rc" = 22 ] && break
  sleep 1
done
swift --version 2>&1 | head -1

# mklib <version>: mona.LinkedList at that version in /tmp/LinkedList
mklib() {
  rm -rf /tmp/LinkedList && mkdir -p /tmp/LinkedList/Sources/LinkedList
  cat > /tmp/LinkedList/Package.swift <<'PKG'
// swift-tools-version: 5.9
import PackageDescription

let package = Package(
    name: "LinkedList",
    products: [.library(name: "LinkedList", targets: ["LinkedList"])],
    targets: [.target(name: "LinkedList")]
)
PKG
  printf 'public func greet() -> String { "linked list %s" }\n' "$1" > /tmp/LinkedList/Sources/LinkedList/LinkedList.swift
}

echo "== no credentials and a wrong password: a clean error, not a hang"
t swift package-registry set --global "$R/swift-all"
mklib 1.0.0 && cd /tmp/LinkedList
out="$(t swift package-registry publish mona.LinkedList 1.0.0 --url "$R/swift-all" 2>&1)" && fail "a publish without credentials succeeded"
echo "$out" | grep -q 'authentication credentials' || fail "publish without credentials: $out"
mkapp /tmp/App "$LINKED" "$PRODUCT" "$GREET" && cd /tmp/App
out="$(t swift package resolve 2>&1)" && fail "a resolve without credentials succeeded"
echo "$out" | grep -q 'authentication credentials' || fail "resolve without credentials: $out"
out="$(t swift package-registry login "$R/swift-all/login" --username ci --password wrong --no-confirm 2>&1)" && fail "a login with a wrong password succeeded"
echo "$out" | grep -q 'authentication credentials' || fail "login with a wrong password: $out"

echo "== login with a password, publish through the group, a second publish is refused"
t swift package-registry login "$R/swift-all/login" --username ci --password smoke-pass --no-confirm >/dev/null
cd /tmp/LinkedList
t swift package-registry publish mona.LinkedList 1.0.0 --url "$R/swift-all" | grep -q 'successfully published' || fail "publish"
out="$(t swift package-registry publish mona.LinkedList 1.0.0 --url "$R/swift-all" 2>&1)" && fail "a second publish succeeded"
echo "$out" | grep -q 'server error 409' || fail "second publish: $out"

echo "== build and run a package that needs it"
cd /tmp/App
test "$(run)" = "linked list 1.0.0" || fail "the app with 1.0.0"
grep -q '"kind" : "registry"' Package.resolved || fail "Package.resolved: $(cat Package.resolved)"

echo "== a second version with metadata and a version-specific manifest: package update"
mklib 1.1.0 && cd /tmp/LinkedList
cp Package.swift Package@swift-5.10.swift
cat > package-metadata.json <<'JSON'
{"description": "A linked list", "licenseURL": "https://example.com/mona/LICENSE", "readmeURL": "https://example.com/mona/README.md",
 "repositoryURLs": ["https://github.com/mona/LinkedList.git", "git@github.com:mona/LinkedList.git"]}
JSON
t swift package-registry publish mona.LinkedList 1.1.0 --url "$R/swift-all" >/dev/null
cd /tmp/App
t swift package update >/dev/null 2>&1
test "$(run)" = "linked list 1.1.0" || fail "the app after package update"

echo "== what the registry says of it"
doc="$(curl -fsS -u ci:smoke-pass -H "$ACCEPT" "$R/swift-all/mona/LinkedList/1.1.0")"
echo "$doc" | grep -q '"description":"A linked list"' || fail "release metadata: $doc"
curl -fsS -u ci:smoke-pass -H "$ACCEPT" "$R/swift-all/identifiers?url=https://github.com/mona/LinkedList" | grep -q '"mona.LinkedList"' || fail "identifiers"
curl -fsS -u ci:smoke-pass -D - -o /dev/null -H "$ACCEPT" "$R/swift-all/mona/LinkedList/1.1.0/Package.swift" | grep -qi 'filename="Package@swift-5.10.swift"' || fail "the manifest's alternate Link"
curl -fsS -u ci:smoke-pass -H "$ACCEPT" "$R/swift-all/mona/LinkedList/1.1.0/Package.swift?swift-version=5.10" | grep -q 'swift-tools-version' || fail "the version-specific manifest"
test "$(curl -s -o /dev/null -w '%{http_code}' -u ci:smoke-pass -H 'Accept: application/vnd.swift.registry.v2+json' "$R/swift-all/mona/LinkedList")" = 406 || fail "another API version is not 406"

echo "== a URL dependency resolved from the registry: --replace-scm-with-registry"
mkapp /tmp/Scm '.package(url: "https://github.com/mona/LinkedList.git", from: "1.0.0")' "$PRODUCT" "$GREET" && cd /tmp/Scm
test "$(run --replace-scm-with-registry)" = "linked list 1.1.0" || fail "the app with a URL dependency"

echo "== login with an API token, publish a signed release"
rm -f "$HOME/.netrc"
t swift package-registry login "$R/swift-all/login" --token "$TOKEN" --no-confirm >/dev/null
grep -q '^machine registry.smoke.test login token ' "$HOME/.netrc" || fail ".netrc after a token login"
cat > /tmp/cert.cnf <<'CNF'
[req]
distinguished_name = dn
x509_extensions = v3
prompt = no
[dn]
CN = Smoke Publisher
[v3]
basicConstraints = critical,CA:TRUE
keyUsage = critical,digitalSignature,keyCertSign
extendedKeyUsage = codeSigning
CNF
openssl ecparam -name prime256v1 -genkey -noout -out /tmp/key.pem
openssl pkcs8 -topk8 -nocrypt -in /tmp/key.pem -outform DER -out /tmp/key.p8
openssl req -new -x509 -key /tmp/key.pem -days 30 -config /tmp/cert.cnf -outform DER -out /tmp/cert.cer
mklib 1.2.0 && cd /tmp/LinkedList
t swift package-registry publish mona.LinkedList 1.2.0 --url "$R/swift-all" --private-key-path /tmp/key.p8 --cert-chain-paths /tmp/cert.cer >/dev/null
curl -fsS -u ci:smoke-pass -H "$ACCEPT" "$R/swift-all/mona/LinkedList/1.2.0" | grep -q '"signatureFormat":"cms-1.0.0"' || fail "the release has no signature"
cd /tmp/App
t swift package update >/tmp/update.log 2>&1 || { cat /tmp/update.log; fail "package update to the signed release"; }
grep -q 'Smoke Publisher' /tmp/update.log || { cat /tmp/update.log; fail "SwiftPM did not read the signature"; }
test "$(run)" = "linked list 1.2.0" || fail "the app with the signed release"

echo "== PUT a zip that package archive-source made, then delete it"
mklib 1.3.0 && cd /tmp/LinkedList
t swift package archive-source --output /tmp/LinkedList-1.3.0.zip >/dev/null
test "$(curl -s -o /dev/null -w '%{http_code}' -u ci:smoke-pass -H 'Content-Type: application/zip' -T /tmp/LinkedList-1.3.0.zip "$R/swift-hosted/mona/LinkedList/1.3.0")" = 201 || fail "PUT of the zip"
cd /tmp/App
t swift package update >/dev/null 2>&1
test "$(run)" = "linked list 1.3.0" || fail "the app with the zip PUT with curl"
del="$(curl -fsS -u ci:smoke-pass -X DELETE -H 'X-Command-Center-UI: 1' 'http://commandcenter:8081/-/api/repositories/swift-hosted/components?name=mona.linkedlist&version=1.3.0')"
echo "$del" | grep -q '"assets":1' || fail "component delete: $del"
t swift package update >/dev/null 2>&1
test "$(run)" = "linked list 1.2.0" || fail "the app after the delete"

echo "== a package of the remote through the proxy in the group"
t swift package-registry login "$U/swift-hosted/login" --username ci --password smoke-pass --no-confirm >/dev/null
mkdir -p /tmp/Remote/Sources/Remote && cd /tmp/Remote
cat > Package.swift <<'PKG'
// swift-tools-version: 5.9
import PackageDescription

let package = Package(name: "Remote", products: [.library(name: "Remote", targets: ["Remote"])], targets: [.target(name: "Remote")])
PKG
echo 'public func remote() -> String { "from the remote" }' > Sources/Remote/Remote.swift
t swift package-registry publish octo.Remote 2.0.0 --url "$U/swift-hosted" >/dev/null
mkapp /tmp/Both "$LINKED, .package(id: \"octo.Remote\", exact: \"2.0.0\")" "$PRODUCT, .product(name: \"Remote\", package: \"octo.Remote\")" \
  'import LinkedList\nimport Remote\nprint(greet(), remote())'
cd /tmp/Both && fresh
test "$(run)" = "linked list 1.2.0 from the remote" || fail "the app with hosted and proxied packages"
curl -fsS -u ci:smoke-pass -H "$ACCEPT" "$R/swift-all/octo/Remote" | grep -q 'registry.smoke.test/repository/swift-all/octo/Remote/2.0.0' || fail "the proxy's release URLs"
test "$(curl -s -o /dev/null -w '%{http_code}' -u ci:smoke-pass -T /tmp/LinkedList-1.3.0.zip -H 'Content-Type: application/zip' "$R/swift-proxy/octo/Remote/2.0.1")" != 201 || fail "a PUT to the proxy"
echo "SWIFT CLIENT OK"
EOF

cat > "$work/cached.sh" <<'EOF'
set -eu
. /work/lib.sh
echo "== the remote stopped, the client's caches empty: from the proxy's cache"
cd /tmp/Both && fresh
test "$(run)" = "linked list 1.2.0 from the remote" || fail "the app from the proxy's cache"
echo "SWIFT CACHE OK"
EOF

docker network create "$net" >/dev/null
start_postgres
docker exec "$name-pg" createdb -U postgres upstream
docker run -d --name "$name" --network "$net" -e COMMANDCENTER_DATABASE_URL="$pg_url" --network-alias commandcenter --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" commandcenter:smoke-swift >/dev/null
docker run -d --name "$name-up" --network "$net" -e COMMANDCENTER_DATABASE_URL="${pg_url/\/cc\?/\/upstream?}" --network-alias upstream --memory 128m \
  -v "$root/$work/upstream.yaml:/etc/commandcenter/config.yaml:ro" commandcenter:smoke-swift >/dev/null
docker run -d --name "$name-tls" --network "$net" --network-alias registry.smoke.test --network-alias upstream.smoke.test --memory 128m \
  -v "$root/$work/Caddyfile:/etc/caddy/Caddyfile:ro" caddy:2-alpine >/dev/null

ready=0
for i in $(seq 1 30); do
  if docker exec "$name" commandcenter healthcheck >/dev/null 2>&1 && docker exec "$name-up" commandcenter healthcheck >/dev/null 2>&1 &&
    docker cp "$name-tls:/data/caddy/pki/authorities/local/root.crt" "$work/ca.crt" >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "FAIL: commandcenter did not become ready"; docker logs "$name"; docker logs "$name-up"; docker logs "$name-tls"; exit 1
fi

token="$(docker exec "$name" commandcenter token create -config /etc/commandcenter/config.yaml -user ci)"
docker run -d --name "$name-c" --network "$net" --memory 2g -e TOKEN="$token" -v "$root/$work:/work:ro" "${SMOKE_SWIFT_IMAGE:-swift:6.2}" sleep 3600 >/dev/null
docker exec "$name-c" bash /work/run.sh
docker rm -f -v "$name-up" >/dev/null
docker exec "$name-c" bash /work/cached.sh

echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "SWIFT SMOKE OK"
