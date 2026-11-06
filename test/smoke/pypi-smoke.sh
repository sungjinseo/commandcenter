#!/usr/bin/env bash
# Builds the image, starts commandcenter, then uses real pip, twine and uv (python:3.12) to:
#   1. install build tools through the pypi-all group (files come from PyPI via pypi-proxy)
#   2. build smoke-hello (its build backend is also fetched through the proxy)
#   3. upload it to pypi-hosted with twine; a second upload must be rejected, in
#      words --skip-existing knows (twine 6.1: from 6.2 twine refuses that flag
#      itself for any repository but PyPI)
#   4. install it into a fresh venv through the group, with pip and with uv; its
#      dependency (six) comes via the proxy
#   5. pip download through the proxy, with hashes and by the Nexus path of a file
#   6. replace a file under writePolicy allow (pypi-dev), the new bytes are what pip gets
#   7. upload a wheel with 600 extras (1200 form fields) and one with uv publish
#   8. twine upload --sign: gpg --verify the signature it stored, by both path forms
#   9. pip search (XML-RPC) of a hosted repository and through the group
#  10. the Nexus API: one asset per wheel, search/assets/download, the pypi attributes,
#      other spellings of a name, pypi.summary, DELETE of one file
#  11. a routing rule that names projects, directly and through a group: the files
#      are on files.pythonhosted.org, the rule sees the request path
#  12. a project named like a public one: the group offers only the hosted files, a
#      group with mergeMembers both
#  13. the root page of pypi-proxy: pypi.org's whole list (about 48 MB), in 128 MiB
# The proxy downloads about 100 MB from pypi.org and files.pythonhosted.org.
# Finally prints the container's memory usage.
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-pypi-$$"
net=commandcenter-pypi-smoke-$$
name=commandcenter-pypi-smoke-$$
. test/postgres.sh

cleanup() {
  docker rm -f -v "$name" "$name-pg" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap 'docker rm -f "$name-client" >/dev/null 2>&1 || true; cleanup' EXIT
cleanup
rm -rf "$work" && mkdir -p "$work"
cp -r test/smoke/pypi/pkg "$work/pkg"

docker build -q -t commandcenter:smoke-pypi . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i --name "$name-hash" --memory 256m commandcenter:smoke-pypi hash-password)"

cat > "$work/config.yaml" <<EOF
anonymousRead: true
baseURL: "http://commandcenter:8081"
users:
  - { name: ci, passwordHash: "$hash", roles: [deployer] }
roles:
  deployer: { write: [pypi-hosted, pypi-dev] }
routingRules:
  - { name: only-six, mode: ALLOW, matchers: ['^/simple/six/.*', '^/packages/six/.*'] }
repositories:
  - { name: pypi-proxy,  format: pypi, type: proxy,  remote: "https://pypi.org/" }
  - { name: pypi-hosted, format: pypi, type: hosted }
  - { name: pypi-dev,    format: pypi, type: hosted, writePolicy: allow }
  - { name: pypi-all,    format: pypi, type: group,  members: [pypi-hosted, pypi-proxy], writeMember: pypi-hosted }
  - { name: pypi-merged, format: pypi, type: group,  members: [pypi-hosted, pypi-proxy], mergeMembers: true }
  - { name: pypi-ruled,  format: pypi, type: proxy,  remote: "https://pypi.org/", routingRule: only-six }
  - { name: pypi-ruled-all, format: pypi, type: group, members: [pypi-ruled] }
EOF

cat > "$work/run.sh" <<'EOF'
set -eu
fail() { echo "FAIL: $*"; exit 1; }
CC=http://commandcenter:8081
R=$CC/repository
API=$CC/service/rest/v1
export PIP_INDEX_URL=$R/pypi-all/simple/
export PIP_TRUSTED_HOST=commandcenter PIP_DISABLE_PIP_VERSION_CHECK=1 PIP_NO_CACHE_DIR=1 PIP_ROOT_USER_ACTION=ignore
UPLOAD=$R/pypi-hosted/
TWINE="twine upload --non-interactive --disable-progress-bar -u ci -p smoke-pass"
apt-get update -qq && apt-get install -y -qq --no-install-recommends curl gnupg >/dev/null 2>&1 || fail "apt-get install curl gnupg (the test signs a file)"
status() { curl -s -o /tmp/body -w '%{http_code}' "$@"; }
# want <status> <what> <curl arguments>: the request must answer <status>
want() { w="$1" what="$2"; shift 2; got="$(status "$@")"; [ "$got" = "$w" ] || fail "$what: $got, want $w: $(head -c 300 /tmp/body)"; }
sha() { sha256sum "$1" | cut -d' ' -f1; }
# json <python expression on d>: reads the JSON in /tmp/body
json() { /usr/local/bin/python -c "import json; d = json.load(open('/tmp/body')); print($1)"; }

echo "== install build tools through the proxy"
python -m venv /tmp/tools && . /tmp/tools/bin/activate
pip install -q build twine uv
pip --version; twine --version | head -1; uv --version; gpg --version | head -1

echo "== build (the work directory is mounted read-only: everything is built under /tmp)"
cp -r /work/pkg /tmp/pkg && cd /tmp/pkg && python -m build -q >/dev/null

echo "== upload"
$TWINE --repository-url "$UPLOAD" dist/*

echo "== re-upload must be rejected; --skip-existing of twine 6.1 passes"
if $TWINE --verbose --repository-url "$UPLOAD" dist/* >/tmp/reupload.log 2>&1; then
  echo "FAIL: re-upload succeeded"; exit 1
fi
grep -qi "already exists" /tmp/reupload.log || { echo "FAIL: no already-exists message"; cat /tmp/reupload.log; exit 1; }
python -m venv /tmp/twine61 && /tmp/twine61/bin/pip install -q twine==6.1.0
/tmp/twine61/bin/twine upload --non-interactive --disable-progress-bar -u ci -p smoke-pass --skip-existing --repository-url "$UPLOAD" dist/* >/tmp/skip.log 2>&1 ||
  { cat /tmp/skip.log; fail "twine --skip-existing"; }
grep -q "appears to already exist" /tmp/skip.log || { cat /tmp/skip.log; fail "twine did not skip"; }

echo "== install through the group (dependency via the proxy)"
python -m venv /tmp/app
/tmp/app/bin/pip install -q smoke-hello
/tmp/app/bin/python -c 'import smoke_hello; assert smoke_hello.greet() == "hello six"'

echo "== uv pip install through the group and through the proxy"
uv venv -q /tmp/uvapp
VIRTUAL_ENV=/tmp/uvapp uv pip install -q --no-cache --index-url "$R/pypi-all/simple/" smoke-hello
/tmp/uvapp/bin/python -c 'import smoke_hello; assert smoke_hello.greet() == "hello six"'
uv venv -q /tmp/uvproxy
VIRTUAL_ENV=/tmp/uvproxy uv pip install -q --no-cache --index-url "$R/pypi-proxy/simple/" idna==3.10
/tmp/uvproxy/bin/python -c 'import idna'

echo "== pip download through the proxy: with hashes, and by the Nexus path of a file"
mkdir /tmp/dl
pip download -q --no-deps -d /tmp/dl -i "$R/pypi-proxy/simple/" six==1.16.0
wheel=six-1.16.0-py2.py3-none-any.whl
test -s "/tmp/dl/$wheel"
printf 'six==1.16.0 --hash=sha256:%s\n' "$(sha "/tmp/dl/$wheel")" > /tmp/req.txt
pip download -q --no-deps --require-hashes -d /tmp/dl2 -i "$R/pypi-proxy/simple/" -r /tmp/req.txt
pip index versions -i "$R/pypi-proxy/simple/" six | grep -q '1\.16\.0'
smoke_wheel=smoke_hello-1.0.0-py3-none-any.whl
test "$(curl -fsS "$R/pypi-hosted/packages/smoke-hello/1.0.0/$smoke_wheel" | sha256sum | cut -d' ' -f1)" = "$(sha "dist/$smoke_wheel")" || fail "the Nexus path of a file"

echo "== writePolicy allow: the same file again replaces it"
cp -r /work/pkg /tmp/dev && cd /tmp/dev
python -m build -q --wheel >/dev/null
$TWINE --repository-url "$R/pypi-dev/" dist/*
first="$(sha "dist/$smoke_wheel")"
echo 'REBUILT = True' >> smoke_hello/__init__.py
rm -rf dist build && python -m build -q --wheel >/dev/null
second="$(sha "dist/$smoke_wheel")"
test "$first" != "$second"
$TWINE --repository-url "$R/pypi-dev/" dist/*
pip download -q --no-deps -d /tmp/dev-dl -i "$R/pypi-dev/simple/" smoke-hello==1.0.0
test "$(sha "/tmp/dev-dl/$smoke_wheel")" = "$second" || fail "pip got the file as it was before the upload replaced it"
curl -fsS "$R/pypi-dev/simple/smoke-hello/" | grep -q "sha256=$second" || fail "the project page lists the old hash"

echo "== a wheel with 600 extras: 1200 form fields"
mkdir -p /tmp/big/smoke_big && cd /tmp/big && : > smoke_big/__init__.py
python - <<'PY'
extras = "\n".join('e%d = ["dep%d"]' % (i, i) for i in range(600))
open("pyproject.toml", "w").write('[build-system]\nrequires = ["setuptools>=68"]\nbuild-backend = "setuptools.build_meta"\n\n'
    '[project]\nname = "smoke-big"\nversion = "1.0.0"\n\n[project.optional-dependencies]\n' + extras + '\n\n[tool.setuptools]\npackages = ["smoke_big"]\n')
PY
python -m build -q --wheel >/dev/null
$TWINE --repository-url "$UPLOAD" dist/*
pip download -q --no-deps -d /tmp/big-dl -i "$R/pypi-hosted/simple/" smoke-big==1.0.0
test -s /tmp/big-dl/smoke_big-1.0.0-py3-none-any.whl

echo "== uv publish"
mkdir -p /tmp/uvp/smoke_uv && cd /tmp/uvp && : > smoke_uv/__init__.py
printf '[build-system]\nrequires = ["setuptools>=68"]\nbuild-backend = "setuptools.build_meta"\n\n[project]\nname = "smoke-uv"\nversion = "1.0.0"\n\n[tool.setuptools]\npackages = ["smoke_uv"]\n' > pyproject.toml
python -m build -q >/dev/null
uv publish -q --publish-url "$UPLOAD" -u ci -p smoke-pass dist/*
pip download -q --no-deps -d /tmp/uv-dl -i "$R/pypi-hosted/simple/" smoke-uv==1.0.0
if uv publish -q --publish-url "$UPLOAD" -u ci -p smoke-pass dist/* >/tmp/uvagain.log 2>&1; then fail "a second uv publish succeeded"; fi

echo "== twine upload --sign: the signature is stored and verifies"
mkdir -p /tmp/signed/smoke_signed && cd /tmp/signed && : > smoke_signed/__init__.py
printf '[build-system]\nrequires = ["setuptools>=68"]\nbuild-backend = "setuptools.build_meta"\n\n[project]\nname = "smoke-signed"\nversion = "1.0.0"\n\n[tool.setuptools]\npackages = ["smoke_signed"]\n' > pyproject.toml
python -m build -q --wheel >/dev/null
export GNUPGHOME=/tmp/gnupg; mkdir -m 700 $GNUPGHOME
gpg --batch --pinentry-mode loopback --passphrase '' --quick-generate-key 'Smoke Signer <smoke@example.com>' rsa2048 sign never 2>/dev/null
$TWINE --sign --identity smoke@example.com --repository-url "$UPLOAD" dist/smoke_signed-1.0.0-py3-none-any.whl >/dev/null
signed=smoke_signed-1.0.0-py3-none-any.whl
for p in "packages/smoke-signed/$signed.asc" "packages/smoke-signed/1.0.0/$signed.asc"; do
  curl -fsS -o /tmp/sig.asc "$R/pypi-hosted/$p" || fail "GET $p"
  gpg --verify /tmp/sig.asc "dist/$signed" 2>/dev/null || fail "gpg --verify of $p"
done
curl -fsS -o /tmp/sig.asc "$R/pypi-all/packages/smoke-signed/$signed.asc" || fail "the signature through the group"
curl -fsS "$R/pypi-hosted/simple/smoke-signed/" | grep -q 'data-gpg-sig="true"' || fail "the file's link does not say it has a signature"
want 200 "the JSON API" "$R/pypi-hosted/pypi/smoke-signed/json"
test "$(json "d['urls'][0]['has_sig']")" = True || fail "has_sig in the JSON API"

echo "== pip search"
pip search -i "$R/pypi-hosted/pypi" smoke-he </dev/null | grep -q '^smoke-hello (1\.0\.0)' || fail "pip search of the hosted repository"
pip search -i "$R/pypi-all/pypi" smoke-hello </dev/null | grep -q '^smoke-hello (1\.0\.0)' || fail "pip search through the group"
if pip search -i "$R/pypi-proxy/pypi" smoke-hello </dev/null >/tmp/search.log 2>&1; then
  test ! -s /tmp/search.log || { cat /tmp/search.log; fail "a proxy's pip search"; }
fi

echo "== the Nexus API"
want 200 "components" "$API/search?repository=pypi-hosted&name=smoke-big"
test "$(json "len(d['items'][0]['assets'])")" = 1 || fail "a release of one wheel is one asset: $(head -c 600 /tmp/body)"
test "$(curl -s -o /dev/null -w '%{http_code} %{redirect_url}' "$API/search/assets/download?repository=pypi-hosted&name=smoke-big&version=1.0.0")" = \
  "302 $R/pypi-hosted/packages/smoke-big/smoke_big-1.0.0-py3-none-any.whl" || fail "search/assets/download"
want 200 "search assets" "$API/search/assets?repository=pypi-hosted&name=smoke-big"
test "$(json "d['items'][0]['pypi']['name'] + ' ' + d['items'][0]['pypi']['version']")" = "smoke-big 1.0.0" || fail "the pypi attributes"
for spelled in smoke_big smoke.big SMOKE-BIG; do
  want 200 "name=$spelled" "$API/search?repository=pypi-hosted&name=$spelled"
  test "$(json "len(d['items'])")" = 1 || fail "name=$spelled finds nothing"
done
want 200 "pypi.summary" "$API/search?repository=pypi-hosted&pypi.summary=nomatch"
test "$(json "len(d['items'])")" = 0 || fail "pypi.summary=nomatch"
want 200 "components of smoke-hello" "$API/search?repository=pypi-hosted&name=smoke-hello"
sdist_id="$(json "[a['id'] for a in d['items'][0]['assets'] if a['path'].endswith('.tar.gz')][0]")"
test "$(json "len(d['items'][0]['assets'])")" = 2 || fail "a wheel and an sdist are two assets"
want 401 "DELETE of one file without credentials" -X DELETE "$API/assets/$sdist_id"
want 204 "DELETE of one file" -u ci:smoke-pass -X DELETE "$API/assets/$sdist_id"
curl -fsS "$R/pypi-hosted/simple/smoke-hello/" > /tmp/page.html
grep -q "$smoke_wheel" /tmp/page.html && ! grep -q 'smoke_hello-1.0.0.tar.gz' /tmp/page.html || fail "the project page after the DELETE: $(cat /tmp/page.html)"
pip download -q --no-deps -d /tmp/after -i "$R/pypi-hosted/simple/" smoke-hello==1.0.0
want 404 "the core metadata of the deleted file" "$R/pypi-hosted/packages/smoke-hello/smoke_hello-1.0.0.tar.gz.metadata"

echo "== a routing rule that names projects sees the request path"
for rp in pypi-ruled pypi-ruled-all; do
  pip download -q --no-deps -d "/tmp/ruled-$rp" -i "$R/$rp/simple/" six==1.17.0
  test -s "/tmp/ruled-$rp/six-1.17.0-py2.py3-none-any.whl"
  if pip download -q --no-deps -d "/tmp/refused-$rp" -i "$R/$rp/simple/" idna >/dev/null 2>&1; then fail "$rp: a project the rule does not name"; fi
done
want 403 "a file the rule does not name" "$R/pypi-ruled/packages/idna/idna-3.10-py3-none-any.whl"

echo "== a project named like a public one: the group's owner rule, and mergeMembers"
mkdir -p /tmp/six/six_stub && cd /tmp/six && : > six_stub/__init__.py
printf '[build-system]\nrequires = ["setuptools>=68"]\nbuild-backend = "setuptools.build_meta"\n\n[project]\nname = "six"\nversion = "99.0"\n\n[tool.setuptools]\npackages = ["six_stub"]\n' > pyproject.toml
python -m build -q --wheel >/dev/null
$TWINE --repository-url "$UPLOAD" dist/six-99.0-py3-none-any.whl >/dev/null
test "$(pip index versions -i "$R/pypi-all/simple/" six | sed -n 's/^Available versions: //p')" = 99.0 || fail "the default group offers more than the hosted files"
want 404 "a public file through the default group" "$R/pypi-all/packages/six/$wheel"
versions="$(pip index versions -i "$R/pypi-merged/simple/" six | sed -n 's/^Available versions: //p')"
case "$versions" in 99.0,*1.16.0*) ;; *) fail "the merged group offers: $versions" ;; esac
pip download -q --no-deps -d /tmp/merged-dl -i "$R/pypi-merged/simple/" six==1.16.0
test "$(sha "/tmp/merged-dl/$wheel")" = "$(sha "/tmp/dl/$wheel")"
pip download -q --no-deps -d /tmp/merged-dl -i "$R/pypi-merged/simple/" six==99.0
test -s /tmp/merged-dl/six-99.0-py3-none-any.whl

echo "== the root page of the proxy: pypi.org's list"
curl -fsS -o /tmp/root.html "$R/pypi-proxy/simple/"
links="$(grep -c '<a ' /tmp/root.html)"
test "$links" -gt 500000 || fail "the proxy lists $links projects"
grep -q '<a href="six/">six</a>' /tmp/root.html || fail "no six in the proxy's list"
curl -fsS -H 'Accept: application/vnd.pypi.simple.v1+json' -o /tmp/body "$R/pypi-proxy/simple/"
test "$(json "len(d['projects'])")" = "$links" || fail "the JSON list differs"
curl -fsS -o /tmp/root.html "$R/pypi-all/simple/"
grep -q '<a href="smoke-big/">smoke-big</a>' /tmp/root.html || fail "no hosted project in the group's list"
test "$(grep -c '<a href="six/">' /tmp/root.html)" = 1 || fail "a project of two members is listed twice"
echo "PYPI CLIENT OK"
EOF

docker network create "$net" >/dev/null
start_postgres
docker run -d --name "$name" --network "$net" -e COMMANDCENTER_DATABASE_URL="$pg_url" --network-alias commandcenter --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" commandcenter:smoke-pypi >/dev/null

# No port is published: the image's own health check asks the server.
ready=0
for i in $(seq 1 30); do
  if docker exec "$name" commandcenter healthcheck 2>/dev/null; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "FAIL: commandcenter did not become ready"; docker logs "$name"; exit 1
fi

docker run --rm --name "$name-client" --network "$net" --memory 2g -v "$root/$work:/work:ro" python:3.12-slim sh /work/run.sh

echo "== what the clients asked for: the core metadata files, not only the files"
docker logs "$name" > "$work/server.log" 2>&1
asked() {
  grep -q "\"method\":\"$1\",\"path\":\"/repository/$2\",\"status\":$3," "$work/server.log" ||
    { echo "FAIL: the client made no $1 $2 answered $3"; exit 1; }
}
asked GET 'pypi-all/packages/six/six-[0-9.]*-py2.py3-none-any.whl.metadata' 200 # pip and uv read PEP 658 metadata
asked POST 'pypi-hosted/pypi' 200                                               # pip search

if [ "$(docker inspect -f '{{.State.OOMKilled}} {{.RestartCount}}' "$name")" != "false 0" ]; then
  echo "FAIL: the server was killed for its memory"; exit 1
fi
echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "PYPI SMOKE OK"
