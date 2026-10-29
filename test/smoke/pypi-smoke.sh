#!/usr/bin/env bash
# Builds the image, starts commandcenter, then uses real pip and twine (python:3.12) to:
#   1. install build tools through the pypi-all group (files come from PyPI via pypi-proxy)
#   2. build smoke-hello (its build backend is also fetched through the proxy)
#   3. upload it to pypi-hosted with twine; a second upload must be rejected
#   4. install it into a fresh venv through the group; its dependency (six) comes via the proxy
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
  docker rm -f "$name" "$name-pg" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup
rm -rf "$work" && mkdir -p "$work"
cp -r test/smoke/pypi/pkg "$work/pkg"

docker build -q -t commandcenter:smoke-pypi . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i commandcenter:smoke-pypi hash-password)"

cat > "$work/config.yaml" <<EOF
anonymousRead: true
baseURL: "http://commandcenter:8081"
users:
  - { name: ci, passwordHash: "$hash", roles: [deployer] }
roles:
  deployer: { write: [pypi-hosted] }
repositories:
  - { name: pypi-proxy,  format: pypi, type: proxy,  remote: "https://pypi.org/" }
  - { name: pypi-hosted, format: pypi, type: hosted }
  - { name: pypi-all,    format: pypi, type: group,  members: [pypi-hosted, pypi-proxy], writeMember: pypi-hosted }
EOF

cat > "$work/run.sh" <<'EOF'
set -eu
export PIP_INDEX_URL=http://commandcenter:8081/repository/pypi-all/simple/
export PIP_TRUSTED_HOST=commandcenter PIP_DISABLE_PIP_VERSION_CHECK=1 PIP_NO_CACHE_DIR=1 PIP_ROOT_USER_ACTION=ignore
UPLOAD=http://commandcenter:8081/repository/pypi-hosted/

echo "== install build tools through the proxy"
python -m venv /tmp/tools && . /tmp/tools/bin/activate
pip install -q build twine

echo "== build"
cd /work/pkg && python -m build -q >/dev/null

echo "== upload"
twine upload --non-interactive --disable-progress-bar --repository-url "$UPLOAD" -u ci -p smoke-pass dist/*

echo "== re-upload must be rejected"
if twine upload --verbose --non-interactive --disable-progress-bar --repository-url "$UPLOAD" -u ci -p smoke-pass dist/* >/tmp/reupload.log 2>&1; then
  echo "FAIL: re-upload succeeded"; exit 1
fi
grep -qi "already exists" /tmp/reupload.log || { echo "FAIL: no already-exists message"; cat /tmp/reupload.log; exit 1; }

echo "== install through the group (dependency via the proxy)"
deactivate
python -m venv /tmp/app
/tmp/app/bin/pip install -q smoke-hello
/tmp/app/bin/python -c 'import smoke_hello; assert smoke_hello.greet() == "hello six"'
echo "PYPI CLIENT OK"
EOF

docker network create "$net" >/dev/null
start_postgres
docker run -d --name "$name" --network "$net" -e COMMANDCENTER_DATABASE_URL="$pg_url" --network-alias commandcenter --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" -p 18083:8081 commandcenter:smoke-pypi >/dev/null

ready=0
for i in $(seq 1 30); do
  if curl -fs http://localhost:18083/service/rest/v1/status >/dev/null; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "FAIL: commandcenter did not become ready"; docker logs "$name"; exit 1
fi

docker run --rm --network "$net" -v "$root/$work:/work" python:3.12-slim sh /work/run.sh

echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "PYPI SMOKE OK"
