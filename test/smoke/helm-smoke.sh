#!/usr/bin/env bash
# Builds the image, starts commandcenter, then uses the real helm client
# (alpine/helm: Helm 3.22; SMOKE_HELM_IMAGE=alpine/helm:4.3.1 runs Helm 4) to:
#   1. package and sign a chart (helm package --sign), upload it and its
#      provenance file to helm-hosted with curl -T; a second upload is refused
#   2. helm repo add, update, search and pull it, and helm pull --verify
#   3. push the next version with the cm-push plugin (--context-path), when
#      the plugin installs, else with curl; and a chart with its provenance
#      file as ChartMuseum's form (chart and prov fields)
#   4. upload and pull a chart whose version is not strict SemVer (v1.2.3, 1.2)
#   5. upload a chart and a provenance file through the component upload API
#   6. resolve a chart dependency from the helm-all group (helm dependency
#      update), pull through the group and upload through it
#   7. DELETE a chart, as scripts written for Nexus do
#   8. pull cert-manager through helm-proxy (charts.jetstack.io) with --verify:
#      a v-prefixed version, its provenance file through the proxy, and the
#      cached chart listed as a component
#   9. publish a version again in a chart repository the test serves
#      (helm repo index): helm-mirror follows after invalidate-cache, and a
#      version newer than the cached index is found by its URL
#  10. pull through a proxy with a routing rule that names charts
# The proxy downloads about 0.7 MB from charts.jetstack.io.
# Finally prints the container's memory usage.
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-helm-$$"
net=commandcenter-helm-smoke-$$
name=commandcenter-helm-smoke-$$
helm_image="${SMOKE_HELM_IMAGE:-alpine/helm:3.22.1}"
. test/postgres.sh

cleanup() {
  docker rm -f -v "$name" "$name-pg" "$name-up" "$name-client" >/dev/null 2>&1 || true
  docker volume rm "$name-up" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup
rm -rf "$work" && mkdir -p "$work"

docker build -q -t commandcenter:smoke-helm . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i --name "$name-hash" --memory 256m commandcenter:smoke-helm hash-password)"

cat > "$work/config.yaml" <<EOF
anonymousRead: true
baseURL: "http://commandcenter:8081"
users:
  - { name: admin, passwordHash: "$hash", roles: [admin] }
  - { name: ci, passwordHash: "$hash", roles: [deployer] }
roles:
  deployer: { write: [helm-hosted] }
routingRules:
  - { name: only-demo, mode: ALLOW, matchers: ['^/index\\.yaml\$', '^/demo-.*'] }
repositories:
  - { name: helm-hosted, format: helm, type: hosted }
  - { name: helm-proxy,  format: helm, type: proxy, remote: "https://charts.jetstack.io/" }
  - { name: helm-all,    format: helm, type: group, members: [helm-hosted, helm-proxy], writeMember: helm-hosted }
  - { name: helm-mirror, format: helm, type: proxy, remote: "http://upstream:8000/" }
  - { name: helm-ruled,  format: helm, type: proxy, remote: "http://upstream:8000/", routingRule: only-demo }
  - { name: helm-ruled-all, format: helm, type: group, members: [helm-ruled] }
EOF

cat > "$work/run.sh" <<'EOF'
set -eu
CC=http://commandcenter:8081
R=$CC/repository
fail() { echo "FAIL: $*"; exit 1; }
status() { curl -s -o /tmp/body -w '%{http_code}' "$@"; }
# want <status> <what> <curl arguments>: the request must answer <status>
want() { w="$1" what="$2"; shift 2; got="$(status "$@")"; [ "$got" = "$w" ] || fail "$what: $got, want $w: $(head -c 300 /tmp/body)"; }
sha() { sha256sum "$1" | cut -d' ' -f1; }
# listed <repo/chart>: the versions helm search finds, one per line
listed() { helm search repo "$1" --versions | awk 'NR>1 {print $2}'; }
# pull <repo/chart> <version> [helm pull flags]: downloads into a new directory and prints the file
pull() { c="$1" v="$2"; shift 2; d="$(mktemp -d)"; helm pull "$c" --version "$v" -d "$d" "$@" >/tmp/pull.log 2>&1 || { cat /tmp/pull.log; fail "helm pull $c $v $*"; }; ls "$d"/*.tgz; }
helm version --short
apk add --no-cache gnupg >/dev/null 2>&1 || fail "apk add gnupg (the test signs a chart)"
mkdir /tmp/w && cd /tmp/w

echo "== package and sign a chart"
export GNUPGHOME=/tmp/gnupg; mkdir -m 700 $GNUPGHOME
gpg --batch --pinentry-mode loopback --passphrase '' --quick-generate-key 'Smoke Signer <smoke@example.com>' rsa2048 sign never 2>/dev/null
gpg --batch --pinentry-mode loopback --passphrase '' --export-secret-keys > /tmp/secring.gpg 2>/dev/null
gpg --export > /tmp/pubring.gpg
helm create demo >/dev/null
# pkg <version>: packages demo at that version, signed
pkg() { sed -i "s/^version: .*/version: \"$1\"/" demo/Chart.yaml; helm package demo --sign --key 'Smoke Signer' --keyring /tmp/secring.gpg >/dev/null 2>&1 || fail "helm package --sign $1"; }
pkg 1.0.0
test -s demo-1.0.0.tgz.prov

echo "== upload with curl -T: the provenance file needs its chart; a second upload is refused"
want 400 "provenance file before its chart" -u ci:smoke-pass -T demo-1.0.0.tgz.prov $R/helm-hosted/
want 201 "PUT chart" -u ci:smoke-pass -T demo-1.0.0.tgz $R/helm-hosted/
grep -q '"saved":true' /tmp/body
want 201 "PUT provenance file" -u ci:smoke-pass -T demo-1.0.0.tgz.prov $R/helm-hosted/
want 409 "PUT chart again" -u ci:smoke-pass -T demo-1.0.0.tgz $R/helm-hosted/
want 401 "PUT without credentials" -T demo-1.0.0.tgz $R/helm-hosted/

echo "== helm repo add, update, search, pull, pull --verify"
helm repo add cc $R/helm-hosted >/dev/null
helm repo add all $R/helm-all >/dev/null
helm repo update >/dev/null
listed cc/demo | grep -qx 1.0.0 || fail "helm search repo does not list demo 1.0.0"
f="$(pull cc/demo 1.0.0)"
test "$(sha "$f")" = "$(sha demo-1.0.0.tgz)"
f="$(pull cc/demo 1.0.0 --verify --keyring /tmp/pubring.gpg)"
grep -q "Signed by: Smoke Signer" /tmp/pull.log || { cat /tmp/pull.log; fail "helm pull --verify did not name the signer"; }
test -s "$f.prov"
helm template x "$f" >/dev/null

echo "== cm-push plugin"
pkg 1.1.0
if helm plugin install https://github.com/chartmuseum/helm-push >/tmp/plugin.log 2>&1 ||
   helm plugin install --verify=false https://github.com/chartmuseum/helm-push >/tmp/plugin.log 2>&1; then  # Helm 4: the plugin has no provenance
  helm plugin list | grep cm-push
  # The plugin posts to <context path>/api/charts; without --context-path it
  # takes the repository URL's path for a ChartMuseum tenant (/api/repository/helm-hosted/charts).
  helm cm-push demo-1.1.0.tgz cc -u ci -p smoke-pass --context-path /repository/helm-hosted >/tmp/push.log 2>&1 || { cat /tmp/push.log; fail "helm cm-push"; }
  if helm cm-push demo-1.1.0.tgz cc -u ci -p smoke-pass --context-path /repository/helm-hosted >/tmp/push.log 2>&1; then fail "a second cm-push succeeded"; fi
  grep -q "409" /tmp/push.log || { cat /tmp/push.log; fail "the second cm-push did not report 409"; }
else
  echo "SKIP: the cm-push plugin did not install ($(tail -1 /tmp/plugin.log)); pushing 1.1.0 with curl"
  want 201 "POST api/charts" -u ci:smoke-pass -F chart=@demo-1.1.0.tgz $R/helm-hosted/api/charts
fi
# The plugin packages the chart again, so what it stores is not the file a
# provenance file was made for; ChartMuseum's form takes both as they are.
pkg 1.1.1
want 201 "POST api/charts with chart and prov fields" -u ci:smoke-pass -F chart=@demo-1.1.1.tgz -F prov=@demo-1.1.1.tgz.prov $R/helm-hosted/api/charts
helm repo update >/dev/null
pull cc/demo 1.1.0 >/dev/null
pull cc/demo 1.1.1 --verify --keyring /tmp/pubring.gpg >/dev/null

echo "== chart versions that are not strict SemVer"
for v in v1.2.3 1.2; do
  pkg "$v"
  want 201 "PUT demo-$v.tgz" -u ci:smoke-pass -T "demo-$v.tgz" $R/helm-hosted/
done
helm repo update >/dev/null
order="$(curl -fsS $R/helm-hosted/index.yaml | sed -n 's/^ *version: //p' | tr -d '"' | tr '\n' ' ')"
test "$order" = "v1.2.3 1.2 1.1.1 1.1.0 1.0.0 " || fail "index.yaml does not list the versions newest first: $order"
test "$(sha "$(pull cc/demo v1.2.3)")" = "$(sha demo-v1.2.3.tgz)"
test "$(sha "$(pull cc/demo 1.2)")" = "$(sha demo-1.2.tgz)"
sed -i 's/^version: .*/version: latest/' demo/Chart.yaml
if helm package demo >/dev/null 2>&1; then fail "helm packaged the version \"latest\""; fi
# A version with a version in it: its file name also reads as chart demo-1.0.0 version 2.3.4.
pkg 1.0.0-2.3.4
want 201 "PUT demo-1.0.0-2.3.4.tgz" -u ci:smoke-pass -T demo-1.0.0-2.3.4.tgz $R/helm-hosted/
helm repo update >/dev/null
test "$(sha "$(pull cc/demo 1.0.0-2.3.4)")" = "$(sha demo-1.0.0-2.3.4.tgz)"
want 200 "DELETE demo-1.0.0-2.3.4.tgz" -u ci:smoke-pass -X DELETE $R/helm-hosted/demo-1.0.0-2.3.4.tgz
curl -fsS $R/helm-hosted/index.yaml | grep -q '1\.0\.0-2\.3\.4' && fail "a deleted version is still in the index"

echo "== component upload API: a chart and its provenance file"
pkg 1.3.0
want 204 "REST upload of a chart" -u ci:smoke-pass -F helm.asset=@demo-1.3.0.tgz "$CC/service/rest/v1/components?repository=helm-hosted"
want 204 "REST upload of a provenance file" -u ci:smoke-pass -F helm.asset=@demo-1.3.0.tgz.prov "$CC/service/rest/v1/components?repository=helm-hosted"
curl -fsS "$CC/service/rest/v1/search?repository=helm-hosted&name=demo&version=1.3.0" > /tmp/search.json
grep -q '"path":"/demo-1.3.0.tgz"' /tmp/search.json && grep -q '"path":"/demo-1.3.0.tgz.prov"' /tmp/search.json || { cat /tmp/search.json; fail "the component does not list the chart and its provenance file"; }

echo "== the group: a dependency, a pull, an upload"
mkdir -p app && printf 'apiVersion: v2\nname: app\nversion: 0.1.0\ndependencies:\n  - name: demo\n    version: 1.0.0\n    repository: %s\n' "$R/helm-all" > app/Chart.yaml
helm repo update >/dev/null
helm dependency update app >/tmp/dep.log 2>&1 || { cat /tmp/dep.log; fail "helm dependency update"; }
test "$(sha app/charts/demo-1.0.0.tgz)" = "$(sha demo-1.0.0.tgz)"
pull all/demo 1.3.0 --verify --keyring /tmp/pubring.gpg >/dev/null
pkg 1.4.0
want 201 "PUT through the group" -u ci:smoke-pass -T demo-1.4.0.tgz $R/helm-all/
want 200 "the chart in the group's writeMember" $R/helm-hosted/demo-1.4.0.tgz

echo "== DELETE a chart"
want 401 "DELETE without credentials" -X DELETE $R/helm-hosted/demo-1.4.0.tgz
want 200 "DELETE" -u ci:smoke-pass -X DELETE $R/helm-hosted/demo-1.4.0.tgz
want 404 "DELETE again" -u ci:smoke-pass -X DELETE $R/helm-hosted/demo-1.4.0.tgz
want 200 "DELETE a chart with a provenance file" -u ci:smoke-pass -X DELETE $R/helm-hosted/demo-1.3.0.tgz
want 404 "the provenance file of a deleted chart" $R/helm-hosted/demo-1.3.0.tgz.prov
helm repo update >/dev/null
if listed cc/demo | grep -qx -e 1.4.0 -e 1.3.0; then fail "a deleted version is still in the index"; fi

echo "== proxy of charts.jetstack.io: a v-prefixed version, verified with its provenance file"
helm repo add ccp $R/helm-proxy >/dev/null
f="$(pull ccp/cert-manager v1.16.2)"
if curl -fsS -o /tmp/cm.gpg https://cert-manager.io/public-keys/cert-manager-keyring-2021-09-20-1020CF3C033D4F35BAE1C19E1226061C665DF13E.gpg; then
  pull all/cert-manager v1.16.2 --verify --keyring /tmp/cm.gpg >/dev/null
  grep -q "Signed by: " /tmp/pull.log || { cat /tmp/pull.log; fail "helm pull --verify through the proxy"; }
else
  echo "SKIP: cert-manager's keyring could not be downloaded; the provenance file is fetched without helm verifying it"
  curl -fsS $R/helm-proxy/cert-manager-v1.16.2.tgz.prov | grep -q "BEGIN PGP SIGNED MESSAGE"
fi
curl -fsS "$CC/service/rest/v1/search?repository=helm-proxy&name=cert-manager" > /tmp/search.json
grep -q '"version":"v1.16.2"' /tmp/search.json && grep -q '"path":"/cert-manager-v1.16.2.tgz.prov"' /tmp/search.json || { cat /tmp/search.json; fail "the cached chart is no component of the proxy"; }
want 404 "a chart jetstack does not have" $R/helm-proxy/cert-manager-v0.0.0.tgz

echo "== a version published again upstream: the proxy follows after invalidate-cache"
# upstream <description> [version]: publishes demo 1.0.0 with other content, other 1.0.0 and demo <version> under /up/charts/.
# The test's server knows Last-Modified to the second, so the next publish waits for the next second.
upstream() {
  sleep 1
  mkdir -p /up/charts /tmp/up && rm -rf /tmp/up/* && cp -r demo /tmp/up/demo && cp -r demo /tmp/up/other
  sed -i -e 's/^version: .*/version: 1.0.0/' -e "s/^description: .*/description: $1/" /tmp/up/demo/Chart.yaml
  sed -i -e 's/^version: .*/version: 1.0.0/' -e 's/^name: .*/name: other/' /tmp/up/other/Chart.yaml
  helm package /tmp/up/demo /tmp/up/other -d /up/charts >/dev/null
  if [ -n "${2:-}" ]; then sed -i "s/^version: .*/version: $2/" /tmp/up/demo/Chart.yaml; helm package /tmp/up/demo -d /up/charts >/dev/null; fi
  helm repo index /up/charts --url charts && mv /up/charts/index.yaml /up/index.yaml
}
upstream first
helm repo add mirror $R/helm-mirror >/dev/null
first="$(sha "$(pull mirror/demo 1.0.0)")"
test "$first" = "$(sha /up/charts/demo-1.0.0.tgz)"
upstream second
second="$(sha /up/charts/demo-1.0.0.tgz)"
test "$first" != "$second"
helm repo update mirror >/dev/null
test "$(sha "$(pull mirror/demo 1.0.0)")" = "$first"          # the cached index stands for metadataMaxAge
want 403 "invalidate-cache as a deployer" -u ci:smoke-pass -X POST "$CC/service/rest/v1/repositories/helm-mirror/invalidate-cache"
want 204 "invalidate-cache" -u admin:smoke-pass -X POST "$CC/service/rest/v1/repositories/helm-mirror/invalidate-cache"
helm repo update mirror >/dev/null
test "$(sha "$(pull mirror/demo 1.0.0)")" = "$second" || fail "the proxy still serves the chart as it was before it was published again"

echo "== a version newer than the proxy's cached index is found by its URL, and the index is read again"
upstream third 1.1.0
want 200 "a version newer than the cached index" $R/helm-mirror/demo-1.1.0.tgz
test "$(sha /tmp/body)" = "$(sha /up/charts/demo-1.1.0.tgz)"
helm repo update mirror >/dev/null
listed mirror/demo | grep -qx 1.1.0 || fail "the proxy's index does not list the new version"
test "$(sha "$(pull mirror/demo 1.0.0)")" = "$(sha /up/charts/demo-1.0.0.tgz)" || fail "the chart published a third time was not fetched again"

echo "== a routing rule that names charts sees the request path, not the upstream's charts/ path"
helm repo add ruled $R/helm-ruled >/dev/null
helm repo add ruled-all $R/helm-ruled-all >/dev/null
test "$(sha "$(pull ruled/demo 1.0.0)")" = "$(sha /up/charts/demo-1.0.0.tgz)"
test "$(sha "$(pull ruled-all/demo 1.1.0)")" = "$(sha /up/charts/demo-1.1.0.tgz)"
want 403 "a chart the rule does not allow" $R/helm-ruled/other-1.0.0.tgz
want 404 "the same through a group" $R/helm-ruled-all/other-1.0.0.tgz
echo "HELM CLIENT OK"
EOF

docker network create "$net" >/dev/null
docker volume create "$name-up" >/dev/null
start_postgres
# The chart repository helm-mirror and helm-ruled proxy: what the client writes into the volume.
docker run -d --name "$name-up" --network "$net" --network-alias upstream --memory 128m -v "$name-up:/srv:ro" \
  python:3.12-slim python -m http.server -d /srv 8000 >/dev/null
docker run -d --name "$name" --network "$net" -e COMMANDCENTER_DATABASE_URL="$pg_url" --network-alias commandcenter --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" commandcenter:smoke-helm >/dev/null

# No port is published: the image's own health check asks the server.
ready=0
for i in $(seq 1 30); do
  if docker exec "$name" commandcenter healthcheck 2>/dev/null; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "FAIL: commandcenter did not become ready"; docker logs "$name"; exit 1
fi

docker run --rm --name "$name-client" --network "$net" --memory 1g -v "$root/$work:/work:ro" -v "$name-up:/up" \
  --entrypoint sh "$helm_image" /work/run.sh

echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "HELM SMOKE OK"
