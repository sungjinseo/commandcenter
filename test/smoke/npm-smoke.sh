#!/usr/bin/env bash
# Builds the image, starts commandcenter, then uses the real clients (node:22:
# npm and yarn classic from the image; yarn berry and pnpm in pinned versions
# that corepack fetches through the group), with a fake remote in their
# container for the npm-odd proxy, to:
#   1. start with a stale token, run a real `npm login` (legacy flow, and the
#      default flow, which tries web login and must fall back), check whoami
#   2. publish @smoke/hello through the npm-all group (lands in npm-hosted)
#   3. reject a republish of the same version (writePolicy allow_once)
#   4. install it from an empty cache with npm, yarn classic, yarn berry and
#      pnpm, each again from its lockfile; its dependency comes through
#      npm-proxy, and every resolved URL is the group's
#   5. publish through the group with pnpm, yarn classic and yarn berry
#   6. writePolicy: allow takes the version again and serves the new bytes;
#      deny refuses publish, dist-tag, deprecate and unpublish
#   7. a proxy (npm-odd, fake remote) fetches tarballs where the document says
#      (GitHub Packages and GitLab layouts), caches no web page as a document
#      or a tarball, and keeps cached versions the remote dropped
#   8. a group with mergeMembers answers with every member's versions; one
#      without answers with the first member that has the package
#   9. npm search on hosted, proxy (the public registry's) and group
#  10. REST: every asset's download URL answers, a tarball's npm attributes,
#      the component upload API, DELETE without a revision, conditional
#      requests, a real npm audit, and Nexus's repository.npm.reindex task
#  11. publish --tag and dist-tag add, ls and rm, view; deprecate through the
#      group warns at install and "" clears it; unpublish one version, then the
#      whole package; audit goes to npm-proxy's remote, npm-hosted answers {}
#  12. npm logout revokes the token
# Finally prints the container's memory usage.
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-npm-$$"
net=commandcenter-npm-smoke-$$
name=commandcenter-npm-smoke-$$
. test/postgres.sh

cleanup() {
  docker rm -f -v "$name" "$name-pg" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap 'docker rm -f "$name-client" >/dev/null 2>&1 || true; cleanup' EXIT
cleanup
rm -rf "$work" && mkdir -p "$work"
cp -r test/smoke/npm/pkg test/smoke/npm/up.js test/smoke/npm/upstream.js "$work/"

docker build -q -t commandcenter:smoke-npm . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i commandcenter:smoke-npm hash-password)"

cat > "$work/config.yaml" <<EOF
anonymousRead: true
baseURL: "http://commandcenter:8081"
users:
  - { name: ci, passwordHash: "$hash", roles: [deployer] }
  - { name: root, passwordHash: "$hash", roles: [admin] }
roles:
  deployer: { write: [npm-hosted, npm-redeploy, npm-frozen, npm-fork] }
repositories:
  - { name: npm-proxy,    format: npm, type: proxy,  remote: "https://registry.npmjs.org/" }
  - { name: npm-hosted,   format: npm, type: hosted }
  - { name: npm-all,      format: npm, type: group,  members: [npm-hosted, npm-proxy], writeMember: npm-hosted }
  - { name: npm-redeploy, format: npm, type: hosted, writePolicy: allow }
  - { name: npm-frozen,   format: npm, type: hosted }
  - { name: npm-odd,      format: npm, type: proxy,  remote: "http://upstream:8080/", metadataMaxAge: 2s }
  - { name: npm-fork,     format: npm, type: hosted }
  - { name: npm-first,    format: npm, type: group,  members: [npm-fork, npm-odd] }
  - { name: npm-merged,   format: npm, type: group,  members: [npm-fork, npm-odd], mergeMembers: true }
EOF

cat > "$work/run.sh" <<'EOF'
set -eu
BERRY=4.18.1 PNPM=12.16.1
BASE=http://commandcenter:8081
C=$BASE/repository
REG=$C/npm-all/
HOSTED=$C/npm-hosted/
ODD=$C/npm-odd/
API=$BASE/service/rest/v1
# The work directory is mounted read-only: everything is made under /tmp.
export HOME=/tmp/home npm_config_userconfig=/tmp/.npmrc npm_config_cache=/tmp/npm-cache npm_config_update_notifier=false
export COREPACK_NPM_REGISTRY=${REG%/} COREPACK_ENABLE_DOWNLOAD_PROMPT=0 COREPACK_HOME=/tmp/corepack
mkdir -p "$HOME"
cp -r /work/pkg /tmp/pkg
node /work/upstream.js & # the remote of npm-odd: this container, by its alias
berry() { corepack "yarn@$BERRY" "$@"; }
pnpm() { corepack "pnpm@$PNPM" "$@"; }
up() { node /work/up.js "$@"; }
fail() { echo "FAIL: $*"; exit 1; }
for _ in $(seq 50); do up mode normal 2>/dev/null && break; sleep 0.2; done
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
# refused <text> <command...>: the command fails and says text.
refused() {
  want=$1; shift
  if out=$("$@" 2>&1); then fail "succeeded: $*"; fi
  echo "$out" | grep -q -- "$want" || { echo "$out"; fail "not refused with $want: $*"; }
}
# project: a new project directory, $P, as the current directory.
project() { P="$(mktemp -d /tmp/p.XXXXXX)"; cd "$P"; npm init -y >/dev/null; }
# install <registry> <spec...>: npm install in a new project, with an empty npm cache.
install() { reg=$1; shift; project; npm install "$@" --registry "$reg" --cache "$P/.cache" --no-audit --no-fund --loglevel=error >/dev/null; }
# versions <package> <registry>: the versions it lists, sorted, with an empty npm cache.
versions() { npm view "$1" versions --json --registry "$2" --cache "$(mktemp -d)" | tr -d ' \n[]"' | tr , '\n' | sort | tr '\n' ' '; }
latest() { npm view "$1" dist-tags.latest --registry "$2" --cache "$(mktemp -d)"; }
greets() { node -e 'if (require("@smoke/hello").greet() !== "hello 7") process.exit(1)'; }
# login <registry> [flag...]: a real npm login under a pty: npm refuses piped
# stdin ("Exit handler never called"), and the answers are fed with pauses so
# that each prompt exists before its answer arrives (typing ahead hangs).
login() {
  reg=$1; shift
  (sleep 3; printf 'ci\n'; sleep 2; printf 'smoke-pass\n'; sleep 2) | timeout 60 script -qec "npm login --registry $reg $*" /dev/null >/dev/null
}

echo "clients: node $(node --version), npm $(npm --version), yarn $(yarn --version), yarn $(berry --version), pnpm $(pnpm --version)"

# A stale token is present first: a real npm login must replace it.
printf 'registry=%s\n//commandcenter:8081/repository/npm-all/:_authToken=cc_stale\n' "$REG" > /tmp/.npmrc

echo "== stale token is rejected"
test "$(code -H 'Authorization: Bearer cc_stale' "${REG}-/whoami")" = 401

echo "== npm login, legacy flow, replaces it"
login "$REG" --auth-type=legacy
! grep -q cc_stale /tmp/.npmrc

echo "== whoami"
test "$(npm whoami)" = ci

echo "== npm login, default flow: web login is declined, npm falls back"
login "$HOSTED"
test "$(npm whoami --registry "$HOSTED")" = ci
tok="$(sed -n 's/.*repository\/npm-all\/:_authToken=//p' /tmp/.npmrc)"
test -n "$tok"
for r in npm-redeploy npm-frozen npm-fork; do printf '//commandcenter:8081/repository/%s/:_authToken=%s\n' "$r" "$tok" >> /tmp/.npmrc; done

echo "== publish through the group"
cd /tmp/pkg && npm publish --loglevel=error

echo "== republish must be rejected (writePolicy allow_once)"
refused E403 npm publish

echo "== npm install (dependency comes through the proxy)"
project
npm install --loglevel=error --no-fund @smoke/hello >/dev/null
greets
grep -q "\"resolved\": \"${REG}is-number/-/is-number-7.0.0.tgz\"" package-lock.json || fail "npm: the dependency did not resolve through the group"
rm -rf node_modules
npm ci --cache "$P/.cache" --no-audit --no-fund --loglevel=error >/dev/null
greets

echo "== yarn classic: add through the group, then from the lockfile with an empty cache"
project
yarn add @smoke/hello --registry "$REG" --cache-folder "$P/.cache" --silent --no-progress
greets
grep -q "resolved \"${REG}@smoke/hello/-/hello-1.0.0.tgz" yarn.lock || fail "yarn: the package did not resolve through the group"
grep -q "resolved \"${REG}is-number/-/is-number-7.0.0.tgz" yarn.lock || fail "yarn: the dependency did not resolve through the group"
rm -rf node_modules "$P/.cache"
yarn install --frozen-lockfile --registry "$REG" --cache-folder "$P/.cache" --silent --no-progress
greets

# Yarn 4 by default quarantines a version younger than a day by the document's
# time (npmMinimalAgeGate), which the package published above is.
echo "== yarn berry: add through the group, then from the lockfile with an empty cache"
berryrc() {
  cat > .yarnrc.yml <<Y
npmRegistryServer: "$1"
unsafeHttpWhitelist: ["commandcenter"]
nodeLinker: node-modules
enableTelemetry: false
enableGlobalCache: false
npmPreapprovedPackages: ["@smoke/*"]
${2:+npmAuthToken: "$2"}
Y
}
project
berryrc "$REG"
berry add @smoke/hello >/dev/null
greets
grep -q '"@smoke/hello@npm:1.0.0"' yarn.lock || fail "berry: no @smoke/hello in yarn.lock"
rm -rf node_modules .yarn/cache
berry install --immutable >/dev/null
greets

echo "== pnpm: add through the group, then from the lockfile with an empty store"
project
printf 'registry=%s\n' "$REG" > .npmrc
pnpm add @smoke/hello --store-dir "$P/.store" >/dev/null
greets
rm -rf node_modules "$P/.store"
pnpm install --frozen-lockfile --store-dir "$P/.store" >/dev/null
greets

echo "== pnpm, yarn classic and yarn berry publish through the group"
project
printf '{"name":"@smoke/others","version":"1.0.0","main":"index.js"}\n' > package.json
echo 'exports.by = "others";' > index.js
cp /tmp/.npmrc .npmrc
pnpm publish --no-git-checks >/dev/null
yarn publish --new-version 1.1.0 --non-interactive --no-git-tag-version --registry "$REG" --silent --no-progress >/dev/null
npm version 1.2.0 --no-git-tag-version >/dev/null
berryrc "$REG" "$tok"
touch yarn.lock
berry install >/dev/null
berry npm publish >/dev/null
test "$(versions @smoke/others "$REG")" = "1.0.0 1.1.0 1.2.0 " || fail "published by the three clients: $(versions @smoke/others "$REG")"
project
printf 'registry=%s\n' "$REG" > .npmrc
pnpm add @smoke/others --store-dir "$P/.store" >/dev/null
test "$(node -p 'require("@smoke/others/package.json").version')" = 1.2.0

echo "== writePolicy allow: a version published again is replaced; deny: every write is refused"
R=$C/npm-redeploy/
project
printf '{"name":"@smoke/re","version":"1.0.0","main":"index.js","description":"redeployed","license":"MIT"}\n' > package.json
echo 'exports.v = 1;' > index.js
npm publish --registry "$R" --loglevel=error >/dev/null
first="$(npm view @smoke/re dist.shasum --registry "$R")"
echo 'exports.v = 2;' > index.js
npm publish --registry "$R" --loglevel=error >/dev/null
test "$(npm view @smoke/re dist.shasum --registry "$R" --cache "$(mktemp -d)")" != "$first" || fail "the second publish did not replace dist.shasum"
install "$R" @smoke/re
test "$(node -p 'require("@smoke/re").v')" = 2 || fail "a clean install got the first file"
test "$(npm view @smoke/re _npmUser.name --registry "$R")" = ci || fail "_npmUser is not who published"
curl -fsS "${R}@smoke%2fre" | node -e 'const d = JSON.parse(require("fs").readFileSync(0)); if (d.description !== "redeployed" || d.license !== "MIT") process.exit(1)' || fail "the document's top lacks the latest version's fields"

F=$C/npm-frozen/
project
printf '{"name":"@smoke/ro","version":"1.0.0"}\n' > package.json
npm publish --registry "$F" --loglevel=error >/dev/null
test "$(code -u root:smoke-pass -X PUT -H 'Content-Type: application/json' \
  -d '{"name":"npm-frozen","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":false,"writePolicy":"DENY"}}' \
  "$API/repositories/npm/hosted/npm-frozen")" = 204
npm version 1.0.1 --no-git-tag-version >/dev/null
refused 'is read-only' npm publish --registry "$F"
refused 'is read-only' npm dist-tag add @smoke/ro@1.0.0 beta --registry "$F"
refused 'is read-only' npm dist-tag rm @smoke/ro latest --registry "$F"
refused 'is read-only' npm deprecate @smoke/ro@1.0.0 old --registry "$F"
refused 'is read-only' npm unpublish @smoke/ro --force --registry "$F"
test "$(versions @smoke/ro "$F")" = "1.0.0 " || fail "a read-only repository changed"

echo "== a proxy fetches a tarball from where the remote's document says"
up add @odd/hub 1.0.0 /download/@odd/hub/1.0.0/0123abcdef
up add @odd/lab 1.0.0 /api/packages/npm/@odd/lab/-/@odd/lab-1.0.0.tgz
install "$ODD" @odd/hub
test "$(node -p 'require("@odd/hub").v')" = @odd/hub@1.0.0
grep -q "\"resolved\": \"${ODD}@odd/hub/-/hub-1.0.0.tgz\"" package-lock.json || fail "the hash-named tarball's URL: $(grep resolved package-lock.json)"
install "$ODD" @odd/lab
test "$(node -p 'require("@odd/lab").v')" = @odd/lab@1.0.0
install "$ODD" @odd/hub @odd/lab # from the cache now
test "$(up asked /download/@odd/hub/1.0.0/0123abcdef)" = 1 || fail "the tarball was fetched more than once"
test "$(up asked /api/packages/npm/@odd/lab/-/@odd/lab-1.0.0.tgz)" = 1 || fail "the tarball was fetched more than once"
test "$(code "${ODD}@odd/hub/-/0123abcdef")" = 200 || fail "the hash a lockfile from Nexus holds is not answered"
project
printf 'registry=%s\n' "$ODD" > .npmrc
pnpm add @odd/hub --store-dir "$P/.store" >/dev/null
test "$(node -p 'require("@odd/hub").v')" = @odd/hub@1.0.0
project
yarn add @odd/lab --registry "$ODD" --cache-folder "$P/.cache" --silent --no-progress
test "$(node -p 'require("@odd/lab").v')" = @odd/lab@1.0.0

echo "== a web page is cached neither as a document nor as a tarball"
up add @odd/page 1.0.0 /@odd/page/-/page-1.0.0.tgz
up mode html
refused E502 install "$ODD" @odd/page
up mode normal
install "$ODD" @odd/page
up add @odd/page2 1.0.0 /@odd/page2/-/page2-1.0.0.tgz
test "$(npm view @odd/page2 version --registry "$ODD")" = 1.0.0
up mode html
refused E502 install "$ODD" @odd/page2
up mode normal
install "$ODD" @odd/page2
test "$(node -p 'require("@odd/page2").v')" = @odd/page2@1.0.0

echo "== a proxy keeps the versions the remote dropped while their tarballs are cached"
up add @odd/keep 1.0.0 /@odd/keep/-/keep-1.0.0.tgz
up add @odd/keep 1.1.0 /@odd/keep/-/keep-1.1.0.tgz
up add @odd/keep 1.2.0 /@odd/keep/-/keep-1.2.0.tgz
install "$ODD" @odd/keep@1.1.0 # 1.2.0 was never fetched
up drop @odd/keep 1.1.0 1.2.0
sleep 3
test "$(versions @odd/keep "$ODD")" = "1.0.0 1.1.0 " || fail "after the remote dropped two: $(versions @odd/keep "$ODD")"
test "$(latest @odd/keep "$ODD")" = 1.0.0
install "$ODD" @odd/keep@1.1.0
test "$(node -p 'require("@odd/keep").v')" = @odd/keep@1.1.0
refused ETARGET install "$ODD" @odd/keep@1.2.0
up stub @odd/keep
sleep 3
test "$(versions @odd/keep "$ODD")" = "1.1.0 " || fail "after the remote unpublished the package: $(versions @odd/keep "$ODD")"
test "$(latest @odd/keep "$ODD")" = 1.1.0
install "$ODD" @odd/keep
up gone @odd/keep
sleep 3
test "$(versions @odd/keep "$ODD")" = "1.1.0 " || fail "after the remote dropped the package: $(versions @odd/keep "$ODD")"
install "$ODD" @odd/keep@1.1.0
test "$(node -p 'require("@odd/keep").v')" = @odd/keep@1.1.0
up mode fail # auto-block counts failures: this stays the last request of this proxy to fail
sleep 3
install "$ODD" @odd/keep@1.1.0
up mode normal

echo "== mergeMembers: the group answers with every member; without it, with the first that has the package"
project
printf '{"name":"@odd/both","version":"1.0.0-acme.1","main":"index.js"}\n' > package.json
echo 'exports.v = "acme";' > index.js
npm publish --registry "$C/npm-fork/" --tag latest --loglevel=error >/dev/null
up add @odd/both 1.0.0 /@odd/both/-/both-1.0.0.tgz
up add @odd/both 1.1.0 /@odd/both/-/both-1.1.0.tgz
FIRST=$C/npm-first/ MERGED=$C/npm-merged/
test "$(versions @odd/both "$FIRST")" = "1.0.0-acme.1 " || fail "first member: $(versions @odd/both "$FIRST")"
refused ETARGET install "$FIRST" @odd/both@1.1.0
test "$(code "${FIRST}@odd/both/-/both-1.1.0.tgz")" = 404 || fail "a later member's tarball through the first-member group"
test "$(versions @odd/both "$MERGED")" = "1.0.0 1.0.0-acme.1 1.1.0 " || fail "merged: $(versions @odd/both "$MERGED")"
test "$(latest @odd/both "$MERGED")" = 1.1.0
install "$MERGED" @odd/both
test "$(node -p 'require("@odd/both").v')" = @odd/both@1.1.0
grep -q "\"resolved\": \"${MERGED}@odd/both/-/both-1.1.0.tgz\"" package-lock.json || fail "the merged document's tarball URL: $(grep resolved package-lock.json)"
install "$MERGED" @odd/both@1.0.0-acme.1
test "$(node -p 'require("@odd/both").v')" = acme
project
yarn add @odd/both@1.1.0 --registry "$MERGED" --cache-folder "$P/.cache" --silent --no-progress
test "$(node -p 'require("@odd/both").v')" = @odd/both@1.1.0
project
printf 'registry=%s\n' "$MERGED" > .npmrc
pnpm add @odd/both@1.1.0 --store-dir "$P/.store" >/dev/null
test "$(node -p 'require("@odd/both").v')" = @odd/both@1.1.0
project
berryrc "$MERGED"
berry add @odd/both@1.1.0 >/dev/null
test "$(node -p 'require("@odd/both").v')" = @odd/both@1.1.0

echo "== npm search: hosted, the public registry through the proxy, and the group"
searched() { node -e 'for (const p of JSON.parse(require("fs").readFileSync(0))) console.log(p.name)'; }
npm search smoke --registry "$HOSTED" > /tmp/search.txt
grep -q '@smoke/hello' /tmp/search.txt || { cat /tmp/search.txt; fail "npm search on hosted"; }
test "$(npm search --json others --registry "$HOSTED" | searched)" = @smoke/others || fail "npm search --json on hosted"
npm search --json express --registry "$C/npm-proxy/" > /tmp/search.json
searched < /tmp/search.json | grep -qx express || fail "npm search through the proxy"
npm search --json hello --registry "$REG" > /tmp/search.json
searched < /tmp/search.json | sed -n 1p | grep -qx @smoke/hello || fail "npm search through the group: the hosted hit comes first"
test "$(searched < /tmp/search.json | wc -l)" -gt 1 || fail "npm search through the group: no hit of the proxy"
test "$(npm search --json nothing-is-called-this-8f3a --registry "$HOSTED" | searched | wc -l)" = 0

echo "== REST: every download URL answers, a tarball's npm attributes, a reindex"
curl -fsS "$API/assets?repository=npm-hosted" > /tmp/assets.json
node -e 'for (const i of JSON.parse(require("fs").readFileSync("/tmp/assets.json")).items) console.log(i.downloadUrl)' |
  while read -r u; do curl -fsS -o /dev/null "$u" || fail "asset download URL $u"; done
node -e '
  const items = JSON.parse(require("fs").readFileSync("/tmp/assets.json")).items;
  const t = items.find((i) => i.path === "/@smoke/hello/-/hello-1.0.0.tgz");
  if (!t || t.npm.name !== "@smoke/hello" || t.npm.version !== "1.0.0") process.exit(1);
  if (!items.some((i) => i.path === "/@smoke/hello")) process.exit(1);' || fail "/v1/assets: $(cat /tmp/assets.json)"
curl -fsS -u root:smoke-pass -H 'Content-Type: application/json' -d \
  '{"type":"repository.npm.reindex","name":"reindex","frequency":{"schedule":"manual"},"properties":{"repositoryName":"npm-hosted"}}' "$API/tasks" > /tmp/task.json
id="$(node -p 'JSON.parse(require("fs").readFileSync("/tmp/task.json")).id')"
test "$(code -u root:smoke-pass -X POST "$API/tasks/$id/run")" = 204
for _ in $(seq 30); do
  result="$(curl -fsS -u root:smoke-pass "$API/tasks/$id" | node -p 'JSON.parse(require("fs").readFileSync(0)).lastRunResult')"
  [ "$result" = null ] || break
  sleep 1
done
test "$result" = OK || fail "repository.npm.reindex: $result"
test "$(versions @smoke/hello "$HOSTED")" = "1.0.0 "

echo "== the component upload API: npm pack, curl, install; deny refuses it too"
project
printf '{"name":"@smoke/up","version":"1.2.0","main":"index.js"}\n' > package.json
echo 'exports.v = "up";' > index.js
npm pack --silent >/dev/null
test "$(code -u ci:smoke-pass -F npm.asset=@smoke-up-1.2.0.tgz "$API/components?repository=npm-hosted")" = 204
test "$(code -u ci:smoke-pass -F npm.asset=@smoke-up-1.2.0.tgz "$API/components?repository=npm-frozen")" = 400
install "$REG" @smoke/up
test "$(node -p 'require("@smoke/up").v')" = up
grep -q '"integrity": "sha512-' package-lock.json || fail "no integrity for an uploaded version"

echo "== conditional requests on documents; DELETE without a revision"
etag="$(curl -fsS -D- -o /dev/null "${HOSTED}@smoke%2fhello" | tr -d '\r' | sed -n 's/^[Ee][Tt]ag: //p')"
test -n "$etag"
test "$(code -H "If-None-Match: $etag" "${HOSTED}@smoke%2fhello")" = 304
npm view @smoke/hello version --registry "$HOSTED" --cache /tmp/warm >/dev/null
npm view @smoke/hello version --registry "$HOSTED" --cache /tmp/warm --loglevel=http > /tmp/warm.log 2>&1
grep -q 'cache revalidated' /tmp/warm.log || { cat /tmp/warm.log; fail "a warm npm cache was not revalidated"; }
project
printf '{"name":"@smoke/gone","version":"1.0.0"}\n' > package.json
npm publish --registry "$R" --loglevel=error >/dev/null
npm version 1.0.1 --no-git-tag-version >/dev/null
npm publish --registry "$R" --loglevel=error >/dev/null
test "$(code -u ci:smoke-pass -X DELETE "${R}@smoke/gone/-/gone-1.0.1.tgz")" = 200
test "$(versions @smoke/gone "$R")" = "1.0.0 "
test "$(code -u ci:smoke-pass -X DELETE "${R}@smoke/gone")" = 200
test "$(code "${R}@smoke%2fgone")" = 404

echo "== npm audit through the group, of a package with a known advisory"
project
npm install lodash@4.17.20 --no-audit --no-fund --loglevel=error >/dev/null
if npm audit --json > /tmp/audit-report.json; then fail "npm audit found nothing in lodash 4.17.20"; fi
node -e 'if (!(JSON.parse(require("fs").readFileSync("/tmp/audit-report.json")).metadata.vulnerabilities.total > 0)) process.exit(1)' || fail "npm audit report"

echo "== dist-tag and view, through the group and through the proxy"
npm dist-tag add @smoke/hello@1.0.0 beta >/dev/null
npm dist-tag ls @smoke/hello | grep -q 'beta: 1.0.0'
test "$(npm view @smoke/hello version)" = 1.0.0
npm dist-tag ls lodash --registry "$C/npm-proxy/" > /tmp/tags.txt
grep -q '^latest: ' /tmp/tags.txt || { cat /tmp/tags.txt; fail "dist-tags through the proxy"; }

echo "== publish --tag of a prerelease, dist-tag rm"
cd /tmp/pkg && npm version 1.0.2-beta.1 --no-git-tag-version >/dev/null && npm publish --tag next --loglevel=error >/dev/null
test "$(npm view @smoke/hello dist-tags.next)" = 1.0.2-beta.1
test "$(npm view @smoke/hello version)" = 1.0.0 || fail "--tag next moved latest"
npm dist-tag rm @smoke/hello next >/dev/null
test -z "$(npm view @smoke/hello dist-tags.next)"

echo "== deprecate through the group (?write=true reads the writeMember only)"
npm deprecate @smoke/hello@1.0.0 "smoke deprecation" --loglevel=error
npm view @smoke/hello@1.0.0 deprecated | grep -q 'smoke deprecation'
project
npm install @smoke/hello@1.0.0 --cache "$P/.cache" --no-audit --no-fund > install.log 2>&1
grep -q 'deprecated @smoke/hello@1.0.0: smoke deprecation' install.log || { cat install.log; fail "npm install did not warn"; }
npm deprecate @smoke/hello@1.0.0 "" --loglevel=error
test -z "$(npm view @smoke/hello@1.0.0 deprecated)" || fail "undeprecate"
npm deprecate @smoke/hello@1.0.0 "smoke deprecation" --loglevel=error

echo "== unpublish one version (latest moves back), then the whole package"
cd /tmp/pkg && npm version 1.0.1 --no-git-tag-version >/dev/null && npm publish --loglevel=error
test "$(npm view @smoke/hello version)" = 1.0.1
npm unpublish @smoke/hello@1.0.1 --force --loglevel=error
test "$(npm view @smoke/hello version)" = 1.0.0
test "$(code "${REG}@smoke/hello/-/hello-1.0.1.tgz")" = 404
test "$(code "${REG}@smoke/hello/-/hello-1.0.0.tgz")" = 200
npm unpublish @smoke/hello --force --loglevel=error
test "$(code "${HOSTED}@smoke%2fhello")" = 404

echo "== audit goes to the group's proxy member's remote; a hosted repository answers an empty report"
test "$(curl -s -o /tmp/audit.json -w '%{http_code}' -H "Authorization: Bearer $tok" -H 'Content-Type: application/json' \
  -d '{"express":["4.0.0"]}' "${REG}-/npm/v1/security/advisories/bulk")" = 200
grep -q '"express"' /tmp/audit.json
test "$(curl -s -H "Authorization: Bearer $tok" -H 'Content-Type: application/json' -d '{"express":["4.0.0"]}' \
  "${HOSTED}-/npm/v1/security/advisories/bulk")" = "{}"

echo "== npm logout revokes the token"
test "$(code -H "Authorization: Bearer $tok" "${REG}-/whoami")" = 200
npm logout --registry "$REG"
test "$(code -H "Authorization: Bearer $tok" "${REG}-/whoami")" = 401
echo "NPM CLIENT OK"
EOF

docker network create "$net" >/dev/null
start_postgres
docker run -d --name "$name" --network "$net" -e COMMANDCENTER_DATABASE_URL="$pg_url" --network-alias commandcenter --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" commandcenter:smoke-npm >/dev/null

# No port is published: the image's own health check asks the server.
ready=0
for i in $(seq 1 30); do
  if docker exec "$name" commandcenter healthcheck >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "FAIL: commandcenter did not become ready within 30s" >&2
  docker logs "$name" >&2 || true
  exit 1
fi

docker run --rm --name "$name-client" --network "$net" --network-alias upstream --memory 2g -v "$root/$work:/work:ro" node:22 sh /work/run.sh

echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "NPM SMOKE OK"
