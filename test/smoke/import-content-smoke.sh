#!/usr/bin/env bash
# Builds the image and starts two Command Centers, a source and an empty target. Every hosted format
# import-nexus copies gets a package on the source, built with its real tools and dated years back
# (the source takes X-Command-Center-Publish-Time from its administrator). Then the image's own
# `commandcenter import-nexus -config`, in a container of its own:
#   1. plans with -dry-run, which must change nothing
#   2. makes the configuration on the target and copies the content; a read-only repository is
#      opened for its copy and closed again, and keeps refusing uploads
#   3. runs again and must find nothing to copy and nothing to open
# and each package is installed from the target, from an empty cache, by the format's real client:
# mvn, npm and yarn 4 (whose age gate refuses a version younger than a day, so the publish time
# must have come along), pip, helm (--verify), apt-get, dnf (repodata depth 3 and a package group),
# dotnet, gem, R, git-lfs, go, micromamba, skopeo and curl.
# No Nexus Repository is involved: the source's Nexus API stands in for one, and the docker image is
# read from a registry:2, as the connector of a Nexus docker repository would be.
# Publishing and installing fetch the clients' own tools from the internet: about 150 MiB, and the
# images of the other smokes when they are not there yet.
# Finally prints the containers' memory usage.
#   SMOKE_NAME  the prefix of the containers, network and volume
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-import-content-$$"
name="${SMOKE_NAME:-commandcenter-import-content-smoke-$$}"
net="$name"
. test/postgres.sh
A=http://cc-a:8081
B=http://cc-b:8081
v=/service/rest/v1
published=2020-01-02T03:04:05Z
published_unix=1577934245
berry=4.18.1

cleanup() {
  docker rm -f -v "$name-a" "$name-b" "$name-run" "$name-pg" "$name-reg" "$name-curl" "$name-c" "$name-hash" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  docker volume rm "$name-share" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup

fail() { echo "FAIL: $*" >&2; exit 1; }

# Built before the work directory exists: a new directory in the build
# context would compile the binary again on every run.
docker build -q -t commandcenter:smoke-import-content . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i --name "$name-hash" --memory 256m commandcenter:smoke-import-content hash-password)"
mkdir -p "$work/out"
chmod 777 "$work/out"
cp test/smoke/go/mkzip.go "$work/"

cat > "$work/config-a.yaml" <<EOF
baseURL: "$A"
users:
  - { name: admin, passwordHash: "$hash", roles: [admin] }
repositories:
  - { name: maven-hosted,  format: maven,    type: hosted, versionPolicy: release }
  - { name: npm-hosted,    format: npm,      type: hosted }
  - { name: npm-all,       format: npm,      type: group,  members: [npm-hosted] }
  - { name: pypi-hosted,   format: pypi,     type: hosted }
  - { name: helm-hosted,   format: helm,     type: hosted }
  - { name: apt-hosted,    format: apt,      type: hosted, distribution: stable }
  - { name: yum-hosted,    format: yum,      type: hosted, depth: 3 }
  - { name: nuget-hosted,  format: nuget,    type: hosted }
  - { name: gems-hosted,   format: rubygems, type: hosted }
  - { name: r-hosted,      format: r,        type: hosted }
  - { name: lfs-hosted,    format: gitlfs,   type: hosted }
  - { name: go-hosted,     format: go,       type: hosted }
  - { name: conda-hosted,  format: conda,    type: hosted }
  - { name: raw-hosted,    format: raw,      type: hosted }
  - { name: raw-frozen,    format: raw,      type: hosted }
  - { name: docker-hosted, format: docker,   type: hosted, anonymousRead: false }
  - { name: docker-open,   format: docker,   type: hosted }
EOF
# Anonymous read on the target: the clients install without credentials.
cat > "$work/config-b.yaml" <<EOF
baseURL: "$B"
anonymousRead: true
users:
  - { name: admin, passwordHash: "$hash", roles: [admin] }
EOF

docker network create "$net" >/dev/null
docker volume create "$name-share" >/dev/null
start_postgres
docker update --memory 512m --memory-swap 512m "$name-pg" >/dev/null
docker exec "$name-pg" psql -q -U postgres -c 'CREATE DATABASE ccb' >/dev/null
docker run -d --name "$name-a" --network "$net" --network-alias cc-a -e COMMANDCENTER_DATABASE_URL="$pg_url" --memory 128m \
  -v "$root/$work/config-a.yaml:/etc/commandcenter/config.yaml:ro" commandcenter:smoke-import-content >/dev/null
docker run -d --name "$name-b" --network "$net" --network-alias cc-b -e COMMANDCENTER_DATABASE_URL="${pg_url/\/cc\?/\/ccb\?}" --memory 128m \
  -v "$root/$work/config-b.yaml:/etc/commandcenter/config.yaml:ro" commandcenter:smoke-import-content >/dev/null
docker run -d --name "$name-reg" --network "$net" --network-alias registry --memory 128m registry:2 >/dev/null
docker run -d --name "$name-curl" --network "$net" --memory 128m --user 0 -v "$name-share:/share" --entrypoint sleep curlimages/curl:8.23.0 infinity >/dev/null
for s in a b; do
  ready=0
  for i in $(seq 1 30); do
    if docker exec "$name-$s" commandcenter healthcheck >/dev/null 2>&1; then ready=1; break; fi
    sleep 1
  done
  [ "$ready" = 1 ] || { docker logs "$name-$s" >&2 || true; fail "cc-$s did not become ready"; }
done

# curl_ <arguments>: curl on the network, its output on stdout.
curl_() { docker exec "$name-curl" curl -sS "$@"; }
# code <arguments>: the status of the request; its body is in /tmp/body of the curl container.
code() { docker exec "$name-curl" curl -sS -o /tmp/body -w '%{http_code}' "$@"; }
# up <method> <file under /share> <URL>: an administrator's upload to the source, dated years back.
up() {
  local got
  got="$(code -u admin:smoke-pass -H "X-Command-Center-Publish-Time: $published" -X "$1" -T "/share/$2" "$3")"
  case "$got" in 2*) ;; *) docker exec "$name-curl" cat /tmp/body >&2 || true; fail "$1 $3: HTTP $got" ;; esac
}
# client <image> <script under the work directory> [docker run arguments...]: runs a script in a new container.
client() {
  local image=$1 script=$2
  shift 2
  docker rm -f "$name-c" >/dev/null 2>&1 || true
  docker run --rm --name "$name-c" --network "$net" --memory 2g --user 0 -e A="$A" -e B="$B" -v "$name-share:/share" -v "$root/$work:/work:ro" "$@" \
    --entrypoint sh "$image" "/work/$script"
}
# holds <URL> <text>: the answer holds the text.
holds() { local b; b="$(curl_ -u admin:smoke-pass "$1")"; case "$b" in *"$2"*) ;; *) fail "$1 lacks $2: ${b:0:600}" ;; esac; }
# asset_time <repository> <path>: the file's times on the target are the stated one.
asset_time() {
  local b
  b="$(curl_ -u admin:smoke-pass "$B$v/assets?repository=$1")"
  case "$b" in *"\"blobCreated\":\"2020-01-02T03:04:05.000+00:00\""*"\"path\":\"/$2\""*|*"\"path\":\"/$2\""*"\"blobCreated\":\"2020-01-02T03:04:05.000+00:00\""*) ;;
    *) fail "$1 $2: the time did not come along: ${b:0:600}" ;; esac
}

echo "== build the packages with the formats' tools and put them on the source"
cat > "$work/build-maven.sh" <<'EOF'
set -eu
mkdir -p /tmp/j /share/maven && cd /tmp/j
echo hello > hello.txt
jar cf /share/maven/hello-1.0.jar hello.txt
printf '<project><modelVersion>4.0.0</modelVersion><groupId>org.example</groupId><artifactId>hello</artifactId><version>1.0</version></project>' > /share/maven/hello-1.0.pom
EOF
client maven:3.9-eclipse-temurin-21 build-maven.sh
up PUT maven/hello-1.0.jar "$A/repository/maven-hosted/org/example/hello/1.0/hello-1.0.jar"
up PUT maven/hello-1.0.pom "$A/repository/maven-hosted/org/example/hello/1.0/hello-1.0.pom"

# npm: documents as npm publish sends them, which it cannot date: a scoped package with two
# versions (the first deprecated), and one without a scope.
cat > "$work/npmdoc.js" <<'EOF'
const fs = require('fs'), crypto = require('crypto'), { execSync } = require('child_process');
function doc(name, version, extra) {
  const dir = fs.mkdtempSync('/tmp/p');
  fs.writeFileSync(dir + '/package.json', JSON.stringify({ name, version, description: 'the first words', main: 'index.js' }));
  fs.writeFileSync(dir + '/index.js', `module.exports = () => ${JSON.stringify(name + ' ' + version)};\n`);
  const tgz = execSync('npm pack --silent', { cwd: dir }).toString().trim();
  const data = fs.readFileSync(dir + '/' + tgz);
  const short = name.replace(/^@[^/]*\//, '');
  const out = { _id: name, name, 'dist-tags': { latest: version }, versions: { [version]: Object.assign({ name, version, description: 'as published', main: 'index.js',
      _npmUser: { name: 'alice', email: 'alice@example.org' }, gitHead: 'abc123', dist: { shasum: crypto.createHash('sha1').update(data).digest('hex') } }, extra) },
    _attachments: { [`${short}-${version}.tgz`]: { content_type: 'application/octet-stream', length: data.length, data: data.toString('base64') } } };
  fs.mkdirSync('/share/npm', { recursive: true });
  fs.writeFileSync(`/share/npm/${short}-${version}.json`, JSON.stringify(out));
}
doc('@smoke/hello', '1.0.0', { deprecated: 'use 1.1.0' });
doc('@smoke/hello', '1.1.0', {});
doc('@smoke/hello', '1.2.0', {});
doc('smoke-plain', '2.0.0', {});
EOF
cat > "$work/build-npm.sh" <<'EOF'
set -eu
node /work/npmdoc.js
EOF
client node:22 build-npm.sh
for f in hello-1.0.0:@smoke%2fhello hello-1.1.0:@smoke%2fhello smoke-plain-2.0.0:smoke-plain; do
  got="$(code -u admin:smoke-pass -H "X-Command-Center-Publish-Time: $published" -H 'Content-Type: application/json' -X PUT --data-binary "@/share/npm/${f%%:*}.json" "$A/repository/npm-hosted/${f##*:}")"
  case "$got" in 2*) ;; *) docker exec "$name-curl" cat /tmp/body >&2; fail "publish $f: HTTP $got" ;; esac
done
got="$(code -u admin:smoke-pass -H 'Content-Type: application/json' -X PUT --data '"1.0.0"' "$A/repository/npm-hosted/-/package/@smoke%2fhello/dist-tags/beta")"
case "$got" in 2*) ;; *) fail "dist-tag: HTTP $got" ;; esac

cat > "$work/build-pypi.sh" <<'EOF'
set -eu
export PIP_DISABLE_PIP_VERSION_CHECK=1 PIP_ROOT_USER_ACTION=ignore
pip install -q build
mkdir -p /tmp/p/smoke_lib /share/pypi && cd /tmp/p
printf '[build-system]\nrequires = ["setuptools>=68"]\nbuild-backend = "setuptools.build_meta"\n[project]\nname = "smoke-lib"\nversion = "1.0.0"\n[tool.setuptools]\npackages = ["smoke_lib"]\n' > pyproject.toml
echo 'def hi(): return "hi from smoke-lib"' > smoke_lib/__init__.py
python -m build -q --outdir /share/pypi >/dev/null
# What twine --sign sent: an armored signature beside the file. Nothing here checks it; it must come along.
printf -- '-----BEGIN PGP SIGNATURE-----\n\nsmoke\n-----END PGP SIGNATURE-----\n' > /share/pypi/smoke_lib-1.0.0.tar.gz.asc
EOF
client python:3.12-slim build-pypi.sh
got="$(code -u admin:smoke-pass -H "X-Command-Center-Publish-Time: $published" -F :action=file_upload -F name=smoke-lib -F version=1.0.0 -F content=@/share/pypi/smoke_lib-1.0.0.tar.gz -F gpg_signature=@/share/pypi/smoke_lib-1.0.0.tar.gz.asc "$A/repository/pypi-hosted/")"
case "$got" in 2*) ;; *) docker exec "$name-curl" cat /tmp/body >&2; fail "PyPI sdist: HTTP $got" ;; esac
got="$(code -u admin:smoke-pass -H "X-Command-Center-Publish-Time: $published" -F :action=file_upload -F name=smoke-lib -F version=1.0.0 -F content=@/share/pypi/smoke_lib-1.0.0-py3-none-any.whl "$A/repository/pypi-hosted/")"
case "$got" in 2*) ;; *) docker exec "$name-curl" cat /tmp/body >&2; fail "PyPI wheel: HTTP $got" ;; esac

cat > "$work/build-helm.sh" <<'EOF'
set -eu
apk add --no-cache gnupg >/dev/null 2>&1
export GNUPGHOME=/tmp/gnupg; mkdir -m 700 $GNUPGHOME
gpg --batch --pinentry-mode loopback --passphrase '' --quick-generate-key 'Smoke Signer <smoke@example.com>' rsa2048 sign never 2>/dev/null
gpg --batch --pinentry-mode loopback --passphrase '' --export-secret-keys > /tmp/secring.gpg 2>/dev/null
mkdir -p /share/helm && gpg --export > /share/helm/pubring.gpg
cd /tmp && helm create demo >/dev/null
sed -i 's/^version: .*/version: "1.0.0"/' demo/Chart.yaml
helm package demo --sign --key 'Smoke Signer' --keyring /tmp/secring.gpg -d /share/helm >/dev/null 2>&1
EOF
client alpine/helm:3.22.1 build-helm.sh
up PUT helm/demo-1.0.0.tgz "$A/repository/helm-hosted/demo-1.0.0.tgz"
up PUT helm/demo-1.0.0.tgz.prov "$A/repository/helm-hosted/demo-1.0.0.tgz.prov"

cat > "$work/build-apt.sh" <<'EOF'
set -eu
d=/tmp/deb/smoke-hello_1.0_all; mkdir -p "$d/DEBIAN" "$d/usr/bin" /share/apt
printf '#!/bin/sh\necho "smoke-hello 1.0"\n' > "$d/usr/bin/smoke-hello"; chmod 755 "$d/usr/bin/smoke-hello"
printf 'Package: smoke-hello\nVersion: 1.0\nArchitecture: all\nMaintainer: Smoke <smoke@example.com>\nDescription: smoke\n A smoke package.\n' > "$d/DEBIAN/control"
dpkg-deb --root-owner-group --build "$d" /share/apt >/dev/null
EOF
client debian:trixie-slim build-apt.sh
up PUT apt/smoke-hello_1.0_all.deb "$A/repository/apt-hosted/pool/smoke-hello_1.0_all.deb"

cat > "$work/smoke-demo.spec" <<'EOF'
Name:       smoke-demo
Version:    1.0
Release:    1
Summary:    Command Center import smoke package
License:    MIT
BuildArch:  noarch
%description
A tiny package.
%install
mkdir -p %{buildroot}%{_bindir}
printf '#!/bin/sh\necho "smoke-demo 1.0"\n' > %{buildroot}%{_bindir}/smoke-demo
chmod 755 %{buildroot}%{_bindir}/smoke-demo
%files
%{_bindir}/smoke-demo
EOF
cat > "$work/build-yum.sh" <<'EOF'
set -eu
dnf -y -q install rpm-build >/tmp/tools.log 2>&1 || { tail -30 /tmp/tools.log; exit 1; }
rpmbuild -bb --quiet --define "_topdir /tmp/rpmbuild" /work/smoke-demo.spec
mkdir -p /share/yum && cp /tmp/rpmbuild/RPMS/noarch/smoke-demo-1.0-1.noarch.rpm /share/yum/
cat > /share/yum/comps.xml <<'X'
<?xml version="1.0" encoding="UTF-8"?>
<comps><group><id>smoke-group</id><name>Smoke group</name><description>smoke</description><default>true</default><uservisible>true</uservisible>
<packagelist><packagereq type="mandatory">smoke-demo</packagereq></packagelist></group></comps>
X
EOF
client rockylinux/rockylinux:9 build-yum.sh
up PUT yum/smoke-demo-1.0-1.noarch.rpm "$A/repository/yum-hosted/7/os/x86_64/smoke-demo-1.0-1.noarch.rpm"
up PUT yum/comps.xml "$A/repository/yum-hosted/7/os/x86_64/repodata/comps.xml"

cat > "$work/build-nuget.sh" <<'EOF'
set -eu
export DOTNET_CLI_TELEMETRY_OPTOUT=1 DOTNET_NOLOGO=1
mkdir -p /tmp/lib /share/nuget && cd /tmp/lib
cat > Smoke.Lib.csproj <<'X'
<Project Sdk="Microsoft.NET.Sdk">
  <PropertyGroup><TargetFramework>net10.0</TargetFramework><PackageId>Smoke.Lib</PackageId><Version>1.0.0</Version><Authors>smoke</Authors><Description>smoke</Description></PropertyGroup>
</Project>
X
printf 'namespace Smoke; public static class Greeter { public static string Hi() => "hi from Smoke.Lib"; }\n' > Greeter.cs
dotnet pack -c Release -o /share/nuget -v q >/dev/null
EOF
client mcr.microsoft.com/dotnet/sdk:10.0 build-nuget.sh
up PUT nuget/Smoke.Lib.1.0.0.nupkg "$A/repository/nuget-hosted/api/v2/package"

cat > "$work/build-gem.sh" <<'EOF'
set -eu
mkdir -p /tmp/g/lib /share/gem && cd /tmp/g
echo 'module SmokeLib; def self.hi = "hi from smoke-lib"; end' > lib/smoke_lib.rb
cat > smoke-lib.gemspec <<'X'
Gem::Specification.new do |s|
  s.name = "smoke-lib"
  s.version = "1.0.0"
  s.summary = "smoke"
  s.authors = ["smoke"]
  s.license = "MIT"
  s.files = ["lib/smoke_lib.rb"]
end
X
gem build smoke-lib.gemspec >/dev/null 2>&1
cp smoke-lib-1.0.0.gem /share/gem/
EOF
client ruby:4.0-slim build-gem.sh
up POST gem/smoke-lib-1.0.0.gem "$A/repository/gems-hosted/api/v1/gems"

cat > "$work/build-r.sh" <<'EOF'
set -eu
d=/tmp/src/smokelib; mkdir -p "$d/R" /share/r
printf 'Package: smokelib\nVersion: 1.0\nTitle: Smoke Package\nDescription: A package for the smoke test.\nAuthor: Smoke\nMaintainer: Smoke <smoke@example.com>\nLicense: MIT + file LICENSE\nEncoding: UTF-8\n' > "$d/DESCRIPTION"
printf 'YEAR: 2026\nCOPYRIGHT HOLDER: Smoke\n' > "$d/LICENSE"
printf 'export(hi)\n' > "$d/NAMESPACE"
printf 'hi <- function() "hi from smokelib"\n' > "$d/R/hi.R"
cd /share/r && R CMD build --no-manual --no-build-vignettes "$d" >/dev/null
EOF
client r-base:4.6.2 build-r.sh
up PUT r/smokelib_1.0.tar.gz "$A/repository/r-hosted/src/contrib/smokelib_1.0.tar.gz"

cat > "$work/git-common.sh" <<'EOF'
export GIT_TERMINAL_PROMPT=0 HOME=/tmp
git config --global user.name smoke
git config --global user.email smoke@example.com
git config --global init.defaultBranch main
git config --global credential.helper store
git config --global --add safe.directory '*'
git lfs install >/dev/null
EOF
cat > "$work/build-lfs.sh" <<'EOF'
set -eu
. /work/git-common.sh
printf 'protocol=http\nhost=cc-a:8081\nusername=admin\npassword=smoke-pass\n\n' | git credential approve
git init -q --bare /share/git/project.git
git clone -q /share/git/project.git /tmp/a 2>/dev/null
cd /tmp/a
git config -f .lfsconfig lfs.url "$A/repository/lfs-hosted/info/lfs"
git lfs track '*.bin' >/dev/null
head -c 1048576 /dev/urandom > model.bin
sha256sum model.bin > /share/git/expected.sha256
git add .lfsconfig .gitattributes model.bin
git commit -qm 'an LFS object'
git push -q origin main 2>/tmp/push.log || { cat /tmp/push.log; exit 1; }
EOF
client alpine/git:v2.54.0 build-lfs.sh

cat > "$work/build-go.sh" <<'EOF'
set -eu
export GOTOOLCHAIN=local GOCACHE=/tmp/gocache
mkdir -p /tmp/hello /share/go
printf 'module example.com/hello\n\ngo 1.21\n' > /tmp/hello/go.mod
printf 'package hello\n\nfunc Greet() string { return "hello from the target" }\n' > /tmp/hello/hello.go
go run /work/mkzip.go /tmp/hello example.com/hello@v1.0.0 /share/go/v1.0.0.zip
EOF
client golang:1.27 build-go.sh
up PUT go/v1.0.0.zip "$A/repository/go-hosted/example.com/hello/@v/v1.0.0.zip"

cat > "$work/build-conda.sh" <<'EOF'
set -eu
d=/tmp/pkg/ccsmoke-1.0-0; mkdir -p "$d/info" "$d/share/ccsmoke" /share/conda
echo "hello from ccsmoke 1.0" > "$d/share/ccsmoke/hello.txt"
printf 'share/ccsmoke/hello.txt\n' > "$d/info/files"
size=$(wc -c < "$d/share/ccsmoke/hello.txt"); sum=$(sha256sum "$d/share/ccsmoke/hello.txt" | cut -d' ' -f1)
printf '{"paths_version":1,"paths":[{"_path":"share/ccsmoke/hello.txt","path_type":"hardlink","sha256":"%s","size_in_bytes":%s}]}\n' "$sum" "$size" > "$d/info/paths.json"
printf '{"name":"ccsmoke","version":"1.0","build":"0","build_number":0,"subdir":"noarch","noarch":"generic","depends":[],"license":"MIT","timestamp":1760000000000}\n' > "$d/info/index.json"
printf '{"summary":"smoke","license":"MIT"}\n' > "$d/info/about.json"
cd /share/conda && micromamba package compress "$d" ccsmoke-1.0-0.conda >/dev/null
EOF
client mambaorg/micromamba:2.10.0-ubuntu24.04 build-conda.sh -e MAMBA_ROOT_PREFIX=/tmp/mm
up PUT conda/ccsmoke-1.0-0.conda "$A/repository/conda-hosted/noarch/ccsmoke-1.0-0.conda"

docker exec "$name-curl" sh -c 'echo "hello raw" > /share/readme.txt; echo "frozen in time" > /share/frozen.txt'
up PUT readme.txt "$A/repository/raw-hosted/docs/readme.txt"
up PUT frozen.txt "$A/repository/raw-frozen/docs/frozen.txt"
# Read-only on the source from here on, as it would be on Nexus.
got="$(code -u admin:smoke-pass -H 'X-Command-Center-UI: 1' -H 'Content-Type: application/json' -X PUT "$A$v/repositories/raw/hosted/raw-frozen" \
  -d '{"name":"raw-frozen","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":false,"writePolicy":"DENY"},"raw":{"contentDisposition":"INLINE"}}')"
[ "$got" = 204 ] || { docker exec "$name-curl" cat /tmp/body >&2; fail "raw-frozen: HTTP $got"; }

# Docker: the image goes to a stand-in for the connector, which the import reads, and to the
# source, which lists it. Pushed as the formats' clients do.
docker image inspect alpine:3.20 >/dev/null 2>&1 || docker pull -q alpine:3.20 >/dev/null
docker save alpine:3.20 | docker exec -i "$name-curl" sh -c 'cat > /share/alpine.tar'
skopeo() { docker run --rm --name "$name-c" --network "$net" --memory 512m -v "$name-share:/share" quay.io/skopeo/stable:v1.22.3 "$@"; }
skopeo copy -q --dest-tls-verify=false docker-archive:/share/alpine.tar docker://registry:5000/app:v1
skopeo copy -q --dest-tls-verify=false --dest-creds admin:smoke-pass docker-archive:/share/alpine.tar docker://cc-a:8081/docker-hosted/app:v1
digest="$(skopeo inspect --tls-verify=false --format '{{.Digest}}' docker://registry:5000/app:v1)"

# import <args>: the image's import-nexus from the source to the target; its output goes to $work/import.log.
import() {
  docker run --rm --name "$name-run" --network "$net" --memory 128m --user "$(id -u):$(id -g)" \
    -e NEXUS_PASSWORD=smoke-pass -e COMMANDCENTER_TOKEN=smoke-pass -v "$root/$work/out:/out" commandcenter:smoke-import-content \
    import-nexus -from "$A" -from-user admin -to "$B" -to-user admin -docker-from http://registry:5000 "$@" > "$work/import.log" 2>&1 ||
    { cat "$work/import.log" >&2; fail "import-nexus $* failed"; }
}
# has <text>: the last import's output holds the line part.
has() { grep -qF -- "$1" "$work/import.log" || { cat "$work/import.log" >&2; fail "import-nexus output lacks: $1"; }; }
hasnt() { if grep -qF -- "$1" "$work/import.log"; then cat "$work/import.log" >&2; fail "import-nexus output has: $1"; fi; }

echo "== dry run: the plan, and nothing made"
import -config -dry-run
has "$(printf 'config\trepository\traw-frozen\twould create')"
has "$(printf 'config\trepository\traw-frozen\twould stay read-only but for its copy')"
has 'configuration (dry run, nothing was written): '
has 'dry run: nothing was written'
has 'docker: anonymous pull would be off on 1 repositories'
[ "$(curl_ -u admin:smoke-pass "$B$v/repositorySettings")" = "[]" ] || fail "the dry run made repositories"

echo "== import the configuration and the content"
import -config -passwords-out /out/passwords.txt
for r in maven-hosted:maven npm-hosted:npm pypi-hosted:pypi helm-hosted:helm apt-hosted:apt yum-hosted:yum nuget-hosted:nuget gems-hosted:rubygems r-hosted:r \
  lfs-hosted:gitlfs go-hosted:go conda-hosted:conda raw-hosted:raw raw-frozen:raw docker-hosted:docker; do
  has "  ${r%%:*} -> ${r%%:*} (${r##*:}): "
done
hasnt 'error:'
has ' 0 failed, '
has 'publish times: kept'
has 'docker: anonymous pull is off on 1 repositories'
has 'needs manual action: repository npm-all: Nexus merges this group'
has "$(printf 'config\trepository\traw-frozen\topening for its copy')"
has "$(printf 'config\trepository\traw-frozen\tclosed: read-only, as Nexus has it')"
tail -n 1 "$work/import.log" | grep -q '^configuration: .* 0 failed, ' || { cat "$work/import.log" >&2; fail "the last line does not summarize"; }
holds "$B$v/repositories/raw/hosted/raw-frozen" '"writePolicy":"DENY"'
got="$(code -u admin:smoke-pass -T /share/readme.txt "$B/repository/raw-frozen/docs/new.txt")"
[ "$got" = 400 ] || fail "raw-frozen took an upload after its copy: HTTP $got"
got="$(code "$B/repository/raw-frozen/docs/frozen.txt")"
[ "$got" = 200 ] || fail "raw-frozen: its file: HTTP $got"
got="$(code "$B$v/tags/import-nexus-open.raw-frozen" -u admin:smoke-pass)"
[ "$got" = 404 ] || fail "the marker stayed: HTTP $got"

echo "== a second run has nothing to copy and opens nothing"
import -config
has ': 0 created, 0 updated, '
hasnt 'error:'
hasnt 'opening for its copy'
hasnt "$(printf '\tok\n')"
has '  raw-frozen -> raw-frozen (raw): 0 copied, 1 skipped, 0 failed'
has '  npm-hosted -> npm-hosted (npm): 0 copied, 3 skipped, 0 failed'
has '  nuget-hosted -> nuget-hosted (nuget): 0 copied, 1 skipped, 0 failed'
has '  go-hosted -> go-hosted (go): 0 copied, 1 skipped, 0 failed'

echo "== the publish times came along"
holds "$B/repository/helm-hosted/index.yaml" 'created: "2020-01-02T03:04:05Z"'
holds "$B/repository/pypi-hosted/pypi/smoke-lib/json" '"upload_time":"2020-01-02T03:04:05"'
holds "$B/repository/nuget-hosted/registration/smoke.lib/1.0.0.json" '"published":"2020-01-02T03:04:05Z"'
holds "$B/repository/go-hosted/example.com/hello/@v/v1.0.0.info" '"Time":"2020-01-02T03:04:05Z"'
asset_time raw-hosted docs/readme.txt
asset_time maven-hosted org/example/hello/1.0/hello-1.0.jar
asset_time apt-hosted pool/main/s/smoke-hello/smoke-hello_1.0_all.deb
asset_time r-hosted src/contrib/smokelib_1.0.tar.gz
asset_time gems-hosted gems/smoke-lib-1.0.0.gem
docker exec "$name-curl" sh -c "curl -sS '$B/repository/yum-hosted/7/os/x86_64/repodata/repomd.xml' | sed -n 's/.*href=\"\(repodata\/[^\"]*primary.xml.gz\)\".*/\1/p' > /tmp/primary"
docker exec "$name-curl" sh -c "curl -sS '$B/repository/yum-hosted/7/os/x86_64/'\$(cat /tmp/primary) | gunzip | grep -q 'time file=\"$published_unix\"'" || fail "yum: <time file> did not come along"

echo "== install from the target, from empty caches, with the real clients"
echo "-- curl"
[ "$(curl_ "$B/repository/raw-hosted/docs/readme.txt")" = "hello raw" ] || fail "raw"

echo "-- mvn"
cat > "$work/settings.xml" <<EOF
<settings><mirrors><mirror><id>target</id><mirrorOf>*,!central</mirrorOf><url>$B/repository/maven-hosted/</url></mirror></mirrors></settings>
EOF
cat > "$work/use-maven.sh" <<'EOF'
set -eu
mvn -v | head -1
mvn -B -q -s /work/settings.xml -Dmaven.repo.local=/tmp/m2 dependency:get -Dartifact=org.example:hello:1.0 -DremoteRepositories=target::default::$B/repository/maven-hosted/
cmp /tmp/m2/org/example/hello/1.0/hello-1.0.jar /share/maven/hello-1.0.jar
EOF
client maven:3.9-eclipse-temurin-21 use-maven.sh

echo "-- npm, yarn 4"
cat > "$work/use-npm.sh" <<EOF
berry=$berry
EOF
cat >> "$work/use-npm.sh" <<'EOF'
set -eu
REG=$B/repository/npm-hosted/
export npm_config_update_notifier=false npm_config_fund=false npm_config_audit=false COREPACK_ENABLE_DOWNLOAD_PROMPT=0 COREPACK_HOME=/tmp/corepack
echo "npm $(npm --version), yarn $berry"
mkdir -p /tmp/n && cd /tmp/n && npm init -y >/dev/null
npm install --registry "$REG" @smoke/hello 2>/dev/null >/dev/null
test "$(node -p 'require("@smoke/hello")()')" = "@smoke/hello 1.1.0"
npm view --registry "$REG" @smoke/hello@1.0.0 deprecated | grep -q 'use 1.1.0'
npm view --registry "$REG" @smoke/hello@1.1.0 description | grep -q 'as published'
npm view --registry "$REG" @smoke/hello@1.1.0 _npmUser.name | grep -q alice
npm view --registry "$REG" @smoke/hello dist-tags.beta | grep -q 1.0.0
npm view --registry "$REG" @smoke/hello time --json | grep -q '"1.1.0": "2020-01-02T03:04:05.000Z"'
npm install --registry "$REG" smoke-plain >/dev/null 2>&1
mkdir -p /tmp/y && cd /tmp/y
cat > .yarnrc.yml <<Y
npmRegistryServer: "$REG"
unsafeHttpWhitelist: ["cc-b"]
nodeLinker: node-modules
enableTelemetry: false
Y
printf '{"name":"y","packageManager":"yarn@%s"}\n' "$berry" > package.json
touch yarn.lock
corepack yarn@$berry add @smoke/hello >/tmp/yarn.log 2>&1 || { cat /tmp/yarn.log; echo "FAIL: yarn 4 refused an imported version"; exit 1; }
test "$(node -p 'require("@smoke/hello")()')" = "@smoke/hello 1.1.0"
EOF
client node:22 use-npm.sh
# The control: the same client, its age gate on, and a version published just now.
got="$(code -u admin:smoke-pass -H 'Content-Type: application/json' -X PUT --data-binary @/share/npm/hello-1.2.0.json "$B/repository/npm-hosted/@smoke%2fhello")"
case "$got" in 2*) ;; *) docker exec "$name-curl" cat /tmp/body >&2; fail "publish 1.2.0: HTTP $got" ;; esac
cat > "$work/use-yarn-gate.sh" <<EOF
berry=$berry
EOF
cat >> "$work/use-yarn-gate.sh" <<'EOF'
set -u
REG=$B/repository/npm-hosted/
export COREPACK_ENABLE_DOWNLOAD_PROMPT=0 COREPACK_HOME=/tmp/corepack
mkdir -p /tmp/y && cd /tmp/y
cat > .yarnrc.yml <<Y
npmRegistryServer: "$REG"
unsafeHttpWhitelist: ["cc-b"]
nodeLinker: node-modules
enableTelemetry: false
Y
printf '{"name":"y","packageManager":"yarn@%s"}\n' "$berry" > package.json
touch yarn.lock
if corepack yarn@$berry add @smoke/hello@1.2.0 >/tmp/yarn.log 2>&1; then echo "FAIL: yarn 4 took a version published just now: its gate is off"; exit 1; fi
grep -q quarantined /tmp/yarn.log || { cat /tmp/yarn.log; echo "FAIL: not refused by the age gate"; exit 1; }
EOF
client node:22 use-yarn-gate.sh

echo "-- pip"
cat > "$work/use-pypi.sh" <<'EOF'
set -eu
export PIP_DISABLE_PIP_VERSION_CHECK=1 PIP_ROOT_USER_ACTION=ignore PIP_NO_CACHE_DIR=1
pip --version
pip install -q --index-url "$B/repository/pypi-hosted/simple/" --trusted-host cc-b smoke-lib
test "$(python -c 'import smoke_lib; print(smoke_lib.hi())')" = "hi from smoke-lib"
EOF
client python:3.12-slim use-pypi.sh
[ "$(curl_ "$B/repository/pypi-hosted/packages/smoke-lib/smoke_lib-1.0.0.tar.gz.asc" | head -1)" = "-----BEGIN PGP SIGNATURE-----" ] || fail "PyPI: the signature did not come along"

echo "-- helm"
cat > "$work/use-helm.sh" <<'EOF'
set -eu
helm version --short
helm repo add cc "$B/repository/helm-hosted" >/dev/null
helm pull cc/demo --version 1.0.0 -d /tmp --verify --keyring /share/helm/pubring.gpg >/tmp/pull.log 2>&1 || { cat /tmp/pull.log; exit 1; }
grep -q 'Signed by: Smoke Signer' /tmp/pull.log
cmp /tmp/demo-1.0.0.tgz /share/helm/demo-1.0.0.tgz
EOF
client alpine/helm:3.22.1 use-helm.sh

echo "-- apt-get"
cat > "$work/use-apt.sh" <<'EOF'
set -eu
export DEBIAN_FRONTEND=noninteractive
apt-get --version | head -1
rm -f /etc/apt/sources.list /etc/apt/sources.list.d/*
echo "deb [trusted=yes] $B/repository/apt-hosted stable main" > /etc/apt/sources.list.d/cc.list
apt-get update -qq
apt-get install -y -qq smoke-hello >/dev/null
test "$(smoke-hello)" = "smoke-hello 1.0"
EOF
client debian:trixie-slim use-apt.sh

echo "-- dnf, repodata depth 3, a package group"
cat > "$work/use-yum.sh" <<'EOF'
set -eu
echo "dnf $(dnf --version | head -1)"
rm -f /etc/yum.repos.d/*.repo
printf '[cc]\nname=cc\nbaseurl=%s/repository/yum-hosted/7/os/x86_64/\ngpgcheck=0\n' "$B" > /etc/yum.repos.d/cc.repo
dnf -y -q --disablerepo='*' --enablerepo=cc group install smoke-group >/tmp/dnf.log 2>&1 || { cat /tmp/dnf.log; exit 1; }
test "$(smoke-demo)" = "smoke-demo 1.0"
EOF
client rockylinux/rockylinux:9 use-yum.sh

echo "-- dotnet"
cat > "$work/use-nuget.sh" <<'EOF'
set -eu
export DOTNET_CLI_TELEMETRY_OPTOUT=1 DOTNET_NOLOGO=1
echo "dotnet $(dotnet --version)"
mkdir -p /tmp/app && cd /tmp/app
printf '<configuration><packageSources><clear /></packageSources></configuration>\n' > NuGet.Config
dotnet nuget add source "$B/repository/nuget-hosted/index.json" -n cc --allow-insecure-connections --configfile NuGet.Config >/dev/null
cat > app.csproj <<'X'
<Project Sdk="Microsoft.NET.Sdk">
  <PropertyGroup><OutputType>Exe</OutputType><TargetFramework>net10.0</TargetFramework></PropertyGroup>
  <ItemGroup><PackageReference Include="Smoke.Lib" Version="1.0.0" /></ItemGroup>
</Project>
X
printf 'System.Console.WriteLine(Smoke.Greeter.Hi());\n' > Program.cs
dotnet restore --packages /tmp/packages >/dev/null
dotnet build --no-restore -v q >/dev/null
test "$(dotnet run --no-build)" = "hi from Smoke.Lib"
EOF
client mcr.microsoft.com/dotnet/sdk:10.0 use-nuget.sh

echo "-- gem"
cat > "$work/use-gem.sh" <<'EOF'
set -eu
echo "gem $(gem --version)"
gem install --no-document --clear-sources --source "$B/repository/gems-hosted/" smoke-lib >/dev/null
test "$(ruby -e 'require "smoke_lib"; puts SmokeLib.hi')" = "hi from smoke-lib"
EOF
client ruby:4.0-slim use-gem.sh

echo "-- R"
cat > "$work/use-r.sh" <<'EOF'
set -eu
R --version | head -1
cat > /tmp/use.R <<X
options(warn = 2)
install.packages("smokelib", repos = "$B/repository/r-hosted", lib = "/tmp")
library(smokelib, lib.loc = "/tmp")
stopifnot(hi() == "hi from smokelib")
X
R --no-echo --no-save --no-restore -f /tmp/use.R >/tmp/r.log 2>&1 || { cat /tmp/r.log; exit 1; }
EOF
client r-base:4.6.2 use-r.sh

echo "-- git-lfs"
cat > "$work/use-lfs.sh" <<'EOF'
set -eu
. /work/git-common.sh
git lfs version
git -c lfs.url="$B/repository/lfs-hosted/info/lfs" clone -q /share/git/project.git /tmp/b
cd /tmp/b
sha256sum -c /share/git/expected.sha256 >/dev/null
EOF
client alpine/git:v2.54.0 use-lfs.sh

echo "-- go"
cat > "$work/use-go.sh" <<'EOF'
set -eu
export GOTOOLCHAIN=local GOFLAGS=-mod=mod GOPATH=/tmp/gopath GOCACHE=/tmp/gocache GOPROXY="$B/repository/go-hosted" GONOSUMDB=example.com GOSUMDB=off GOINSECURE=cc-b
go version
mkdir -p /tmp/app && cd /tmp/app
go mod init app >/dev/null 2>&1
cat > main.go <<'GO'
package main

import (
	"fmt"

	"example.com/hello"
)

func main() { fmt.Println(hello.Greet()) }
GO
go get example.com/hello@v1.0.0 >/tmp/go.log 2>&1 || { cat /tmp/go.log; exit 1; }
test "$(go run .)" = "hello from the target"
go list -m -json example.com/hello@v1.0.0 | grep -q '"Time": "2020-01-02T03:04:05Z"'
EOF
client golang:1.27 use-go.sh

echo "-- micromamba"
cat > "$work/use-conda.sh" <<'EOF'
set -eu
echo "micromamba $(micromamba --version)"
micromamba create -y -q -n h -c "$B/repository/conda-hosted" --override-channels ccsmoke >/dev/null
test "$(cat /tmp/mm/envs/h/share/ccsmoke/hello.txt)" = "hello from ccsmoke 1.0"
EOF
client mambaorg/micromamba:2.10.0-ubuntu24.04 use-conda.sh -e MAMBA_ROOT_PREFIX=/tmp/mm

echo "-- skopeo; anonymous pull stayed off"
skopeo --version
[ "$(skopeo inspect --tls-verify=false --creds admin:smoke-pass --format '{{.Digest}}' docker://cc-b:8081/docker-hosted/app:v1)" = "$digest" ] || fail "docker: the digest changed"
skopeo copy -q --src-tls-verify=false --src-creds admin:smoke-pass docker://cc-b:8081/docker-hosted/app:v1 dir:/tmp/out
if skopeo inspect --tls-verify=false --no-creds docker://cc-b:8081/docker-hosted/app:v1 >/dev/null 2>&1; then
  fail "docker: anonymous pull is on, and the source has it off"
fi

echo "IMPORT CONTENT OK"
docker stats --no-stream --format '{{.Name}} memory: {{.MemUsage}}' "$name-a" "$name-b"
