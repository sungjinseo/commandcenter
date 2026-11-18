#!/usr/bin/env bash
# Starts commandcenter with --memory 128m and GOMEMLIMIT=80MiB and sends 16,
# then 64, concurrent readers at each document a request builds or merges: the
# largest of its kind a public registry has (synthetic upstream, test/load/readers)
# or a hosted package of 1500 versions (synthetic rows written into PostgreSQL).
# The server is restarted before each run, so memory.peak is the run's. A run
# fails when the server is OOM-killed, stops, or answers anything but 200.
# About 15 minutes.
#   READERS_ONLY  run only the scenarios whose names match this regexp
#   READERS_CLIENTS  client counts (default "16 64")
#   READERS_CONDA  entries of the conda proxy's repodata.json (default 50000,
#                  20 MB; conda-forge's linux-64 has about 700000, 470 MB)
#   READERS_NAME  the server container's name (and prefix of the others')
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/load/.work-readers-$$"
net=commandcenter-readers-$$
name=${READERS_NAME:-commandcenter-readers-$$}
. test/postgres.sh

cleanup() {
  docker rm -f -v "$name" "$name-pg" "$name-up" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup
rm -rf "$work" && mkdir -p "$work"

docker build -q -t commandcenter:readers . >/dev/null
hash="$(printf 'readers-pass\n' | docker run --rm -i commandcenter:readers hash-password)"
docker run --rm --memory 2g -v "$root:/src" -w /src -v commandcenter-gomod:/go/pkg/mod -v commandcenter-gocache:/root/.cache/go-build \
  -e CGO_ENABLED=0 golang:1.27 go build -buildvcs=false -o "$work/readers" ./test/load/readers

up=http://upstream:8000/
small=http://upstream:8000/small/
cat > "$work/config.yaml" <<EOF
baseURL: "http://commandcenter:8081"
users:
  - { name: admin, passwordHash: "$hash", roles: [admin] }
repositories:
  - { name: pypi-proxy,   format: pypi,        type: proxy,  remote: "$up" }
  - { name: pypi-hosted,  format: pypi,        type: hosted }
  - { name: swift-hosted, format: swift,       type: hosted }
  - { name: cargo-hosted, format: cargo,       type: hosted }
  - { name: go-hosted,    format: go,          type: hosted }
  - { name: hf-proxy,     format: huggingface, type: proxy,  remote: "$up" }
  - { name: tf-proxy,     format: terraform,   type: proxy,  remote: "$up" }
  - { name: npm-proxy,    format: npm,         type: proxy,  remote: "$up" }
  - { name: helm-proxy,   format: helm,        type: proxy,  remote: "$up" }
  - { name: helm-small,   format: helm,        type: proxy,  remote: "$small" }
  - { name: helm-group,   format: helm,        type: group,  members: [helm-small, helm-proxy] }
  - { name: maven-proxy,  format: maven,       type: proxy,  remote: "$up" }
  - { name: maven-small,  format: maven,       type: proxy,  remote: "$small" }
  - { name: maven-group,  format: maven,       type: group,  members: [maven-small, maven-proxy] }
  - { name: r-proxy,      format: r,           type: proxy,  remote: "$up" }
  - { name: r-hosted,     format: r,           type: hosted }
  - { name: r-group,      format: r,           type: group,  members: [r-hosted, r-proxy] }
  - { name: conda-proxy,  format: conda,       type: proxy,  remote: "$up" }
  - { name: conda-small,  format: conda,       type: proxy,  remote: "$small" }
  - { name: conda-group,  format: conda,       type: group,  members: [conda-small, conda-proxy] }
  - { name: conda-merge,  format: conda,       type: group,  members: [conda-small, conda-proxy], mergeMembers: true }
EOF

docker network create "$net" >/dev/null
start_postgres
docker update --memory 1g --memory-swap 1g "$name-pg" >/dev/null
docker run -d --name "$name-up" --network "$net" --network-alias upstream --memory 512m -v "$root/$work:/w:ro" \
  golang:1.27 /w/readers -serve :8000 -conda "${READERS_CONDA:-50000}" >/dev/null
docker run -d --name "$name" --network "$net" -e COMMANDCENTER_DATABASE_URL="$pg_url" --network-alias commandcenter \
  --memory 128m --memory-swap 128m -e GOMEMLIMIT=80MiB \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" commandcenter:readers >/dev/null
reader() {
  docker run --rm --network "$net" --memory 256m -v "$root/$work:/w:ro" golang:1.27 /w/readers -target http://commandcenter:8081 -pass readers-pass "$@"
}
# The health check runs every 30 s: readers -wait polls the status instead.
healthy() {
  reader -wait || { echo "FAIL: commandcenter does not answer"; docker logs --tail 20 "$name"; exit 1; }
}
healthy

# Hosted packages of 1500 versions, written as rows: Swift releases with
# signature and metadata (3 KiB a row), Cargo index lines (2 KiB, aws-sdk-s3's),
# PyPI files (3 a release, with their files and core metadata sidecars, but
# none for the newest release, whose blobs do not exist) and Go modules.
docker exec -i "$name-pg" psql -q -v ON_ERROR_STOP=1 -U postgres -d cc >/dev/null <<'EOF'
INSERT INTO packages(repo, format, name, version, attrs, created_at)
SELECT 'swift-hosted', 'swift', 'acme.big', '1.' || i || '.0',
  '{"id":"acme.big","checksum":"' || md5(i::text) || md5(i::text) || '","size":123456,"signing":{"signatureBase64Encoded":"'
  || repeat('QUJD', 500) || '","signatureFormat":"cms-1.0.0"},"metadata":{"description":"' || repeat('d', 800)
  || '","licenseURL":"https://example.com/LICENSE","readmeURL":"https://example.com/README","repositoryURLs":["https://github.com/acme/big"]},'
  || '"publishedAt":"2026-01-01T00:00:00Z","dependencies":[{"name":"swift-nio","url":"https://github.com/apple/swift-nio","requirement":"from: 2.0.0"}]}',
  1700000000000 + i
FROM generate_series(0, 1499) i;
INSERT INTO packages(repo, format, name, version, attrs, created_at)
SELECT 'cargo-hosted', 'cargo', 'bigcrate', '1.' || i || '.0',
  '{"name":"bigcrate","vers":"1.' || i || '.0","deps":['
  || repeat('{"name":"aws-smithy-runtime","req":"^1.7.1","features":["client"],"optional":false,"default_features":true,"target":null,"kind":"normal"},', 13)
  || '{"name":"tokio","req":"^1","features":[],"optional":false,"default_features":true,"target":null,"kind":"dev"}],"cksum":"'
  || md5(i::text) || md5(i::text) || '","features":{"default":["rustls","rt-tokio"],"rustls":["aws-smithy-runtime/tls-rustls"],"rt-tokio":["aws-smithy-runtime/rt-tokio"]},"yanked":false,"v":2}',
  1700000000000 + i
FROM generate_series(0, 1499) i;
INSERT INTO packages(repo, format, name, version, attrs, created_at)
SELECT 'pypi-hosted', 'pypi', 'bigpkg', f, '{"filename":"' || f || '","version":"1.' || i || '.0","sha256":"' || md5(f) || md5(f)
  || '","requires_python":">=3.8","metadata_sha256":"' || md5(f || 'm') || md5(f || 'm') || '"}', 1700000000000 + i
FROM generate_series(0, 1499) i, LATERAL (VALUES ('bigpkg-1.' || i || '.0.tar.gz'), ('bigpkg-1.' || i || '.0-py3-none-any.whl'),
  ('bigpkg-1.' || i || '.0-cp312-cp312-manylinux_2_17_x86_64.whl')) v(f);
INSERT INTO assets(repo, path, blob_sha256, size, content_type, created_at, last_modified)
SELECT 'pypi-hosted', 'packages/bigpkg/' || version || s, md5(version || s) || md5(version || s), 100000, 'application/octet-stream', created_at, created_at
FROM packages, (VALUES (''), ('.metadata')) x(s) WHERE repo = 'pypi-hosted' AND version NOT LIKE 'bigpkg-1.1499.0%';
INSERT INTO assets(repo, path, blob_sha256, size, content_type, created_at, last_modified)
SELECT 'go-hosted', 'example.com/big/@v/v1.' || i || '.0' || s, md5(i || s) || md5(i || s), 1000, 'application/octet-stream', 1700000000000 + i, 1700000000000 + i
FROM generate_series(0, 1499) i, (VALUES ('.zip'), ('.mod'), ('.info')) x(s);
EOF

swift=application/vnd.swift.registry.v1+json
# name | path | Accept | proxy to invalidate before each wave (3 waves of one read per client), or - for 10 s of reads
scenarios="
pypi-json-proxy|/repository/pypi-proxy/pypi/bigpy/json||-
pypi-json-hosted|/repository/pypi-hosted/pypi/bigpkg/json||-
pypi-simple-hosted|/repository/pypi-hosted/simple/bigpkg/||-
swift-list-hosted|/repository/swift-hosted/acme/big|$swift|-
swift-release-hosted|/repository/swift-hosted/acme/big/1.0.0|$swift|-
cargo-index-hosted|/repository/cargo-hosted/bi/gc/bigcrate||-
go-list-hosted|/repository/go-hosted/example.com/big/@v/list||-
hf-tree-proxy|/repository/hf-proxy/api/models/acme/big/tree/main||-
tf-versions-proxy|/repository/tf-proxy/v1/providers/acme/big/versions||-
npm-proxy|/repository/npm-proxy/bignpm||-
helm-index-proxy|/repository/helm-proxy/index.yaml||helm-proxy
helm-index-group|/repository/helm-group/index.yaml||helm-proxy
maven-metadata-group|/repository/maven-group/com/acme/big/maven-metadata.xml||maven-proxy
r-packages-rds-group|/repository/r-group/src/contrib/PACKAGES.rds||r-proxy
r-packages-gz-group|/repository/r-group/src/contrib/PACKAGES.gz||r-proxy
conda-repodata-group|/repository/conda-group/linux-64/repodata.json||conda-proxy
conda-repodata-merge|/repository/conda-merge/linux-64/repodata.json||conda-proxy
"

# The server's cgroup (systemd or cgroupfs driver) read every 0.2 s while a run
# lasts, as a killed container's cgroup is gone: memory.peak, which counts the
# page cache of the files the server reads and writes (reclaimable), and the
# largest anon (heap and stacks) and sock (socket buffers) of memory.stat.
# They are in $work/mem.
sample_mem() {
  local id d; id="$(docker inspect -f '{{.Id}}' "$name")"
  for d in "/sys/fs/cgroup/system.slice/docker-$id.scope" "/sys/fs/cgroup/docker/$id"; do
    [ -e "$d/memory.peak" ] && break
  done
  local anon=0 sock=0 a s p
  echo "0 0 0" > "$work/mem"
  while p="$(cat "$d/memory.peak" 2>/dev/null)" && read -r a s < <(awk '$1 == "anon" {a = $2} $1 == "sock" {s = $2} END {print a, s}' "$d/memory.stat" 2>/dev/null) && [ -n "$a" ]; do
    [ "$a" -gt "$anon" ] && anon=$a
    [ "$s" -gt "$sock" ] && sock=$s
    echo "$(( p >> 20 )) $(( anon >> 20 )) $(( sock >> 20 ))" > "$work/mem"
    sleep 0.2
  done
}
measure() { sample_mem & sampler=$!; }
measured() { sleep 0.5; kill "$sampler" 2>/dev/null || true; wait "$sampler" 2>/dev/null || true; read -r peak anon sock < "$work/mem"; }

rc=0
printf '%-22s %7s %9s %9s %9s %8s  %s\n' scenario clients peak anon sock result reads
docker restart "$name" >/dev/null && healthy
measure; sleep 1; measured
printf '%-22s %7s %5s MiB %5s MiB %5s MiB\n' "(started, idle)" 0 "$peak" "$anon" "$sock"
while IFS='|' read -r scen path acc inval; do
  [ -n "$scen" ] || continue
  [[ "$scen" =~ ${READERS_ONLY:-.} ]] || continue
  for n in ${READERS_CLIENTS:-16 64}; do
    docker restart "$name" >/dev/null && healthy
    args=(-get "$path" -accept "$acc" -clients "$n")
    if [ "$inval" = - ]; then
      reader -get "$path" -accept "$acc" -clients 1 -seconds 1 >/dev/null 2>&1 || true # caches the upstream document
      docker restart "$name" >/dev/null && healthy
    else
      args+=(-waves 3 -invalidate "$inval")
    fi
    measure
    out="$(reader "${args[@]}" 2>&1)" && ok=1 || ok=0
    measured
    result=survived
    if [ "$(docker inspect -f '{{.State.OOMKilled}}' "$name")" = true ]; then
      result=OOM; rc=1; peak=limit; anon=">$anon" sock=">$sock"
    elif [ "$(docker inspect -f '{{.State.Running}}' "$name")" != true ]; then
      result=stopped; rc=1
    elif [ "$ok" = 0 ]; then
      result=errors; rc=1
    fi
    printf '%-22s %7s %5s MiB %5s MiB %5s MiB %8s  %s\n' "$scen" "$n" "$peak" "$anon" "$sock" "$result" "$(echo "$out" | tail -1)"
  done
done <<< "$scenarios"
[ "$rc" = 0 ] && echo "OK: every run survived" || echo "FAIL: some runs did not survive"
exit "$rc"
