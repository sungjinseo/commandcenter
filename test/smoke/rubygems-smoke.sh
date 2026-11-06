#!/usr/bin/env bash
# Builds the image, starts commandcenter, then uses real gem and Bundler
# (ruby:4.0-slim, RubyGems 4 and Bundler 4) to:
#   1. build smoke-hello 1.0.0 and push it to gems-hosted with the API key of
#      the credentials file (gem push --key); a second push must be refused
#   2. gem install it through the gems-all group; its dependency (rainbow)
#      comes from rubygems.org via gems-proxy
#   3. bundle install a Gemfile that needs it, with the group as the mirror of
#      rubygems.org (Bundler reads the compact index: versions and info/)
#   4. push 1.1.0 through the group with GEM_HOST_API_KEY: bundle update
#      finds it at once
#   5. gem search and gem list -r (the Marshal indexes), and the dependency
#      API read by Ruby
#   6. gem yank 1.1.0: a fresh gem install through the group gets 1.0.0
#   7. use gems-private (no anonymous read, writePolicy allow): the same
#      version is pushed twice, and Bundler installs it with the credentials
#      set for the repository
# The server's request log then shows that the clients used the compact
# index. The client has no route to the internet. The group merges
# rubygems.org's versions file (24 MiB), for 16 clients at once too: the server
# stays within its 128 MiB.
# Finally prints the container's memory usage.
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-rubygems-$$"
net=commandcenter-rubygems-smoke-$$
name=commandcenter-rubygems-smoke-$$
. test/postgres.sh

cleanup() {
  docker rm -f -v "$name" "$name-pg" "$name-client" >/dev/null 2>&1 || true
  docker network rm "$net" "$net-clients" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup

# Built before the work directory exists: a new directory in the build
# context would compile the binary again on every run.
docker build -q -t commandcenter:smoke-rubygems . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i commandcenter:smoke-rubygems hash-password)"

mkdir -p "$work"
cp -r test/smoke/rubygems/hello test/smoke/rubygems/app "$work/"

cat > "$work/config.yaml" <<EOF
anonymousRead: true
baseURL: "http://commandcenter:8081"
users:
  - { name: ci, passwordHash: "$hash", roles: [deployer] }
roles:
  deployer: { write: [gems-hosted, gems-private] }
repositories:
  - { name: gems-proxy,   format: rubygems, type: proxy,  remote: "https://rubygems.org/" }
  - { name: gems-hosted,  format: rubygems, type: hosted }
  - { name: gems-all,     format: rubygems, type: group,  members: [gems-hosted, gems-proxy], writeMember: gems-hosted }
  - { name: gems-private, format: rubygems, type: hosted, writePolicy: allow, anonymousRead: false }
EOF

cat > "$work/run.sh" <<'EOF'
set -eu
REPO=http://commandcenter:8081/repository
HOSTED="$REPO/gems-hosted" GROUP="$REPO/gems-all" PRIVATE="$REPO/gems-private"
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
# The work directory is mounted read-only: everything is built under /tmp.
mkdir /tmp/w
cp -r /work/hello /work/app /tmp/w/

echo "== build smoke-hello 1.0.0 and 1.1.0"
cd /tmp/w/hello
gem build smoke-hello.gemspec >/dev/null 2>&1
SMOKE_VERSION=1.1.0 gem build smoke-hello.gemspec >/dev/null 2>&1
test -f smoke-hello-1.0.0.gem
test -f smoke-hello-1.1.0.gem

echo "== gem push with the API key of the credentials file; a second push is refused"
mkdir -p ~/.gem
printf -- '---\n:cc: %s\n' "$TOKEN" > ~/.gem/credentials
chmod 0600 ~/.gem/credentials   # gem refuses a credentials file others can read
says "Successfully registered gem: smoke-hello (1.0.0)" gem push smoke-hello-1.0.0.gem --host "$HOSTED" --key cc
refused "already exists" gem push smoke-hello-1.0.0.gem --host "$HOSTED" --key cc

echo "== gem install through the group (rainbow comes through the proxy)"
cd /tmp/w   # not where the built gems are: gem install would take a file from there
says "2 gems installed" gem install --remote smoke-hello --clear-sources --source "$GROUP/"
out=$(ruby app/app.rb)
echo "$out"
test "$out" = "hello 7 from 1.0.0"

echo "== bundle install through the group, the mirror of rubygems.org"
cd /tmp/w/app
bundle config set --global mirror.https://rubygems.org "$GROUP/"
bundle config set --local path vendor/bundle
says "Bundle complete" bundle install
test "$(bundle exec ruby app.rb)" = "hello 7 from 1.0.0"

echo "== 1.1.0 pushed through the group with GEM_HOST_API_KEY: bundle update finds it at once"
says "Successfully registered gem: smoke-hello (1.1.0)" env GEM_HOST_API_KEY="$TOKEN" gem push /tmp/w/hello/smoke-hello-1.1.0.gem --host "$GROUP"
says "Bundle updated" bundle update smoke-hello
test "$(bundle exec ruby app.rb)" = "hello 7 from 1.1.0"

echo "== gem search, gem list -r and the dependency API"
says '^smoke-hello (1.1.0)$' gem search '^smoke-hello$' --clear-sources --source "$GROUP/"
says '^smoke-hello (1.1.0, 1.0.0)$' gem list -r --all --clear-sources --source "$HOSTED/"
ruby -ropen-uri -e 'deps = Marshal.load(URI.open(ARGV[0]).read).map { |d| d.values_at(:name, :number, :platform, :dependencies) }
  exit deps == [["smoke-hello", "1.0.0", "ruby", [["rainbow", "~> 3.1"]]], ["smoke-hello", "1.1.0", "ruby", [["rainbow", "~> 3.1"]]]]' \
  "$HOSTED/api/v1/dependencies?gems=smoke-hello"
ruby -ropen-uri -e '16.times.map { Thread.new { URI.open(ARGV[0]).read.size } }.each(&:value)' "$GROUP/versions"

echo "== gem yank 1.1.0: a fresh install through the group gets 1.0.0"
says "Successfully deleted gem: smoke-hello (1.1.0)" gem yank smoke-hello -v 1.1.0 --host "$HOSTED" --key cc
says '^smoke-hello (1.0.0)$' gem list -r --all --clear-sources --source "$HOSTED/"
cd /tmp/w
says "Successfully installed smoke-hello-1.0.0" gem install --remote smoke-hello --clear-sources --source "$GROUP/" -i /tmp/fresh

echo "== gems-private: writePolicy allow takes the version twice, Bundler needs the repository's credentials"
says "Successfully registered gem" env GEM_HOST_API_KEY="$TOKEN" gem push hello/smoke-hello-1.0.0.gem --host "$PRIVATE"
says "Successfully registered gem" env GEM_HOST_API_KEY="$TOKEN" gem push hello/smoke-hello-1.0.0.gem --host "$PRIVATE"
mkdir /tmp/w/private
cd /tmp/w/private
printf 'source "https://rubygems.org"\nsource "%s/" do\n  gem "smoke-hello"\nend\n' "$PRIVATE" > Gemfile
bundle config set --local path vendor/bundle
refused "Authentication is required" bundle install
bundle config set --global "$PRIVATE/" "ci:$TOKEN"
says "Bundle complete" bundle install
test "$(bundle exec ruby ../app/app.rb)" = "hello 7 from 1.0.0"
echo "RUBYGEMS CLIENT OK"
EOF

docker network create "$net" >/dev/null
docker network create --internal "$net-clients" >/dev/null
start_postgres
docker run -d --name "$name" --network "$net" -e COMMANDCENTER_DATABASE_URL="$pg_url" --network-alias commandcenter --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" commandcenter:smoke-rubygems >/dev/null

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
  ruby:4.0-slim sh /work/run.sh

echo "== what the clients asked for: the compact index, and never the group's dependency API"
docker logs "$name" > "$work/server.log" 2>&1
asked() {
  grep -q "\"method\":\"$1\",\"path\":\"/repository/$2\",\"status\":$3," "$work/server.log" ||
    { echo "FAIL: the client made no $1 $2 answered $3"; exit 1; }
}
asked HEAD gems-all/versions 200                             # gem install looks for the compact index
asked GET gems-all/versions 200                              # Bundler: the whole file once,
asked GET gems-all/versions 206                              # then what was appended
asked GET gems-all/info/smoke-hello 200
asked GET gems-all/info/rainbow 200
asked GET 'gems-all/gems/rainbow-[0-9.]*\.gem' 200
asked GET gems-all/quick/Marshal.4.8/smoke-hello-1.0.0.gemspec.rz 200
asked GET gems-all/latest_specs.4.8.gz 200                   # gem search
asked GET gems-hosted/specs.4.8.gz 200                       # gem list -r --all
asked POST gems-all/api/v1/gems 200
asked DELETE gems-hosted/api/v1/gems/yank 200
asked GET gems-private/versions 401
asked GET gems-private/gems/smoke-hello-1.0.0.gem 200
if grep -q '"path":"/repository/gems-all/api/v1/dependencies"' "$work/server.log"; then
  echo "FAIL: a client used the group's dependency API"; exit 1
fi

echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "RUBYGEMS SMOKE OK"
