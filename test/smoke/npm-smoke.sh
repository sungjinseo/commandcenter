#!/usr/bin/env bash
# Builds the image, starts commandcenter, then uses real npm (node:22) to:
#   1. start with a stale token, run a real `npm login` (legacy flow), check whoami
#   2. publish @smoke/hello through the npm-all group (lands in npm-hosted)
#   3. reject a republish of the same version
#   4. install it from an empty cache; its dependency comes through npm-proxy
#   5. add and list a dist-tag, and view the package
#   6. deprecate through the group; unpublish fails (405); audit is 501
#   7. npm logout revokes the token
# Finally prints the container's memory usage.
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-npm-$$"
net=commandcenter-npm-smoke-$$
name=commandcenter-npm-smoke-$$

cleanup() {
  docker rm -f "$name" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup
rm -rf "$work" && mkdir -p "$work"
cp -r test/smoke/npm/pkg "$work/pkg"

docker build -q -t commandcenter:smoke . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i commandcenter:smoke hash-password)"

cat > "$work/config.yaml" <<EOF
anonymousRead: true
baseURL: "http://commandcenter:8081"
users:
  - { name: ci, passwordHash: "$hash", roles: [deployer] }
roles:
  deployer: { write: [npm-hosted] }
repositories:
  - { name: npm-proxy,  format: npm, type: proxy,  remote: "https://registry.npmjs.org/" }
  - { name: npm-hosted, format: npm, type: hosted }
  - { name: npm-all,    format: npm, type: group,  members: [npm-hosted, npm-proxy], writeMember: npm-hosted }
EOF

cat > "$work/run.sh" <<'EOF'
set -eu
REG=http://commandcenter:8081/repository/npm-all/
export npm_config_userconfig=/work/.npmrc
export npm_config_cache=/work/npm-cache
# A stale token is present first: a real npm login must replace it.
printf 'registry=%s\n//commandcenter:8081/repository/npm-all/:_authToken=cc_stale\n' "$REG" > /work/.npmrc

echo "== stale token is rejected"
test "$(curl -s -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer cc_stale' "${REG}-/whoami")" = 401

echo "== npm login (real client, legacy flow, under a pty)"
# npm refuses piped stdin ("Exit handler never called"), so the login runs under a
# pty via util-linux `script`; the answers are fed with pauses so that each prompt
# exists before its answer arrives (typing ahead hangs).
(sleep 2; printf 'ci\n'; sleep 2; printf 'smoke-pass\n'; sleep 1) | timeout 60 script -qec "npm login --registry $REG --auth-type=legacy" /dev/null
! grep -q cc_stale /work/.npmrc

echo "== whoami"
test "$(npm whoami)" = ci

echo "== publish through the group"
cd /work/pkg && npm publish --loglevel=error

echo "== republish must be rejected"
if out=$(npm publish 2>&1); then echo "FAIL: republish succeeded"; exit 1; fi
echo "$out" | grep -q E403 || { echo "FAIL: republish error was not E403:"; echo "$out"; exit 1; }

echo "== install (dependency comes through the proxy)"
mkdir -p /work/app && cd /work/app && npm init -y >/dev/null
npm install --loglevel=error @smoke/hello
node -e 'if (require("@smoke/hello").greet() !== "hello 7") process.exit(1)'

echo "== dist-tag and view"
npm dist-tag add @smoke/hello@1.0.0 beta >/dev/null
npm dist-tag ls @smoke/hello | grep -q 'beta: 1.0.0'
test "$(npm view @smoke/hello version)" = 1.0.0

echo "== deprecate through the group (?write=true reads the writeMember only)"
npm deprecate @smoke/hello@1.0.0 "smoke deprecation" --loglevel=error
npm view @smoke/hello@1.0.0 deprecated | grep -q 'smoke deprecation'

echo "== unpublish must fail visibly and delete nothing"
if out=$(npm unpublish @smoke/hello@1.0.0 --force 2>&1); then echo "FAIL: unpublish reported success:"; echo "$out"; exit 1; fi
echo "$out" | grep -q E405 || { echo "FAIL: unpublish error was not E405:"; echo "$out"; exit 1; }
test "$(npm view @smoke/hello version)" = 1.0.0

echo "== audit is reported as unsupported"
test "$(curl -s -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' -d '{"express":["4.0.0"]}' "${REG}-/npm/v1/security/advisories/bulk")" = 501

echo "== npm logout revokes the token"
tok="$(sed -n 's/.*:_authToken=//p' /work/.npmrc)"
test -n "$tok"
test "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $tok" "${REG}-/whoami")" = 200
npm logout --registry "$REG"
test "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $tok" "${REG}-/whoami")" = 401
echo "NPM CLIENT OK"
EOF

docker network create "$net" >/dev/null
docker run -d --name "$name" --network "$net" --network-alias commandcenter --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" -p 18082:8081 commandcenter:smoke >/dev/null

ready=0
for i in $(seq 1 30); do
  if curl -fs http://localhost:18082/service/rest/v1/status >/dev/null; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "FAIL: commandcenter did not become ready within 30s" >&2
  docker logs "$name" >&2 || true
  exit 1
fi

docker run --rm --network "$net" -v "$root/$work:/work" node:22 sh /work/run.sh

echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "NPM SMOKE OK"
