#!/usr/bin/env bash
# Builds the image, starts commandcenter, then uses the host's real docker
# client (localhost registries are plain-HTTP for it by default) to:
#   1. docker login with a password and with an API token
#   2. push two locally built images to docker-hosted (one tag twice) and
#      pull them back; refuse an unauthenticated push
#   3. refuse the second push of a tag to docker-once (writePolicy allow_once)
#   4. check the registry API with curl (ping challenge, token, catalog,
#      tags, delete by digest) and, when installed, with skopeo and crane
# Finally prints the container's memory usage.
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-docker-$$"
net=commandcenter-docker-smoke-$$
name=commandcenter-docker-smoke-$$
port=18086
reg="localhost:$port"
export DOCKER_CONFIG="$root/$work/dockercfg" # keep the user's docker login store untouched

cleanup() {
  docker rm -f "$name" "$name-c" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  docker rmi -f "$reg/docker-hosted/smoke/hello:1.0" "$reg/docker-hosted/smoke/hello:2.0" \
    "$reg/docker-once/smoke/hello:1.0" smoke-hello:1 smoke-hello:2 >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup
rm -rf "$work" && mkdir -p "$work/dockercfg" "$work/img"

docker build -q -t commandcenter:smoke-docker . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i commandcenter:smoke-docker hash-password)"

cat > "$work/config.yaml" <<EOF
anonymousRead: true
baseURL: "http://$reg"
users:
  - { name: ci, passwordHash: "$hash", roles: [deployer] }
roles:
  deployer: { write: [docker-hosted, docker-once] }
repositories:
  - { name: docker-hosted, format: docker, type: hosted, writePolicy: allow }
  - { name: docker-once,   format: docker, type: hosted, writePolicy: allow_once }
EOF

# Two tiny images built without any network access.
printf 'hello one\n' > "$work/img/hello.txt"
printf 'FROM scratch\nCOPY hello.txt /hello.txt\n' > "$work/img/Dockerfile"
docker build -q -t smoke-hello:1 "$work/img" >/dev/null
printf 'hello two\n' > "$work/img/hello.txt"
docker build -q -t smoke-hello:2 "$work/img" >/dev/null

docker network create "$net" >/dev/null
docker run -d --name "$name" --network "$net" --network-alias commandcenter --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" -p "$port:8081" commandcenter:smoke-docker >/dev/null

ready=0
for i in $(seq 1 30); do
  if curl -fs "http://$reg/service/rest/v1/status" >/dev/null; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "FAIL: commandcenter did not become ready"; docker logs "$name"; exit 1
fi

echo "== ping answers the token challenge"
curl -s -o /dev/null -D "$work/ping.h" "http://$reg/v2/"
grep -q "^HTTP/1.1 401" "$work/ping.h"
grep -qi "^www-authenticate: Bearer realm=\"http://$reg/v2/token\",service=\"$reg\"" "$work/ping.h"
grep -qi "^docker-distribution-api-version: registry/2.0" "$work/ping.h"

echo "== unauthenticated push is refused"
docker tag smoke-hello:1 "$reg/docker-hosted/smoke/hello:1.0"
if docker push "$reg/docker-hosted/smoke/hello:1.0" >"$work/anon.log" 2>&1; then
  echo "FAIL: anonymous push succeeded"; exit 1
fi
grep -qi "authentication required\|unauthorized\|denied" "$work/anon.log" || { cat "$work/anon.log"; exit 1; }

echo "== login with the password, push, push again under another tag"
printf 'smoke-pass' | docker login "$reg" -u ci --password-stdin >/dev/null
docker push "$reg/docker-hosted/smoke/hello:1.0" >/dev/null
docker tag smoke-hello:2 "$reg/docker-hosted/smoke/hello:2.0"
docker push "$reg/docker-hosted/smoke/hello:2.0" >/dev/null
docker push "$reg/docker-hosted/smoke/hello:2.0" >/dev/null # the same manifest again is fine

echo "== allow_once refuses a different image under an existing tag"
docker tag smoke-hello:1 "$reg/docker-once/smoke/hello:1.0"
docker push "$reg/docker-once/smoke/hello:1.0" >/dev/null
docker tag smoke-hello:2 "$reg/docker-once/smoke/hello:1.0"
if docker push "$reg/docker-once/smoke/hello:1.0" >"$work/once.log" 2>&1; then
  echo "FAIL: allow_once tag overwrite succeeded"; exit 1
fi
grep -qi "allow_once\|denied" "$work/once.log" || { cat "$work/once.log"; exit 1; }

echo "== login with an API token"
docker logout "$reg" >/dev/null
token="$(docker exec "$name" commandcenter token create -config /etc/commandcenter/config.yaml -user ci)"
printf '%s' "$token" | docker login "$reg" -u ci --password-stdin >/dev/null
docker push "$reg/docker-hosted/smoke/hello:2.0" >/dev/null
docker logout "$reg" >/dev/null

echo "== anonymous pull"
docker rmi -f "$reg/docker-hosted/smoke/hello:1.0" "$reg/docker-hosted/smoke/hello:2.0" >/dev/null
docker pull "$reg/docker-hosted/smoke/hello:1.0" >/dev/null
docker pull "$reg/docker-hosted/smoke/hello:2.0" >/dev/null
docker create --name "$name-c" "$reg/docker-hosted/smoke/hello:2.0" /hello.txt >/dev/null # scratch image: nothing runs
docker cp "$name-c:/hello.txt" "$work/pulled.txt"
docker rm "$name-c" >/dev/null
test "$(cat "$work/pulled.txt")" = "hello two"

echo "== registry API: token, tags, catalog, delete by digest"
tok="$(curl -fs -u ci:smoke-pass "http://$reg/v2/token?service=$reg&scope=repository:docker-hosted/smoke/hello:pull,push,delete&scope=registry:catalog:*" | sed 's/.*"token":"\([^"]*\)".*/\1/')"
test -n "$tok"
tags="$(curl -fs -H "Authorization: Bearer $tok" "http://$reg/v2/docker-hosted/smoke/hello/tags/list")"
echo "$tags"
test "$tags" = '{"name":"docker-hosted/smoke/hello","tags":["1.0","2.0"]}'
catalog="$(curl -fs -H "Authorization: Bearer $tok" "http://$reg/v2/_catalog")"
echo "$catalog"
echo "$catalog" | grep -q '"docker-hosted/smoke/hello"'
digest="$(curl -fsI -H "Authorization: Bearer $tok" -H 'Accept: application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.oci.image.index.v1+json' \
  "http://$reg/v2/docker-hosted/smoke/hello/manifests/1.0" | tr -d '\r' | awk 'tolower($1)=="docker-content-digest:"{print $2}')"
test -n "$digest"
curl -s -o /dev/null -w '%{http_code}\n' -X DELETE -H "Authorization: Bearer $tok" "http://$reg/v2/docker-hosted/smoke/hello/manifests/1.0" | grep -q 400
curl -s -o /dev/null -w '%{http_code}\n' -X DELETE -H "Authorization: Bearer $tok" "http://$reg/v2/docker-hosted/smoke/hello/manifests/$digest" | grep -q 202
tags="$(curl -fs -H "Authorization: Bearer $tok" "http://$reg/v2/docker-hosted/smoke/hello/tags/list")"
test "$tags" = '{"name":"docker-hosted/smoke/hello","tags":["2.0"]}'
docker rmi -f "$reg/docker-hosted/smoke/hello:1.0" >/dev/null
if docker pull "$reg/docker-hosted/smoke/hello:1.0" >/dev/null 2>&1; then
  echo "FAIL: deleted manifest still pulls"; exit 1
fi

if command -v skopeo >/dev/null 2>&1; then
  echo "== skopeo"
  skopeo inspect --tls-verify=false "docker://$reg/docker-hosted/smoke/hello:2.0" | grep -q '"Name"'
  skopeo copy --tls-verify=false --dest-creds "ci:smoke-pass" \
    "docker://$reg/docker-hosted/smoke/hello:2.0" "docker://$reg/docker-hosted/smoke/pulled:1.0"
  skopeo list-tags --tls-verify=false "docker://$reg/docker-hosted/smoke/pulled" | grep -q '"1.0"'
fi
if command -v crane >/dev/null 2>&1; then
  echo "== crane"
  crane ls --insecure "$reg/docker-hosted/smoke/hello" | grep -q '^2.0$'
  crane catalog --insecure "$reg" | grep -q '^docker-hosted/smoke/hello$'
  crane digest --insecure "$reg/docker-hosted/smoke/hello:2.0" | grep -q '^sha256:'
fi

echo "DOCKER CLIENT OK"
echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "DOCKER SMOKE OK"
