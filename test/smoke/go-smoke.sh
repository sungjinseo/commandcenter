#!/usr/bin/env bash
# Builds the image, starts commandcenter, then uses the real go command (golang:1.27) to:
#   1. publish example.com/hello v1.0.0 to go-hosted with PUT (.mod and .zip); a re-upload must get 409
#   2. build an app that needs example.com/hello (hosted) and rsc.io/quote (proxy) through go-all
#   3. list the hosted module's versions through the group
#   4. fetch a module through a proxy with an ALLOW routing rule and the default GOSUMDB: the
#      probe of the proxy's checksum database is a 404, so the go command verifies at
#      sum.golang.org itself
#   5. find that module in the REST search by its real and by its case-escaped name
#   6. delete a version through DELETE /v1/assets/{id}, hosted and proxied: all its files go
#   7. a group whose proxy is down answers 502: GOPROXY=<group>,direct stops there, and
#      GOPROXY=<group>|direct goes on (the guide's advice)
# Finally prints the container's memory usage.
#   SMOKE_PREFIX  start of the container and network names (default commandcenter)
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-go-$$"
name="${SMOKE_PREFIX:-commandcenter}-go-smoke-$$"
net="$name"
. test/postgres.sh

cleanup() {
  docker rm -f -v "$name" "$name-pg" "$name-go" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup
rm -rf "$work" && mkdir -p "$work"
cp test/smoke/go/mkzip.go test/smoke/go/json.go "$work/"

docker build -q -t commandcenter:smoke-go . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i --name "$name-hash" --memory 128m commandcenter:smoke-go hash-password)"

cat > "$work/config.yaml" <<EOF
anonymousRead: true
baseURL: "http://commandcenter:8081"
users:
  - { name: ci, passwordHash: "$hash", roles: [deployer] }
  - { name: admin, passwordHash: "$hash", roles: [admin] }
roles:
  deployer: { write: [go-hosted] }
repositories:
  - { name: go-proxy,  format: go, type: proxy,  remote: "https://proxy.golang.org/" }
  - { name: go-hosted, format: go, type: hosted }
  - { name: go-all,    format: go, type: group,  members: [go-hosted, go-proxy], writeMember: go-hosted }
  - { name: go-down,   format: go, type: proxy,  remote: "http://127.0.0.1:9/" }
  - { name: go-broken, format: go, type: group,  members: [go-hosted, go-down] }
EOF

cat > "$work/run.sh" <<'EOF'
set -eu
REG=http://commandcenter:8081/repository
API=http://commandcenter:8081/service/rest/v1
export GOTOOLCHAIN=local GOFLAGS=-mod=mod GOPATH=/tmp/gopath GOCACHE=/tmp/gocache
go version

api() { curl -fsS -u admin:smoke-pass -H 'Content-Type: application/json' "$@"; }
status() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
go build -o /tmp/json /work/json.go
# names <search parameters>: the names of the components a search answers.
names() { curl -fsS "$API/search?$1" | /tmp/json items name | sort -u; }
# asset <search parameters> <suffix>: the id of the asset a search answers whose path ends so.
asset() { curl -fsS "$API/search/assets?$1" | /tmp/json items id path | grep "$2\$" | cut -d' ' -f1; }

echo "== build example.com/hello v1.0.0"
mkdir -p /tmp/hello
printf 'module example.com/hello\n\ngo 1.21\n' > /tmp/hello/go.mod
printf 'package hello\n\nfunc Greet() string { return "hello from commandcenter" }\n' > /tmp/hello/hello.go
go run /work/mkzip.go /tmp/hello example.com/hello@v1.0.0 /tmp/hello.zip

echo "== publish to go-hosted"
curl -fsS -u ci:smoke-pass -X PUT --data-binary @/tmp/hello/go.mod "$REG/go-hosted/example.com/hello/@v/v1.0.0.mod"
curl -fsS -u ci:smoke-pass -X PUT --data-binary @/tmp/hello.zip "$REG/go-hosted/example.com/hello/@v/v1.0.0.zip"

echo "== re-upload must be rejected"
code=$(curl -s -o /dev/null -w '%{http_code}' -u ci:smoke-pass -X PUT --data-binary @/tmp/hello.zip "$REG/go-hosted/example.com/hello/@v/v1.0.0.zip")
test "$code" = 409

echo "== build an app through go-all (rsc.io/quote via the proxy)"
export GOPROXY="$REG/go-all" GONOSUMDB=example.com
mkdir -p /tmp/app && cd /tmp/app
go mod init app >/dev/null 2>&1
cat > main.go <<'GO'
package main

import (
	"fmt"

	"example.com/hello"
	"rsc.io/quote"
)

func main() { fmt.Println(hello.Greet(), "/", quote.Hello()) }
GO
go get example.com/hello@v1.0.0 rsc.io/quote@v1.5.2
out=$(go run .)
echo "$out"
echo "$out" | grep -q "hello from commandcenter"

echo "== list versions through the group"
go list -m -versions example.com/hello | grep -q v1.0.0

echo "== a proxy with an ALLOW routing rule: go get with the default GOSUMDB"
api -X POST "$API/routing-rules" -d '{"name":"go-allow","mode":"ALLOW","matchers":["^/github\\.com/.*"]}' >/dev/null
api -X POST "$API/repositories/go/proxy" -d '{"name":"go-routed","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":true},
  "proxy":{"remoteUrl":"https://proxy.golang.org/","contentMaxAge":-1,"metadataMaxAge":1440},"negativeCache":{"enabled":true,"timeToLive":10},
  "httpClient":{"blocked":false,"autoBlock":true},"routingRule":"go-allow"}' >/dev/null
test "$(status "$REG/go-routed/sumdb/sum.golang.org/supported")" = 404
test "$(status "$REG/go-routed/rsc.io/quote/@v/list")" = 403
mkdir -p /tmp/routed && cd /tmp/routed
go mod init routed >/dev/null 2>&1
GOPROXY="$REG/go-routed" GONOSUMDB= go get github.com/BurntSushi/toml@v1.4.0
grep -q 'github.com/BurntSushi/toml v1.4.0 h1:' go.sum

echo "== REST search: the real name and the case-escaped one"
for n in github.com/BurntSushi/toml 'github.com/!burnt!sushi/toml' 'github.com/!burnt!sushi/*'; do
  test "$(names "repository=go-routed&name=$n")" = github.com/BurntSushi/toml
done
test -z "$(names 'repository=go-routed&name=github.com/burnt!sushi/toml')"

echo "== DELETE /v1/assets/{id} of a cached file: the version's three files go, and come back"
id=$(asset 'repository=go-routed&name=github.com/BurntSushi/toml&version=v1.4.0' '\.zip')
test "$(status -u admin:smoke-pass -X DELETE "$API/assets/$id")" = 204
test -z "$(names 'repository=go-routed&name=github.com/BurntSushi/toml&version=v1.4.0')"
go clean -modcache
GOPROXY="$REG/go-routed" GONOSUMDB= go mod download github.com/BurntSushi/toml@v1.4.0
test "$(names 'repository=go-routed&name=github.com/BurntSushi/toml&version=v1.4.0')" = github.com/BurntSushi/toml

echo "== DELETE /v1/assets/{id} of a hosted file: the version leaves the list"
cd /tmp/app
go run /work/mkzip.go /tmp/hello example.com/hello@v1.1.0 /tmp/hello11.zip
curl -fsS -u ci:smoke-pass -X PUT --data-binary @/tmp/hello11.zip "$REG/go-hosted/example.com/hello/@v/v1.1.0.zip"
go list -m -versions example.com/hello | grep -q v1.1.0
id=$(asset 'repository=go-hosted&name=example.com/hello&version=v1.1.0' '\.mod')
test "$(status -u admin:smoke-pass -X DELETE "$API/assets/$id")" = 204
test "$(status "$REG/go-hosted/example.com/hello/@v/v1.1.0.zip")" = 404
test "$(status "$REG/go-hosted/example.com/hello/@v/v1.1.0.info")" = 404
if go list -m -versions example.com/hello | grep -q v1.1.0; then echo "FAIL: v1.1.0 is still listed"; exit 1; fi
go list -m -versions example.com/hello | grep -q v1.0.0

echo "== a group whose proxy is down: 502, and what GOPROXY does with it"
test "$(status "$REG/go-broken/rsc.io/quote/@v/list")" = 502
test "$(status "$REG/go-broken/example.com/hello/@v/list")" = 200
if GOPROXY="$REG/go-broken,direct" go list -m -versions rsc.io/quote > /tmp/comma.log 2>&1; then cat /tmp/comma.log; echo "FAIL: a comma went on after a 502"; exit 1; fi
grep -q '502' /tmp/comma.log || { cat /tmp/comma.log; echo "FAIL: no 502 from the group"; exit 1; }
GOPROXY="$REG/go-broken|direct" go list -m -versions rsc.io/quote | grep -q v1.5.2
echo "GO CLIENT OK"
EOF

docker network create "$net" >/dev/null
start_postgres
docker update --memory 512m --memory-swap 512m "$name-pg" >/dev/null
docker run -d --name "$name" --network "$net" -e COMMANDCENTER_DATABASE_URL="$pg_url" --network-alias commandcenter --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" -p 127.0.0.1:18084:8081 commandcenter:smoke-go >/dev/null

ready=0
for i in $(seq 1 30); do
  if curl -fs http://localhost:18084/service/rest/v1/status >/dev/null; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "FAIL: commandcenter did not become ready"; docker logs "$name"; exit 1
fi

docker run --rm --name "$name-go" --network "$net" --memory 2g -v "$root/$work:/work:ro" golang:1.27 sh /work/run.sh

echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "GO SMOKE OK"
