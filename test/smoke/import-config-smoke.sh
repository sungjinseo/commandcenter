#!/usr/bin/env bash
# Builds the image and starts two Command Centers: a source and an empty target. The source is
# configured through its API (blob store, cleanup policy, routing rule, content selector, hosted,
# proxy and group repositories, privilege, roles, user) and given a raw file and a Maven artifact.
# Then the image's own `commandcenter import-nexus -config`, in a third container:
#   1. plans with -dry-run, which must change nothing
#   2. makes the configuration on the target and copies the content of the hosted repositories
#   3. runs again and must find nothing to do
# The two instances' exported configuration documents must then be the same, the copied files
# must be served by the target, and the migrated user must sign in with the generated password.
# No Nexus Repository is involved: the source's Nexus API stands in for one.
# Finally prints the containers' memory usage.
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-import-$$"
net=commandcenter-import-smoke-$$
name=commandcenter-import-smoke-$$
. test/postgres.sh
pa=$((20000 + $$ % 20000))
pb=$((pa + 1))
a=http://127.0.0.1:$pa
b=http://127.0.0.1:$pb
v=/service/rest/v1

cleanup() {
  docker rm -f -v "$name-a" "$name-b" "$name-run" "$name-pg" >/dev/null 2>&1 || true # -v: their anonymous data volumes too
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup
rm -rf "$work" && mkdir -p "$work/out"

fail() { echo "FAIL: $*" >&2; exit 1; }

docker build -q -t commandcenter:smoke-import . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i commandcenter:smoke-import hash-password)"
for s in a b; do
  cat > "$work/config-$s.yaml" <<EOF
baseURL: "http://cc-$s:8081"
users:
  - { name: admin, passwordHash: "$hash", roles: [admin] }
EOF
done

docker network create "$net" >/dev/null
start_postgres
docker update --memory 512m --memory-swap 512m "$name-pg" >/dev/null
docker exec "$name-pg" psql -q -U postgres -c 'CREATE DATABASE ccb' >/dev/null
docker run -d --name "$name-a" --network "$net" --network-alias cc-a -e COMMANDCENTER_DATABASE_URL="$pg_url" --memory 128m \
  -v "$root/$work/config-a.yaml:/etc/commandcenter/config.yaml:ro" -p "127.0.0.1:$pa:8081" commandcenter:smoke-import >/dev/null
docker run -d --name "$name-b" --network "$net" --network-alias cc-b -e COMMANDCENTER_DATABASE_URL="${pg_url/\/cc\?/\/ccb\?}" --memory 128m \
  -v "$root/$work/config-b.yaml:/etc/commandcenter/config.yaml:ro" -p "127.0.0.1:$pb:8081" commandcenter:smoke-import >/dev/null

for base in "$a" "$b"; do
  ready=0
  for i in $(seq 1 30); do
    if curl -fs "$base$v/status" >/dev/null; then ready=1; break; fi
    sleep 1
  done
  if [ "$ready" != 1 ]; then
    docker logs "$name-a" >&2 || true
    docker logs "$name-b" >&2 || true
    fail "commandcenter at $base not ready after 30s"
  fi
done

# api <base> <method> <path> [json]: an administrator's request that must succeed.
api() {
  local code
  code="$(curl -s -o "$work/resp" -w '%{http_code}' -u admin:smoke-pass -H 'X-Command-Center-UI: 1' -H 'Content-Type: application/json' \
    -X "$2" ${4:+-d "$4"} "$1$3")"
  case "$code" in 2*) ;; *) fail "$2 $3: HTTP $code $(cat "$work/resp")" ;; esac
}

echo "== configure the source through its API"
api "$a" POST $v/blobstores/file '{"name":"fast","path":"fast","softQuota":{"type":"spaceUsedQuota","limit":1073741824}}'
api "$a" POST $v/cleanup-policies '{"name":"old-prereleases","format":"maven2","notes":"a month","criteriaLastBlobUpdated":30,"criteriaReleaseType":"PRERELEASES"}'
api "$a" POST $v/routing-rules '{"name":"no-internal","description":"ours stay here","mode":"BLOCK","matchers":["^/com/example/.*"]}'
api "$a" POST $v/security/content-selectors '{"name":"team-a","description":"team A","expression":"format == \"maven2\" and path =^ \"/com/example/a/\""}'
api "$a" POST $v/repositories/maven/hosted '{"name":"maven-releases","online":true,
  "storage":{"blobStoreName":"fast","strictContentTypeValidation":false,"writePolicy":"ALLOW_ONCE"},"cleanup":{"policyNames":["old-prereleases"]},
  "maven":{"versionPolicy":"RELEASE","layoutPolicy":"STRICT","contentDisposition":"INLINE"}}'
api "$a" POST $v/repositories/maven/proxy '{"name":"maven-central","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":true},
  "proxy":{"remoteUrl":"https://repo1.maven.org/maven2/","contentMaxAge":-1,"metadataMaxAge":60},"negativeCache":{"enabled":true,"timeToLive":5},
  "httpClient":{"blocked":false,"autoBlock":true},"routingRule":"no-internal",
  "maven":{"versionPolicy":"RELEASE","layoutPolicy":"PERMISSIVE","contentDisposition":"INLINE"}}'
api "$a" POST $v/repositories/maven/group '{"name":"maven-public","online":true,"group":{"memberNames":["maven-releases","maven-central"]}}'
api "$a" POST $v/repositories/raw/hosted '{"name":"raw-hosted","online":true,
  "storage":{"blobStoreName":"default","strictContentTypeValidation":false,"writePolicy":"ALLOW"},"raw":{"contentDisposition":"ATTACHMENT"}}'
api "$a" POST $v/security/privileges/repository-view '{"name":"deploy-releases","description":"deploy","format":"maven2","repository":"maven-releases","actions":["READ","EDIT"]}'
api "$a" POST $v/security/roles '{"id":"z-base","name":"Base","description":"everyone","privileges":["nx-repository-view-*-*-browse"],"roles":[]}'
api "$a" POST $v/security/roles '{"id":"developers","name":"Developers","description":"deploys","privileges":["deploy-releases"],"roles":["z-base"]}'
api "$a" POST $v/security/users '{"userId":"alice","firstName":"Alice","lastName":"Kim","emailAddress":"alice@example.org","password":"source-pass-1","status":"active","roles":["developers"]}'

echo "== give the source content"
pom='<project><modelVersion>4.0.0</modelVersion><groupId>org.example</groupId><artifactId>app</artifactId><version>1.0</version></project>'
# put <path> <content>: uploads a file to a repository of the source.
put() {
  local code
  code="$(curl -s -o "$work/resp" -w '%{http_code}' -u admin:smoke-pass -X PUT --data-binary "$2" "$a/repository/$1")"
  case "$code" in 2*) ;; *) fail "PUT $1: HTTP $code $(cat "$work/resp")" ;; esac
}
put raw-hosted/docs/readme.txt "hello raw"
put maven-releases/org/example/app/1.0/app-1.0.pom "$pom"
put maven-releases/org/example/app/1.0/app-1.0.jar "jar 1.0"

# import <args>: the image's import-nexus from the source to the target; its output goes to $work/import.log.
import() {
  docker run --rm --name "$name-run" --network "$net" --memory 128m --user "$(id -u):$(id -g)" \
    -e NEXUS_PASSWORD=smoke-pass -e COMMANDCENTER_TOKEN=smoke-pass -v "$root/$work/out:/out" commandcenter:smoke-import \
    import-nexus -from http://cc-a:8081 -from-user admin -to http://cc-b:8081 -to-user admin "$@" > "$work/import.log" 2>&1 ||
    { cat "$work/import.log" >&2; fail "import-nexus $* failed"; }
}
# has <text>: the last import's output holds the line part.
has() { grep -qF -- "$1" "$work/import.log" || { cat "$work/import.log" >&2; fail "import-nexus output lacks: $1"; }; }

echo "== dry run: the plan, and nothing made"
import -config -dry-run
has "$(printf 'config\trepository\tmaven-public\twould create')"
has "$(printf 'config\tuser\talice\twould create')"
has 'configuration (dry run, nothing was written): '
[ "$(curl -fs -u admin:smoke-pass "$b$v/repositorySettings")" = "[]" ] || fail "the dry run made repositories"

echo "== import the configuration and the content"
import -config -passwords-out /out/passwords.txt
for line in 'blobstore\tfast\tcreated' 'cleanup-policy\told-prereleases\tcreated' 'routing-rule\tno-internal\tcreated' 'content-selector\tteam-a\tcreated' \
  'repository\tmaven-releases\tcreated' 'repository\tmaven-central\tcreated' 'repository\tmaven-public\tcreated' 'repository\traw-hosted\tcreated' \
  'privilege\tdeploy-releases\tcreated' 'role\tz-base\tcreated' 'role\tdevelopers\tcreated' 'user\talice\tcreated'; do
  has "$(printf "config\t$line")"
done
has '  maven-releases -> maven-releases (maven): 1 copied, 0 skipped, 0 failed'
has '  raw-hosted -> raw-hosted (raw): 1 copied, 0 skipped, 0 failed'
has 'needs manual action: user alice: hand over the generated password from /out/passwords.txt'
tail -n 1 "$work/import.log" | grep -q '^configuration: .* 0 failed, ' || { cat "$work/import.log" >&2; fail "the last line does not summarize"; }

echo "== the two configurations are the same"
sections='blobStores,routingRules,contentSelectors,cleanupPolicies,repositories,roles,users'
curl -fs -u admin:smoke-pass "$a/-/api/configuration?include=$sections" > "$work/a.yaml"
curl -fs -u admin:smoke-pass "$b/-/api/configuration?include=$sections" > "$work/b.yaml"
grep -q 'name: maven-public' "$work/a.yaml" || fail "the source's export lacks its repositories"
diff "$work/a.yaml" "$work/b.yaml" || fail "the exported configurations differ"

echo "== the target serves the content"
[ "$(curl -fs -u admin:smoke-pass "$b/repository/raw-hosted/docs/readme.txt")" = "hello raw" ] || fail "raw file not copied"
[ "$(curl -fs -u admin:smoke-pass "$b/repository/maven-public/org/example/app/1.0/app-1.0.jar")" = "jar 1.0" ] || fail "maven jar not served through the group"

echo "== the migrated user signs in with the generated password"
[ "$(stat -c %a "$work/out/passwords.txt")" = 600 ] || fail "passwords file mode is not 600"
pw="$(awk -F'\t' '$1 == "alice" { print $2 }' "$work/out/passwords.txt")"
curl -fs -u "alice:$pw" "$b/-/api/me" | grep -q '"user":"alice"' || fail "alice cannot sign in on the target"
grep -qF -- "$pw" "$work/import.log" && fail "the generated password was printed"

echo "== a second run has nothing to do"
import -config
has ': 0 created, 0 updated, '
has '  raw-hosted -> raw-hosted (raw): 0 copied, 1 skipped, 0 failed'

echo "IMPORT CONFIG OK"
docker stats --no-stream --format '{{.Name}} memory: {{.MemUsage}}' "$name-a" "$name-b"
