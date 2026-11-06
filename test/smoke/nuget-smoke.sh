#!/usr/bin/env bash
# Builds the image, starts commandcenter, then uses the real dotnet CLI
# (mcr.microsoft.com/dotnet/sdk:10.0, NuGet 7) to:
#   1. pack Smoke.Hello 1.0.0; its dependency (Newtonsoft.Json, from nuget.org
#      via nuget-proxy) is restored through the nuget-all group
#   2. push it to nuget-hosted with an API key (dotnet nuget push reads the V3
#      service index); a second push must get 409, which --skip-duplicate
#      accepts, and a push without a key 401
#   3. restore an app that needs Smoke.Hello into an empty package folder
#      through the group (Newtonsoft.Json comes through the proxy), and run it
#   4. search (dotnet package search) and read registrations (--exact-match)
#      of the hosted and the proxied package through the group; 16 clients at
#      once on a search of 1000 hits (10 MiB from nuget.org)
#   5. use nuget-private (no anonymous read, writePolicy allow) with the
#      source's stored credentials: the same version is pushed twice
#   6. push 1.0.1 with a symbols package through the group (the .snupkg is
#      left out: the service index has no SymbolPackagePublish resource, and
#      the endpoint refuses one) and delete it with dotnet nuget delete
# The server's request log then shows which V3 resources the client used.
# The sources are plain HTTP, which NuGet takes only with
# allowInsecureConnections (--allow-insecure-connections). The client has no
# route to the internet, and the server stays within its 128 MiB.
# Finally prints the container's memory usage.
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-nuget-$$"
net=commandcenter-nuget-smoke-$$
name=commandcenter-nuget-smoke-$$
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
docker build -q -t commandcenter:smoke-nuget . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i commandcenter:smoke-nuget hash-password)"

mkdir -p "$work"
cp -r test/smoke/nuget/hello test/smoke/nuget/app "$work/"

cat > "$work/config.yaml" <<EOF
anonymousRead: true
baseURL: "http://commandcenter:8081"
users:
  - { name: ci, passwordHash: "$hash", roles: [deployer] }
roles:
  deployer: { write: [nuget-hosted, nuget-private] }
repositories:
  - { name: nuget-proxy,   format: nuget, type: proxy,  remote: "https://api.nuget.org/v3/index.json" }
  - { name: nuget-hosted,  format: nuget, type: hosted }
  - { name: nuget-all,     format: nuget, type: group,  members: [nuget-hosted, nuget-proxy], writeMember: nuget-hosted }
  - { name: nuget-private, format: nuget, type: hosted, writePolicy: allow, anonymousRead: false }
EOF

cat > "$work/run.sh" <<'EOF'
set -eu
export DOTNET_CLI_TELEMETRY_OPTOUT=1 DOTNET_NOLOGO=1
REPO=http://commandcenter:8081/repository
HOSTED="$REPO/nuget-hosted/index.json"
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
cd /tmp/w
cp -r /work/hello /work/app .
printf '<configuration><packageSources><clear /></packageSources></configuration>\n' > NuGet.Config
dotnet nuget add source "$REPO/nuget-all/index.json" -n cc --allow-insecure-connections --configfile NuGet.Config >/dev/null

echo "== pack (the dependency is restored through the group)"
cd /tmp/w/hello
dotnet pack -c Release -o /tmp/out -v q
test -f /tmp/out/Smoke.Hello.1.0.0.nupkg

echo "== push to nuget-hosted with an API key"
says "Your package was pushed" dotnet nuget push /tmp/out/Smoke.Hello.1.0.0.nupkg -s "$HOSTED" -k "$TOKEN" --allow-insecure-connections

echo "== a second push is refused (409), --skip-duplicate accepts that; no key is 401"
refused "409 (Conflict)" dotnet nuget push /tmp/out/Smoke.Hello.1.0.0.nupkg -s "$HOSTED" -k "$TOKEN" --allow-insecure-connections
says "already exists" dotnet nuget push /tmp/out/Smoke.Hello.1.0.0.nupkg -s "$HOSTED" -k "$TOKEN" --allow-insecure-connections --skip-duplicate
refused "401 (Unauthorized)" dotnet nuget push /tmp/out/Smoke.Hello.1.0.0.nupkg -s "$HOSTED" --allow-insecure-connections

echo "== restore into an empty package folder through the group, and run the app"
cd /tmp/w/app
dotnet restore --packages /tmp/packages >/dev/null
test -d /tmp/packages/smoke.hello/1.0.0
test -d /tmp/packages/newtonsoft.json/13.0.3
dotnet build --no-restore -v q >/dev/null
out=$(dotnet run --no-build)
echo "$out"
test "$out" = "hello 7"

echo "== search and registration through the group"
# take 1: the hosted member's hit comes before nuget.org's
says '"id": "Smoke.Hello"' dotnet package search smoke.hello --source cc --take 1 --format json
says '"version": "1.0.0"' dotnet package search Smoke.Hello --source cc --exact-match --format json
says '"version": "13.0.3"' dotnet package search Newtonsoft.Json --source cc --exact-match --format json
curl -fsS "$REPO/nuget-all/index.json" > /tmp/index.json
for t in PackageBaseAddress/3.0.0 RegistrationsBaseUrl/3.6.0 SearchQueryService/3.5.0 PackagePublish/2.0.0; do
  grep -q "\"$t\"" /tmp/index.json || { echo "FAIL: no $t in the service index"; exit 1; }
done
seq 16 | xargs -P 16 -I{} curl -fsS -o /dev/null "$REPO/nuget-all/query?q=microsoft&take=1000&prerelease=true"

echo "== nuget-private: stored credentials, writePolicy allow takes the version twice"
test "$(curl -s -o /dev/null -w '%{http_code}' "$REPO/nuget-private/index.json")" = 401
mkdir /tmp/private
cd /tmp/private
printf '<configuration><packageSources><clear /></packageSources></configuration>\n' > NuGet.Config
dotnet nuget add source "$REPO/nuget-private/index.json" -n private -u ci -p "$TOKEN" --store-password-in-clear-text \
  --allow-insecure-connections --configfile NuGet.Config >/dev/null
says "Your package was pushed" dotnet nuget push /tmp/out/Smoke.Hello.1.0.0.nupkg -s private -k "$TOKEN"
says "Your package was pushed" dotnet nuget push /tmp/out/Smoke.Hello.1.0.0.nupkg -s private -k "$TOKEN"
says '"version": "1.0.0"' dotnet package search Smoke.Hello --source private --exact-match --format json

echo "== 1.0.1 with a symbols package through the group: only the .nupkg is pushed"
cd /tmp/w/hello
dotnet pack -c Release -o /tmp/out -v q -p:Version=1.0.1 -p:IncludeSymbols=true -p:SymbolPackageFormat=snupkg
test -f /tmp/out/Smoke.Hello.1.0.1.snupkg
cd /tmp/w
says "Your package was pushed" dotnet nuget push /tmp/out/Smoke.Hello.1.0.1.nupkg -s cc -k "$TOKEN"
if grep -qi "snupkg" /tmp/out.log; then echo "FAIL: the client pushed the symbols package"; cat /tmp/out.log; exit 1; fi
test "$(curl -s -o /dev/null -w '%{http_code}' -u "ci:$TOKEN" -T /tmp/out/Smoke.Hello.1.0.1.snupkg "$REPO/nuget-hosted/api/v2/symbolpackage")" = 400
test "$(curl -fsS "$REPO/nuget-hosted/v3-flatcontainer/smoke.hello/index.json")" = '{"versions":["1.0.0","1.0.1"]}'

echo "== delete 1.0.1 through the group"
says "was deleted successfully" dotnet nuget delete Smoke.Hello 1.0.1 -s cc -k "$TOKEN" --non-interactive
test "$(curl -fsS "$REPO/nuget-hosted/v3-flatcontainer/smoke.hello/index.json")" = '{"versions":["1.0.0"]}'
test "$(curl -s -o /dev/null -w '%{http_code}' "$REPO/nuget-all/v3-flatcontainer/smoke.hello/1.0.1/smoke.hello.1.0.1.nupkg")" = 404
echo "NUGET CLIENT OK"
EOF

docker network create "$net" >/dev/null
docker network create --internal "$net-clients" >/dev/null
start_postgres
docker run -d --name "$name" --network "$net" -e COMMANDCENTER_DATABASE_URL="$pg_url" --network-alias commandcenter --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" commandcenter:smoke-nuget >/dev/null

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
  mcr.microsoft.com/dotnet/sdk:10.0 sh /work/run.sh

echo "== the V3 resources the client asked for"
docker logs "$name" > "$work/server.log" 2>&1
asked() {
  grep -q "\"method\":\"$1\",\"path\":\"/repository/$2\",\"status\":$3," "$work/server.log" ||
    { echo "FAIL: the client made no $1 $2 answered $3"; exit 1; }
}
asked GET nuget-all/index.json 200                                                        # service index
asked GET nuget-all/v3-flatcontainer/smoke.hello/index.json 200                           # flat container
asked GET nuget-all/v3-flatcontainer/smoke.hello/1.0.0/smoke.hello.1.0.0.nupkg 200
asked GET nuget-all/v3-flatcontainer/newtonsoft.json/13.0.3/newtonsoft.json.13.0.3.nupkg 200
asked GET nuget-all/query 200                                                             # search
asked GET nuget-all/registration/smoke.hello/index.json 200                               # registration
asked GET nuget-all/registration/newtonsoft.json/index.json 200
asked PUT nuget-hosted/api/v2/package/ 201                                                # push
asked PUT nuget-hosted/api/v2/package/ 409
asked DELETE nuget-all/api/v2/package/Smoke.Hello/1.0.1 204

echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "NUGET SMOKE OK"
