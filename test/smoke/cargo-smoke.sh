#!/usr/bin/env bash
# Builds the image, starts commandcenter, then uses real cargo (rust:1-slim) to:
#   1. publish smoke-hello 1.0.0 to cargo-all (lands in cargo-hosted); a republish must fail
#   2. yank and un-yank it
#   3. build an app that needs smoke-hello (hosted) and itoa (crates.io via cargo-proxy),
#      with crates.io replaced by commandcenter
# Finally prints the container's memory usage.
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-cargo-$$"
net=commandcenter-cargo-smoke-$$
name=commandcenter-cargo-smoke-$$

cleanup() {
  docker rm -f "$name" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup
rm -rf "$work" && mkdir -p "$work"
cp -r test/smoke/cargo/hello test/smoke/cargo/app "$work/"

docker build -q -t commandcenter:smoke-cargo . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i commandcenter:smoke-cargo hash-password)"

cat > "$work/config.yaml" <<EOF
anonymousRead: true
baseURL: "http://commandcenter:8081"
users:
  - { name: ci, passwordHash: "$hash", roles: [deployer] }
roles:
  deployer: { write: [cargo-hosted] }
repositories:
  - { name: cargo-proxy,  format: cargo, type: proxy,  remote: "https://index.crates.io/" }
  - { name: cargo-hosted, format: cargo, type: hosted }
  - { name: cargo-all,    format: cargo, type: group,  members: [cargo-hosted, cargo-proxy], writeMember: cargo-hosted }
EOF

cat > "$work/run.sh" <<'EOF'
set -eu
export CARGO_HOME=/tmp/cargo-home
mkdir -p "$CARGO_HOME"
cat > "$CARGO_HOME/config.toml" <<'TOML'
[registries.commandcenter]
index = "sparse+http://commandcenter:8081/repository/cargo-all/"

[source.crates-io]
replace-with = "commandcenter"
TOML
export CARGO_REGISTRIES_COMMANDCENTER_TOKEN="$TOKEN"

echo "== publish through the group"
cd /work/hello
cargo publish --registry commandcenter --allow-dirty --no-verify

echo "== republish must be rejected"
if cargo publish --registry commandcenter --allow-dirty --no-verify >/tmp/republish.log 2>&1; then
  echo "FAIL: republish succeeded"; exit 1
fi
grep -Eq "already (exists|uploaded)" /tmp/republish.log || { cat /tmp/republish.log; exit 1; }

echo "== yank and un-yank"
cargo yank --registry commandcenter smoke-hello@1.0.0
cargo yank --registry commandcenter --undo smoke-hello@1.0.0

echo "== build an app through the group (itoa via the proxy)"
cd /work/app
out=$(cargo run -q)
echo "$out"
test "$out" = "hello 7"
echo "CARGO CLIENT OK"
EOF

docker network create "$net" >/dev/null
docker run -d --name "$name" --network "$net" --network-alias commandcenter --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" -p 18085:8081 commandcenter:smoke-cargo >/dev/null

ready=0
for i in $(seq 1 30); do
  if curl -fs http://localhost:18085/service/rest/v1/status >/dev/null; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "FAIL: commandcenter did not become ready"; docker logs "$name"; exit 1
fi

token="$(docker exec "$name" commandcenter token create -config /etc/commandcenter/config.yaml -user ci)"
docker run --rm --network "$net" -e TOKEN="$token" -v "$root/$work:/work" rust:1-slim sh /work/run.sh

echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "CARGO SMOKE OK"
