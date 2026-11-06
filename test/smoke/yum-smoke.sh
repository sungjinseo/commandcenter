#!/usr/bin/env bash
# Builds the image, starts commandcenter, then uses real dnf, rpmbuild and
# rpmsign (Rocky Linux 9) to:
#   1. install rpm-build and rpm-sign through the yum-all group: Rocky BaseOS
#      and AppStream come from dl.rockylinux.org via yum-proxy, gpgcheck=1
#   2. build smoke-demo 1.0 and 2.0 and smoke-extra (signed with a key made
#      here) and an unsigned package, and upload them to yum-hosted with
#      curl PUT
#   3. dnf install from the generated repodata (the dependency comes via the
#      proxy, the recommended package from hosted), dnf upgrade to 2.0,
#      delete 2.0 with the Nexus REST API and dnf downgrade to 1.0
#   4. check gpgcheck (signed, unsigned) and repo_gpgcheck (hosted repomd.xml
#      is not signed; the proxy passes Rocky's repomd.xml.asc through)
#   5. use repositories with a repodata depth of 1 (a component upload API
#      upload too) and 3: one repodata/ per directory of that depth
# The proxy downloads about 100 MB from dl.rockylinux.org (the repodata of
# BaseOS and AppStream is 80 MB of it).
# Finally prints the container's memory usage.
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-yum-$$"
net=commandcenter-yum-smoke-$$
name=commandcenter-yum-smoke-$$
. test/postgres.sh

cleanup() {
  docker rm -f -v "$name" "$name-pg" "$name-c" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup
rm -rf "$work" && mkdir -p "$work"

docker build -q -t commandcenter:smoke-yum . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i commandcenter:smoke-yum hash-password)"

cat > "$work/config.yaml" <<EOF
anonymousRead: true
baseURL: "http://commandcenter:8081"
users:
  - { name: ci, passwordHash: "$hash", roles: [deployer] }
roles:
  deployer: { write: [yum-hosted, yum-depth, yum-depth3] }
repositories:
  - { name: yum-proxy,  format: yum, type: proxy,  remote: "https://dl.rockylinux.org/pub/" }
  - { name: yum-hosted, format: yum, type: hosted }
  - { name: yum-depth,  format: yum, type: hosted, depth: 1 }
  - { name: yum-depth3, format: yum, type: hosted, depth: 3 }
  - { name: yum-all,    format: yum, type: group,  members: [yum-hosted, yum-proxy], writeMember: yum-hosted }
EOF

cat > "$work/smoke-demo.spec" <<'EOF'
Name:       %{pkg}
Version:    %{ver}
Release:    1
Summary:    Command Center yum smoke package
License:    MIT
URL:        https://example.com/smoke
BuildArch:  noarch
Requires:   which
%{?rec:Recommends: %{rec}}

%description
A tiny package for the yum smoke test.

%install
mkdir -p %{buildroot}%{_bindir} %{buildroot}%{_datadir}/%{name}
printf '#!/bin/sh\necho "%{name} %{version} $(which sh)"\n' > %{buildroot}%{_bindir}/%{name}
chmod 755 %{buildroot}%{_bindir}/%{name}
echo payload > %{buildroot}%{_datadir}/%{name}/data.txt

%files
%{_bindir}/%{name}
%{_datadir}/%{name}

%changelog
* Mon Jan 05 2026 Smoke Test <smoke@example.com> - %{ver}-1
- Version %{ver}
EOF

cat > "$work/run.sh" <<'EOF'
set -eu
R=http://commandcenter:8081/repository
REST=http://commandcenter:8081/service/rest/v1
fail() { echo "FAIL: $*"; exit 1; }
status() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

rm -f /etc/yum.repos.d/*.repo
cat > /etc/yum.repos.d/cc.repo <<'REPO'
[cc-baseos]
name=Rocky BaseOS through yum-all (yum-proxy)
baseurl=http://commandcenter:8081/repository/yum-all/rocky/$releasever/BaseOS/$basearch/os/
gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-Rocky-$releasever
[cc-appstream]
name=Rocky AppStream through yum-all (yum-proxy)
baseurl=http://commandcenter:8081/repository/yum-all/rocky/$releasever/AppStream/$basearch/os/
gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-Rocky-$releasever
REPO

echo "== install rpm-build and rpm-sign through the group (Rocky via the proxy, gpgcheck=1)"
dnf -y -q install rpm-build rpm-sign >/tmp/tools.log 2>&1 || { tail -30 /tmp/tools.log; fail "dnf install through the proxy"; }
rpm -q rpm-build rpm-sign

echo "== build smoke-demo 1.0 and 2.0 (recommends smoke-extra), smoke-extra and smoke-unsigned; sign all but the last"
build() { rpmbuild -bb --quiet --define "_topdir /tmp/rpmbuild" --define "pkg $1" --define "ver $2" ${3:+--define "rec $3"} /work/smoke-demo.spec; }
build smoke-demo 1.0 smoke-extra; build smoke-demo 2.0 smoke-extra; build smoke-extra 1.0; build smoke-unsigned 1.0
cd /tmp/rpmbuild/RPMS/noarch
export GNUPGHOME=/tmp/gnupg
mkdir -m 700 "$GNUPGHOME"
gpg --batch --quiet --passphrase '' --quick-gen-key 'Smoke RPM <rpm@example.com>' rsa3072 sign never
gpg --armor --export rpm@example.com > /tmp/RPM-GPG-KEY-smoke
rpmsign --define '_gpg_name rpm@example.com' --addsign smoke-demo-*.rpm smoke-extra-*.rpm >/dev/null 2>&1

echo "== upload with curl PUT (through the group, to a directory, and what must be refused)"
test "$(status $R/yum-hosted/repodata/repomd.xml)" = 404 || fail "repomd.xml before the first upload"
test "$(status -u ci:smoke-pass --upload-file smoke-demo-1.0-1.noarch.rpm $R/yum-all/)" = 201 || fail "upload through the group"
for f in smoke-extra-1.0-1.noarch.rpm smoke-unsigned-1.0-1.noarch.rpm; do
  test "$(status -u ci:smoke-pass --upload-file $f $R/yum-hosted/extra/)" = 201 || fail "upload $f to a directory"
done
test "$(status -u ci:smoke-pass --upload-file smoke-demo-1.0-1.noarch.rpm $R/yum-hosted/)" = 409 || fail "re-upload is not 409"
test "$(status -u ci:smoke-pass -T smoke-demo-2.0-1.noarch.rpm $R/yum-hosted/other-9.9-1.noarch.rpm)" = 400 || fail "wrong file name is not 400"
test "$(status --upload-file smoke-demo-2.0-1.noarch.rpm $R/yum-hosted/)" = 401 || fail "anonymous upload is not 401"

cat >> /etc/yum.repos.d/cc.repo <<'REPO'
[cc]
name=Command Center yum-hosted through yum-all
baseurl=http://commandcenter:8081/repository/yum-all/
gpgcheck=1
gpgkey=file:///tmp/RPM-GPG-KEY-smoke
repo_gpgcheck=0
REPO

echo "== dnf install: signed package from hosted, its dependency via the proxy, what it recommends from hosted"
dnf -y -q install smoke-demo
test "$(smoke-demo)" = "smoke-demo 1.0 /usr/bin/sh" || fail "smoke-demo 1.0 does not run"
rpm -q which >/dev/null || fail "the dependency was not installed"
rpm -q smoke-extra >/dev/null || fail "the recommended package was not installed"

echo "== the generated metadata: info, files, changelog, weak dependency"
info="$(dnf -q repoquery --repo cc --qf '%{name}|%{evr}|%{arch}|%{license}|%{url}|%{summary}|%{location}' smoke-demo smoke-extra)"
echo "$info"
echo "$info" | grep -qx 'smoke-demo|1.0-1|noarch|MIT|https://example.com/smoke|Command Center yum smoke package|smoke-demo-1.0-1.noarch.rpm' || fail "primary.xml of smoke-demo"
echo "$info" | grep -q '|extra/smoke-extra-1.0-1.noarch.rpm$' || fail "location of the package in a directory"
dnf -q repoquery --repo cc -l smoke-demo | grep -qx /usr/share/smoke-demo/data.txt || fail "filelists.xml"
dnf -q repoquery --repo cc --whatprovides /usr/share/smoke-demo/data.txt | grep -q '^smoke-demo-0:1.0-1.noarch$' || fail "whatprovides a file"
dnf -q repoquery --repo cc --changelog smoke-demo | grep -q 'Version 1.0' || fail "other.xml (changelog)"
dnf -q repoquery --repo cc --requires smoke-demo | grep -qx which || fail "requires"
dnf -q repoquery --repo cc --recommends smoke-demo | grep -qx smoke-extra || fail "recommends (weak dependency)"

echo "== gpgcheck=1 refuses the unsigned package; --nogpgcheck installs it"
if dnf -y -q install smoke-unsigned >/tmp/unsigned.log 2>&1; then fail "an unsigned package was installed with gpgcheck=1"; fi
grep -Eqi 'not signed|GPG check FAILED' /tmp/unsigned.log || { cat /tmp/unsigned.log; fail "no gpg error for the unsigned package"; }
dnf -y -q --nogpgcheck install smoke-unsigned
test "$(smoke-unsigned)" = "smoke-unsigned 1.0 /usr/bin/sh"

echo "== repo_gpgcheck=1: refused on hosted (repomd.xml is not signed)"
test "$(status $R/yum-hosted/repodata/repomd.xml.asc)" = 404 || fail "repomd.xml.asc of hosted is not 404"
if dnf -q --repo cc --setopt=cc.repo_gpgcheck=1 --refresh makecache >/tmp/repogpg.log 2>&1; then fail "repo_gpgcheck=1 passed on hosted"; fi
grep -q 'repomd.xml.asc' /tmp/repogpg.log || { cat /tmp/repogpg.log; fail "no repomd.xml.asc error"; }

echo "== repo_gpgcheck=1 through the proxy (Rocky extras: repomd.xml.asc is passed through)"
cat >> /etc/yum.repos.d/cc.repo <<'REPO'
[cc-extras]
name=Rocky extras through yum-proxy
baseurl=http://commandcenter:8081/repository/yum-proxy/rocky/$releasever/extras/$basearch/os/
gpgcheck=1
repo_gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-Rocky-$releasever
REPO
dnf -y -q --setopt=install_weak_deps=False install epel-release
rpm -q epel-release
rm -f /etc/yum.repos.d/epel*.repo
extras="$R/yum-proxy/rocky/$(rpm -E '%{rhel}')/extras/$(uname -m)/os"
curl -fsS "$extras/repodata/repomd.xml.asc" | grep -q 'BEGIN PGP SIGNATURE' || fail "the proxy does not serve repomd.xml.asc"
test "$(status "$extras/")" = 404 || fail "the proxy served a directory listing"
test "$(status "$extras/repodata/")" = 404 || fail "the proxy served a directory listing"

echo "== second version: dnf upgrade"
test "$(status -u ci:smoke-pass --upload-file smoke-demo-2.0-1.noarch.rpm $R/yum-hosted/)" = 201 || fail "upload 2.0"
dnf -y -q --refresh upgrade smoke-demo
test "$(smoke-demo)" = "smoke-demo 2.0 /usr/bin/sh" || fail "smoke-demo was not upgraded"

echo "== delete 2.0 (Nexus REST API): the repodata follows, dnf downgrades"
before="$(curl -fsS $R/yum-hosted/repodata/repomd.xml | grep -o 'repodata/[0-9a-f]*-primary.xml.gz')"
id="$(curl -fsS "$REST/search?repository=yum-hosted&name=smoke-demo&version=2.0-1.noarch" | python3 -c 'import json, sys; print(json.load(sys.stdin)["items"][0]["id"])')"
test "$(status -u ci:smoke-pass -X DELETE "$REST/components/$id")" = 204 || fail "component delete"
test "$(status $R/yum-hosted/smoke-demo-2.0-1.noarch.rpm)" = 404 || fail "the deleted rpm is still served"
test "$(status $R/yum-hosted/$before)" = 404 || fail "the previous primary.xml.gz is still served"
dnf -q --refresh repoquery --repo cc smoke-demo | grep -qx 'smoke-demo-0:1.0-1.noarch' || fail "repodata after delete"
dnf -y -q downgrade smoke-demo
test "$(smoke-demo)" = "smoke-demo 1.0 /usr/bin/sh" || fail "smoke-demo was not downgraded"

echo "== depth 1: one repodata per top-level directory"
test "$(status -u ci:smoke-pass --upload-file smoke-demo-1.0-1.noarch.rpm $R/yum-depth/)" = 400 || fail "depth 1: upload to the root is not 400"
test "$(status -u ci:smoke-pass -F yum.directory=el9 -F yum.asset=@smoke-demo-1.0-1.noarch.rpm -F yum.asset.filename=smoke-demo-1.0-1.noarch.rpm "$REST/components?repository=yum-depth")" = 204 || fail "depth 1: component upload API"
test "$(status -u ci:smoke-pass --upload-file smoke-demo-2.0-1.noarch.rpm $R/yum-depth/el8/sub/)" = 201 || fail "depth 1: upload to el8/sub/"
test "$(status $R/yum-depth/repodata/repomd.xml)" = 404 || fail "depth 1: repodata at the root"
dnf -q --repofrompath "d9,$R/yum-depth/el9/" --repo d9 --nogpgcheck repoquery --qf '%{name}-%{evr} %{location}' | tee /tmp/d9.txt
test "$(cat /tmp/d9.txt)" = "smoke-demo-1.0-1 smoke-demo-1.0-1.noarch.rpm" || fail "depth 1: el9"
dnf -q --repofrompath "d8,$R/yum-depth/el8/" --repo d8 --nogpgcheck repoquery --qf '%{name}-%{evr} %{location}' | tee /tmp/d8.txt
test "$(cat /tmp/d8.txt)" = "smoke-demo-2.0-1 sub/smoke-demo-2.0-1.noarch.rpm" || fail "depth 1: el8"
dnf -y -q --repofrompath "d8,$R/yum-depth/el8/" --repo d8 --nogpgcheck upgrade smoke-demo
test "$(smoke-demo)" = "smoke-demo 2.0 /usr/bin/sh" || fail "depth 1: install from el8/"

echo "== depth 3: 7/os/x86_64/ and 7/os/aarch64/ have a repodata each"
test "$(status -u ci:smoke-pass --upload-file smoke-extra-1.0-1.noarch.rpm $R/yum-depth3/7/os/)" = 400 || fail "depth 3: upload too shallow is not 400"
test "$(status -u ci:smoke-pass --upload-file smoke-demo-1.0-1.noarch.rpm $R/yum-depth3/7/os/x86_64/Packages/s/)" = 201 || fail "depth 3: upload to 7/os/x86_64/Packages/s/"
test "$(status -u ci:smoke-pass --upload-file smoke-extra-1.0-1.noarch.rpm $R/yum-depth3/7/os/aarch64/)" = 201 || fail "depth 3: upload to 7/os/aarch64/"
test "$(status $R/yum-depth3/7/os/repodata/repomd.xml)" = 404 || fail "depth 3: repodata too shallow"
dnf -q --repofrompath "d3,$R/yum-depth3/7/os/x86_64/" --repo d3 --nogpgcheck repoquery --qf '%{name}-%{evr} %{location}' | tee /tmp/d3.txt
test "$(cat /tmp/d3.txt)" = "smoke-demo-1.0-1 Packages/s/smoke-demo-1.0-1.noarch.rpm" || fail "depth 3: 7/os/x86_64"
dnf -y -q --repofrompath "d3,$R/yum-depth3/7/os/x86_64/" --repo d3 --nogpgcheck downgrade smoke-demo
test "$(smoke-demo)" = "smoke-demo 1.0 /usr/bin/sh" || fail "depth 3: install from 7/os/x86_64/"
echo "YUM CLIENT OK"
EOF

docker network create "$net" >/dev/null
start_postgres
docker run -d --name "$name" --network "$net" -e COMMANDCENTER_DATABASE_URL="$pg_url" --network-alias commandcenter --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" commandcenter:smoke-yum >/dev/null

ready=0
for i in $(seq 1 30); do
  if docker exec "$name" commandcenter healthcheck >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "FAIL: commandcenter did not become ready"; docker logs "$name"; exit 1
fi

docker run --rm --name "$name-c" --network "$net" --memory 1g -v "$root/$work:/work" "${SMOKE_YUM_IMAGE:-rockylinux/rockylinux:9}" sh /work/run.sh

echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "YUM SMOKE OK"
