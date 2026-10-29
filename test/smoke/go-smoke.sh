#!/usr/bin/env bash
# Builds the image, starts commandcenter, then uses the real go command (golang:1.26) to:
#   1. publish example.com/hello v1.0.0 to go-hosted with PUT (.mod and .zip); a re-upload must get 409
#   2. build an app that needs example.com/hello (hosted) and rsc.io/quote (proxy) through go-all
#   3. list the hosted module's versions through the group
# Finally prints the container's memory usage.
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-go-$$"
net=commandcenter-go-smoke-$$
name=commandcenter-go-smoke-$$
. test/postgres.sh

cleanup() {
  docker rm -f "$name" "$name-pg" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup
rm -rf "$work" && mkdir -p "$work"
cp test/smoke/go/mkzip.go "$work/mkzip.go"

docker build -q -t commandcenter:smoke-go . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i commandcenter:smoke-go hash-password)"

cat > "$work/config.yaml" <<EOF
anonymousRead: true
baseURL: "http://commandcenter:8081"
users:
  - { name: ci, passwordHash: "$hash", roles: [deployer] }
roles:
  deployer: { write: [go-hosted] }
repositories:
  - { name: go-proxy,  format: go, type: proxy,  remote: "https://proxy.golang.org/" }
  - { name: go-hosted, format: go, type: hosted }
  - { name: go-all,    format: go, type: group,  members: [go-hosted, go-proxy], writeMember: go-hosted }
EOF

cat > "$work/run.sh" <<'EOF'
set -eu
REG=http://commandcenter:8081/repository
export GOTOOLCHAIN=local GOFLAGS=-mod=mod GOPATH=/tmp/gopath GOCACHE=/tmp/gocache

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
echo "GO CLIENT OK"
EOF

docker network create "$net" >/dev/null
start_postgres
docker run -d --name "$name" --network "$net" -e COMMANDCENTER_DATABASE_URL="$pg_url" --network-alias commandcenter --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" -p 18084:8081 commandcenter:smoke-go >/dev/null

ready=0
for i in $(seq 1 30); do
  if curl -fs http://localhost:18084/service/rest/v1/status >/dev/null; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "FAIL: commandcenter did not become ready"; docker logs "$name"; exit 1
fi

docker run --rm --network "$net" -v "$root/$work:/work" golang:1.26 sh /work/run.sh

echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "GO SMOKE OK"
