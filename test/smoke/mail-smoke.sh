#!/usr/bin/env bash
# Builds the image, starts mailpit (a fake SMTP server with an HTTP API) and
# commandcenter on one docker network, points the SMTP settings at mailpit
# through the API and checks: the test mail arrives, a second test within 10 s
# is refused (429), and a failing scheduled task with notifyEmail sends a
# failure mail that names the task. Finally prints the container's memory
# usage.
#
# The failing task: a repository.rebuild-metadata task is accepted while its
# repository exists; deleting the repository afterwards makes the run fail
# ("no longer exists"), so no test hook is needed.
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-mail-$$"
net=commandcenter-mail-smoke-$$
name=commandcenter-mail-smoke-$$
mailname=commandcenter-mail-smoke-mailpit-$$
mailalias=mailpit
mailimage=axllent/mailpit:v1.21
base=http://localhost:18088
mailapi=http://localhost:18089

cleanup() {
  docker rm -f "$name" "$mailname" >/dev/null 2>&1 || true
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
hash="$(printf 'mail-smoke-pass\n' | docker run --rm -i commandcenter:smoke hash-password)"

cat > "$work/config.yaml" <<EOF2
baseURL: "http://localhost:18088"
users:
  - { name: admin, passwordHash: "$hash", roles: [admin] }
repositories:
  - { name: maven-releases, format: maven, type: hosted }
  - { name: maven-doomed, format: maven, type: hosted }
EOF2

docker network create "$net" >/dev/null

echo "== mailpit"
docker run -d --name "$mailname" --network "$net" --network-alias "$mailalias" -p 18089:8025 "$mailimage" >/dev/null
ready=0
for i in $(seq 1 30); do
  if curl -fs "$mailapi/api/v1/messages" >/dev/null; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  docker logs "$mailname" >&2 || true
  fail "mailpit not ready after 30s"
fi

echo "== commandcenter"
docker run -d --name "$name" --network "$net" --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" -p 18088:8081 commandcenter:smoke >/dev/null
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

echo "== login"
expect 200 "$(status -c "$jar" "${ui[@]}" -X POST -d '{"user":"admin","password":"mail-smoke-pass"}' "$base/-/api/login")" "login"

# wait_mail <recipient> <subject phrase>: polls mailpit (15 s at
# most) for a message to the recipient whose subject contains the phrase;
# prints the message's JSON.
wait_mail() {
  local i id
  for i in $(seq 1 30); do
    id="$(curl -s -G --data-urlencode "query=to:$1 subject:\"$2\"" "$mailapi/api/v1/search" \
      | sed -nE 's/^.*"messages":\[\{"ID":"([^"]+)".*/\1/p')"
    if [ -n "$id" ]; then curl -s "$mailapi/api/v1/message/$id"; return 0; fi
    sleep 0.5
  done
  echo "last search: $(curl -s -G --data-urlencode "query=to:$1" "$mailapi/api/v1/search")" >&2
  echo "mailpit log: $(docker logs "$mailname" 2>&1 | tail -5)" >&2
  echo "server log: $(docker logs "$name" 2>&1 | tail -5)" >&2
  fail "no mail to $1 mentioning $2 within 15s"
}

echo "== smtp settings and the test mail"
smtp="{\"enabled\":true,\"host\":\"$mailalias\",\"port\":1025,\"security\":\"none\",\"from\":\"cc@example.test\"}"
expect 200 "$(status -b "$jar" "${ui[@]}" -X PUT -d "$smtp" "$base/-/api/settings/smtp")" "smtp settings"
curl -s -b "$jar" "$base/-/api/settings/smtp" | grep -q '"enabled":true' || fail "smtp settings were not stored"
expect 200 "$(status -b "$jar" "${ui[@]}" -X POST -d '{"to":"admin@example.test"}' "$base/-/api/settings/smtp/test")" "smtp test"
msg="$(wait_mail admin@example.test "Test message")"
echo "$msg" | grep -q '"Address":"cc@example.test"' || fail "test mail sender: $msg"
echo "$msg" | grep -q '"Address":"admin@example.test"' || fail "test mail recipient: $msg"
expect 429 "$(status -b "$jar" "${ui[@]}" -X POST -d '{"to":"admin@example.test"}' "$base/-/api/settings/smtp/test")" "second smtp test within 10 s"

echo "== failure notification"
mk='{"name":"Doomed rebuild","type":"repository.rebuild-metadata","schedule":"manual","params":{"repo":"maven-doomed"},"notifyEmail":["ops@example.test"]}'
created="$(curl -s -b "$jar" "${ui[@]}" -X POST -d "$mk" "$base/-/api/tasks")"
tid="$(echo "$created" | sed -nE 's/^\{"id":"([^"]+)".*/\1/p')"
[ -n "$tid" ] || fail "task create: $created"
expect 204 "$(status -b "$jar" "${ui[@]}" -X DELETE "$base/-/api/repositories/maven-doomed")" "delete the repository the task targets"
expect 202 "$(status -b "$jar" "${ui[@]}" -X POST "$base/-/api/tasks/$tid/run")" "run the doomed task"
one=""
for i in $(seq 1 50); do
  one="$(curl -s -b "$jar" "$base/-/api/tasks/$tid")"
  echo "$one" | grep -q '"status":"Running"' || break
  sleep 0.2
done
echo "$one" | grep -q '"lastResult":"Failed"' || fail "the doomed task did not fail: $one"
msg="$(wait_mail ops@example.test "Doomed rebuild")"
echo "$msg" | grep -q 'Doomed rebuild' || fail "failure mail does not name the task: $msg"
echo "$msg" | grep -q 'no longer exists' || fail "failure mail lacks the run message: $msg"
# A successful run sends nothing: the compact task has no recipients, and a
# second failure notice is not expected without another run.
count="$(curl -s -G --data-urlencode "query=to:ops@example.test" "$mailapi/api/v1/search" | sed -nE 's/.*"messages_count":([0-9]+).*/\1/p')"
expect 1 "$count" "failure mails to ops@example.test"


echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "MAIL SMOKE OK"
