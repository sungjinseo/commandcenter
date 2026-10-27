#!/usr/bin/env bash
# Opt-in (needs ~3 GB RAM and a few minutes): the import from a REAL Nexus Repository 3.
#   1. start a fresh sonatype/nexus3 container (own name and network, DISPOSABLE) and wait for it
#   2. create hosted maven/npm/pypi/cargo repositories, an npm proxy (with credentials) and an npm group
#   3. upload samples with the real clients: mvn deploy:deploy-file, npm publish (two versions and a
#      dist-tag), twine (a wheel) and the cargo publish API (two crates, one yanked)
#   4. start commandcenter, run `commandcenter import nexus --wait`, then run it again (idempotent)
#   5. verify through the real clients against commandcenter (mvn dependency:get, npm install,
#      pip download, cargo fetch) with sha256 equality against what Nexus serves, and the definitions
# Names carry the shell PID, so runs can overlap (the fixed host ports aside).
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-import-$$"
net=commandcenter-import-smoke-$$
nexus=commandcenter-import-nexus-$$
name=commandcenter-import-smoke-$$
img=commandcenter:smoke-import
nx=http://127.0.0.1:18097   # Nexus from the host
cc=http://127.0.0.1:18098   # commandcenter from the host
nxauth=admin:admin123
ccpass=smoke-pass

cleanup() {
  docker rm -f "$name" "$nexus" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
[ "${KEEP:-}" = 1 ] || trap cleanup EXIT
cleanup
rm -rf "$work" && mkdir -p "$work"
fail() { echo "FAIL: $*" >&2; exit 1; }
sha() { sha256sum "$1" | cut -d' ' -f1; }
# nxget <path> <file>: fetch from Nexus (basic auth) into a file.
nxget() { curl -fsS -u "$nxauth" -o "$2" "$nx$1"; }
nxapi() { # nxapi <method> <path> <json>
  curl -fsS -u "$nxauth" -H 'Content-Type: application/json' -X "$1" "$nx/service/rest/v1$2" ${3:+-d "$3"} >/dev/null
}

echo "== start a fresh Nexus Repository (a disposable test container)"
docker network create "$net" >/dev/null
# NEXUS_SECURITY_RANDOMPASSWORD=false makes the admin password admin123 (a throwaway container).
docker run -d --name "$nexus" --network "$net" --network-alias nexus \
  -e NEXUS_SECURITY_RANDOMPASSWORD=false \
  -e INSTALL4J_ADD_VM_PARAMS="-Xms512m -Xmx1200m -XX:MaxDirectMemorySize=1g" \
  -p 127.0.0.1:18097:8081 sonatype/nexus3:latest >/dev/null
deadline=$(( $(date +%s) + 600 ))
until curl -fs "$nx/service/rest/v1/status" >/dev/null 2>&1; do
  [ "$(date +%s)" -lt "$deadline" ] || { docker logs "$nexus" 2>&1 | tail -20; fail "Nexus did not become ready in 10 minutes"; }
  sleep 5
done
# Nexus 3.97 Community Edition requires the EULA to be accepted before any content access. This is a
# disposable test container, so the smoke test accepts it automatically.
# The disclaimer text must come back byte for byte (it has non-ASCII quotes), so go through files.
curl -fsS -u "$nxauth" -o "$work/eula.json" "$nx/service/rest/v1/system/eula"
sed -i 's/"accepted"[[:space:]]*:[[:space:]]*false/"accepted" : true/' "$work/eula.json"
curl -fsS -u "$nxauth" -H 'Content-Type: application/json' -X POST "$nx/service/rest/v1/system/eula" \
  --data-binary "@$work/eula.json" >/dev/null
# The writable status endpoint answers before everything is up: wait for the repositories API too.
until curl -fs -u "$nxauth" "$nx/service/rest/v1/repositories" >/dev/null 2>&1; do sleep 3; done

echo "== create the repositories"
st='"storage":{"blobStoreName":"default","strictContentTypeValidation":true,"writePolicy":"ALLOW"}'
for f in npm pypi cargo; do nxapi POST "/repositories/$f/hosted" "{\"name\":\"imp-$f\",\"online\":true,$st}"; done
nxapi POST /repositories/maven/hosted "{\"name\":\"imp-maven\",\"online\":true,$st,\"maven\":{\"versionPolicy\":\"RELEASE\",\"layoutPolicy\":\"STRICT\",\"contentDisposition\":\"INLINE\"}}"
nxapi POST /repositories/npm/proxy '{"name":"imp-npm-proxy","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":true},"proxy":{"remoteUrl":"https://registry.npmjs.org","contentMaxAge":1440,"metadataMaxAge":1440},"negativeCache":{"enabled":true,"timeToLive":1440},"httpClient":{"blocked":false,"autoBlock":true,"authentication":{"type":"username","username":"proxyuser","password":"proxypass"}}}'
nxapi POST /repositories/npm/group '{"name":"imp-npm-group","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":true},"group":{"memberNames":["imp-npm","imp-npm-proxy"]}}'

echo "== upload the samples with the real clients"
mkdir -p "$work/p1" "$work/p2" "$work/out"
echo 'module.exports = 1' | tee "$work/p1/index.js" > "$work/p2/index.js"
echo '{"name":"imp-demo","version":"1.0.0","main":"index.js"}' > "$work/p1/package.json"
echo '{"name":"imp-demo","version":"2.0.0-beta.1","main":"index.js"}' > "$work/p2/package.json"
cat > "$work/npm-up.sh" <<'EOF'
set -eu
cat > /root/.npmrc <<R
registry=http://nexus:8081/repository/imp-npm/
//nexus:8081/repository/imp-npm/:_auth=$(printf 'admin:admin123' | base64)
R
cd /work/p1 && npm publish >/dev/null 2>&1
cd /work/p2 && npm publish --tag beta >/dev/null 2>&1
npm view imp-demo dist-tags --json
EOF
docker run --rm --network "$net" -v "$root/$work:/work" node:22 sh /work/npm-up.sh

cat > "$work/mvn-up.sh" <<'EOF'
set -eu
mkdir -p /tmp/j && cd /tmp/j && echo hello > hello.txt && jar cf /tmp/imp-hello-1.0.0.jar hello.txt
cat > /tmp/s.xml <<S
<settings><servers><server><id>nx</id><username>admin</username><password>admin123</password></server></servers></settings>
S
mvn -B -q -s /tmp/s.xml deploy:deploy-file -Dfile=/tmp/imp-hello-1.0.0.jar -DgroupId=com.example -DartifactId=imp-hello \
  -Dversion=1.0.0 -Dpackaging=jar -DgeneratePom=true -Durl=http://nexus:8081/repository/imp-maven/ -DrepositoryId=nx
EOF
docker run --rm --network "$net" -v "$root/$work:/work" maven:3.9-eclipse-temurin-21 sh /work/mvn-up.sh

cat > "$work/prep.py" <<'EOF'
import io, json, struct, tarfile, zipfile, hashlib, base64, urllib.request, sys
nx = "http://nexus:8081"; auth = base64.b64encode(b"admin:admin123").decode()
def wheel():
    n = "imp_pkg-1.0.0"
    files = {
      "imp_pkg/__init__.py": b"VALUE = 1\n",
      f"{n}.dist-info/METADATA": b"Metadata-Version: 2.1\nName: imp-pkg\nVersion: 1.0.0\nSummary: import smoke\nRequires-Python: >=3.8\n",
      f"{n}.dist-info/WHEEL": b"Wheel-Version: 1.0\nGenerator: smoke\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
    }
    rec = ""
    for k, v in files.items():
        d = base64.urlsafe_b64encode(hashlib.sha256(v).digest()).rstrip(b"=").decode()
        rec += f"{k},sha256={d},{len(v)}\n"
    files[f"{n}.dist-info/RECORD"] = (rec + f"{n}.dist-info/RECORD,,\n").encode()
    with zipfile.ZipFile("/work/imp_pkg-1.0.0-py3-none-any.whl", "w", zipfile.ZIP_DEFLATED) as z:
        for k, v in files.items(): z.writestr(k, v)
def call(method, url, body=None):
    h = {"Authorization": "Basic " + auth, "Content-Type": "application/octet-stream"}
    print(method, url.rsplit("/repository/", 1)[1], urllib.request.urlopen(urllib.request.Request(url, data=body, method=method, headers=h)).status)
def crate(ver):
    name = "impcrate"
    toml = f'[package]\nname = "{name}"\nversion = "{ver}"\nedition = "2021"\ndescription = "import smoke"\nlicense = "MIT"\n'.encode()
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w:gz") as t:
        for p, b in ((f"{name}-{ver}/Cargo.toml", toml), (f"{name}-{ver}/src/lib.rs", b"pub fn value() -> u32 { 7 }\n")):
            ti = tarfile.TarInfo(p); ti.size = len(b); ti.mtime = 0; t.addfile(ti, io.BytesIO(b))
    data = buf.getvalue()
    meta = json.dumps({"name": name, "vers": ver, "deps": [], "features": {}, "authors": [], "description": "import smoke",
                       "license": "MIT", "readme": None, "keywords": [], "categories": [], "links": None}).encode()
    call("PUT", f"{nx}/repository/imp-cargo/api/v1/crates/new", struct.pack("<I", len(meta)) + meta + struct.pack("<I", len(data)) + data)
if sys.argv[1] == "wheel":
    wheel()
else:
    crate("1.0.0"); crate("1.1.0")
    call("DELETE", f"{nx}/repository/imp-cargo/api/v1/crates/impcrate/1.1.0/yank")
EOF
docker run --rm --network "$net" -v "$root/$work:/work" python:3.12-slim sh -c '
  set -e; pip install -q --root-user-action=ignore twine >/dev/null 2>&1
  python /work/prep.py wheel
  twine upload --non-interactive --disable-progress-bar --repository-url http://nexus:8081/repository/imp-pypi/ -u admin -p admin123 /work/imp_pkg-1.0.0-py3-none-any.whl
  python /work/prep.py crate'

echo "== start commandcenter (no repositories configured: the import creates them)"
docker build -q -t "$img" . >/dev/null
hash="$(printf '%s\n' "$ccpass" | docker run --rm -i "$img" hash-password)"
cat > "$work/config.yaml" <<EOF
anonymousRead: true
baseURL: "http://commandcenter:8081"
users:
  - { name: admin, passwordHash: "$hash", roles: [admin] }
EOF
docker run -d --name "$name" --network "$net" --network-alias commandcenter --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" -p 127.0.0.1:18098:8081 "$img" >/dev/null
ready=0
for i in $(seq 1 30); do
  if curl -fs "$cc/service/rest/v1/status" >/dev/null; then ready=1; break; fi
  sleep 1
done
[ "$ready" = 1 ] || { docker logs "$name" 2>&1 | tail -20; fail "commandcenter did not become ready"; }
token="$(docker exec "$name" commandcenter token create -config /etc/commandcenter/config.yaml -user admin)"

echo "== import from Nexus"
imp() {
  docker exec -e NEXUS_PASSWORD=admin123 "$name" commandcenter import nexus \
    --server http://localhost:8081 --token "$token" --from http://nexus:8081 --user admin --job smoke "$@"
}
imp --wait | tee "$work/import1.log"
grep -q 'imported 7 assets' "$work/import1.log" || fail "expected 7 imported assets (maven jar+pom, 2 npm, wheel, 2 crates)"
grep -q 'failed 0' "$work/import1.log" || fail "assets failed"
grep -q 'proxies needing credentials: imp-npm-proxy' "$work/import1.log" || fail "the proxy with credentials is not reported"
grep -q 'not imported: ' "$work/import1.log" || fail "the not-imported notes are missing"

echo "== the definitions exist"
repos="$(curl -fsS -H "Authorization: Bearer $token" "$cc/-/api/repositories")"
for r in imp-maven imp-npm imp-pypi imp-cargo imp-npm-proxy imp-npm-group; do
  grep -q "\"name\" *: *\"$r\"" <<<"$repos" || fail "repository $r was not created: $repos"
done
report="$(curl -fsS -H "Authorization: Bearer $token" "$cc/-/api/import/jobs/smoke")"
grep -q '"proxiesNeedingCredentials":\["imp-npm-proxy"\]' <<<"$report" || fail "report: proxy credentials: $report"
curl -fsS -H "Authorization: Bearer $token" "$cc/-/api/repositories/imp-npm-proxy" | grep -q "https://registry.npmjs.org" || fail "the proxy remote URL was not kept"

echo "== run the same job again: nothing is downloaded"
imp --wait | tee "$work/import2.log"
grep -q 'imported 0 assets' "$work/import2.log" || fail "a re-run imported assets"

echo "== maven: dependency:get through commandcenter"
# Maven 3.8+ blocks plain-http repositories unless a mirror entry names them.
cat > "$work/s2.xml" <<'EOF'
<settings><mirrors><mirror><id>cc-mirror</id><mirrorOf>cc</mirrorOf><url>http://commandcenter:8081/repository/imp-maven/</url></mirror></mirrors></settings>
EOF
cat > "$work/mvn-get.sh" <<'EOF'
set -eu
mvn -B -q -s /work/s2.xml -Dmaven.repo.local=/work/m2 dependency:get -Dartifact=com.example:imp-hello:1.0.0 \
  -DremoteRepositories=cc::default::http://commandcenter:8081/repository/imp-maven/
EOF
docker run --rm --network "$net" -v "$root/$work:/work" maven:3.9-eclipse-temurin-21 sh /work/mvn-get.sh
nxget /repository/imp-maven/com/example/imp-hello/1.0.0/imp-hello-1.0.0.jar "$work/out/nx.jar"
[ "$(sha "$work/m2/com/example/imp-hello/1.0.0/imp-hello-1.0.0.jar")" = "$(sha "$work/out/nx.jar")" ] || fail "maven jar sha256 differs"
curl -fs "$cc/repository/imp-maven/com/example/imp-hello/maven-metadata.xml" | grep -q '<version>1.0.0</version>' || fail "maven-metadata.xml was not rebuilt"
curl -fs "$cc/repository/imp-maven/com/example/imp-hello/1.0.0/imp-hello-1.0.0.pom.sha1" >/dev/null || fail "no pom sha1"

echo "== npm: install (the dist-tag resolves) and pack"
cat > "$work/npm-get.sh" <<'EOF'
set -eu
reg=http://commandcenter:8081/repository/imp-npm/
mkdir -p /work/npm-app && cd /work/npm-app && npm init -y >/dev/null
npm install --registry "$reg" imp-demo@beta >/dev/null
v="$(node -p "require('imp-demo/package.json').version")"
[ "$v" = 2.0.0-beta.1 ] || { echo "beta resolved to $v"; exit 1; }
tags="$(npm view --registry "$reg" imp-demo dist-tags --json | tr -d ' \n')"
[ "$tags" = '{"latest":"1.0.0","beta":"2.0.0-beta.1"}' ] || [ "$tags" = '{"beta":"2.0.0-beta.1","latest":"1.0.0"}' ] || { echo "dist-tags: $tags"; exit 1; }
npm install --registry "$reg" imp-demo@latest >/dev/null
[ "$(node -p "require('imp-demo/package.json').version")" = 1.0.0 ] || { echo "latest is not 1.0.0"; exit 1; }
cd /work/out && npm pack --registry "$reg" imp-demo@1.0.0 >/dev/null
# the same through the imported group (proxy member unreachable-credentials must not matter for hosted hits)
cd /work/npm-app && npm install --registry http://commandcenter:8081/repository/imp-npm-group/ imp-demo@1.0.0 >/dev/null
EOF
docker run --rm --network "$net" -v "$root/$work:/work" node:22 sh /work/npm-get.sh
nxget /repository/imp-npm/imp-demo/-/imp-demo-1.0.0.tgz "$work/out/nx.tgz"
[ "$(sha "$work/out/imp-demo-1.0.0.tgz")" = "$(sha "$work/out/nx.tgz")" ] || fail "npm tarball sha256 differs"

echo "== pypi: pip download"
docker run --rm --network "$net" -v "$root/$work:/work" python:3.12-slim sh -c '
  PIP_ROOT_USER_ACTION=ignore pip download -q --no-deps --trusted-host commandcenter \
    --index-url http://commandcenter:8081/repository/imp-pypi/simple/ imp-pkg==1.0.0 -d /work/out/pip'
nxget /repository/imp-pypi/packages/imp-pkg/1.0.0/imp_pkg-1.0.0-py3-none-any.whl "$work/out/nx.whl"
[ "$(sha "$work/out/pip/imp_pkg-1.0.0-py3-none-any.whl")" = "$(sha "$work/out/nx.whl")" ] || fail "wheel sha256 differs"

echo "== cargo: fetch (the yanked 1.1.0 is not selected) and the index"
mkdir -p "$work/capp/src" && echo 'fn main() {}' > "$work/capp/src/main.rs"
cat > "$work/capp/Cargo.toml" <<'EOF'
[package]
name = "capp"
version = "0.1.0"
edition = "2021"
[dependencies]
impcrate = { version = "1", registry = "commandcenter" }
EOF
cat > "$work/cargo-get.sh" <<'EOF'
set -eu
export CARGO_HOME=/work/cargo-home
mkdir -p "$CARGO_HOME"
cat > "$CARGO_HOME/config.toml" <<'T'
[registries.commandcenter]
index = "sparse+http://commandcenter:8081/repository/imp-cargo/"
T
cd /work/capp && cargo fetch
grep -A1 'name = "impcrate"' Cargo.lock | grep -q 'version = "1.0.0"' || { cat Cargo.lock; echo "the yanked 1.1.0 was selected"; exit 1; }
EOF
docker run --rm --network "$net" -v "$root/$work:/work" rust:1-slim sh /work/cargo-get.sh
crate="$(find "$work/cargo-home/registry/cache" -name 'impcrate-1.0.0.crate' | head -1)"
[ -n "$crate" ] || fail "cargo fetched no crate"
nxget /repository/imp-cargo/crates/impcrate/1.0.0/download "$work/out/nx.crate"
[ "$(sha "$crate")" = "$(sha "$work/out/nx.crate")" ] || fail "crate sha256 differs"
curl -fs "$cc/repository/imp-cargo/im/pc/impcrate" | grep '"vers":"1.1.0"' | grep -q '"yanked":true' || fail "the yank was not preserved"

echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "IMPORT SMOKE OK"
