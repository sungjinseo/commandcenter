#!/usr/bin/env bash
# Builds the image, starts commandcenter and a second one (the remote of a
# proxy), then uses real dnf, rpmbuild and rpmsign (Rocky Linux 9) to:
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
#   6. upload under a name that is not the package's (Nexus's documentation
#      PUTs test.rpm): stored under its NVRA, in the directory given
#   7. package groups: upload repodata/comps.xml (and .gz), dnf group list and
#      dnf group install, delete it
#   8. a group with mergeMembers, one baseurl over a hosted repository and a
#      proxy of the second commandcenter: dnf sees and installs from both, a
#      package in both comes from the first, changes of either show, package
#      groups pass through, and with the second server gone it still works
#   9. the same over a hosted repository and Rocky's own BaseOS (primary.xml
#      300 MiB when unpacked), in the server's 128 MiB, and Docker's repository,
#      whose metadata is zstd compressed
# The proxy downloads about 100 MB from dl.rockylinux.org (the repodata of
# BaseOS and AppStream is 80 MB of it), 5 MB more for 9, and 1 MB from
# download.docker.com.
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
  docker rm -f -v "$name" "$name-pg" "$name-up" "$name-c" >/dev/null 2>&1 || true
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
  deployer: { write: [yum-hosted, yum-depth, yum-depth3, yum-first, yum-depth5] }
repositories:
  - { name: yum-proxy,  format: yum, type: proxy,  remote: "https://dl.rockylinux.org/pub/" }
  - { name: yum-hosted, format: yum, type: hosted }
  - { name: yum-depth,  format: yum, type: hosted, depth: 1 }
  - { name: yum-depth3, format: yum, type: hosted, depth: 3 }
  - { name: yum-all,    format: yum, type: group,  members: [yum-hosted, yum-proxy], writeMember: yum-hosted }
  - { name: yum-first,  format: yum, type: hosted }
  - { name: yum-second, format: yum, type: proxy,  remote: "http://upstream:8081/repository/yum-source/", metadataMaxAge: "10s" }
  - { name: yum-merged, format: yum, type: group,  members: [yum-first, yum-second], writeMember: yum-first, mergeMembers: true }
  - { name: yum-plain,  format: yum, type: group,  members: [yum-first, yum-second] }
  - { name: yum-depth5, format: yum, type: hosted, depth: 5 }
  - { name: yum-rocky,  format: yum, type: group,  members: [yum-depth5, yum-proxy], mergeMembers: true }
  - { name: yum-docker, format: yum, type: proxy,  remote: "https://download.docker.com/linux/centos/" }
  - { name: yum-zstd,   format: yum, type: group,  members: [yum-depth3, yum-docker], mergeMembers: true }
EOF

cat > "$work/upstream.yaml" <<EOF
anonymousRead: true
baseURL: "http://upstream:8081"
users:
  - { name: ci, passwordHash: "$hash", roles: [deployer] }
roles:
  deployer: { write: [yum-source] }
repositories:
  - { name: yum-source, format: yum, type: hosted }
EOF

cat > "$work/comps.xml" <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE comps PUBLIC "-//Red Hat, Inc.//DTD Comps info//EN" "comps.dtd">
<comps>
  <group>
    <id>smoke-tools</id>
    <name>Smoke Tools</name>
    <description>Package group of the yum smoke test</description>
    <default>false</default>
    <uservisible>true</uservisible>
    <packagelist>
      <packagereq type="mandatory">@GROUPED@</packagereq>
    </packagelist>
  </group>
</comps>
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
echo %{?origin}%{!?origin:payload} > %{buildroot}%{_datadir}/%{name}/data.txt

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
U=http://upstream:8081/repository
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
build() { rpmbuild -bb --quiet --define "_topdir /tmp/rpmbuild" --define "pkg $1" --define "ver $2" ${3:+--define "rec $3"} ${4:+--define "origin $4"} /work/smoke-demo.spec; }
build smoke-demo 1.0 smoke-extra; build smoke-demo 2.0 smoke-extra; build smoke-extra 1.0; build smoke-unsigned 1.0; build smoke-grouped 1.0
cd /tmp/rpmbuild/RPMS/noarch
export GNUPGHOME=/tmp/gnupg
mkdir -m 700 "$GNUPGHOME"
gpg --batch --quiet --passphrase '' --quick-gen-key 'Smoke RPM <rpm@example.com>' rsa3072 sign never
gpg --armor --export rpm@example.com > /tmp/RPM-GPG-KEY-smoke
rpmsign --define '_gpg_name rpm@example.com' --addsign smoke-demo-*.rpm smoke-extra-*.rpm smoke-grouped-*.rpm >/dev/null 2>&1

echo "== upload with curl PUT (through the group, to a directory, and what must be refused)"
test "$(status $R/yum-hosted/repodata/repomd.xml)" = 404 || fail "repomd.xml before the first upload"
test "$(status -u ci:smoke-pass --upload-file smoke-demo-1.0-1.noarch.rpm $R/yum-all/)" = 201 || fail "upload through the group"
for f in smoke-extra-1.0-1.noarch.rpm smoke-unsigned-1.0-1.noarch.rpm; do
  test "$(status -u ci:smoke-pass --upload-file $f $R/yum-hosted/extra/)" = 201 || fail "upload $f to a directory"
done
test "$(status -u ci:smoke-pass --upload-file smoke-demo-1.0-1.noarch.rpm $R/yum-hosted/)" = 409 || fail "re-upload is not 409"
test "$(status -u ci:smoke-pass -T smoke-demo-2.0-1.noarch.rpm $R/yum-hosted/notes.txt)" = 400 || fail "a file that is no RPM is not 400"
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

echo "== second version, under the name Nexus's documentation uploads with: stored under its NVRA; dnf upgrade"
test "$(curl -s -o /dev/null -D /tmp/put.head -w '%{http_code}' -u ci:smoke-pass --upload-file smoke-demo-2.0-1.noarch.rpm $R/yum-hosted/test.rpm)" = 201 || fail "upload 2.0 as test.rpm"
grep -qi '^location: smoke-demo-2.0-1.noarch.rpm' /tmp/put.head || { cat /tmp/put.head; fail "no Location for the name it was stored under"; }
test "$(status $R/yum-hosted/smoke-demo-2.0-1.noarch.rpm)" = 200 || fail "not stored under its NVRA"
test "$(status $R/yum-hosted/test.rpm)" = 404 || fail "test.rpm is served"
test "$(status -u ci:smoke-pass --upload-file smoke-demo-2.0-1.noarch.rpm $R/yum-hosted/again.rpm)" = 409 || fail "the same package under another name is not 409"
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
test "$(status -u ci:smoke-pass --upload-file smoke-extra-1.0-1.noarch.rpm $R/yum-depth3/7/os/test.rpm)" = 400 || fail "depth 3: another name too shallow is not 400"
test "$(status -u ci:smoke-pass --upload-file smoke-unsigned-1.0-1.noarch.rpm $R/yum-depth3/7/os/x86_64/test.rpm)" = 201 || fail "depth 3: another name"
test "$(status $R/yum-depth3/7/os/x86_64/smoke-unsigned-1.0-1.noarch.rpm)" = 200 || fail "depth 3: stored under its NVRA in the directory given"

echo "== package groups: repodata/comps.xml on hosted"
sed 's/@GROUPED@/smoke-grouped/' /work/comps.xml > /tmp/comps.xml
test "$(status -u ci:smoke-pass --upload-file smoke-grouped-1.0-1.noarch.rpm $R/yum-hosted/)" = 201 || fail "upload smoke-grouped"
test "$(status -u ci:smoke-pass --upload-file /tmp/comps.xml $R/yum-hosted/repodata/comps.xml)" = 201 || fail "upload comps.xml"
test "$(status -u ci:smoke-pass --upload-file /tmp/comps.xml $R/yum-hosted/repodata/comps.xml)" = 409 || fail "a second comps.xml is not 409"
test "$(status -u ci:smoke-pass --upload-file smoke-grouped-1.0-1.noarch.rpm $R/yum-depth/el7/repodata/comps.xml)" = 400 || fail "an RPM as comps.xml is not 400"
test "$(status -u ci:smoke-pass --upload-file /tmp/comps.xml $R/yum-depth/repodata/comps.xml)" = 400 || fail "depth 1: comps.xml at the root is not 400"
curl -fsS $R/yum-hosted/repodata/repomd.xml | grep -q '<data type="group">' || fail "repomd.xml does not list comps.xml"
curl -fsS $R/yum-hosted/repodata/comps.xml | cmp - /tmp/comps.xml || fail "GET of comps.xml"
dnf -q --refresh --repo cc group list | tee /tmp/groups.txt
grep -q 'Smoke Tools' /tmp/groups.txt || fail "dnf group list"
dnf -y -q --disablerepo='*' --enablerepo=cc --enablerepo=cc-baseos group install smoke-tools
rpm -q smoke-grouped >/dev/null || fail "dnf group install"
test "$(status -X DELETE $R/yum-hosted/repodata/comps.xml)" = 401 || fail "anonymous delete of comps.xml is not 401"
test "$(status -u ci:smoke-pass -X DELETE $R/yum-hosted/repodata/comps.xml)" = 204 || fail "delete of comps.xml"
test "$(status $R/yum-hosted/repodata/comps.xml)" = 404 || fail "comps.xml after the delete"
if curl -fsS $R/yum-hosted/repodata/repomd.xml | grep -q '<data type="group">'; then fail "repomd.xml still lists comps.xml"; fi
if dnf -q --refresh --repo cc group list 2>&1 | grep -q 'Smoke Tools'; then fail "dnf group list after the delete"; fi

echo "== package groups: comps.xml.gz at a depth"
gzip -c /tmp/comps.xml > /tmp/comps.xml.gz
test "$(status -u ci:smoke-pass --upload-file /tmp/comps.xml.gz $R/yum-depth/el9/repodata/comps.xml.gz)" = 201 || fail "upload comps.xml.gz"
curl -fsS $R/yum-depth/el9/repodata/repomd.xml | grep -q '<data type="group_gz">' || fail "repomd.xml does not list comps.xml.gz"
dnf -q --refresh --repofrompath "d9,$R/yum-depth/el9/" --repo d9 --nogpgcheck group list | grep -q 'Smoke Tools' || fail "dnf group list from comps.xml.gz"

echo "== a group with mergeMembers: yum-first and a proxy of yum-source on a second server, at one baseurl"
mkdir -p /tmp/both
build smoke-first 1.0; build smoke-first 2.0; build smoke-second 1.0; build smoke-second 2.0; build smoke-third 1.0
build smoke-both 1.0 "" first; mv smoke-both-1.0-1.noarch.rpm /tmp/both/first.rpm
build smoke-both 1.0 "" second; mv smoke-both-1.0-1.noarch.rpm /tmp/both/second.rpm
test "$(status -u ci:smoke-pass --upload-file smoke-first-1.0-1.noarch.rpm $R/yum-merged/)" = 201 || fail "upload to the group's writeMember"
test "$(status -u ci:smoke-pass --upload-file /tmp/both/first.rpm $R/yum-first/smoke-both-1.0-1.noarch.rpm)" = 201 || fail "upload smoke-both to the first"
test "$(status -u ci:smoke-pass --upload-file smoke-second-1.0-1.noarch.rpm $U/yum-source/Packages/)" = 201 || fail "upload to the second server"
test "$(status -u ci:smoke-pass --upload-file /tmp/both/second.rpm $U/yum-source/Packages/smoke-both-1.0-1.noarch.rpm)" = 201 || fail "upload smoke-both to the second server"
cat >> /etc/yum.repos.d/cc.repo <<'REPO'
[cc-merged]
name=yum-first and yum-second through yum-merged
baseurl=http://commandcenter:8081/repository/yum-merged/
gpgcheck=0
[cc-plain]
name=the same group without mergeMembers
baseurl=http://commandcenter:8081/repository/yum-plain/
gpgcheck=0
REPO
merged() { dnf -q --refresh --repo cc-merged "$@"; }
test "$(dnf -q --refresh --repo cc-plain repoquery --qf '%{name}' | sort | tr '\n' ' ')" = "smoke-both smoke-first " || fail "without mergeMembers the first member alone answers"
merged repoquery --qf '%{name}-%{version} %{location}' | sort | tee /tmp/merged.txt
test "$(cut -d' ' -f1 /tmp/merged.txt | tr '\n' ' ')" = "smoke-both-1.0 smoke-first-1.0 smoke-second-1.0 " || fail "dnf does not see both members"
grep -qx 'smoke-second-1.0 Packages/smoke-second-1.0-1.noarch.rpm' /tmp/merged.txt || fail "the location of a proxied package"
merged repoquery -l smoke-second | grep -qx /usr/share/smoke-second/data.txt || fail "filelists of the proxy member"
merged repoquery --whatprovides /usr/share/smoke-first/data.txt | grep -q '^smoke-first-0:1.0-1.noarch$' || fail "filelists of the hosted member"
merged repoquery --changelog smoke-second | grep -q 'Version 1.0' || fail "other of the proxy member"
merged repoquery --requires smoke-first | grep -qx which || fail "requires of the merged primary"
dnf -y -q --disablerepo='*' --enablerepo=cc-merged install smoke-first smoke-second smoke-both
test "$(smoke-first)" = "smoke-first 1.0 /usr/bin/sh" || fail "smoke-first from hosted"
test "$(smoke-second)" = "smoke-second 1.0 /usr/bin/sh" || fail "smoke-second through the proxy member"
test "$(cat /usr/share/smoke-both/data.txt)" = first || fail "a package in both does not come from the first member"
curl -fsS $R/yum-merged/smoke-both-1.0-1.noarch.rpm | cmp - /tmp/both/first.rpm || fail "the path of a package in both answers another file"

echo "== a change of a member shows: hosted at once, the proxy after its metadataMaxAge"
test "$(status -u ci:smoke-pass --upload-file smoke-first-2.0-1.noarch.rpm $R/yum-merged/any-name.rpm)" = 201 || fail "upload smoke-first 2.0"
dnf -y -q --refresh --disablerepo='*' --enablerepo=cc-merged upgrade smoke-first
test "$(smoke-first)" = "smoke-first 2.0 /usr/bin/sh" || fail "smoke-first was not upgraded"
test "$(status -u ci:smoke-pass --upload-file smoke-second-2.0-1.noarch.rpm $U/yum-source/Packages/)" = 201 || fail "upload smoke-second 2.0"
test "$(status -u ci:smoke-pass --upload-file smoke-third-1.0-1.noarch.rpm $U/yum-source/Packages/)" = 201 || fail "upload smoke-third"
sleep 11
dnf -y -q --refresh --disablerepo='*' --enablerepo=cc-merged upgrade smoke-second
test "$(smoke-second)" = "smoke-second 2.0 /usr/bin/sh" || fail "smoke-second was not upgraded"

echo "== package groups through the merge, with a package of the proxy member; a member's signature does not sign a merge"
sed 's/@GROUPED@/smoke-third/' /work/comps.xml > /tmp/comps3.xml
test "$(status -u ci:smoke-pass --upload-file /tmp/comps3.xml $R/yum-first/repodata/comps.xml)" = 201 || fail "upload comps.xml to the first member"
merged group list | grep -q 'Smoke Tools' || fail "dnf group list through the merge"
dnf -y -q --refresh --disablerepo='*' --enablerepo=cc-merged group install smoke-tools
rpm -q smoke-third >/dev/null || fail "dnf group install through the merge"
test "$(status $R/yum-merged/repodata/repomd.xml.asc)" = 404 || fail "a signature on a merge"
if dnf -q --repo cc-merged --setopt=cc-merged.repo_gpgcheck=1 --refresh makecache >/tmp/repogpg2.log 2>&1; then fail "repo_gpgcheck=1 passed on a merge"; fi
grep -q 'repomd.xml.asc' /tmp/repogpg2.log || { cat /tmp/repogpg2.log; fail "no repomd.xml.asc error on a merge"; }

echo "== merge a hosted repository with Rocky's BaseOS (a depth of 5 is its path), in the server's memory"
os=rocky/$(rpm -E '%{rhel}')/BaseOS/$(uname -m)/os
build smoke-local 1.0
test "$(status -u ci:smoke-pass --upload-file smoke-local-1.0-1.noarch.rpm $R/yum-depth5/$os/)" = 201 || fail "upload to the path of BaseOS"
curl -fsS -m 300 -o /dev/null "$R/yum-rocky/$os/repodata/repomd.xml" # a first merge this large can outlast dnf's 30 seconds on a busy machine
test "$(dnf -q --refresh --repofrompath "m,$R/yum-rocky/$os/" --repo m --nogpgcheck repoquery --qf '%{name}' smoke-local tree bash | sort -u | tr '\n' ' ')" = "bash smoke-local tree " || fail "dnf does not see hosted and BaseOS"
dnf -y -q --repofrompath "m,$R/yum-rocky/$os/" --disablerepo='*' --enablerepo=m --nogpgcheck install smoke-local tree
smoke-local >/dev/null && tree --version >/dev/null || fail "install from the merge with BaseOS"
curl -fsS "$R/yum-rocky/$os/repodata/repomd.xml" | grep -q '<data type="group">' || fail "BaseOS's package groups are not passed on"

echo "== merge with Docker's repository (zstd compressed metadata)"
stable=$(rpm -E '%{rhel}')/$(uname -m)/stable
curl -fsS "$R/yum-docker/$stable/repodata/repomd.xml" | grep -q 'primary.xml.zst' || echo "note: Docker's repository no longer uses zstd"
test "$(status -u ci:smoke-pass --upload-file smoke-local-1.0-1.noarch.rpm $R/yum-depth3/$stable/)" = 201 || fail "upload to the path of Docker's repository"
test "$(dnf -q --refresh --repofrompath "z,$R/yum-zstd/$stable/" --repo z --nogpgcheck repoquery --qf '%{name}' smoke-local docker-ce-cli | sort -u | tr '\n' ' ')" = "docker-ce-cli smoke-local " || fail "dnf does not see hosted and Docker's repository"
echo "YUM CLIENT OK"
EOF

cat > "$work/run-gone.sh" <<'EOF'
set -eu
R=http://commandcenter:8081/repository
fail() { echo "FAIL: $*"; exit 1; }
rm -f /etc/yum.repos.d/*.repo
cat > /etc/yum.repos.d/cc.repo <<'REPO'
[cc-baseos]
name=Rocky BaseOS through yum-proxy, for what smoke-second requires
baseurl=http://commandcenter:8081/repository/yum-proxy/rocky/$releasever/BaseOS/$basearch/os/
gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-Rocky-$releasever
[cc-merged]
name=yum-merged with the second server gone
baseurl=http://commandcenter:8081/repository/yum-merged/
gpgcheck=0
REPO
echo "== the second server is gone: the merge stands, dnf installs what the proxy has cached"
curl -fsS -D /tmp/head -o /dev/null $R/yum-merged/repodata/repomd.xml
grep -qi '^warning: 110' /tmp/head || { cat /tmp/head; fail "no Warning on what the remote could not confirm"; }
dnf -q --refresh --repo cc-merged repoquery --qf '%{name}-%{version}' | sort | tr '\n' ' ' | tee /tmp/merged.txt
echo
grep -q 'smoke-second-2.0' /tmp/merged.txt || fail "the proxy member's packages are gone from the merge"
dnf -y -q install smoke-second-2.0 smoke-first
test "$(smoke-second)" = "smoke-second 2.0 /usr/bin/sh" || fail "smoke-second from the proxy's cache"
echo "YUM GONE OK"
EOF

docker network create "$net" >/dev/null
start_postgres
docker exec "$name-pg" createdb -U postgres upstream
docker run -d --name "$name" --network "$net" -e COMMANDCENTER_DATABASE_URL="$pg_url" --network-alias commandcenter --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" commandcenter:smoke-yum >/dev/null
docker run -d --name "$name-up" --network "$net" -e COMMANDCENTER_DATABASE_URL="${pg_url/\/cc\?/\/upstream?}" --network-alias upstream --memory 128m \
  -v "$root/$work/upstream.yaml:/etc/commandcenter/config.yaml:ro" commandcenter:smoke-yum >/dev/null

ready=0
for i in $(seq 1 30); do
  if docker exec "$name" commandcenter healthcheck >/dev/null 2>&1 && docker exec "$name-up" commandcenter healthcheck >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "FAIL: commandcenter did not become ready"; docker logs "$name"; docker logs "$name-up"; exit 1
fi

client() { docker run --rm --name "$name-c" --network "$net" --memory 1g -v "$root/$work:/work" "${SMOKE_YUM_IMAGE:-rockylinux/rockylinux:9}" sh "$1"; }
rc=0
client /work/run.sh || rc=$?
if [ "$rc" = 0 ]; then
  docker rm -f -v "$name-up" >/dev/null
  sleep 11 # the proxy's metadataMaxAge: it asks the second server again
  client /work/run-gone.sh || rc=$?
fi
if [ "$(docker inspect -f '{{.State.OOMKilled}}' "$name")" = true ]; then
  echo "FAIL: commandcenter was OOM-killed"; rc=1
fi
if [ "$rc" != 0 ]; then
  echo "== last commandcenter log lines"; docker logs --tail 40 "$name" 2>&1 || true
  exit "$rc"
fi

echo "== memory"
docker logs "$name" 2>&1 | grep 'yum group merge' || true
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "YUM SMOKE OK"
