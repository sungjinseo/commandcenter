#!/usr/bin/env bash
# Builds the image, starts commandcenter, then exercises the web UI and its JSON API
# with curl: redirect, CSP, login (cookie session + UI header), browse/asset,
# token create/use/revoke, GC, status and anonymous visibility.
# Finally prints the container's memory usage.
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-ui-$$"
net=commandcenter-ui-smoke-$$
name=commandcenter-ui-smoke-$$
base=http://localhost:18086

cleanup() {
  docker rm -f "$name" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup
rm -rf "$work" && mkdir -p "$work"

fail() { echo "FAIL: $*" >&2; exit 1; }
# expect <want> <got> <what>
expect() { [ "$1" = "$2" ] || fail "$3: want $1, got $2"; }

docker build -q -t commandcenter:smoke . >/dev/null

echo "== JS parse check (node --check on every static module)"
docker run --rm -v "$root/internal/ui/static:/s:ro" node:20-alpine sh -c 'for f in /s/*.js; do node --check "$f" || exit 1; done'   || fail "node --check failed on a static JS file"
hash="$(printf 'ui-pass\n' | docker run --rm -i commandcenter:smoke hash-password)"

cat > "$work/config.yaml" <<EOF2
anonymousRead: false
baseURL: "http://localhost:18086"
metricsFlushSeconds: 2
users:
  - { name: admin, passwordHash: "$hash", roles: [admin] }
  - { name: other, passwordHash: "$hash", roles: [] }
repositories:
  - { name: maven-releases, format: maven, type: hosted }
  - { name: open-releases,  format: maven, type: hosted, anonymousRead: true }
EOF2

docker network create "$net" >/dev/null
docker run -d --name "$name" --network "$net" --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" -p 18086:8081 commandcenter:smoke >/dev/null

ready=0
for i in $(seq 1 30); do
  if curl -fs "$base/service/rest/v1/status" >/dev/null; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  docker logs "$name" >&2 || true
  fail "commandcenter not ready after 30s"
fi

jar="$work/cookies.txt"
ui=(-H 'X-Command-Center-UI: 1')
status() { curl -s -o /dev/null -w '%{http_code}' "$@" || true; }

echo "== redirect and static page"
curl -s -D "$work/root.headers" -o "$work/root.body" "$base/"
head -1 "$work/root.headers" | grep -q " 302" || fail "GET / not 302"
grep -qi "^location: .*/ui/" "$work/root.headers" || fail "GET / does not redirect to /ui/"
curl -s -D "$work/ui.headers" -o "$work/ui.body" "$base/ui/"
head -1 "$work/ui.headers" | grep -q ' 200' || fail "GET /ui/ not 200"
grep -qi '^content-security-policy:.*default-src' "$work/ui.headers" || fail "no CSP on /ui/"

echo "== login"
login='{"user":"admin","password":"ui-pass"}'
expect 403 "$(status -c "$jar" -X POST -d "$login" "$base/-/api/login")" "login without UI header"
expect 200 "$(status -c "$jar" "${ui[@]}" -X POST -d "$login" "$base/-/api/login")" "login with UI header"
grep -q "^#HttpOnly_.*cc_session" "$jar" || fail "session cookie is not HttpOnly: $(cat "$jar")"
expect 401 "$(status "${ui[@]}" -X POST -d '{"user":"admin","password":"nope"}' "$base/-/api/login")" "bad password"
me="$(curl -s -b "$jar" "$base/-/api/me")"
echo "$me" | grep -q '"user":"admin"' || fail "me: $me"

echo "== maven upload, browse, asset sha1"
printf '<project><groupId>com.example</groupId><artifactId>hello</artifactId><version>1.0.0</version></project>\n' > "$work/hello-1.0.0.pom"
expect 201 "$(status -u admin:ui-pass -X PUT --data-binary @"$work/hello-1.0.0.pom" \
  "$base/repository/maven-releases/com/example/hello/1.0.0/hello-1.0.0.pom")" "maven PUT"
curl -s -b "$jar" "$base/-/api/repositories/maven-releases/browse?path=com/example/hello/1.0.0/" \
  | grep -q 'hello-1.0.0.pom' || fail "browse does not list the pom"
want="$(sha1sum "$work/hello-1.0.0.pom" | cut -d' ' -f1)"
asset="$(curl -s -b "$jar" "$base/-/api/repositories/maven-releases/asset?path=com/example/hello/1.0.0/hello-1.0.0.pom")"
echo "$asset" | grep -q "\"sha1\":\"$want\"" || fail "asset sha1 mismatch (want $want): $asset"

echo "== tokens"
resp="$(curl -s -b "$jar" "${ui[@]}" -X POST -d '{}' "$base/-/api/tokens")"
token="$(echo "$resp" | sed -nE 's/.*"token":"([^"]+)".*/\1/p')"
tid="$(echo "$resp" | sed -nE 's/.*"id":"([^"]+)".*/\1/p')"
[ -n "$token" ] && [ -n "$tid" ] || fail "token create: $resp"
url="$base/repository/maven-releases/com/example/hello/1.0.0/hello-1.0.0.pom"
expect 200 "$(status -H "Authorization: Bearer $token" "$url")" "token as Bearer"
expect 200 "$(status -u "admin:$token" "$url")" "token as Basic password"
expect 401 "$(status -u "other:$token" "$url")" "token as another user's Basic password"
echo "== tokens cannot mint tokens or sessions"
expect 403 "$(status -H "Authorization: Bearer $token" "${ui[@]}" -X POST -d '{}' "$base/-/api/tokens")" "Bearer token creating a token"
expect 403 "$(status -u "admin:$token" "${ui[@]}" -X POST -d '{}' "$base/-/api/tokens")" "Basic token creating a token"
expect 401 "$(status "${ui[@]}" -X POST -d "{\"user\":\"admin\",\"password\":\"$token\"}" "$base/-/api/login")" "login with a token"
expect 200 "$(status -b "$jar" "$base/-/api/tokens")" "token list"
expect 401 "$(status -b "$jar" -X DELETE "$base/-/api/tokens/$tid")" "token DELETE with cookie only"
expect 204 "$(status -b "$jar" "${ui[@]}" -X DELETE "$base/-/api/tokens/$tid")" "token DELETE with UI header"
expect 401 "$(status -H "Authorization: Bearer $token" "$url")" "revoked token"
expect 401 "$(status -u "admin:$token" "$url")" "revoked token as Basic password"

echo "== repository content is sandboxed"
curl -s -D "$work/asset.headers" -o "$work/asset.body" -u admin:ui-pass "$url" || fail "fetching the asset failed (curl exit $?)"
grep -qi "^content-security-policy: sandbox" "$work/asset.headers" || fail "no sandbox CSP on repository content"
grep -qi "^x-content-type-options: nosniff" "$work/asset.headers" || fail "no nosniff on repository content"
grep -qi "^cache-control: private" "$work/asset.headers" || fail "authenticated repository content is not Cache-Control: private"

echo "== maven delete with a session (needs the UI header)"
expect 401 "$(status -b "$jar" -X DELETE "$url")" "maven DELETE with cookie only"
expect 204 "$(status -b "$jar" "${ui[@]}" -X DELETE "$url")" "maven DELETE with cookie and UI header"
expect 404 "$(status -u admin:ui-pass "$url")" "deleted asset"

echo "== gc and status"
expect 200 "$(status -b "$jar" "${ui[@]}" -X POST "$base/-/api/gc")" "gc"
st="$(curl -s -b "$jar" "$base/-/api/status")"
rss="$(echo "$st" | sed -nE 's/.*"rssBytes":([0-9]+).*/\1/p')"
[ "${rss:-0}" -gt 0 ] || fail "rssBytes: $st"

echo "== anonymous visibility"
anon="$(curl -s "$base/-/api/repositories")"
echo "$anon" | grep -q '"open-releases"' || fail "anonymous cannot see open-releases: $anon"
if echo "$anon" | grep -q '"maven-releases"'; then fail "anonymous sees maven-releases: $anon"; fi
expect 404 "$(status "$base/-/api/repositories/maven-releases")" "hidden repo is 404 for anonymous"

echo "== components (derived), users, status checks, system information, tasks"
printf 'PK-fake-jar\n' > "$work/hello-2.0.0.jar"
expect 201 "$(status -u admin:ui-pass -X PUT --data-binary @"$work/hello-2.0.0.jar" \
  "$base/repository/maven-releases/com/example/hello/2.0.0/hello-2.0.0.jar")" "maven jar PUT"
anoncomps="$(curl -s -w ' HTTP=%{http_code}' "$base/-/api/search/components?format=maven&name=hello")"
case "$anoncomps" in *" HTTP=200") ;; *) fail "anonymous component search status: $anoncomps" ;; esac
echo "$anoncomps" | grep -qF '"components":[]' || fail "anonymous component search is not empty: $anoncomps"
if echo "$anoncomps" | grep -q 'hello'; then fail "anonymous sees a hidden repository component: $anoncomps"; fi
comps="$(curl -s -b "$jar" "$base/-/api/search/components?format=maven&name=hello")"
echo "$comps" | grep -q '"group":"com.example"' || fail "component group: $comps"
echo "$comps" | grep -q '"name":"hello"' || fail "component name: $comps"
echo "$comps" | grep -q '"version":"2.0.0"' || fail "component version: $comps"
echo "$comps" | grep -q 'hello-2.0.0.jar' || fail "component asset: $comps"
expect 400 "$(status -b "$jar" "$base/-/api/search/components?format=maven")" "component search without criteria"

expect 401 "$(status "$base/-/api/users")" "users as anonymous"
users="$(curl -s -b "$jar" "$base/-/api/users")"
echo "$users" | grep -q '"userId":"admin"' || fail "users: $users"
echo "$users" | grep -q '"userId":"other"' || fail "users: $users"
if echo "$users" | grep -Eq '\$2[aby]\$|passwordHash'; then fail "users leaks a password hash: $users"; fi

expect 401 "$(status "$base/-/api/status/checks")" "status checks as anonymous"
checks="$(curl -s -b "$jar" "$base/-/api/status/checks")"
echo "$checks" | grep -q '"healthy":true' || fail "no healthy check: $checks"
if echo "$checks" | grep -q '"healthy":false'; then fail "unhealthy check: $checks"; fi

expect 401 "$(status "$base/-/api/systeminformation")" "systeminformation as anonymous"
sysinfo="$(curl -s -b "$jar" "$base/-/api/systeminformation")"
echo "$sysinfo" | grep -q '"system-runtime"' || fail "systeminformation: $sysinfo"

tasks="$(curl -s -b "$jar" "$base/-/api/tasks")"
echo "$tasks" | grep -q '"id":"' || fail "tasks: $tasks"
tkid="$(echo "$tasks" | sed -nE 's/.*"id":"([^"]+)".*/\1/p')"
expect 401 "$(status "$base/-/api/tasks")" "tasks as anonymous"
expect 401 "$(status "${ui[@]}" -X POST "$base/-/api/tasks/$tkid/run")" "task run as anonymous"
expect 401 "$(status -b "$jar" -X POST "$base/-/api/tasks/$tkid/run")" "task run with cookie only"
# wait_task <id>: polls the task until it is no longer Running (10 s at most).
wait_task() {
  local i one
  for i in $(seq 1 50); do
    one="$(curl -s -b "$jar" "$base/-/api/tasks/$1")"
    echo "$one" | grep -q '"status":"Running"' || { echo "$one"; return 0; }
    sleep 0.2
  done
  fail "task $1 still running: $one"
}
expect 202 "$(status -b "$jar" "${ui[@]}" -X POST "$base/-/api/tasks/$tkid/run")" "task run"
wait_task "$tkid" | grep -q '"lastResult":"OK"' || fail "task lastResult after run"
tasks="$(curl -s -b "$jar" "$base/-/api/tasks")"
echo "$tasks" | grep -q '"id":"compact-blobstore"' || fail "seed task missing: $tasks"
echo "$tasks" | grep -q '"schedule":"daily:03:00"' || fail "seed task schedule: $tasks"

echo "== scheduled tasks: create, run, delete"
mkbody='{"name":"Rebuild releases","type":"repository.rebuild-metadata","schedule":"manual","params":{"repo":"maven-releases"}}'
expect 401 "$(status -b "$jar" -X POST -d "$mkbody" "$base/-/api/tasks")" "task create with cookie only"
expect 400 "$(status -b "$jar" "${ui[@]}" -X POST -d '{"name":"x","type":"blobstore.compact","schedule":"every day"}' "$base/-/api/tasks")" "task with a bad schedule"
expect 400 "$(status -b "$jar" "${ui[@]}" -X POST -d '{"name":"x","type":"repository.rebuild-metadata","schedule":"manual","params":{"repo":"open-releases-nope"}}' "$base/-/api/tasks")" "rebuild of a missing repository"
created="$(curl -s -b "$jar" "${ui[@]}" -X POST -d "$mkbody" "$base/-/api/tasks")"
rid="$(echo "$created" | sed -nE 's/^\{"id":"([^"]+)".*/\1/p')"
[ -n "$rid" ] || fail "task create: $created"
expect 202 "$(status -b "$jar" "${ui[@]}" -X POST "$base/-/api/tasks/$rid/run")" "rebuild-metadata run"
wait_task "$rid" | grep -q '"lastResult":"OK"' || fail "rebuild-metadata lastResult"
expect 200 "$(status -u admin:ui-pass "$base/repository/maven-releases/com/example/hello/maven-metadata.xml")" "metadata after rebuild"
detail="$(curl -s -b "$jar" "$base/-/api/tasks/$rid")"
echo "$detail" | grep -q '"lastLog":"' || fail "task detail without the run log: $detail"
expect 204 "$(status -b "$jar" "${ui[@]}" -X DELETE "$base/-/api/tasks/$rid")" "task delete"
expect 404 "$(status -b "$jar" "$base/-/api/tasks/$rid")" "deleted task"

echo "== a non-admin user is refused the admin screens"
ojar="$work/other-cookies.txt"
expect 200 "$(status -c "$ojar" "${ui[@]}" -X POST -d '{"user":"other","password":"ui-pass"}' "$base/-/api/login")" "login as other"
for p in users status/checks systeminformation tasks; do
  expect 403 "$(status -b "$ojar" "$base/-/api/$p")" "$p as a non-admin"
done

echo "== cleanup policy, cleanup task, activity, logs, request stats, download counts"
pol='{"name":"smoke-policy","format":"maven","componentAgeDays":0,"lastDownloadedDays":1,"releaseType":"any"}'
expect 401 "$(status -b "$jar" -X POST -d "$pol" "$base/-/api/cleanup/policies")" "policy create with cookie only"
expect 403 "$(status -b "$ojar" "${ui[@]}" -X POST -d "$pol" "$base/-/api/cleanup/policies")" "policy create as a non-admin"
expect 400 "$(status -b "$jar" "${ui[@]}" -X POST -d '{"name":"bad","format":"maven"}' "$base/-/api/cleanup/policies")" "policy without an age"
expect 201 "$(status -b "$jar" "${ui[@]}" -X POST -d "$pol" "$base/-/api/cleanup/policies")" "policy create"
curl -s -b "$jar" "$base/-/api/cleanup/policies" | grep -q '"name":"smoke-policy"' || fail "policy list"
# Anything just uploaded is too new to be a candidate: only the shape is asserted.
pv="$(curl -s -b "$jar" -w ' HTTP=%{http_code}' "$base/-/api/cleanup/policies/smoke-policy/preview")"
case "$pv" in *" HTTP=200") ;; *) fail "policy preview status: $pv" ;; esac
echo "$pv" | grep -q '"count":' || fail "policy preview without a count: $pv"
# Assign the policy to maven-releases (PUT is a full replace: keep its policies).
mrcfg="$(curl -s -b "$jar" "$base/-/api/repositories/maven-releases/config")"
vp="$(echo "$mrcfg" | sed -nE 's/.*"versionPolicy":"([^"]*)".*/\1/p')"
wp="$(echo "$mrcfg" | sed -nE 's/.*"writePolicy":"([^"]*)".*/\1/p')"
mrbody() { printf '{"name":"maven-releases","format":"maven","type":"hosted","versionPolicy":"%s","writePolicy":"%s","cleanupPolicy":"%s"}' "$vp" "$wp" "$1"; }
expect 200 "$(status -b "$jar" "${ui[@]}" -X PUT -d "$(mrbody smoke-policy)" "$base/-/api/repositories/maven-releases")" "assign the cleanup policy to maven-releases"
curl -s -b "$jar" "$base/-/api/repositories/maven-releases/config" | grep -q '"cleanupPolicy":"smoke-policy"' || fail "maven-releases has no cleanup policy"
ckbody='{"name":"Smoke cleanup","type":"repository.cleanup","schedule":"manual","params":{"repos":[]}}'
cr="$(curl -s -b "$jar" "${ui[@]}" -X POST -d "$ckbody" "$base/-/api/tasks")"
cid="$(echo "$cr" | sed -nE 's/^\{"id":"([^"]+)".*/\1/p')"
[ -n "$cid" ] || fail "cleanup task create: $cr"
expect 409 "$(status -b "$jar" "${ui[@]}" -X POST "$base/-/api/tasks/$cid/stop")" "stop of a task that is not running"
expect 202 "$(status -b "$jar" "${ui[@]}" -X POST "$base/-/api/tasks/$cid/run")" "cleanup task run"
cdone="$(wait_task "$cid")"
echo "$cdone" | grep -q '"lastResult":"OK"' || fail "cleanup task result: $cdone"
echo "$cdone" | grep -q 'maven-releases: policy' || fail "cleanup log has no maven-releases line: $cdone"
# The uploaded jar is too new to be a candidate: it is still served and listed.
expect 200 "$(status -u admin:ui-pass "$base/repository/maven-releases/com/example/hello/2.0.0/hello-2.0.0.jar")" "jar GET after cleanup"
curl -s -b "$jar" "$base/-/api/search/components?format=maven&name=hello" | grep -q 'hello-2.0.0.jar' || fail "component gone after cleanup"
expect 204 "$(status -b "$jar" "${ui[@]}" -X DELETE "$base/-/api/tasks/$cid")" "cleanup task delete"
expect 409 "$(status -b "$jar" "${ui[@]}" -X DELETE "$base/-/api/cleanup/policies/smoke-policy")" "delete of a policy in use"
expect 200 "$(status -b "$jar" "${ui[@]}" -X PUT -d "$(mrbody '')" "$base/-/api/repositories/maven-releases")" "unassign the cleanup policy"
expect 204 "$(status -b "$jar" "${ui[@]}" -X DELETE "$base/-/api/cleanup/policies/smoke-policy")" "policy delete"
# wait_for <what> <cmd...>: runs cmd until it succeeds (15 s at most).
wait_for() {
  local what="$1" i; shift
  for i in $(seq 1 75); do "$@" && return 0; sleep 0.2; done
  fail "timed out waiting for $what"
}
has_upload() { curl -s -b "$jar" "$base/-/api/events?kind=upload" | grep -q '"kind":"upload"'; }
wait_for "an upload event" has_upload
has_run() { curl -s -b "$jar" "$base/-/api/events?kind=task.run" | grep -q '"kind":"task.run"'; }
wait_for "a task.run event" has_run
expect 401 "$(status "$base/-/api/events")" "events as anonymous"
expect 403 "$(status -b "$ojar" "$base/-/api/events")" "events as a non-admin"
logs="$(curl -s -b "$jar" "$base/-/api/logs")"
echo "$logs" | grep -q '"message":"' || fail "logs returned no lines: $logs"
expect 403 "$(status -b "$ojar" "$base/-/api/logs")" "logs as a non-admin"
# Download counts and request statistics are flushed every metricsFlushSeconds (2 here).
jarurl="$base/repository/maven-releases/com/example/hello/2.0.0/hello-2.0.0.jar"
for i in 1 2; do expect 200 "$(status -u admin:ui-pass "$jarurl")" "jar GET $i"; done
has_downloads() {
  curl -s -b "$jar" "$base/-/api/repositories/maven-releases/asset?path=com/example/hello/2.0.0/hello-2.0.0.jar" \
    | grep -Eq '"downloads":[1-9]'
}
wait_for "the asset download count to rise" has_downloads
has_requests() {
  curl -s -b "$jar" "$base/-/api/stats/requests?range=24h" | grep -Eq '"totals":\{[^}]*"requests":[1-9]'
}
wait_for "a non-zero request total" has_requests
expect 403 "$(status -b "$ojar" "$base/-/api/stats/requests?range=24h")" "request stats as a non-admin"

echo "== static scripts are served as JavaScript"
for js in app shell browse dash search admin util repoform upload ops access settings i18n; do
  curl -s -D "$work/js.headers" -o "$work/js.body" "$base/ui/$js.js" || fail "fetching /ui/$js.js failed (curl exit $?)"
  head -1 "$work/js.headers" | grep -q ' 200' || fail "GET /ui/$js.js not 200"
  grep -qi '^content-type: .*javascript' "$work/js.headers" || fail "/ui/$js.js is not JavaScript: $(grep -i '^content-type' "$work/js.headers")"
done

echo "== repository management (create, serve, deny, group-member 409, delete)"
api="$base/-/api/repositories"
newrepo='{"name":"smoke-hosted","format":"maven","type":"hosted","versionPolicy":"release","writePolicy":"allow"}'
expect 403 "$(status -b "$ojar" "${ui[@]}" -X POST -d "$newrepo" "$api")" "repository create as a non-admin"
expect 401 "$(status -b "$jar" -X POST -d "$newrepo" "$api")" "repository create with cookie only"
expect 201 "$(status -b "$jar" "${ui[@]}" -X POST -d "$newrepo" "$api")" "repository create"
expect 400 "$(status -b "$jar" "${ui[@]}" -X POST -d "$newrepo" "$api")" "duplicate repository create"
rurl="$base/repository/smoke-hosted/com/example/new/1.0/new-1.0.pom"
printf '<project><groupId>com.example</groupId><artifactId>new</artifactId><version>1.0</version></project>\n' > "$work/new-1.0.pom"
expect 201 "$(status -u admin:ui-pass -X PUT --data-binary @"$work/new-1.0.pom" "$rurl")" "PUT into the new repository"
expect 200 "$(status -u admin:ui-pass "$rurl")" "GET from the new repository"
cfg="$(curl -s -b "$jar" "$api/smoke-hosted/config")"
echo "$cfg" | grep -q '"writePolicy":"allow"' || fail "repository config: $cfg"
# PUT is a full replace: send the whole body with writePolicy deny.
deny='{"name":"smoke-hosted","format":"maven","type":"hosted","versionPolicy":"release","writePolicy":"deny"}'
expect 200 "$(status -b "$jar" "${ui[@]}" -X PUT -d "$deny" "$api/smoke-hosted")" "repository update to deny"
expect 400 "$(status -u admin:ui-pass -X PUT --data-binary @"$work/new-1.0.pom" \
  "$base/repository/smoke-hosted/com/example/new/2.0/new-2.0.pom")" "PUT into a deny repository"
echo "== group member cannot be deleted"
grp='{"name":"smoke-group","format":"maven","type":"group","members":["smoke-hosted"]}'
expect 201 "$(status -b "$jar" "${ui[@]}" -X POST -d "$grp" "$api")" "group create"
expect 409 "$(status -b "$jar" "${ui[@]}" -X DELETE "$api/smoke-hosted")" "delete of a group member"
expect 204 "$(status -b "$jar" "${ui[@]}" -X DELETE "$api/smoke-group")" "group delete"
echo "== token-auth admin cannot manage repositories"
resp="$(curl -s -b "$jar" "${ui[@]}" -X POST -d '{}' "$base/-/api/tokens")"
token2="$(echo "$resp" | sed -nE 's/.*"token":"([^"]+)".*/\1/p')"
[ -n "$token2" ] || fail "token create: $resp"
expect 403 "$(status -H "Authorization: Bearer $token2" "${ui[@]}" -X POST \
  -d '{"name":"tok-repo","format":"maven","type":"hosted"}' "$api")" "token-auth repository create"
expect 403 "$(status -H "Authorization: Bearer $token2" "${ui[@]}" -X DELETE "$api/smoke-hosted")" "token-auth repository delete"
expect 204 "$(status -b "$jar" "${ui[@]}" -X DELETE "$api/smoke-hosted")" "repository delete"
expect 404 "$(status -u admin:ui-pass "$rurl")" "GET after the repository delete"
expect 404 "$(status -b "$jar" "$api/smoke-hosted/config")" "config after the repository delete"

echo "== access control: users, roles, last admin, own password, realms"
users="$base/-/api/users"
roles="$base/-/api/roles"
expect 403 "$(status -b "$ojar" "${ui[@]}" -X POST -d '{"id":"x","password":"password-1"}' "$users")" "user create as a non-admin"
expect 400 "$(status -b "$jar" "${ui[@]}" -X POST -d '{"id":"short","password":"abc"}' "$users")" "user create with a short password"
expect 201 "$(status -b "$jar" "${ui[@]}" -X POST -d '{"id":"smoke-user","name":"Smoke","email":"s@example.com","password":"password-1","roles":[]}' "$users")" "user create"
expect 409 "$(status -b "$jar" "${ui[@]}" -X POST -d '{"id":"smoke-user","password":"password-1"}' "$users")" "duplicate user create"
sjar="$work/smoke-user-cookies.txt"
expect 200 "$(status -c "$sjar" "${ui[@]}" -X POST -d '{"user":"smoke-user","password":"password-1"}' "$base/-/api/login")" "login as the new user"
expect 403 "$(status -b "$sjar" "$users")" "user list as the new non-admin user"
# A role that may add but not delete in the hosted repository.
rolebody='{"id":"smoke-writer","description":"add, no delete","privileges":[{"target":"repo:maven-releases","actions":["browse","read","add"]}]}'
expect 201 "$(status -b "$jar" "${ui[@]}" -X POST -d "$rolebody" "$roles")" "role create"
expect 409 "$(status -b "$jar" "${ui[@]}" -X PUT -d '{"description":"x","privileges":[]}' "$roles/admin")" "admin role edit"
expect 200 "$(status -b "$jar" "${ui[@]}" -X PUT -d '{"description":"add, no delete (edited)","privileges":[{"target":"repo:maven-releases","actions":["browse","read","add"]}]}' "$roles/smoke-writer")" "role edit (the UI sends no id)"
curl -s -b "$jar" "$roles/smoke-writer" | grep -q 'edited' || fail "role edit was not stored"
expect 200 "$(status -b "$jar" "${ui[@]}" -X PUT -d '{"name":"Smoke","email":"s@example.com","roles":["smoke-writer"],"status":"active"}' "$users/smoke-user")" "assign the role"
wurl="$base/repository/maven-releases/com/example/acl/1.0/acl-1.0.pom"
printf '<project><groupId>com.example</groupId><artifactId>acl</artifactId><version>1.0</version></project>
' > "$work/acl-1.0.pom"
expect 201 "$(status -u smoke-user:password-1 -X PUT --data-binary @"$work/acl-1.0.pom" "$wurl")" "PUT with the add privilege"
expect 403 "$(status -u smoke-user:password-1 -X DELETE "$wurl")" "DELETE without the delete privilege"
expect 409 "$(status -b "$jar" "${ui[@]}" -X DELETE "$roles/smoke-writer")" "delete a role that is in use"
# The last admin who can log in stays.
expect 409 "$(status -b "$jar" "${ui[@]}" -X PUT -d '{"name":"","email":"","roles":["admin"],"status":"disabled"}' "$users/admin")" "disable the last admin"
expect 409 "$(status -b "$jar" "${ui[@]}" -X PUT -d '{"name":"","email":"","roles":[],"status":"active"}' "$users/admin")" "demote the last admin"
# The user changes their own password.
expect 403 "$(status -b "$sjar" "${ui[@]}" -X PUT -d '{"currentPassword":"wrong-pass","newPassword":"password-2"}' "$base/-/api/account/password")" "own password with a wrong current one"
expect 204 "$(status -b "$sjar" -c "$sjar" "${ui[@]}" -X PUT -d '{"currentPassword":"password-1","newPassword":"password-2"}' "$base/-/api/account/password")" "own password change"
expect 401 "$(status -u smoke-user:password-1 -X PUT --data-binary @"$work/acl-1.0.pom" "$wurl")" "old password after the change"
expect 200 "$(status "${ui[@]}" -X POST -d '{"user":"smoke-user","password":"password-2"}' "$base/-/api/login")" "login with the new password"
expect 204 "$(status -b "$jar" "${ui[@]}" -X DELETE "$users/smoke-user")" "user delete"
expect 204 "$(status -b "$jar" "${ui[@]}" -X DELETE "$roles/smoke-writer")" "role delete"
realms="$(curl -s -b "$jar" "$base/-/api/realms")"
echo "$realms" | grep -q '"realms"' || fail "realms: $realms"
echo "$realms" | grep -q '"available"' || fail "realms available: $realms"

echo "== operations and system: settings, offline, blob stores, e-mail, task notify"
expect 403 "$(status -b "$ojar" "${ui[@]}" -X PUT -d '{"maxUploadMiB":77}' "$base/-/api/settings")" "settings as a non-admin"
expect 400 "$(status -b "$jar" "${ui[@]}" -X PUT -d '{}' "$base/-/api/settings")" "settings without fields"
expect 200 "$(status -b "$jar" "${ui[@]}" -X PUT -d '{"maxUploadMiB":77}' "$base/-/api/settings")" "settings update"
curl -s -b "$jar" "$base/-/api/settings" | grep -q '"maxUploadMiB":77' || fail "settings GET does not show maxUploadMiB 77"
expect 200 "$(status -b "$jar" "${ui[@]}" -X PUT -d '{"maxUploadMiB":1024}' "$base/-/api/settings")" "settings restore"
# a repository goes offline (503) and back online
on='{"name":"smoke-off","format":"maven","type":"hosted","versionPolicy":"release","writePolicy":"allow","online":true}'
off='{"name":"smoke-off","format":"maven","type":"hosted","versionPolicy":"release","writePolicy":"allow","online":false}'
expect 201 "$(status -b "$jar" "${ui[@]}" -X POST -d "$on" "$api")" "offline test repository create"
ourl="$base/repository/smoke-off/com/example/off/1.0/off-1.0.pom"
printf '<project><groupId>com.example</groupId><artifactId>off</artifactId><version>1.0</version></project>\n' > "$work/off-1.0.pom"
expect 201 "$(status -u admin:ui-pass -X PUT --data-binary @"$work/off-1.0.pom" "$ourl")" "PUT into the repository before going offline"
expect 200 "$(status -b "$jar" "${ui[@]}" -X PUT -d "$off" "$api/smoke-off")" "repository offline"
expect 503 "$(status -u admin:ui-pass "$ourl")" "GET from an offline repository"
curl -s -b "$jar" "$api" | grep -Eq '"name":"smoke-off"[^}]*"online":false' || fail "repository list does not show smoke-off offline"
expect 200 "$(status -b "$jar" "${ui[@]}" -X PUT -d "$on" "$api/smoke-off")" "repository online again"
expect 200 "$(status -u admin:ui-pass "$ourl")" "GET after going online"
expect 204 "$(status -b "$jar" "${ui[@]}" -X DELETE "$api/smoke-off")" "offline test repository delete"
# a blob store with a hosted repository in it
bs="$base/-/api/blobstores"
expect 403 "$(status -b "$ojar" "$bs")" "blob stores as a non-admin"
expect 201 "$(status -b "$jar" "${ui[@]}" -X POST -d '{"name":"smoke-store"}' "$bs")" "blob store create"
expect 409 "$(status -b "$jar" "${ui[@]}" -X POST -d '{"name":"smoke-store"}' "$bs")" "duplicate blob store create"
curl -s -b "$jar" "$bs" | grep -q '"name":"smoke-store"' || fail "blob store list"
inbs='{"name":"smoke-in-store","format":"maven","type":"hosted","versionPolicy":"release","writePolicy":"allow","blobStore":"smoke-store"}'
expect 201 "$(status -b "$jar" "${ui[@]}" -X POST -d "$inbs" "$api")" "repository in the blob store"
curl -s -b "$jar" "$api" | grep -Eq '"name":"smoke-in-store"[^}]*"blobStore":"smoke-store"' || fail "repository list does not show the blob store"
surl="$base/repository/smoke-in-store/com/example/bs/1.0/bs-1.0.pom"
printf '<project><groupId>com.example</groupId><artifactId>bs</artifactId><version>1.0</version></project>\n' > "$work/bs-1.0.pom"
expect 201 "$(status -u admin:ui-pass -X PUT --data-binary @"$work/bs-1.0.pom" "$surl")" "PUT into the blob store repository"
curl -s -u admin:ui-pass "$surl" | cmp -s - "$work/bs-1.0.pom" || fail "download from the blob store repository differs"
expect 409 "$(status -b "$jar" "${ui[@]}" -X DELETE "$bs/smoke-store")" "delete of a blob store in use"
expect 409 "$(status -b "$jar" "${ui[@]}" -X DELETE "$bs/default")" "delete of the default blob store"
expect 204 "$(status -b "$jar" "${ui[@]}" -X DELETE "$api/smoke-in-store")" "blob store repository delete"
expect 204 "$(status -b "$jar" "${ui[@]}" -X DELETE "$bs/smoke-store")" "blob store delete"
# e-mail: disabled means the test is refused
smtpoff='{"host":"","port":0,"security":"none","username":"","from":"","enabled":false}'
expect 200 "$(status -b "$jar" "${ui[@]}" -X PUT -d "$smtpoff" "$base/-/api/settings/smtp")" "smtp disabled"
expect 400 "$(status -b "$jar" "${ui[@]}" -X POST -d '{"to":"a@example.com"}' "$base/-/api/settings/smtp/test")" "smtp test while disabled"
# a task with notification addresses
nbody='{"name":"Notify","type":"repository.rebuild-metadata","schedule":"manual","params":{"repo":"maven-releases"},"notifyEmail":["ops@example.com","dev@example.com"]}'
ncreated="$(curl -s -b "$jar" "${ui[@]}" -X POST -d "$nbody" "$base/-/api/tasks")"
nid="$(echo "$ncreated" | sed -nE 's/^\{"id":"([^"]+)".*/\1/p')"
[ -n "$nid" ] || fail "task create with notifyEmail: $ncreated"
curl -s -b "$jar" "$base/-/api/tasks/$nid" | grep -q '"notifyEmail":\["ops@example.com","dev@example.com"\]' || fail "notifyEmail did not round-trip"
expect 204 "$(status -b "$jar" "${ui[@]}" -X DELETE "$base/-/api/tasks/$nid")" "notify task delete"

echo "== logout"
expect 204 "$(status -b "$jar" "${ui[@]}" -X POST "$base/-/api/logout")" "logout"

echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "UI SMOKE OK"
