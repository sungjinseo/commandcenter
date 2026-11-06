#!/usr/bin/env bash
# Builds the image, starts commandcenter behind a TLS proxy (Caddy with its
# own CA: Terraform only talks to https registries), then:
#   1. publishes a module and a provider (two platforms, SHA256SUMS signed
#      with a key made here, manifest) to tf-hosted with curl and an API
#      token, the way docs/guide.md says
#   2. real terraform (hashicorp/terraform) installs the private module:
#      refused without a token, then `init` with the token in the CLI
#      configuration and ~/.netrc for the archive, and `apply`
#   3. after the group is opened for anonymous reads (terraform sends no
#      credentials when it fetches SHA256SUMS, its signature and the zip),
#      `init` with TF_TOKEN_<host> installs the hosted provider, a provider
#      and a module of registry.terraform.io through tf-proxy, and
#      `providers lock` records a second platform
# The proxy downloads about 11 MB (hashicorp/null for two platforms); the
# proxied module is a git:: source the client clones from GitHub itself.
# Finally prints the container's memory usage.
#   SMOKE_TF_IMAGE  the client image (default hashicorp/terraform:1.17); an
#                   OpenTofu image runs tofu against registry.opentofu.org
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-terraform-$$"
net=commandcenter-terraform-smoke-$$
name=commandcenter-terraform-smoke-$$
. test/postgres.sh
image="${SMOKE_TF_IMAGE:-hashicorp/terraform:1.17}"
remote=https://registry.terraform.io/
case "$image" in *opentofu*) remote=https://registry.opentofu.org/ ;; esac
host=registry.smoke.test

cleanup() {
  docker rm -f -v "$name" "$name-pg" "$name-tls" "$name-pub" "$name-c" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup
rm -rf "$work" && mkdir -p "$work/private" "$work/public"

docker build -q -t commandcenter:smoke-terraform . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i commandcenter:smoke-terraform hash-password)"
docker network create "$net" >/dev/null

# The publisher: curl, gpg, tar and python3 (zipfile). It makes the key
# that signs the provider's SHA256SUMS before the server starts.
docker run -d --name "$name-pub" --network "$net" --memory 512m -v "$root/$work:/work:ro" rockylinux/rockylinux:9 sleep 3600 >/dev/null
pubkey="$(docker exec "$name-pub" sh -c 'export GNUPGHOME=/root/gnupg && mkdir -m 700 $GNUPGHOME &&
  gpg --batch --quiet --passphrase "" --quick-gen-key "Smoke Provider <tf@example.com>" rsa3072 sign never && gpg --armor --export tf@example.com')"

# tf-all sorts first, so the host's /.well-known/terraform.json names it.
{
  cat <<EOF
anonymousRead: false
baseURL: "https://$host"
users:
  - { name: admin, passwordHash: "$hash", roles: [admin] }
  - { name: ci, passwordHash: "$hash", roles: [deployer] }
roles:
  deployer: { write: [tf-hosted] }
repositories:
  - { name: tf-proxy, format: terraform, type: proxy, remote: "$remote" }
  - { name: tf-all,   format: terraform, type: group, members: [tf-hosted, tf-proxy], writeMember: tf-hosted }
  - name: tf-hosted
    format: terraform
    type: hosted
    gpgPublicKey: |
EOF
  printf '%s\n' "$pubkey" | sed 's/^/      /'
} > "$work/config.yaml"

printf '%s {\n\ttls internal\n\treverse_proxy commandcenter:8081\n}\n' "$host" > "$work/Caddyfile"

cat > "$work/publish.sh" <<'EOF'
set -eu
H=https://registry.smoke.test/repository/tf-hosted
A="Authorization: Bearer $TOKEN"
export GNUPGHOME=/root/gnupg CURL_CA_BUNDLE=/work/ca.crt
fail() { echo "FAIL: $*"; exit 1; }
put() { curl -s -o /dev/null -w '%{http_code}' -X PUT -H "$A" --data-binary @"$1" "$2"; }
case "$(uname -m)" in aarch64) arch=arm64 ;; *) arch=amd64 ;; esac
mkdir -p /tmp/pub/vpc && cd /tmp/pub

echo "== publish the module acme/vpc/aws 1.2.0"
printf 'variable "name" {\n  type = string\n}\n\noutput "greeting" {\n  value = "hello ${var.name}"\n}\n' > vpc/main.tf
tar -czf vpc-1.2.0.tgz -C vpc .
test "$(put vpc-1.2.0.tgz $H/v1/modules/acme/vpc/aws/1.2.0)" = 201 || fail "module upload"
test "$(curl -s -o /dev/null -w '%{http_code}' -X PUT --data-binary @vpc-1.2.0.tgz $H/v1/modules/acme/vpc/aws/1.2.1)" = 401 || fail "upload without a token is not 401"

echo "== publish the provider acme/demo 2.0.0 (linux_$arch, darwin_arm64, signed SHA256SUMS, manifest)"
printf '#!/bin/sh\necho "not a real provider" >&2\nexit 1\n' > terraform-provider-demo_v2.0.0
chmod 755 terraform-provider-demo_v2.0.0
python3 -m zipfile -c "terraform-provider-demo_2.0.0_linux_$arch.zip" terraform-provider-demo_v2.0.0
cp "terraform-provider-demo_2.0.0_linux_$arch.zip" terraform-provider-demo_2.0.0_darwin_arm64.zip
sha256sum terraform-provider-demo_2.0.0_*.zip > terraform-provider-demo_2.0.0_SHA256SUMS
gpg --batch --quiet --detach-sign terraform-provider-demo_2.0.0_SHA256SUMS
printf '{"version": 1, "metadata": {"protocol_versions": ["6.0"]}}\n' > terraform-registry-manifest.json
B=$H/v1/providers/acme/demo/2.0.0
test "$(put "terraform-provider-demo_2.0.0_linux_$arch.zip" "$B/linux/$arch")" = 201 || fail "provider zip upload"
test "$(put terraform-provider-demo_2.0.0_darwin_arm64.zip $B/darwin/arm64)" = 201 || fail "second platform upload"
test "$(put terraform-provider-demo_2.0.0_SHA256SUMS $B/shasums)" = 201 || fail "SHA256SUMS upload"
test "$(put terraform-provider-demo_2.0.0_SHA256SUMS.sig $B/shasums.sig)" = 201 || fail "signature upload"
test "$(put terraform-registry-manifest.json $B/manifest)" = 201 || fail "manifest upload"
versions="$(curl -fsS -H "$A" https://registry.smoke.test/repository/tf-all/v1/providers/acme/demo/versions)"
echo "$versions" | grep -q '"protocols":\["6.0"\]' || fail "provider versions: $versions"

echo "== allow_once: the same file again is accepted, another content is 409"
test "$(put vpc-1.2.0.tgz $H/v1/modules/acme/vpc/aws/1.2.0)" = 201 || fail "the same module again"
printf 'output "other" {\n  value = 1\n}\n' > vpc/other.tf
tar -czf other.tgz -C vpc .
test "$(put other.tgz $H/v1/modules/acme/vpc/aws/1.2.0)" = 409 || fail "another archive for the version is not 409"
EOF

cat > "$work/private/main.tf" <<EOF
module "vpc" {
  source  = "$host/acme/vpc/aws"
  version = "1.2.0"
  name    = "smoke"
}

output "greeting" {
  value = module.vpc.greeting
}
EOF

cat > "$work/public/main.tf" <<EOF
terraform {
  required_providers {
    demo = {
      source  = "$host/acme/demo"
      version = "2.0.0"
    }
    null = {
      source  = "$host/hashicorp/null"
      version = "3.2.3"
    }
  }
}

module "vpc" {
  source  = "$host/acme/vpc/aws"
  version = "1.2.0"
  name    = "smoke"
}

module "dir" {
  source   = "$host/hashicorp/dir/template"
  version  = "1.0.2"
  base_dir = "."
}
EOF

cat > "$work/run.sh" <<'EOF'
set -eu
tf="$(command -v terraform || command -v tofu)"
export SSL_CERT_FILE=/work/ca.crt TF_IN_AUTOMATION=1
fail() { echo "FAIL: $*"; exit 1; }
case "$(uname -m)" in aarch64) arch=arm64 ;; *) arch=amd64 ;; esac
"$tf" version | head -1
cp -r "/work/$1" /tmp/project && cd /tmp/project

if [ "$1" = private ]; then
  echo "== private module: refused without a token"
  if "$tf" init -input=false -no-color >/tmp/anonymous.log 2>&1; then fail "init without a token succeeded"; fi
  grep -q '401' /tmp/anonymous.log || { cat /tmp/anonymous.log; fail "no 401 without a token"; }
  echo "== private module: token in the CLI configuration, ~/.netrc for the archive"
  export TF_CLI_CONFIG_FILE=/work/terraformrc
  cp /work/netrc "$HOME/.netrc" && chmod 600 "$HOME/.netrc"
  "$tf" init -input=false -no-color >/tmp/init.log 2>&1 || { cat /tmp/init.log; fail "init of the private module"; }
  "$tf" apply -input=false -no-color -auto-approve >/tmp/apply.log 2>&1 || { cat /tmp/apply.log; fail "apply"; }
  test "$("$tf" output -raw greeting)" = "hello smoke" || fail "the module's output"
else
  echo "== hosted and proxied providers and modules: TF_TOKEN_<host>, anonymous file downloads"
  "$tf" init -input=false -no-color >/tmp/init.log 2>&1 || { cat /tmp/init.log; fail "init"; }
  grep -E '^- Installed ' /tmp/init.log
  p=.terraform/providers/registry.smoke.test
  test -x "$p/acme/demo/2.0.0/linux_$arch/terraform-provider-demo_v2.0.0" || fail "the hosted provider is not installed"
  ls "$p/hashicorp/null/3.2.3/linux_$arch/" | grep -q '^terraform-provider-null' || fail "the proxied provider is not installed"
  test -f .terraform/modules/vpc/main.tf || fail "the hosted module is not installed"
  ls .terraform/modules/dir/*.tf >/dev/null 2>&1 || fail "the proxied module is not installed"
  echo "== providers lock for a second platform"
  "$tf" providers lock -no-color -platform="linux_$arch" -platform=darwin_arm64 >/tmp/lock.log 2>&1 || { cat /tmp/lock.log; fail "providers lock"; }
  test "$(grep -c 'zh:' .terraform.lock.hcl)" -ge 4 || { cat .terraform.lock.hcl; fail "the lock file's hashes"; }
fi
echo "TERRAFORM CLIENT OK ($1)"
EOF

start_postgres
docker run -d --name "$name" --network "$net" -e COMMANDCENTER_DATABASE_URL="$pg_url" --network-alias commandcenter --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" commandcenter:smoke-terraform >/dev/null
docker run -d --name "$name-tls" --network "$net" --network-alias "$host" --memory 128m \
  -v "$root/$work/Caddyfile:/etc/caddy/Caddyfile:ro" caddy:2-alpine >/dev/null

ready=0
for i in $(seq 1 30); do
  if docker exec "$name" commandcenter healthcheck >/dev/null 2>&1 &&
    docker cp "$name-tls:/data/caddy/pki/authorities/local/root.crt" "$work/ca.crt" >/dev/null 2>&1 &&
    docker exec "$name-pub" curl -fs --cacert /work/ca.crt -o /dev/null "https://$host/.well-known/terraform.json"; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "FAIL: commandcenter did not become ready behind the TLS proxy"; docker logs "$name"; docker logs "$name-tls"; exit 1
fi

token="$(docker exec "$name" commandcenter token create -config /etc/commandcenter/config.yaml -user ci)"
printf 'credentials "%s" {\n  token = "%s"\n}\n' "$host" "$token" > "$work/terraformrc"
printf 'machine %s login ci password %s\n' "$host" "$token" > "$work/netrc"

docker exec -e TOKEN="$token" "$name-pub" sh /work/publish.sh
# client <project> [docker run options]: terraform on a copy of the project
client() {
  project="$1"
  shift
  docker run --rm --name "$name-c" --network "$net" --memory 1g -v "$root/$work:/work:ro" "$@" --entrypoint sh "$image" /work/run.sh "$project"
}
client private

echo "== open tf-all for anonymous reads"
docker exec "$name-pub" curl -fsS -o /dev/null -u admin:smoke-pass -H 'X-Command-Center-UI: 1' -H 'Content-Type: application/json' -X PUT \
  http://commandcenter:8081/-/api/repositories/tf-all \
  -d '{"name":"tf-all","format":"terraform","type":"group","members":["tf-hosted","tf-proxy"],"writeMember":"tf-hosted","anonymousRead":true}'
client public -e "TF_TOKEN_$(echo "$host" | tr . _)=$token"

echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "TERRAFORM SMOKE OK"
