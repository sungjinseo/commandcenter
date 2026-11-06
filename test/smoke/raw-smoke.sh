#!/usr/bin/env bash
# Builds the image, starts commandcenter, then uses real curl and GNU wget
# (buildpack-deps:trixie-curl) on raw repositories to:
#   1. PUT with both clients (wget answers the Basic challenge), GET, HEAD
#      and check the stored content types
#   2. ask for ranges (curl -r, resume with curl -C - and wget -c) and make
#      conditional requests (ETag, If-Modified-Since, curl -z, wget -N)
#   3. read directories: index.html, the HTML listing (mirrored with
#      wget -r) and the JSON listing
#   4. overwrite (allow_once refuses, allow replaces) and DELETE
#   5. read a real static site (nginx.org) through raw-proxy and the group
#   6. PUT and GET a 300 MiB file and a chunked upload, and check that the
#      server's memory stays flat (RSS, and no OOM kill at --memory 128m)
# The proxy downloads about 1.2 MB from nginx.org; the large file needs
# about 1 GB of disk for a moment.
# Finally prints the container's memory usage.
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-raw-$$"
net=commandcenter-raw-smoke-$$
name=commandcenter-raw-smoke-$$
. test/postgres.sh

cleanup() {
  docker rm -f -v "$name" "$name-pg" "$name-c" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup
rm -rf "$work" && mkdir -p "$work"

docker build -q -t commandcenter:smoke-raw . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i commandcenter:smoke-raw hash-password)"

cat > "$work/config.yaml" <<EOF
anonymousRead: true
baseURL: "http://commandcenter:8081"
users:
  - { name: admin, passwordHash: "$hash", roles: [admin] }
  - { name: ci, passwordHash: "$hash", roles: [deployer] }
roles:
  deployer: { write: [raw-hosted, raw-allow] }
repositories:
  - { name: raw-proxy,  format: raw, type: proxy,  remote: "https://nginx.org/" }
  - { name: raw-hosted, format: raw, type: hosted }
  - { name: raw-allow,  format: raw, type: hosted, writePolicy: allow }
  - { name: raw-all,    format: raw, type: group,  members: [raw-hosted, raw-proxy], writeMember: raw-hosted }
EOF

cat > "$work/run.sh" <<'EOF'
set -eu
CC=http://commandcenter:8081
R=$CC/repository
fail() { echo "FAIL: $*"; exit 1; }
status() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
# header <name> <curl arguments>: the value of a response header
header() { n="$1"; shift; curl -s -o /dev/null -D - "$@" | tr -d '\r' | sed -n "s/^$n: //Ip" | tail -1; }
rss() { curl -fsS -u admin:smoke-pass "$CC/-/metrics" | sed -n 's/.*"rssBytes":\([0-9]*\).*/\1/p'; }
cd /tmp
printf 'echo installed\n' > installer.sh
printf '<html><body>docs home</body></html>\n' > index.html
seq 1 20000 > numbers.txt
size=$(wc -c < numbers.txt)
curl --version | head -1
wget --version | head -1

echo "== PUT with curl -T (through the group) and wget --method=PUT; GET with both"
test "$(status -u ci:smoke-pass -T installer.sh "$R/raw-all/tools/installer.sh")" = 201 || fail "curl -T through the group"
wget -q --user ci --password smoke-pass --method=PUT --body-file=numbers.txt -O /dev/null "$R/raw-hosted/data/numbers.txt" || fail "wget --method=PUT"
curl -fsS "$R/raw-hosted/tools/installer.sh" | cmp - installer.sh || fail "curl GET"
wget -q -O got.txt "$R/raw-all/data/numbers.txt" && cmp got.txt numbers.txt || fail "wget GET through the group"
test "$(status -T installer.sh "$R/raw-hosted/tools/anonymous.sh")" = 401 || fail "anonymous PUT is not 401"
test "$(status --path-as-is -u ci:smoke-pass -T installer.sh "$R/raw-hosted/a/%2e%2e/b.sh")" = 400 || fail "a path with an encoded .. is not 400"
test "$(status -u ci:smoke-pass -T installer.sh "$R/raw-hosted/with%20space/%ED%95%9C%EA%B8%80.sh")" = 201 || fail "a path with a space and Hangul"
curl -fsS "$R/raw-hosted/with%20space/%ED%95%9C%EA%B8%80.sh" | cmp - installer.sh || fail "GET of the path with a space and Hangul"

echo "== HEAD and content types"
curl -sI "$R/raw-hosted/data/numbers.txt" | tr -d '\r' > head.txt
grep -q '^HTTP/1.1 200' head.txt && grep -qi "^Content-Length: $size$" head.txt && grep -qi '^Accept-Ranges: bytes$' head.txt &&
  grep -qi '^ETag: "' head.txt && grep -qi '^Last-Modified: ' head.txt && grep -qi '^Content-Type: text/plain' head.txt || { cat head.txt; fail "HEAD"; }
test "$(header Content-Type "$R/raw-hosted/tools/installer.sh")" = "text/x-sh; charset=utf-8" || fail "the type of .sh"
# the extension decides; without one the declared type; curl --data-binary's form type is no type
curl -fsS -u ci:smoke-pass -H 'Content-Type: text/plain' -T installer.sh "$R/raw-hosted/types/page.html" >/dev/null
curl -fsS -u ci:smoke-pass -H 'Content-Type: text/x-smoke' -T installer.sh "$R/raw-hosted/types/declared" >/dev/null
curl -fsS -u ci:smoke-pass -X PUT --data-binary @installer.sh "$R/raw-hosted/types/form" >/dev/null
test "$(header Content-Type "$R/raw-hosted/types/page.html")" = "text/html; charset=utf-8" || fail "the extension does not decide the type"
test "$(header Content-Type "$R/raw-hosted/types/declared")" = "text/x-smoke" || fail "the declared type is not kept"
test "$(header Content-Type "$R/raw-hosted/types/form")" = "application/octet-stream" || fail "the form type was kept"

echo "== ranges: curl -r, resume with curl -C - and wget -c"
url="$R/raw-hosted/data/numbers.txt"
test "$(status -r 100-199 "$url")" = 206 || fail "range status"
curl -fsS -r 100-199 "$url" > part && tail -c +101 numbers.txt | head -c 100 | cmp - part || fail "range body"
test "$(header Content-Range -r 100-199 "$url")" = "bytes 100-199/$size" || fail "Content-Range"
curl -fsS -r -50 "$url" > part && tail -c 50 numbers.txt | cmp - part || fail "suffix range"
test "$(status -r "$size-" "$url")" = 416 || fail "a range past the end is not 416"
test "$(status -r 0-9,20-29 "$url")" = 206 || fail "two ranges"
head -c 50000 numbers.txt > resume.txt && curl -fsS -C - -o resume.txt "$url" && cmp resume.txt numbers.txt || fail "curl -C - resume"
head -c 50000 numbers.txt > wresume.txt && wget -q -c -O wresume.txt "$url" && cmp wresume.txt numbers.txt || fail "wget -c resume"

echo "== conditional requests: ETag, If-Modified-Since, If-Range, curl -z and --etag-compare, wget -N"
etag="$(header ETag "$url")"
modified="$(header Last-Modified "$url")"
test "$(status -H "If-None-Match: $etag" "$url")" = 304 || fail "If-None-Match with the ETag"
test "$(status -H 'If-None-Match: "other"' "$url")" = 200 || fail "If-None-Match with another ETag"
test "$(status -H "If-Modified-Since: $modified" "$url")" = 304 || fail "If-Modified-Since with Last-Modified"
test "$(status -H 'If-Modified-Since: Sat, 01 Jan 2000 00:00:00 GMT' "$url")" = 200 || fail "If-Modified-Since with an old date"
test "$(status -r 0-9 -H "If-Range: $etag" "$url")" = 206 || fail "If-Range with the ETag"
test "$(status -r 0-9 -H 'If-Range: "other"' "$url")" = 200 || fail "If-Range with another ETag"
test "$(status -z got.txt "$url")" = 304 || fail "curl -z with a newer local file"
curl -fsS --etag-save etag.txt -o /dev/null "$url"
test "$(status --etag-compare etag.txt "$url")" = 304 || fail "curl --etag-compare"
mkdir n && (cd n && wget -q -N "$url" && wget -N "$url" 2>&1 | grep -q '304 Not Modified') || fail "wget -N did not get a 304"

echo "== directories: index.html, the HTML listing (wget -r) and the JSON listing"
curl -fsS -u ci:smoke-pass -T index.html "$R/raw-hosted/docs/index.html" >/dev/null
curl -fsS "$R/raw-hosted/docs/" | cmp - index.html || fail "a directory does not answer its index.html"
test "$(header Content-Type "$R/raw-all/docs/")" = "text/html; charset=utf-8" || fail "the type of the index through the group"
curl -fsS "$R/raw-hosted/data/" | grep -q '<a href="numbers.txt">numbers.txt</a>' || fail "HTML listing"
curl -fsS -H 'Accept: application/json' "$R/raw-hosted/" | grep -q '{"name":"data","dir":true}' || fail "JSON listing"
test "$(status "$R/raw-hosted/nothing/")" = 404 || fail "a directory without files is not 404"
wget -q -r -np -nH --cut-dirs=2 -e robots=off --restrict-file-names=nocontrol -P mirror "$R/raw-hosted/" || fail "wget -r"
cmp mirror/data/numbers.txt numbers.txt && cmp mirror/tools/installer.sh installer.sh && cmp "mirror/with space/한글.sh" installer.sh || fail "the mirror wget -r made"

echo "== overwrite and DELETE"
test "$(status -u ci:smoke-pass -T index.html "$R/raw-hosted/tools/installer.sh")" = 400 || fail "allow_once: overwrite is not 400"
test "$(status -u ci:smoke-pass -T installer.sh "$R/raw-allow/file.txt")" = 201 || fail "allow: first PUT"
first="$(header ETag "$R/raw-allow/file.txt")"
test "$(status -u ci:smoke-pass -T index.html "$R/raw-allow/file.txt")" = 201 || fail "allow: overwrite"
curl -fsS "$R/raw-allow/file.txt" | cmp - index.html || fail "allow: the new content"
test "$(header ETag "$R/raw-allow/file.txt")" != "$first" || fail "the ETag did not change with the content"
test "$(status -X DELETE "$R/raw-allow/file.txt")" = 401 || fail "anonymous DELETE is not 401"
test "$(status -u ci:smoke-pass -X DELETE "$R/raw-allow/file.txt")" = 204 || fail "DELETE"
test "$(status "$R/raw-allow/file.txt")" = 404 || fail "the deleted file is still served"
test "$(status -u ci:smoke-pass -X DELETE "$R/raw-allow/file.txt")" = 404 || fail "DELETE of a missing file is not 404"

echo "== proxy of nginx.org and the group"
curl -fsS "$R/raw-proxy/" | grep -q '<title>nginx</title>' || fail "the site's index.html at the proxy root"
curl -fsS "$R/raw-all/download/nginx-1.24.0.tar.gz.asc" | grep -q 'BEGIN PGP SIGNATURE' || fail "a file of the site through the group"
curl -fsS -o nginx.tar.gz "$R/raw-proxy/download/nginx-1.24.0.tar.gz" && gzip -t nginx.tar.gz || fail "the tarball through the proxy"
test "$(header Content-Length -I "$R/raw-proxy/download/nginx-1.24.0.tar.gz")" = "$(wc -c < nginx.tar.gz)" || fail "HEAD of the cached tarball"
test "$(header Content-Type "$R/raw-proxy/favicon.ico")" = "image/x-icon" || fail "the remote's content type"
curl -fsS -r 0-4 "$R/raw-proxy/download/nginx-1.24.0.tar.gz.asc" | grep -qx -e '-----' || fail "a range of a cached file"
wget -q -c -O nginx.tar.gz "$R/raw-all/download/nginx-1.24.0.tar.gz" || fail "wget -c of a complete file"
test "$(status "$R/raw-proxy/download/")" = 404 || fail "the proxy answered a directory listing"
test "$(status "$R/raw-proxy/no/such/file.txt")" = 404 || fail "a file the site does not have is not 404"
test "$(status -u admin:smoke-pass -T installer.sh "$R/raw-proxy/x.sh")" = 405 || fail "PUT to a proxy is not 405"

echo "== a 300 MiB file and a chunked upload; memory stays flat"
before=$(rss)
head -c 314572800 /dev/urandom > big.bin
sum="$(sha256sum < big.bin | cut -d' ' -f1)"
test "$(status -u ci:smoke-pass -T big.bin "$R/raw-hosted/big/big.bin")" = 201 || fail "PUT of the large file"
test "$(curl -fsS "$R/raw-hosted/big/big.bin" | sha256sum | cut -d' ' -f1)" = "$sum" || fail "curl GET of the large file"
test "$(wget -q -O - "$R/raw-all/big/big.bin" | sha256sum | cut -d' ' -f1)" = "$sum" || fail "wget GET of the large file"
curl -fsS -r -1048576 "$R/raw-hosted/big/big.bin" > part && tail -c 1048576 big.bin | cmp - part || fail "the last MiB of the large file"
head -c 33554432 big.bin > chunk.bin
test "$(curl -s -o /dev/null -w '%{http_code}' -u ci:smoke-pass -T - "$R/raw-hosted/big/chunked.bin" < chunk.bin)" = 201 || fail "chunked PUT"
curl -fsS "$R/raw-hosted/big/chunked.bin" | cmp - chunk.bin || fail "GET of the chunked upload"
after=$(rss)
echo "rss before $((before / 1048576)) MiB, after $((after / 1048576)) MiB"
test "$after" -lt 104857600 || fail "the server's RSS is $after bytes after the large file"
echo "RAW CLIENT OK"
EOF

docker network create "$net" >/dev/null
start_postgres
docker run -d --name "$name" --network "$net" -e COMMANDCENTER_DATABASE_URL="$pg_url" --network-alias commandcenter --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" commandcenter:smoke-raw >/dev/null

ready=0
for i in $(seq 1 30); do
  if docker exec "$name" commandcenter healthcheck >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "FAIL: commandcenter did not become ready"; docker logs "$name"; exit 1
fi

docker run --rm --name "$name-c" --network "$net" --memory 1g -v "$root/$work:/work" buildpack-deps:trixie-curl sh /work/run.sh

if [ "$(docker inspect -f '{{.State.OOMKilled}}' "$name")" = "true" ]; then
  echo "FAIL: commandcenter was OOM-killed"; exit 1
fi
echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "RAW SMOKE OK"
