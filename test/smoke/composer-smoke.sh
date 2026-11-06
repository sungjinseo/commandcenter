#!/usr/bin/env bash
# Builds the image, starts commandcenter, then uses real Composer (composer:2,
# Composer 2.11 on PHP 8.5) to:
#   1. zip smoke/hello 1.0.0 with git archive and upload it to composer-hosted
#      (PUT packages/upload/<vendor>/<package>/<version>); a second upload
#      must get 409
#   2. composer search it and composer require it and psr/log (from Packagist
#      via composer-proxy) through the composer-all group: packages.json,
#      search.json, p2/ metadata and both dist archives come from commandcenter
#   3. upload 1.1.0 through the group (the files at the root of the zip):
#      composer update takes it and composer show lists both versions
#   4. delete 1.1.0: composer update goes back to 1.0.0, and composer install
#      from the lock file works with an empty cache
#   5. upload and require a branch version (dev-main, read from ~dev.json)
#   6. use composer-private (no anonymous read, writePolicy allow): the same
#      version is uploaded twice, and Composer reads it with auth.json
#   7. use composer-proxy alone: require and composer audit, which must not
#      reach for Packagist; 16 clients at once on the largest metadata file
#      (aws/aws-sdk-php, 1.6 MiB)
#   8. security advisories of a package that has them (symfony/http-foundation
#      4.4.0, on Packagist) through the group and the proxy: composer require
#      refuses it at Composer's default audit.block-insecure, composer audit
#      reports it
#   9. the private-name rule, against a repository that logs what it hears: a
#      hosted package's name never reaches it, however Composer asks, and an API
#      that fails is a failure for composer audit, not "no advisories"
# The server's request log then shows what Composer asked for.
# The repositories are plain HTTP, which Composer takes only with
# secure-http false. The client has no route to the internet, and the server
# stays within its 128 MiB.
# Finally prints the container's memory usage.
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-composer-$$"
net=commandcenter-composer-smoke-$$
name=commandcenter-composer-smoke-$$
. test/postgres.sh

cleanup() {
  docker rm -f -v "$name" "$name-pg" "$name-client" "$name-spy" >/dev/null 2>&1 || true
  docker network rm "$net" "$net-clients" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup

# Built before the work directory exists: a new directory in the build
# context would compile the binary again on every run.
docker build -q -t commandcenter:smoke-composer . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i commandcenter:smoke-composer hash-password)"

mkdir -p "$work"
cp -r test/smoke/composer/hello test/smoke/composer/app test/smoke/composer/spy.php "$work/"

cat > "$work/config.yaml" <<EOF
anonymousRead: true
baseURL: "http://commandcenter:8081"
users:
  - { name: ci, passwordHash: "$hash", roles: [deployer] }
roles:
  deployer: { write: [composer-hosted, composer-private] }
repositories:
  - { name: composer-proxy,   format: composer, type: proxy,  remote: "https://repo.packagist.org/" }
  - { name: composer-hosted,  format: composer, type: hosted }
  - { name: composer-all,     format: composer, type: group,  members: [composer-hosted, composer-proxy], writeMember: composer-hosted }
  - { name: composer-private, format: composer, type: hosted, writePolicy: allow, anonymousRead: false }
  - { name: composer-spy,       format: composer, type: proxy, remote: "http://spy:8000/" }
  - { name: composer-spy-all,   format: composer, type: group, members: [composer-hosted, composer-spy] }
  - { name: composer-spy-first, format: composer, type: group, members: [composer-spy, composer-hosted] }
  - { name: composer-down,      format: composer, type: proxy, remote: "http://spy:8000/down/" }
  - { name: composer-down-all,  format: composer, type: group, members: [composer-hosted, composer-down] }
EOF

cat > "$work/run.sh" <<'EOF'
set -eu
export COMPOSER_HOME=/tmp/composer-home COMPOSER_NO_INTERACTION=1
REPO=http://commandcenter:8081/repository
HOSTED="$REPO/composer-hosted" GROUP="$REPO/composer-all" PRIVATE="$REPO/composer-private"
# says TEXT COMMAND...: the command succeeds and says TEXT; refused: it fails
# and says TEXT. The output goes to a file, not a pipe: a client cut off by
# grep -q stops in the middle of its work.
says() {
  want=$1; shift
  "$@" >/tmp/out.log 2>&1 || { echo "FAIL: exit $? (expected \"$want\")"; cat /tmp/out.log; return 1; }
  grep -q -- "$want" /tmp/out.log || { echo "FAIL: no \"$want\" in the output"; cat /tmp/out.log; return 1; }
}
refused() {
  want=$1; shift
  if "$@" >/tmp/out.log 2>&1; then echo "FAIL: succeeded (expected \"$want\")"; cat /tmp/out.log; return 1; fi
  grep -q -- "$want" /tmp/out.log || { echo "FAIL: no \"$want\" in the output"; cat /tmp/out.log; return 1; }
}
# upload ZIP REPOSITORY VERSION, unpublish REPOSITORY VERSION: the HTTP status of the documented PUT and DELETE.
upload() { curl -s -o /dev/null -w '%{http_code}' -u "ci:$TOKEN" -T "$1" "$2/packages/upload/smoke/hello/$3"; }
unpublish() { curl -s -o /dev/null -w '%{http_code}' -u "ci:$TOKEN" -X DELETE "$1/packages/upload/smoke/hello/$2"; }
# The work directory is mounted read-only: everything is built under /tmp.
mkdir /tmp/w
cp -r /work/hello /work/app /tmp/w/

echo "== zip smoke/hello with git archive (a directory around the files) and upload 1.0.0; a second upload is refused"
cd /tmp/w/hello
git init -q .
git add -A
git -c user.name=smoke -c user.email=smoke@example.com commit -q -m 1.0.0
git archive --format=zip --prefix=hello-1.0.0/ -o /tmp/hello-1.0.0.zip HEAD
test "$(upload /tmp/hello-1.0.0.zip "$HOSTED" 1.0.0)" = 201
test "$(upload /tmp/hello-1.0.0.zip "$HOSTED" 1.0.0)" = 409

echo "== composer search and composer require through the group (psr/log comes through the proxy)"
cd /tmp/w/app
echo '{}' > composer.json
composer config -g secure-http false      # the repository is plain HTTP
composer config -g repo.packagist false   # nothing but the group
composer config repositories.commandcenter composer "$GROUP/"
says "^smoke/hello " composer search smoke/hello   # nothing is installed yet: the hit is the repository's
says "Installing smoke/hello (1.0.0)" composer require smoke/hello psr/log
out=$(php main.php)
echo "$out"
test "$out" = "hello 7 from 1.0.0"
test "$(grep -c "\"url\": \"$GROUP/dist/" composer.lock)" = 2   # both archives come from commandcenter

echo "== 1.1.0 uploaded through the group (the files at the root of the zip): composer update takes it"
cd /tmp/w/hello
zip -qr /tmp/hello-1.1.0.zip composer.json src
test "$(upload /tmp/hello-1.1.0.zip "$GROUP" 1.1.0)" = 201
cd /tmp/w/app
says "Upgrading smoke/hello (1.0.0 => 1.1.0)" composer update
test "$(php main.php)" = "hello 7 from 1.1.0"
says "versions : \* 1.1.0, 1.0.0" composer show --all smoke/hello

echo "== delete 1.1.0: composer update goes back, composer install works from the lock file with an empty cache"
test "$(unpublish "$HOSTED" 1.1.0)" = 204
test "$(unpublish "$HOSTED" 1.1.0)" = 404
says "Downgrading smoke/hello (1.1.0 => 1.0.0)" composer update
rm -rf vendor "$COMPOSER_HOME/cache"
says "Installing psr/log" composer install
test "$(php main.php)" = "hello 7 from 1.0.0"

echo "== a branch version: dev-main"
test "$(upload /tmp/hello-1.0.0.zip "$HOSTED" dev-main)" = 201
mkdir /tmp/w/dev
cd /tmp/w/dev
cp ../app/main.php .
echo '{}' > composer.json
composer config repositories.commandcenter composer "$GROUP/"
says "Installing smoke/hello (dev-main" composer require smoke/hello:dev-main
test "$(php main.php | cut -d' ' -f1-4)" = "hello 7 from dev-main"

echo "== composer-private: writePolicy allow takes the version twice, Composer needs auth.json"
test "$(upload /tmp/hello-1.1.0.zip "$PRIVATE" 2.0.0)" = 201
test "$(upload /tmp/hello-1.1.0.zip "$PRIVATE" 2.0.0)" = 201
mkdir /tmp/w/private
cd /tmp/w/private
cp ../app/main.php .
# composer-private comes first, so it alone answers for smoke/hello; psr/log comes from the group
printf '{"repositories": [{"type": "composer", "url": "%s/"}, {"type": "composer", "url": "%s/"}]}\n' "$PRIVATE" "$GROUP" > composer.json
refused "required authentication (HTTP 401)" composer require smoke/hello
printf '{"http-basic": {"commandcenter:8081": {"username": "ci", "password": "%s"}}}\n' "$TOKEN" > auth.json
says "Installing smoke/hello (2.0.0)" composer require smoke/hello
test "$(php main.php)" = "hello 7 from 2.0.0"

echo "== composer-proxy alone: require and audit reach for nothing past it; 16 clients on the largest file"
mkdir /tmp/w/proxy
cd /tmp/w/proxy
echo '{}' > composer.json
composer config repositories.proxy composer "$REPO/composer-proxy/"
says "Installing psr/log" composer require psr/log
says "No security vulnerability advisories found" composer audit
seq 16 | xargs -P 16 -I{} curl -fsS -o /dev/null "$REPO/composer-proxy/p2/aws/aws-sdk-php.json"

echo "== a package with security advisories, through the group and the proxy: require refuses it, audit reports it"
for r in composer-all composer-proxy; do
  mkdir "/tmp/w/$r"
  cd "/tmp/w/$r"
  echo '{}' > composer.json
  composer config repositories.cc composer "$REPO/$r/"
  refused "because they are affected by security advisories" composer require symfony/http-foundation:4.4.0 --no-install --ignore-platform-reqs
  composer require symfony/http-foundation:4.4.0 --no-install --no-blocking --ignore-platform-reqs >/dev/null 2>&1
  if composer audit --locked --format=json > /tmp/audit.json 2>/dev/null; then echo "FAIL: composer audit through $r found nothing"; cat /tmp/audit.json; exit 1; fi
  found=$(php -r 'echo count(json_decode(file_get_contents("/tmp/audit.json"), true)["advisories"]["symfony/http-foundation"] ?? []);')
  echo "$r: $found advisories"
  test "$found" -ge 1
done

echo "== the private-name rule: smoke/hello, hosted here, is not asked about upstream (the script reads the upstream's log); a failing API is a failure"
mkdir /tmp/w/spy
cd /tmp/w/spy
# a lock file as a project has: its own hosted package and a public one
php -r 'echo json_encode(["content-hash" => "x", "aliases" => [], "minimum-stability" => "stable", "stability-flags" => [], "prefer-stable" => false, "prefer-lowest" => false,
  "platform" => new stdClass(), "platform-dev" => new stdClass(), "packages-dev" => [],
  "packages" => array_map(fn ($n) => ["name" => $n, "version" => "1.0.0", "version_normalized" => "1.0.0.0", "type" => "library", "dist" => ["type" => "zip", "url" => "http://nowhere.example/x.zip"]], ["smoke/hello", "spy/vuln"])]);' > composer.lock
for r in composer-spy-all composer-spy-first composer-spy; do
  printf '{"repositories": [{"type": "composer", "url": "%s/"}]}\n' "$REPO/$r" > composer.json
  refused "Found 1 security vulnerability advisory" composer audit --locked --format=plain
done
printf '{"repositories": [{"type": "composer", "url": "%s/"}]}\n' "$REPO/composer-down-all" > composer.json
refused "could not be downloaded (HTTP/1.1 502 Bad Gateway)" composer audit --locked
if grep -q "No security vulnerability advisories" /tmp/out.log; then echo "FAIL: a failing API read as no advisories"; cat /tmp/out.log; exit 1; fi
# 16 clients at once with as many names as Composer sends at a time
php -r 'for ($i = 0; $i < 500; $i++) { echo "packages[]=spy/name-$i&"; } echo "packages[]=spy/vuln";' > /tmp/names.form
seq 16 | xargs -P 16 -I{} curl -fsS -o /dev/null -H 'Content-Type: application/x-www-form-urlencoded' --data-binary @/tmp/names.form "$REPO/composer-spy-all/api/security-advisories/"
echo "COMPOSER CLIENT OK"
EOF

docker network create "$net" >/dev/null
docker network create --internal "$net-clients" >/dev/null
start_postgres
docker run -d --name "$name-spy" --network "$net" --network-alias spy --memory 128m -v "$root/$work/spy.php:/spy.php:ro" \
  --entrypoint php composer:2 -S 0.0.0.0:8000 /spy.php >/dev/null
docker run -d --name "$name" --network "$net" -e COMMANDCENTER_DATABASE_URL="$pg_url" --network-alias commandcenter --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" commandcenter:smoke-composer >/dev/null

# The client's network has no route out: all it needs comes through commandcenter.
docker network connect --alias commandcenter "$net-clients" "$name"

# No port is published: the image's own health check asks the server.
ready=0
for i in $(seq 1 30); do
  if docker exec "$name" commandcenter healthcheck 2>/dev/null; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "FAIL: commandcenter did not become ready"; docker logs "$name"; exit 1
fi

token="$(docker exec "$name" commandcenter token create -config /etc/commandcenter/config.yaml -user ci)"
docker run --rm --name "$name-client" --network "$net-clients" --memory 2g -e TOKEN="$token" -v "$root/$work:/work:ro" \
  --entrypoint sh composer:2 /work/run.sh

echo "== what Composer asked for"
docker logs "$name" > "$work/server.log" 2>&1
asked() {
  grep -q "\"method\":\"$1\",\"path\":\"/repository/$2\",\"status\":$3," "$work/server.log" ||
    { echo "FAIL: the client made no $1 $2 answered $3"; exit 1; }
}
asked GET composer-all/packages.json 200
asked GET composer-all/p2/smoke/hello.json 200
asked GET composer-all/p2/psr/log.json 200
asked GET composer-all/dist/smoke/hello/1.0.0.0.zip 200
asked GET composer-all/dist/smoke/hello/1.1.0.0.zip 200
asked GET composer-all/p2/smoke/hello~dev.json 200
asked GET composer-all/dist/smoke/hello/dev-main.zip 200
asked GET 'composer-all/dist/psr/log/[0-9.]*/[0-9a-f]*\.zip' 200
asked GET composer-all/search.json 200
asked GET composer-private/packages.json 401
asked GET composer-private/p2/smoke/hello.json 200
asked GET composer-private/dist/smoke/hello/2.0.0.0.zip 200
asked POST composer-all/api/security-advisories/ 200
asked POST composer-proxy/api/security-advisories/ 200
asked POST composer-down-all/api/security-advisories/ 502

echo "== what the upstream heard: the public name, never the hosted one"
docker logs "$name-spy" > "$work/spy.log" 2>&1
grep -q 'ASKED POST /api/security-advisories/ packages\[\]=spy/vuln' "$work/spy.log" || { echo "FAIL: the upstream was not asked about spy/vuln"; cat "$work/spy.log"; exit 1; }
if grep -q 'smoke/hello' "$work/spy.log"; then echo "FAIL: the upstream heard a hosted package's name"; grep 'smoke/hello' "$work/spy.log"; exit 1; fi

echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "COMPOSER SMOKE OK"
