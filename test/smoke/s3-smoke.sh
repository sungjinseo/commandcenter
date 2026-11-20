#!/usr/bin/env bash
# Builds the image, starts commandcenter with the S3 API on its own port
# (:9000) and on a host name of the main port (s3.commandcenter:8081), then
# drives s3 repositories with the real aws cli (amazon/aws-cli) and MinIO
# client (minio/mc):
#   1. an S3 access key from the web UI's API
#   2. aws: mb, cp of a small file and of a 40 MiB one (multipart), ls,
#      head-object, presign + curl, sync, cp between buckets, rm, rb
#   3. mc: on the S3 port and on the S3 host (path-style): mb, cp, ls, cat,
#      stat, mirror, rm, rb
#   4. the objects read back through /repository/<bucket>/
# SMOKE_AWS_IMAGE and SMOKE_MC_IMAGE pick other client images.
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
work="test/smoke/.work-s3-$$"
net=commandcenter-s3-smoke-$$
name=commandcenter-s3-smoke-$$
aws_image="${SMOKE_AWS_IMAGE:-amazon/aws-cli:2.22.35}"
mc_image="${SMOKE_MC_IMAGE:-bitnamilegacy/minio-client:2025.4.16}" # mc 2025-04-16 (minio/mc left Docker Hub)
. test/postgres.sh

cleanup() {
  docker rm -f -v "$name" "$name-pg" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup
rm -rf "$work" && mkdir -p "$work"
root="$(cd "$work" && (pwd -W 2>/dev/null || pwd))"

docker build -q -t commandcenter:smoke-s3 . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i commandcenter:smoke-s3 hash-password)"

docker network create "$net" >/dev/null
start_postgres
cat > "$work/config.yaml" <<EOF
baseURL: "http://commandcenter:8081"
users:
  - { name: admin, passwordHash: "$hash", roles: [admin] }
  - { name: ci, passwordHash: "$hash", roles: [deployer] }
roles:
  deployer: { write: [artifacts, logs] }
repositories:
  - { name: artifacts, format: s3, type: hosted }
  - { name: logs, format: s3, type: hosted }
s3:
  host: s3.commandcenter
  listen: ":9000"
EOF
docker run -d --name "$name" --network "$net" -e COMMANDCENTER_DATABASE_URL="$pg_url" --memory 128m \
  -e COMMANDCENTER_SECRET_KEY="$(head -c 32 /dev/urandom | base64)" \
  --network-alias commandcenter --network-alias s3.commandcenter \
  -v "$root/config.yaml:/etc/commandcenter/config.yaml:ro" commandcenter:smoke-s3 >/dev/null
ready=0
for _ in $(seq 1 30); do
  if docker exec "$name" commandcenter healthcheck >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
test "$ready" = 1 || { echo "FAIL: commandcenter did not become ready"; docker logs "$name"; exit 1; }

curl_c() { docker run --rm --network "$net" -v "$root:/w" -w /w curlimages/curl:8.11.1 "$@"; }
fail() { echo "FAIL: $*" >&2; docker logs --tail 40 "$name" >&2 || true; exit 1; }

echo "== an S3 access key for ci"
key_json="$(curl_c -fsS -u ci:smoke-pass -H 'X-Command-Center-UI: 1' -H 'Content-Type: application/json' \
  -X POST -d '{"name":"smoke"}' http://commandcenter:8081/-/api/s3keys)" || fail "create key"
akid="$(printf '%s' "$key_json" | sed -n 's/.*"accessKeyId":"\([^"]*\)".*/\1/p')"
secret="$(printf '%s' "$key_json" | sed -n 's/.*"secretAccessKey":"\([^"]*\)".*/\1/p')"
test -n "$akid" && test -n "$secret" || fail "no key in $key_json"
admin_json="$(curl_c -fsS -u admin:smoke-pass -H 'X-Command-Center-UI: 1' -H 'Content-Type: application/json' \
  -X POST -d '{"name":"smoke-admin"}' http://commandcenter:8081/-/api/s3keys)"
admin_akid="$(printf '%s' "$admin_json" | sed -n 's/.*"accessKeyId":"\([^"]*\)".*/\1/p')"
admin_secret="$(printf '%s' "$admin_json" | sed -n 's/.*"secretAccessKey":"\([^"]*\)".*/\1/p')"

# Test files.
head -c 1024 /dev/urandom > "$work/small.bin"
head -c $((40 << 20)) /dev/urandom > "$work/big.bin"
mkdir -p "$work/tree/a/b"
echo one > "$work/tree/one.txt"; echo two > "$work/tree/a/two.txt"; echo three > "$work/tree/a/b/three.txt"

echo "== aws cli on the S3 port"
aws() {
  docker run --rm --network "$net" -v "$root:/w" -w /w -e AWS_ACCESS_KEY_ID="$1" -e AWS_SECRET_ACCESS_KEY="$2" \
    -e AWS_DEFAULT_REGION=us-east-1 "$aws_image" --endpoint-url http://commandcenter:9000 "${@:3}"
}
aws "$akid" "$secret" --version
aws "$akid" "$secret" s3 cp small.bin s3://artifacts/x/small.bin
aws "$akid" "$secret" s3 cp big.bin s3://artifacts/x/big.bin
aws "$akid" "$secret" s3 cp s3://artifacts/x/big.bin big.back
cmp "$work/big.bin" "$work/big.back" || fail "multipart round trip"
etag="$(aws "$akid" "$secret" s3api head-object --bucket artifacts --key x/big.bin --query ETag --output text)"
case "$etag" in *-*) ;; *) fail "multipart ETag $etag" ;; esac
aws "$akid" "$secret" s3 ls s3://artifacts/x/ | grep -q small.bin || fail "ls"
aws "$akid" "$secret" s3 sync tree s3://logs/tree
test "$(aws "$akid" "$secret" s3 ls --recursive s3://logs/tree/ | wc -l)" = 3 || fail "sync"
aws "$akid" "$secret" s3 cp s3://artifacts/x/small.bin s3://logs/copied.bin
url="$(aws "$akid" "$secret" s3 presign s3://logs/copied.bin --expires-in 300)"
curl_c -fsS -o small.presigned "$url" || fail "presigned GET"
cmp "$work/small.bin" "$work/small.presigned" || fail "presigned content"
if aws "$akid" "$secret" s3 mb s3://made-by-ci 2>/dev/null; then fail "ci created a bucket"; fi
aws "$admin_akid" "$admin_secret" s3 mb s3://made-by-admin
aws "$admin_akid" "$admin_secret" s3 cp small.bin s3://made-by-admin/f
aws "$admin_akid" "$admin_secret" s3 rm s3://made-by-admin --recursive
aws "$admin_akid" "$admin_secret" s3 rb s3://made-by-admin
aws "$akid" "$secret" s3 rm s3://logs/tree --recursive
test -z "$(aws "$akid" "$secret" s3 ls --recursive s3://logs/tree/)" || fail "rm --recursive"

echo "== mc on the S3 port and on the S3 host (path-style)"
mcx() {
  docker run --rm --network "$net" -v "$root:/w" -w /w -e HOME=/tmp --entrypoint sh "$mc_image" -c \
    "mc alias set port http://commandcenter:9000 $akid $secret >/dev/null && mc alias set host http://s3.commandcenter:8081 $akid $secret --path on >/dev/null && $*"
}
mcx "mc --version | head -1"
mcx "mc cp small.bin port/artifacts/mc/small.bin && mc cat host/artifacts/mc/small.bin > mc.back"
cmp "$work/small.bin" "$work/mc.back" || fail "mc cp/cat"
mcx "mc cp big.bin host/artifacts/mc/big.bin && mc stat port/artifacts/mc/big.bin | grep -q 'Size'"
mcx "mc mirror tree port/logs/mirror >/dev/null && mc ls --recursive host/logs/mirror | wc -l" | grep -qx 3 || fail "mc mirror"
mcx "mc rm --recursive --force port/logs/mirror >/dev/null && mc ls --recursive port/logs/mirror | wc -l" | grep -qx 0 || fail "mc rm"

echo "== the objects under /repository/"
curl_c -fsS -u ci:smoke-pass -o repo.back http://commandcenter:8081/repository/artifacts/x/big.bin || fail "/repository/ GET"
cmp "$work/big.bin" "$work/repo.back" || fail "/repository/ content"

docker stats --no-stream --format 'memory: {{.MemUsage}}' "$name"
echo "PASS: s3-smoke"
