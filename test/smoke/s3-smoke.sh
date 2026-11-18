#!/usr/bin/env bash
# Builds the image and starts two commandcenter nodes on one database and
# one shared data volume, with the S3-compatible API on :9000 (s3.listen),
# node a also behind a TLS proxy (Caddy with its own CA: over https the AWS
# SDKs send aws-chunked bodies with a trailing checksum). Then real S3
# clients in their official images, with S3 access keys made through the
# native API (POST /-/api/s3keys):
#   1. aws cli v2 (amazon/aws-cli): buckets, cp (multipart) and back,
#      metadata, the same files on /repository/, presigned URLs, refusals
#      (no add privilege, a revoked key), over http and https
#   2. boto3 and s3cmd (python:3.12-slim with pip): conditional writes,
#      ranges, copies, multipart by hand (UploadPartCopy, ListParts, abort),
#      listings with paging, DeleteObjects; s3cmd put/get/sync/del
#   3. rclone (rclone/rclone): copy, check, purge
#   4. restic (restic/restic): init, backup, check, restore
#   5. Terraform's S3 backend (hashicorp/terraform) with use_lockfile:
#      init, apply, a second apply refused while the first holds the lock
#   6. a multipart upload whose parts go to different nodes
#   7. 16 concurrent uploads of SMOKE_S3_MIB (default 1024) MiB each to a
#      node with --memory 128m: no OOM kill, RSS stays below 100 MiB
# mc is not run: MinIO archived it and publishes no image any more.
# Needs about 3 times 16 x SMOKE_S3_MIB of free disk for a moment (parts,
# the object being assembled, the client's file). pip and the images need
# network; nothing else leaves the docker network.
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-s3-$$"
name="${SMOKE_NAME:-commandcenter-s3-smoke-$$}"
net="$name-net"
vol="$name-data"
mib="${SMOKE_S3_MIB:-1024}"
. test/postgres.sh

cleanup() {
  docker rm -f -v "$name" "$name-b" "$name-pg" "$name-tls" "$name-py" "$name-c" "$name-tf" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  docker volume rm "$vol" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || docker run --rm -v "$root/test/smoke:/s" alpine:3.20 rm -rf "/s/${work##*/}" || true
}
trap cleanup EXIT
cleanup
rm -rf "$work" && mkdir -p "$work/tf" && chmod 777 "$work" "$work/tf"

docker build -q -t commandcenter:smoke-s3 . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i commandcenter:smoke-s3 hash-password)"
secret="$(head -c 32 /dev/urandom | base64)"

cat > "$work/config.yaml" <<EOF
anonymousRead: false
baseURL: "http://commandcenter:8081"
maxUploadMiB: 2048
s3: { listen: ":9000" }
users:
  - { name: admin, passwordHash: "$hash", roles: [admin] }
  - { name: ci, passwordHash: "$hash", roles: [deployer] }
  - { name: reader, passwordHash: "$hash" }
roles:
  deployer: { write: [s3-data, s3-once] }
repositories:
  - { name: s3-data, format: raw, type: hosted, writePolicy: allow }
  - { name: s3-once, format: raw, type: hosted }
EOF
chmod 644 "$work/config.yaml"
printf 's3.smoke.test {\n\ttls internal\n\treverse_proxy commandcenter:9000\n}\n' > "$work/Caddyfile"

docker network create "$net" >/dev/null
docker volume create "$vol" >/dev/null
docker run --rm -v "$vol:/data" --entrypoint chown alpine:3.20 65532:65532 /data
start_postgres
node() { # node NAME ALIAS: a server on the shared volume
  docker run -d --name "$1" --network "$net" --network-alias "$2" --memory 128m \
    -e COMMANDCENTER_DATABASE_URL="$pg_url" -e COMMANDCENTER_SECRET_KEY="$secret" -v "$vol:/data" \
    -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" commandcenter:smoke-s3 >/dev/null
}
ready() {
  for _ in $(seq 30); do docker exec "$1" commandcenter healthcheck >/dev/null 2>&1 && return; sleep 1; done
  echo "FAIL: $1 did not become ready"; docker logs "$1"; exit 1
}
node "$name" commandcenter
ready "$name"
node "$name-b" commandcenter-b # after a: the first start imports the configuration
ready "$name-b"
docker run -d --name "$name-tls" --network "$net" --network-alias s3.smoke.test --memory 128m \
  -v "$root/$work/Caddyfile:/etc/caddy/Caddyfile:ro" caddy:2-alpine >/dev/null

curl_() { docker run --rm --network "$net" -v "$root/$work:/work" curlimages/curl:8.23.0 "$@"; }
# key USER: an S3 access key made by USER, as "id secret"
key() {
  curl_ -fsS -u "$1:smoke-pass" -H 'X-Command-Center-UI: 1' -X POST http://commandcenter:8081/-/api/s3keys |
    sed -n 's/.*"accessKeyId":"\([^"]*\)".*"secretAccessKey":"\([^"]*\)".*/\1 \2/p'
}
read -r ID SECRET <<<"$(key ci)"
read -r RID RSECRET <<<"$(key reader)"
read -r XID XSECRET <<<"$(key ci)"
test -n "$ID" && test -n "$RID" && test -n "$XID" || { echo "FAIL: S3 access keys"; exit 1; }
for _ in $(seq 30); do
  docker cp "$name-tls:/data/caddy/pki/authorities/local/root.crt" "$work/ca.crt" >/dev/null 2>&1 && break
  sleep 1
done
chmod 644 "$work/ca.crt"

cat > "$work/aws.sh" <<'EOF'
set -eu
fail() { echo "FAIL: $*"; exit 1; }
# cmp A B: whether A ("-": stdin) and B hold the same bytes (the image has no cmp)
cmp() { a="$(if [ "$1" = - ]; then sha256sum; else sha256sum < "$1"; fi)"; b="$(sha256sum < "$2")"; [ "${a%% *}" = "${b%% *}" ]; }
A=http://commandcenter:9000
B=http://commandcenter-b:9000
CC=http://commandcenter:8081
cd /tmp
aws --version
echo hello > small.txt
head -c 20000000 /dev/urandom > big.bin

echo "== aws: buckets, cp (multipart) and back, metadata"
aws s3 ls | grep -q ' s3-data$' || fail "s3 ls lists no s3-data"
aws s3 cp small.txt s3://s3-data/aws/small.txt >/dev/null
aws s3 cp big.bin s3://s3-data/aws/big.bin >/dev/null
aws s3 cp s3://s3-data/aws/big.bin got.bin >/dev/null && cmp got.bin big.bin || fail "cp back"
aws s3api head-object --bucket s3-data --key aws/big.bin --query ETag --output text | grep -q -- '-3"$' || fail "the multipart ETag"
aws s3api put-object --bucket s3-data --key aws/meta.txt --body small.txt --content-type text/x-smoke \
  --metadata team=infra --cache-control max-age=60 >/dev/null
aws s3api head-object --bucket s3-data --key aws/meta.txt > head.json
grep -q '"ContentType": "text/x-smoke"' head.json && grep -q '"team": "infra"' head.json && grep -q '"CacheControl": "max-age=60"' head.json ||
  { cat head.json; fail "metadata round trip"; }
test "$(aws s3 ls s3://s3-data/aws/ | wc -l)" = 3 || fail "s3 ls of the prefix"

echo "== aws: the same files on /repository/, and the other way"
curl -fsS -u ci:smoke-pass "$CC/repository/s3-data/aws/big.bin" | cmp - big.bin || fail "an S3 object on /repository/"
test "$(curl -s -o /dev/null -w '%{content_type}' -u ci:smoke-pass "$CC/repository/s3-data/aws/meta.txt")" = text/x-smoke || fail "its type on /repository/"
curl -fsS -u ci:smoke-pass -T small.txt "$CC/repository/s3-data/raw/put.txt" >/dev/null
aws s3 cp s3://s3-data/raw/put.txt - | cmp - small.txt || fail "a raw upload through S3"
curl -fsS -u ci:smoke-pass "$CC/-/api/repositories/s3-data/browse?path=aws/" | grep -q '"meta.txt"' || fail "browse lists the S3 upload"

echo "== aws: presigned URL"
url="$(aws s3 presign s3://s3-data/aws/small.txt --expires-in 120)"
curl -fsS "$url" | cmp - small.txt || fail "presigned GET"
test "$(curl -s -o /dev/null -w '%{http_code}' "${url%%X-Amz-Signature=*}X-Amz-Signature=0000")" = 403 || fail "a forged presigned URL"

echo "== aws: refusals"
AWS_ACCESS_KEY_ID=$RID AWS_SECRET_ACCESS_KEY=$RSECRET aws s3 cp small.txt s3://s3-data/aws/denied.txt 2>&1 | grep -q AccessDenied ||
  fail "a user without add privilege uploaded"
aws s3 cp small.txt s3://s3-once/once.txt >/dev/null
aws s3 cp big.bin s3://s3-once/once.txt 2>&1 | grep -q AccessDenied || fail "allow_once let a key be overwritten"
aws s3 mb s3://made-by-s3 2>&1 | grep -q AccessDenied || fail "mb made a bucket"
AWS_ACCESS_KEY_ID=$XID AWS_SECRET_ACCESS_KEY=$XSECRET aws s3 ls s3://s3-data/ >/dev/null || fail "the second key"
curl -fsS -u ci:smoke-pass -H 'X-Command-Center-UI: 1' -X DELETE "$CC/-/api/s3keys/$XID" || fail "revoke"
AWS_ACCESS_KEY_ID=$XID AWS_SECRET_ACCESS_KEY=$XSECRET aws s3 ls s3://s3-data/ 2>&1 | grep -q InvalidAccessKeyId || fail "a revoked key"

echo "== aws over https: aws-chunked with a trailing checksum"
export AWS_CA_BUNDLE=/work/ca.crt
H=https://s3.smoke.test
aws --endpoint-url $H --debug s3api put-object --bucket s3-data --key tls/small.txt --body small.txt 2>&1 |
  grep -q 'STREAMING-UNSIGNED-PAYLOAD-TRAILER' || fail "put-object over https did not send a trailing checksum"
aws --endpoint-url $H s3 cp big.bin s3://s3-data/tls/big.bin >/dev/null
aws --endpoint-url $H s3 cp s3://s3-data/tls/big.bin got.bin >/dev/null && cmp got.bin big.bin || fail "cp over https"
aws --endpoint-url $H s3 cp s3://s3-data/tls/small.txt - | cmp - small.txt || fail "the trailer upload's content"
unset AWS_CA_BUNDLE

echo "== a multipart upload whose parts go to different nodes"
head -c 5242880 big.bin > p1 && tail -c +5242881 big.bin | head -c 7000000 > p2
cat p1 p2 > both
up="$(aws --endpoint-url $A s3api create-multipart-upload --bucket s3-data --key ha/two-nodes.bin --query UploadId --output text)"
e1="$(aws --endpoint-url $A s3api upload-part --bucket s3-data --key ha/two-nodes.bin --upload-id "$up" --part-number 1 --body p1 --query ETag --output text)"
e2="$(aws --endpoint-url $B s3api upload-part --bucket s3-data --key ha/two-nodes.bin --upload-id "$up" --part-number 2 --body p2 --query ETag --output text)"
test "$(aws --endpoint-url $B s3api list-parts --bucket s3-data --key ha/two-nodes.bin --upload-id "$up" --query 'length(Parts)')" = 2 ||
  fail "node b does not see node a's part"
printf '{"Parts":[{"PartNumber":1,"ETag":%s},{"PartNumber":2,"ETag":%s}]}' "$(printf '%s' "$e1" | sed 's/"/\\"/g; s/.*/"&"/')" \
  "$(printf '%s' "$e2" | sed 's/"/\\"/g; s/.*/"&"/')" > parts.json
aws --endpoint-url $B s3api complete-multipart-upload --bucket s3-data --key ha/two-nodes.bin --upload-id "$up" --multipart-upload file://parts.json >/dev/null
aws --endpoint-url $A s3 cp s3://s3-data/ha/two-nodes.bin got.bin >/dev/null && cmp got.bin both || fail "the object assembled from two nodes"
echo "AWS CLI OK"
EOF

cat > "$work/py.sh" <<'EOF'
set -eu
fail() { echo "FAIL: $*"; exit 1; }
pip install -q --root-user-action=ignore --disable-pip-version-check boto3 s3cmd
python3 -I /work/boto.py
cd /tmp
cat > s3cfg <<CFG
[default]
access_key = $AWS_ACCESS_KEY_ID
secret_key = $AWS_SECRET_ACCESS_KEY
host_base = commandcenter:9000
host_bucket = commandcenter:9000
use_https = False
signature_v2 = False
bucket_location = us-east-1
CFG
s3cmd --version
mkdir -p tree/a && head -c 7000000 /dev/urandom > tree/a/big && echo one > tree/a/one && echo two > tree/two
s3cmd -c s3cfg ls | grep -q 's3://s3-data' || fail "s3cmd ls"
s3cmd -c s3cfg -q put --multipart-chunk-size-mb=5 tree/a/big s3://s3-data/s3cmd/big
s3cmd -c s3cfg -q get --force s3://s3-data/s3cmd/big got && cmp got tree/a/big || fail "s3cmd get"
s3cmd -c s3cfg -q sync tree/ s3://s3-data/s3cmd/tree/
test "$(s3cmd -c s3cfg ls -r s3://s3-data/s3cmd/tree/ | wc -l)" = 3 || fail "s3cmd sync"
s3cmd -c s3cfg -q del -r --force s3://s3-data/s3cmd/
test -z "$(s3cmd -c s3cfg ls -r s3://s3-data/s3cmd/)" || fail "s3cmd del"
echo "S3CMD OK"
EOF

cat > "$work/boto.py" <<'EOF'
import hashlib, io, os, ssl, sys, urllib.request
import boto3
from botocore.config import Config
from botocore.exceptions import ClientError

B = "s3-data"


def fail(msg):
    print("FAIL:", msg)
    sys.exit(1)


def code(e):
    return e.response["Error"]["Code"]


def meta(h):
    # Caddy, a Go proxy, writes header names capitalized: x-amz-meta-a arrives as X-Amz-Meta-A.
    return {k.lower(): v for k, v in h["Metadata"].items()}


def client(endpoint, **kw):
    # Presigned URLs need signature_version s3v4: boto3 presigns with SigV2 otherwise.
    return boto3.client("s3", endpoint_url=endpoint, region_name="us-east-1",
                        config=Config(s3={"addressing_style": "path"}, signature_version="s3v4", retries={"max_attempts": 1}), **kw)


print("boto3", boto3.__version__)
for s3 in [client("http://commandcenter:9000"), client("https://s3.smoke.test", verify="/work/ca.crt")]:
    s3.delete_objects(Bucket=B, Delete={"Objects": [{"Key": o["Key"]} for o in s3.list_objects_v2(Bucket=B, Prefix="py/").get("Contents", [])]
                                         or [{"Key": "py/none"}]})
    if B not in [b["Name"] for b in s3.list_buckets()["Buckets"]]:
        fail("list_buckets")
    s3.head_bucket(Bucket=B)
    s3.put_object(Bucket=B, Key="py/a.txt", Body=b"alpha", ContentType="text/x-alpha", Metadata={"Owner-Team": "infra"})
    h = s3.head_object(Bucket=B, Key="py/a.txt")
    if h["ContentType"] != "text/x-alpha" or meta(h) != {"owner-team": "infra"} or h["ETag"] != '"%s"' % hashlib.md5(b"alpha").hexdigest():
        fail("head_object: %s" % h)
    try:
        s3.put_object(Bucket=B, Key="py/a.txt", Body=b"x", IfNoneMatch="*")
        fail("If-None-Match: * overwrote")
    except ClientError as e:
        if code(e) != "PreconditionFailed":
            fail("If-None-Match: * answered %s" % code(e))
    if s3.get_object(Bucket=B, Key="py/a.txt", Range="bytes=1-3")["Body"].read() != b"lph":
        fail("range")
    try:
        s3.get_object(Bucket=B, Key="py/a.txt", IfNoneMatch=h["ETag"])
        fail("If-None-Match read")
    except ClientError as e:
        if e.response["ResponseMetadata"]["HTTPStatusCode"] != 304:
            fail("If-None-Match read: %s" % e.response)
    r = s3.get_object(Bucket=B, Key="py/a.txt", ResponseContentType="text/plain", ResponseContentDisposition="attachment")
    if r["ContentType"] != "text/plain" or r["ContentDisposition"] != "attachment":
        fail("response-* overrides: %s" % r)
    s3.copy_object(Bucket=B, Key="py/copy.txt", CopySource={"Bucket": B, "Key": "py/a.txt"})
    if meta(s3.head_object(Bucket=B, Key="py/copy.txt")) != {"owner-team": "infra"}:
        fail("copy kept no metadata")
    s3.copy_object(Bucket=B, Key="py/copy.txt", CopySource={"Bucket": B, "Key": "py/copy.txt"}, MetadataDirective="REPLACE",
                   ContentType="text/plain", Metadata={"x": "y"})
    c = s3.head_object(Bucket=B, Key="py/copy.txt")
    if meta(c) != {"x": "y"} or c["ContentType"] != "text/plain":
        fail("copy REPLACE: %s" % c)
    data = os.urandom(21 * 1024 * 1024)
    s3.upload_fileobj(io.BytesIO(data), B, "py/big.bin")
    parts = [data[i:i + 8 * 1024 * 1024] for i in range(0, len(data), 8 * 1024 * 1024)]
    want = '"%s-%d"' % (hashlib.md5(b"".join(hashlib.md5(p).digest() for p in parts)).hexdigest(), len(parts))
    if s3.head_object(Bucket=B, Key="py/big.bin")["ETag"] != want:
        fail("multipart ETag")
    buf = io.BytesIO()
    s3.download_fileobj(B, "py/big.bin", buf)
    if buf.getvalue() != data:
        fail("multipart content")
    u = s3.create_multipart_upload(Bucket=B, Key="py/manual.bin")["UploadId"]
    e1 = s3.upload_part(Bucket=B, Key="py/manual.bin", UploadId=u, PartNumber=1, Body=data[:5 * 1024 * 1024])["ETag"]
    e2 = s3.upload_part_copy(Bucket=B, Key="py/manual.bin", UploadId=u, PartNumber=2, CopySource={"Bucket": B, "Key": "py/a.txt"},
                             CopySourceRange="bytes=0-2")["CopyPartResult"]["ETag"]
    if [x["Key"] for x in s3.list_multipart_uploads(Bucket=B, Prefix="py/")["Uploads"]] != ["py/manual.bin"]:
        fail("list_multipart_uploads")
    if [(p["PartNumber"], p["Size"]) for p in s3.list_parts(Bucket=B, Key="py/manual.bin", UploadId=u)["Parts"]] != [(1, 5 * 1024 * 1024), (2, 3)]:
        fail("list_parts")
    s3.complete_multipart_upload(Bucket=B, Key="py/manual.bin", UploadId=u,
                                 MultipartUpload={"Parts": [{"PartNumber": 1, "ETag": e1}, {"PartNumber": 2, "ETag": e2}]})
    if s3.get_object(Bucket=B, Key="py/manual.bin")["Body"].read() != data[:5 * 1024 * 1024] + b"alp":
        fail("UploadPartCopy content")
    u = s3.create_multipart_upload(Bucket=B, Key="py/aborted.bin")["UploadId"]
    s3.upload_part(Bucket=B, Key="py/aborted.bin", UploadId=u, PartNumber=1, Body=b"x")
    s3.abort_multipart_upload(Bucket=B, Key="py/aborted.bin", UploadId=u)
    try:
        s3.list_parts(Bucket=B, Key="py/aborted.bin", UploadId=u)
        fail("an aborted upload is listed")
    except ClientError as e:
        if code(e) != "NoSuchUpload":
            fail("aborted upload: %s" % code(e))
    url = s3.generate_presigned_url("put_object", Params={"Bucket": B, "Key": "py/presigned.txt"}, ExpiresIn=300)
    urllib.request.urlopen(urllib.request.Request(url, data=b"via url", method="PUT"), context=ssl.create_default_context(cafile="/work/ca.crt"))
    if s3.get_object(Bucket=B, Key="py/presigned.txt")["Body"].read() != b"via url":
        fail("presigned PUT")
    s3.put_object(Bucket=B, Key="py/dir/x y+z.txt", Body=b"1")
    keys, prefixes = [], []
    for p in s3.get_paginator("list_objects_v2").paginate(Bucket=B, Prefix="py/", Delimiter="/", PaginationConfig={"PageSize": 2}):
        keys += [o["Key"] for o in p.get("Contents", [])]
        prefixes += [x["Prefix"] for x in p.get("CommonPrefixes", [])]
    if keys != ["py/a.txt", "py/big.bin", "py/copy.txt", "py/manual.bin", "py/presigned.txt"] or prefixes != ["py/dir/"]:
        fail("list_objects_v2: %s %s" % (keys, prefixes))
    if [o["Key"] for o in s3.list_objects(Bucket=B, Prefix="py/dir/")["Contents"]] != ["py/dir/x y+z.txt"]:
        fail("list_objects")
    s3.delete_objects(Bucket=B, Delete={"Quiet": True, "Objects": [{"Key": k} for k in keys + ["py/dir/x y+z.txt", "py/none"]]})
    if s3.list_objects_v2(Bucket=B, Prefix="py/")["KeyCount"] != 0:
        fail("delete_objects left keys")
    try:
        s3.get_object(Bucket=B, Key="py/none")
        fail("a missing key")
    except ClientError as e:
        if code(e) != "NoSuchKey":
            fail("a missing key: %s" % code(e))
print("BOTO3 OK")
EOF

cat > "$work/tf/main.tf" <<'EOF'
terraform {
  backend "s3" {
    bucket                      = "s3-data"
    key                         = "tf/terraform.tfstate"
    region                      = "us-east-1"
    endpoints                   = { s3 = "http://commandcenter:9000" }
    use_path_style              = true
    use_lockfile                = true
    skip_credentials_validation = true
    skip_requesting_account_id  = true
    skip_metadata_api_check     = true
    skip_region_validation      = true
  }
}

variable "v" {
  default = "one"
}

# A new value replaces the resource, whose provisioner sleeps for a "slow"
# one: the apply holds the state lock meanwhile.
resource "terraform_data" "x" {
  input            = var.v
  triggers_replace = var.v
  provisioner "local-exec" {
    command = "sleep ${startswith(var.v, "slow") ? 30 : 0}"
  }
}

output "v" {
  value = terraform_data.x.output
}
EOF

# The clients: an S3 key in the environment, the work directory at /work.
cl() { # cl [DOCKER OPTIONS] IMAGE ARGS...: a client container
  docker run --rm --name "$name-c" --network "$net" --memory 1g -v "$root/$work:/work" -e HOME=/tmp \
    -e AWS_ACCESS_KEY_ID="$ID" -e AWS_SECRET_ACCESS_KEY="$SECRET" -e AWS_DEFAULT_REGION=us-east-1 \
    -e RID="$RID" -e RSECRET="$RSECRET" -e XID="$XID" -e XSECRET="$XSECRET" "$@"
}

echo "== aws cli"
cl --entrypoint bash -e AWS_ENDPOINT_URL=http://commandcenter:9000 amazon/aws-cli /work/aws.sh
echo "== boto3 and s3cmd"
cl python:3.12-slim sh /work/py.sh

echo "== rclone"
touch "$work/rclone.conf" # empty: the remote comes from the environment
rc() {
  docker run --rm --name "$name-c" --network "$net" --memory 512m -v "$root/$work:/work" -e HOME=/tmp \
    -e RCLONE_CONFIG=/work/rclone.conf -e RCLONE_CONFIG_CC_TYPE=s3 -e RCLONE_CONFIG_CC_PROVIDER=Other -e RCLONE_CONFIG_CC_ENDPOINT=http://commandcenter:9000 \
    -e RCLONE_CONFIG_CC_ACCESS_KEY_ID="$ID" -e RCLONE_CONFIG_CC_SECRET_ACCESS_KEY="$SECRET" -e RCLONE_CONFIG_CC_REGION=us-east-1 \
    rclone/rclone "$@"
}
rc version | head -1
mkdir -p "$work/tree/a/b" && head -c 30000000 /dev/urandom > "$work/tree/a/b/big" && echo one > "$work/tree/a/one" && echo two > "$work/tree/two"
chmod -R a+rX "$work/tree"
rc copy /work/tree cc:s3-data/rclone
rc check /work/tree cc:s3-data/rclone 2>&1 | grep -q '0 differences found' || { echo "FAIL: rclone check"; exit 1; }
test "$(rc ls cc:s3-data/rclone | wc -l)" = 3 || { echo "FAIL: rclone ls"; exit 1; }
rc purge cc:s3-data/rclone
test -z "$(rc ls cc:s3-data/rclone)" || { echo "FAIL: rclone purge"; exit 1; }
echo "RCLONE OK"

echo "== restic"
rs() { cl -e RESTIC_PASSWORD=smoke-restic restic/restic -r s3:http://commandcenter:9000/s3-data/restic "$@"; }
rs version
rs init >/dev/null
rs backup -q /work/tree
rs check | grep -q 'no errors were found' || { echo "FAIL: restic check"; exit 1; }
rs restore -q latest --target /work/restore
cl --entrypoint diff restic/restic -r /work/tree /work/restore/work/tree || { echo "FAIL: restic restore"; exit 1; }
test "$(rs snapshots --json | grep -o '"id"' | wc -l)" = 1 || { echo "FAIL: restic snapshots"; exit 1; }
echo "RESTIC OK"

echo "== Terraform S3 backend with use_lockfile"
tf() { docker run --rm --network "$net" --memory 512m -v "$root/$work/tf:/tf" -w /tf -e TF_IN_AUTOMATION=1 \
  -e AWS_ACCESS_KEY_ID="$ID" -e AWS_SECRET_ACCESS_KEY="$SECRET" hashicorp/terraform:1.17 "$@"; }
tf version | head -1
tf init -input=false -no-color >/dev/null
tf apply -auto-approve -input=false -no-color | grep -q 'v = "one"' || { echo "FAIL: terraform apply"; exit 1; }
docker run -d --name "$name-tf" --network "$net" --memory 512m -v "$root/$work/tf:/tf" -w /tf -e TF_IN_AUTOMATION=1 \
  -e AWS_ACCESS_KEY_ID="$ID" -e AWS_SECRET_ACCESS_KEY="$SECRET" hashicorp/terraform:1.17 apply -auto-approve -input=false -no-color -var v=slow >/dev/null
locked=0
for _ in $(seq 30); do
  if cl amazon/aws-cli --endpoint-url http://commandcenter:9000 s3api head-object --bucket s3-data --key tf/terraform.tfstate.tflock >/dev/null 2>&1; then
    locked=1; break
  fi
  sleep 1
done
test "$locked" = 1 || { echo "FAIL: the first apply wrote no lock file"; docker logs "$name-tf"; exit 1; }
out="$(tf apply -auto-approve -input=false -no-color -var v=two -lock-timeout=0s 2>&1 || true)"
echo "$out" | grep -q 'Error acquiring the state lock' || { echo "FAIL: the second apply was not refused"; echo "$out"; exit 1; }
test "$(docker wait "$name-tf")" = 0 || { echo "FAIL: the first apply"; docker logs "$name-tf"; exit 1; }
docker rm "$name-tf" >/dev/null
tf apply -auto-approve -input=false -no-color -var v=two | grep -q 'v = "two"' || { echo "FAIL: the apply after the lock"; exit 1; }
echo "TERRAFORM OK"

echo "== 16 concurrent ${mib} MiB uploads to a node with --memory 128m"
cat > "$work/mem.sh" <<EOF
set -eu
# cmp A B: whether A ("-": stdin) and B hold the same bytes (the image has no cmp)
cmp() { a="\$(if [ "\$1" = - ]; then sha256sum; else sha256sum < "\$1"; fi)"; b="\$(sha256sum < "\$2")"; [ "\${a%% *}" = "\${b%% *}" ]; }
cd /tmp
head -c $((mib << 20)) /dev/urandom > blob
rss() { curl -fsS -u admin:smoke-pass http://commandcenter:8081/-/metrics | sed -n 's/.*"rssBytes":\([0-9]*\).*/\1/p'; }
before=\$(rss)
for i in \$(seq 16); do aws s3 cp blob s3://s3-data/mem/\$i.bin >/dev/null & done
failed=0
for job in \$(jobs -p); do wait \$job || failed=1; done
after=\$(rss)
test \$failed = 0 || { echo "FAIL: an upload failed"; exit 1; }
test "\$(aws s3 ls s3://s3-data/mem/ | wc -l)" = 16 || { echo "FAIL: not 16 objects"; exit 1; }
aws s3 cp s3://s3-data/mem/7.bin - | cmp - blob || { echo "FAIL: content"; exit 1; }
aws s3 rm --recursive --quiet s3://s3-data/mem/
echo "rss before \$((before / 1048576)) MiB, after \$((after / 1048576)) MiB"
test "\$after" -lt 104857600 || { echo "FAIL: the server's RSS is \$after bytes"; exit 1; }
EOF
cl --memory 2g --entrypoint bash -e AWS_ENDPOINT_URL=http://commandcenter:9000 amazon/aws-cli /work/mem.sh
for n in "$name" "$name-b"; do
  if [ "$(docker inspect -f '{{.State.OOMKilled}} {{.RestartCount}}' "$n")" != "false 0" ]; then
    echo "FAIL: $n was OOM-killed or restarted"; exit 1
  fi
done
echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name" "$name-b"
echo "S3 SMOKE OK"
