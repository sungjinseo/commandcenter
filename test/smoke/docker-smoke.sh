#!/usr/bin/env bash
# Builds the image, starts commandcenter with hosted docker repositories
# (writePolicy allow and allow_once), proxies of Docker Hub, ghcr.io and
# quay.io and a group over them, then pushes and pulls with the clients CI
# uses:
#   1. the host's docker (localhost registries are plain HTTP for its daemon):
#      login with a password and with an API token, push, pull, re-tag (a
#      cross-repository mount), an unauthenticated push and an allow_once
#      overwrite refused, a layer larger than the server's memory, a
#      multi-arch build, a pull through the group and a push of an image
#      built on a base that came through it
#   2. crane, skopeo, kaniko's executor, oras, helm 3 and 4 and regctl, each
#      from its image, in commandcenter's network namespace (so the registry
#      is localhost:18086 for them as well): copy, re-tag, delete, catalog and
#      tags, image indexes, large layers streamed and in chunks, proxies and
#      the group by tag and by digest, a build with a layer cache, OCI
#      artifacts with referrers, charts
#   3. the OCI distribution conformance suite (pull, push, content management)
#   4. the registry API with curl (challenge, token, pagination, delete)
# Finally prints the container's memory usage. The first run pulls about
# 1 GiB of client images; every run fetches about 10 MiB through the proxies,
# six manifests of it from Docker Hub, which limits anonymous pulls by
# address (100 an hour when this was written).
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-docker-$$"
net=commandcenter-docker-smoke-$$
name=commandcenter-docker-smoke-$$
. test/postgres.sh
port=18086
reg="localhost:$port"
big=160 # MiB of the large layer: more than the server's 128 MiB
export DOCKER_CONFIG="$root/$work/dockercfg" # keep the user's docker login store untouched

crane=gcr.io/go-containerregistry/crane/debug:v0.22.1
skopeo=quay.io/skopeo/stable:v1.22.3
kaniko=gcr.io/kaniko-project/executor:v1.24.0
oras=ghcr.io/oras-project/oras:v1.3.6
helm="alpine/helm:3.22.1 alpine/helm:4.3.1"
regctl=ghcr.io/regclient/regctl:v0.11.7-alpine
conformance=ghcr.io/opencontainers/distribution-spec/conformance:v1.1.1

# The images the host's daemon holds under the test registry, by name: an
# image id may carry the user's own tags too.
host_images() {
  docker images --digests --format '{{.Repository}}:{{.Tag}} {{.Repository}}@{{.Digest}}' 2>/dev/null |
    awk -v r="$reg/" 'index($1, r) == 1 { print ($1 ~ /:<none>$/ ? $2 : $1) }'
}

cleanup() {
  docker rm -fv "$name" "$name-pg" "$name-c" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  host_images | xargs -r docker rmi >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup
rm -rf "$work" && mkdir -p "$work/dockercfg" "$work/img" "$work/app" "$work/kaniko/ctx" "$work/kaniko/auth"

docker build -q -t commandcenter:smoke-docker . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i commandcenter:smoke-docker hash-password)"

# The registry is localhost:$port for every client, so the token realm of
# the challenge (baseURL) is one they all reach: the server listens on $port
# inside the container too.
cat > "$work/config.yaml" <<EOF
listen: ":$port"
anonymousRead: true
baseURL: "http://$reg"
users:
  - { name: ci, passwordHash: "$hash", roles: [deployer] }
roles:
  deployer: { write: [docker-hosted, docker-once] }
repositories:
  - { name: docker-hosted, format: docker, type: hosted, writePolicy: allow }
  - { name: docker-once,   format: docker, type: hosted, writePolicy: allow_once }
  - { name: docker-hub,    format: docker, type: proxy,  remote: "https://registry-1.docker.io" }
  - { name: docker-ghcr,   format: docker, type: proxy,  remote: "https://ghcr.io" }
  - { name: docker-quay,   format: docker, type: proxy,  remote: "https://quay.io" }
  - { name: docker-all,    format: docker, type: group,  members: [docker-hosted, docker-hub, docker-ghcr, docker-quay] }
EOF

# Two tiny images and the large layer, built without any network access.
printf 'hello one\n' > "$work/img/hello.txt"
printf 'FROM scratch\nCOPY hello.txt /hello.txt\n' > "$work/img/Dockerfile"
docker build -q -t "$reg/docker-hosted/smoke/hello:1.0" "$work/img" >/dev/null
tar -C "$work/img" -cf "$work/hello.tar" hello.txt
printf 'hello two\n' > "$work/img/hello.txt"
docker build -q -t "$reg/docker-hosted/smoke/hello:2.0" "$work/img" >/dev/null
head -c "$((big << 20))" /dev/urandom > "$work/big.bin"
tar -C "$work" -cf "$work/big.tar" big.bin && rm "$work/big.bin"

docker network create "$net" >/dev/null
start_postgres
docker run -d --name "$name" --network "$net" -e COMMANDCENTER_DATABASE_URL="$pg_url" --network-alias commandcenter --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" -p "127.0.0.1:$port:$port" commandcenter:smoke-docker >/dev/null

ready=0
for i in $(seq 1 30); do
  if curl -fs "http://$reg/service/rest/v1/status" >/dev/null; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "FAIL: commandcenter did not become ready"; docker logs "$name"; exit 1
fi

# client <docker run arguments>: a tool from its image in commandcenter's
# network namespace, with a home it can write to whoever it runs as.
client() {
  docker run --rm --network "container:$name" --memory 1g -e HOME=/tmp -e REG="$reg" -v "$root/$work:/work:ro" "$@"
}
# requests <method> <path prefix>: how many such requests the server answered.
requests() { docker logs "$name" 2>&1 | grep -c "\"method\":\"$1\",\"path\":\"$2" || true; }

echo "== ping answers the token challenge"
curl -s -o /dev/null -D "$work/ping.h" "http://$reg/v2/"
grep -q "^HTTP/1.1 401" "$work/ping.h"
grep -qi "^www-authenticate: Bearer realm=\"http://$reg/v2/token\",service=\"$reg\"" "$work/ping.h"
grep -qi "^docker-distribution-api-version: registry/2.0" "$work/ping.h"

echo "== docker: an unauthenticated push is refused"
if docker push "$reg/docker-hosted/smoke/hello:1.0" >"$work/anon.log" 2>&1; then
  echo "FAIL: anonymous push succeeded"; exit 1
fi
grep -qi "authentication required\|unauthorized\|denied" "$work/anon.log" || { cat "$work/anon.log"; exit 1; }

echo "== docker: login with the password, push, push again"
printf 'smoke-pass' | docker login "$reg" -u ci --password-stdin >/dev/null 2>&1
docker push -q "$reg/docker-hosted/smoke/hello:1.0" >/dev/null
docker push -q "$reg/docker-hosted/smoke/hello:2.0" >/dev/null
docker push -q "$reg/docker-hosted/smoke/hello:2.0" >/dev/null # the same manifest again is fine

echo "== docker: re-tag into another image and another repository (mounted, not uploaded)"
docker tag "$reg/docker-hosted/smoke/hello:1.0" "$reg/docker-hosted/smoke/copy:1.0"
docker push "$reg/docker-hosted/smoke/copy:1.0" >"$work/retag.log" 2>&1 || { cat "$work/retag.log"; exit 1; }
grep -q "Mounted from docker-hosted/smoke/hello" "$work/retag.log" || { cat "$work/retag.log"; exit 1; }

echo "== docker: allow_once refuses a different image under an existing tag"
docker tag "$reg/docker-hosted/smoke/hello:1.0" "$reg/docker-once/smoke/hello:1.0"
docker push -q "$reg/docker-once/smoke/hello:1.0" >/dev/null
docker push -q "$reg/docker-once/smoke/hello:1.0" >/dev/null # the same image again is fine
docker tag "$reg/docker-hosted/smoke/hello:2.0" "$reg/docker-once/smoke/hello:1.0"
if docker push "$reg/docker-once/smoke/hello:1.0" >"$work/once.log" 2>&1; then
  echo "FAIL: allow_once tag overwrite succeeded"; exit 1
fi
grep -qi "allow_once\|denied" "$work/once.log" || { cat "$work/once.log"; exit 1; }

echo "== docker: login with an API token"
docker logout "$reg" >/dev/null
token="$(docker exec "$name" commandcenter token create -config /etc/commandcenter/config.yaml -user ci)"
printf '%s' "$token" | docker login "$reg" -u ci --password-stdin >/dev/null 2>&1
docker tag "$reg/docker-hosted/smoke/hello:2.0" "$reg/docker-hosted/smoke/hello:token"
docker push -q "$reg/docker-hosted/smoke/hello:token" >/dev/null

echo "== docker: a ${big} MiB layer"
docker import "$work/big.tar" "$reg/docker-hosted/smoke/big:1.0" >/dev/null
docker push -q "$reg/docker-hosted/smoke/big:1.0" >/dev/null

# A multi-arch build needs the containerd image store (the default of new
# installations); crane builds an index below either way.
if docker info -f '{{.DriverStatus}}' | grep -q io.containerd.snapshotter; then
  echo "== docker: a multi-arch build, pushed as an index with its attestations"
  docker build -q --platform linux/amd64,linux/arm64 -t "$reg/docker-hosted/smoke/multi:1.0" "$work/img" >/dev/null
  docker push -q "$reg/docker-hosted/smoke/multi:1.0" >/dev/null
  docker buildx imagetools inspect "$reg/docker-hosted/smoke/multi:1.0" >"$work/multi.txt"
  grep -q "linux/amd64" "$work/multi.txt" && grep -q "linux/arm64" "$work/multi.txt" || { cat "$work/multi.txt"; exit 1; }
fi

echo "== docker: anonymous pull"
docker logout "$reg" >/dev/null
host_images | xargs -r docker rmi >/dev/null
docker pull -q "$reg/docker-hosted/smoke/hello:1.0" >/dev/null
docker pull -q "$reg/docker-hosted/smoke/hello:token" >/dev/null
docker create --name "$name-c" "$reg/docker-hosted/smoke/hello:token" /hello.txt >/dev/null # scratch image: nothing runs
docker cp "$name-c:/hello.txt" "$work/pulled.txt"
docker rm "$name-c" >/dev/null
test "$(cat "$work/pulled.txt")" = "hello two"

cat > "$work/crane.sh" <<'EOF'
set -eu
H=$REG/docker-hosted O=$REG/docker-once A=$REG/docker-all
content() { crane export "$1" - | tar -xO "$2"; }
crane auth login "$REG" -u ci -p smoke-pass >/dev/null 2>&1

echo "== crane: catalog, tags, digest"
crane catalog "$REG" > /tmp/catalog
grep -qx docker-hosted/smoke/hello /tmp/catalog && grep -qx docker-all/smoke/hello /tmp/catalog
crane ls "$H/smoke/hello" | grep -qx 2.0
d=$(crane digest "$H/smoke/hello:2.0")

echo "== crane: re-tag, copy to another repository (mounted), delete by digest"
crane tag "$H/smoke/hello:2.0" stable >/dev/null 2>&1
test "$(crane digest "$H/smoke/hello:stable")" = "$d"
crane -v copy "$H/smoke/hello:2.0" "$O/crane/hello:2.0" 2>/tmp/copy.log
grep -q 'mount=' /tmp/copy.log
if grep -q -- '--> PATCH' /tmp/copy.log; then echo "FAIL: crane copy uploaded a blob the registry has"; exit 1; fi
crane delete "$O/crane/hello@$d"
if crane manifest "$O/crane/hello:2.0" >/dev/null 2>&1; then echo "FAIL: deleted manifest still answers"; exit 1; fi

echo "== crane: the large layer, streamed in one PATCH, and what docker pushed, read back"
want=$(tar -xOf /work/big.tar big.bin | sha256sum)
crane append -f /work/big.tar -t "$H/crane/big:1.0" >/dev/null 2>&1
test "$(content "$H/crane/big:1.0" big.bin | sha256sum)" = "$want"
test "$(content "$H/smoke/big:1.0" big.bin | sha256sum)" = "$want"

echo "== crane: an index of two platforms"
crane append -f /work/hello.tar -t "$H/crane/hello:raw" >/dev/null 2>&1
crane mutate --set-platform linux/amd64 -t "$H/crane/hello:amd64" "$H/crane/hello:raw" >/dev/null 2>&1
crane mutate --set-platform linux/arm64 -t "$H/crane/hello:arm64" "$H/crane/hello:raw" >/dev/null 2>&1
crane index append -t "$H/crane/hello:multi" -m "$H/crane/hello:amd64" -m "$H/crane/hello:arm64" >/dev/null 2>&1
test "$(crane digest --platform linux/arm64 "$H/crane/hello:multi")" = "$(crane digest "$H/crane/hello:arm64")"
test "$(content "$H/crane/hello:multi" hello.txt)" = "hello one"

echo "== crane: proxies of Docker Hub, ghcr.io and quay.io, by tag and by digest"
if ! hub=$(crane digest "$REG/docker-hub/library/busybox:1.36.1" 2>/tmp/hub.log); then
  cat /tmp/hub.log
  if grep -q TOOMANYREQUESTS /tmp/hub.log; then echo "FAIL: Docker Hub's anonymous pull limit is used up for this address; try again later"; fi
  exit 1
fi
test "$(crane digest "$REG/docker-hub/busybox:1.36.1")" = "$hub" # an official image, without library/
crane manifest "$REG/docker-hub/library/busybox@$hub" | grep -q '"manifests"'
ghcr=$(crane digest "$REG/docker-ghcr/containerd/busybox:1.36")
crane manifest "$REG/docker-ghcr/containerd/busybox@$ghcr" >/dev/null
# quay.io lists 100 tags a page: the proxy follows the pages
tags=$(crane ls "$REG/docker-quay/prometheus/prometheus" | wc -l)
test "$tags" -gt 100 || { echo "FAIL: $tags tags of quay.io/prometheus/prometheus"; exit 1; }

echo "== crane: the group answers from the hosted member and from each proxy"
test "$(crane digest "$A/smoke/hello:2.0")" = "$d"
test "$(crane digest "$A/library/busybox:1.36.1")" = "$hub"
test "$(crane digest "$A/containerd/busybox:1.36")" = "$ghcr"
crane manifest "$A/containerd/busybox@$ghcr" >/dev/null

echo "== crane: copy an image through the group; a second copy mounts what the proxy cached"
crane copy --platform linux/amd64 "$A/containerd/busybox:1.36" "$H/mirror/busybox:1.36" >/dev/null 2>&1
crane -v copy --platform linux/amd64 "$A/containerd/busybox:1.36" "$O/mirror/busybox:1.36" 2>/tmp/mount.log
grep -q 'mount=' /tmp/mount.log
if grep -q -- '--> PATCH' /tmp/mount.log; then echo "FAIL: a blob the proxy has cached was uploaded"; exit 1; fi
crane export "$O/mirror/busybox:1.36" - | tar -t | grep -qx bin/busybox
EOF

cat > "$work/skopeo.sh" <<'EOF'
set -eu
export REGISTRY_AUTH_FILE=/tmp/auth.json
H=docker://$REG/docker-hosted O=docker://$REG/docker-once
skopeo login --tls-verify=false -u ci -p smoke-pass "$REG" >/dev/null

echo "== skopeo: inspect, copy an index with every platform, list tags"
skopeo inspect --tls-verify=false "$H/smoke/hello:2.0" | grep -q '"Digest": "sha256:'
skopeo copy -q --all --src-tls-verify=false --dest-tls-verify=false "$H/crane/hello:multi" "$O/skopeo/hello:multi"
skopeo inspect --raw --tls-verify=false "$O/skopeo/hello:multi" | grep -q arm64
skopeo list-tags --tls-verify=false "$O/skopeo/hello" | grep -q '"multi"'

echo "== skopeo: pull to a directory, push it as Docker schema 2 and as OCI"
skopeo copy -q --src-tls-verify=false "$H/crane/hello:amd64" dir:/tmp/hello
skopeo copy -q --dest-tls-verify=false --format v2s2 dir:/tmp/hello "$H/skopeo/hello:v2s2"
skopeo copy -q --dest-tls-verify=false --format oci dir:/tmp/hello "$H/skopeo/hello:oci"
skopeo inspect --raw --tls-verify=false "$H/skopeo/hello:v2s2" | grep -q 'vnd.docker.distribution.manifest.v2+json'
skopeo inspect --raw --tls-verify=false "$H/skopeo/hello:oci" | grep -q 'vnd.oci.image.manifest.v1+json'

echo "== skopeo: delete by tag"
skopeo delete --tls-verify=false "$H/skopeo/hello:oci"
skopeo list-tags --tls-verify=false "$H/skopeo/hello" > /tmp/tags
grep -q '"v2s2"' /tmp/tags
if grep -q '"oci"' /tmp/tags; then echo "FAIL: deleted tag still listed"; exit 1; fi

echo "== skopeo: another platform of an image behind the group"
skopeo inspect --tls-verify=false --override-arch arm64 "docker://$REG/docker-all/containerd/busybox:1.36" | grep -q '"Architecture": "arm64"'
EOF

cat > "$work/oras.sh" <<'EOF'
set -eu
H=$REG/docker-hosted O=$REG/docker-once
cd /tmp
oras login --plain-http -u ci -p smoke-pass "$REG" >/dev/null 2>&1

echo "== oras: push and pull an artifact"
echo '{"hello":"oras"}' > doc.json
oras push --plain-http "$H/oras/doc:1.0" --artifact-type application/vnd.smoke.doc+json doc.json:application/json >/dev/null
oras manifest fetch --plain-http "$H/oras/doc:1.0" | grep -q '"artifactType":"application/vnd.smoke.doc+json"'
mkdir out && oras pull --plain-http -o out "$H/oras/doc:1.0" >/dev/null && cmp out/doc.json doc.json

echo "== oras: attach a referrer and find it (the referrers tag schema: the referrers API answers 404)"
echo sbom > sbom.txt
oras attach --plain-http --artifact-type application/vnd.smoke.sbom "$H/oras/doc:1.0" sbom.txt:text/plain >/dev/null
oras discover --plain-http "$H/oras/doc:1.0" | grep -q application/vnd.smoke.sbom

echo "== oras: copy with its referrers into another repository, tag, delete"
oras cp -r --from-plain-http --to-plain-http "$H/oras/doc:1.0" "$O/oras/doc:1.0" >/dev/null
oras discover --plain-http "$O/oras/doc:1.0" | grep -q application/vnd.smoke.sbom
oras tag --plain-http "$H/oras/doc:1.0" latest >/dev/null
oras repo tags --plain-http "$H/oras/doc" | grep -qx latest
oras manifest delete --plain-http -f "$H/oras/doc:latest" >/dev/null
if oras manifest fetch --plain-http "$H/oras/doc:1.0" >/dev/null 2>&1; then echo "FAIL: deleted artifact still answers"; exit 1; fi
EOF

cat > "$work/helm.sh" <<'EOF'
set -eu
C=oci://$REG/docker-hosted/charts
v=0.$(helm version --template '{{.Version}}' | cut -d. -f1 | tr -d v).0 # a chart version of each helm
cd /tmp
helm create smoke-chart >/dev/null
helm package smoke-chart --version "$v" >/dev/null
helm registry login --plain-http -u ci -p smoke-pass "$REG" >/dev/null 2>&1

echo "== helm $(helm version --short): push a chart, pull it by version and as the newest"
helm push "smoke-chart-$v.tgz" "$C" --plain-http >/dev/null 2>&1
mkdir pulled latest
helm pull "$C/smoke-chart" --version "$v" --plain-http -d pulled >/dev/null 2>&1
cmp "pulled/smoke-chart-$v.tgz" "smoke-chart-$v.tgz"
helm show chart "$C/smoke-chart" --version "$v" --plain-http 2>/dev/null | grep -qx "version: $v"
helm pull "$C/smoke-chart" --plain-http -d latest >/dev/null 2>&1
ls latest | grep -q '^smoke-chart-'
EOF

cat > "$work/regctl.sh" <<'EOF'
set -eu
regctl registry set --tls disabled --blob-chunk 16777216 --blob-max 16777216 "$REG"
printf smoke-pass | regctl registry login -u ci --pass-stdin "$REG" >/dev/null

echo "== regctl: the large layer in 16 MiB chunks"
want="sha256:$(sha256sum /work/big.tar | cut -d' ' -f1)"
test "$(regctl blob put "$REG/docker-hosted/regctl/big" < /work/big.tar)" = "$want"
test "sha256:$(regctl blob get "$REG/docker-hosted/regctl/big" "$want" | sha256sum | cut -d' ' -f1)" = "$want"
EOF

client --entrypoint /busybox/sh "$crane" /work/crane.sh
client --entrypoint sh "$skopeo" /work/skopeo.sh

echo "== kaniko: build on a base pulled through the group and push with an API token; the second build uses the cached layer"
printf 'FROM %s/docker-all/containerd/busybox:1.36\nCOPY hello.txt /hello.txt\nRUN cat /hello.txt > /built.txt && echo kaniko >> /built.txt\n' "$reg" > "$work/kaniko/ctx/Dockerfile"
cp "$work/img/hello.txt" "$work/kaniko/ctx/"
printf '{"auths":{"%s":{"auth":"%s"}}}\n' "$reg" "$(printf 'ci:%s' "$token" | base64 | tr -d '\n')" > "$work/kaniko/auth/config.json"
for tag in 1.0 1.1; do
  # kaniko unpacks the base image into its own root: it runs as root
  docker run --rm --network "container:$name" --memory 1g -v "$root/$work/kaniko/ctx:/workspace:ro" -v "$root/$work/kaniko/auth:/kaniko/.docker:ro" "$kaniko" \
    --context /workspace --destination "$reg/docker-hosted/kaniko/app:$tag" --insecure --insecure-pull \
    --cache=true --cache-repo "$reg/docker-hosted/kaniko/cache" >"$work/kaniko-$tag.log" 2>&1 || { cat "$work/kaniko-$tag.log"; exit 1; }
done
grep -q "Using caching version of cmd: RUN" "$work/kaniko-1.1.log" || { cat "$work/kaniko-1.1.log"; exit 1; }

client --entrypoint sh "$oras" /work/oras.sh
for image in $helm; do client --entrypoint sh "$image" /work/helm.sh; done
client --entrypoint sh "$regctl" /work/regctl.sh
test "$(requests PATCH /v2/docker-hosted/regctl/big/blobs/uploads/)" -ge "$((big / 16))"

echo "== OCI distribution conformance: pull, push, content management"
# Content discovery is left out: the referrers API is not implemented (404).
client -w /tmp -e OCI_ROOT_URL="http://$reg" -e OCI_NAMESPACE=docker-hosted/conformance/a -e OCI_CROSSMOUNT_NAMESPACE=docker-hosted/conformance/b \
  -e OCI_USERNAME=ci -e OCI_PASSWORD=smoke-pass -e OCI_TEST_PULL=1 -e OCI_TEST_PUSH=1 -e OCI_TEST_CONTENT_MANAGEMENT=1 \
  -e OCI_HIDE_SKIPPED_WORKFLOWS=1 -e OCI_REPORT_DIR=/tmp "$conformance" >"$work/conformance.log" 2>&1 || { tail -60 "$work/conformance.log"; exit 1; }
tr -d '\033' < "$work/conformance.log" | grep -a "Passed" | sed 's/\[[0-9;]*m//g'

echo "== docker: pull through the group by tag and by digest, run it"
digest="$(docker pull "$reg/docker-all/library/busybox:1.36.1" | awk '/^Digest:/ { print $2 }')"
docker rmi "$reg/docker-all/library/busybox:1.36.1" >/dev/null
docker pull -q "$reg/docker-all/library/busybox@$digest" >/dev/null
test "$(docker run --rm --name "$name-c" --network none --memory 64m "$reg/docker-all/library/busybox@$digest" echo ran)" = ran

echo "== docker: push an image built on a base that came through the group"
docker pull -q "$reg/docker-all/containerd/busybox:1.36" >/dev/null
printf 'FROM %s/docker-all/containerd/busybox:1.36\nCOPY hello.txt /hello.txt\n' "$reg" > "$work/app/Dockerfile"
cp "$work/img/hello.txt" "$work/app/"
docker build -q -t "$reg/docker-hosted/smoke/app:1.0" "$work/app" >/dev/null
printf 'smoke-pass' | docker login "$reg" -u ci --password-stdin >/dev/null 2>&1
docker push -q "$reg/docker-hosted/smoke/app:1.0" >/dev/null
docker logout "$reg" >/dev/null
docker rmi "$reg/docker-hosted/smoke/app:1.0" >/dev/null

echo "== docker: pull and run what was pushed: that image, kaniko's, the large layer"
docker pull -q "$reg/docker-hosted/smoke/app:1.0" >/dev/null
test "$(docker run --rm --name "$name-c" --network none --memory 64m "$reg/docker-hosted/smoke/app:1.0" cat /hello.txt)" = "hello two"
docker pull -q "$reg/docker-hosted/kaniko/app:1.1" >/dev/null
docker run --rm --name "$name-c" --network none --memory 64m "$reg/docker-hosted/kaniko/app:1.1" cat /built.txt >"$work/built.txt"
test "$(tr '\n' ' ' < "$work/built.txt")" = "hello two kaniko "
docker pull -q "$reg/docker-hosted/crane/big:1.0" >/dev/null

echo "== registry API: token, tags and catalog pages, delete by digest"
tok="$(curl -fs -u ci:smoke-pass "http://$reg/v2/token?service=$reg&scope=repository:docker-hosted/smoke/hello:pull,push,delete&scope=registry:catalog:*" | sed 's/.*"token":"\([^"]*\)".*/\1/')"
test -n "$tok"
tags="$(curl -fs -H "Authorization: Bearer $tok" "http://$reg/v2/docker-hosted/smoke/hello/tags/list")"
test "$tags" = '{"name":"docker-hosted/smoke/hello","tags":["1.0","2.0","stable","token"]}'
curl -fs -D "$work/page.h" -o "$work/page.json" -H "Authorization: Bearer $tok" "http://$reg/v2/docker-hosted/smoke/hello/tags/list?n=2"
test "$(cat "$work/page.json")" = '{"name":"docker-hosted/smoke/hello","tags":["1.0","2.0"]}'
grep -qi '^link: </v2/docker-hosted/smoke/hello/tags/list?last=2.0&n=2>; rel="next"' "$work/page.h"
catalog="$(curl -fs -H "Authorization: Bearer $tok" "http://$reg/v2/_catalog")"
echo "$catalog" | grep -q '"docker-hosted/smoke/hello"'
echo "$catalog" | grep -q '"docker-hub/library/busybox"' # what a proxy has cached
digest="$(curl -fsI -H "Authorization: Bearer $tok" -H 'Accept: application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.oci.image.index.v1+json' \
  "http://$reg/v2/docker-hosted/smoke/hello/manifests/1.0" | tr -d '\r' | awk 'tolower($1)=="docker-content-digest:"{print $2}')"
test -n "$digest"
# A tag is not deleted on its own (400), a digest goes with every tag on it.
test "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE -H "Authorization: Bearer $tok" "http://$reg/v2/docker-hosted/smoke/hello/manifests/1.0")" = 400
test "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE -H "Authorization: Bearer $tok" "http://$reg/v2/docker-hosted/smoke/hello/manifests/$digest")" = 202
tags="$(curl -fs -H "Authorization: Bearer $tok" "http://$reg/v2/docker-hosted/smoke/hello/tags/list")"
test "$tags" = '{"name":"docker-hosted/smoke/hello","tags":["2.0","stable","token"]}'
docker rmi "$reg/docker-hosted/smoke/hello:1.0" >/dev/null
if docker pull "$reg/docker-hosted/smoke/hello:1.0" >/dev/null 2>&1; then
  echo "FAIL: deleted manifest still pulls"; exit 1
fi

echo "DOCKER CLIENT OK"
echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "DOCKER SMOKE OK"
