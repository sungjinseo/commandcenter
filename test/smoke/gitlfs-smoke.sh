#!/usr/bin/env bash
# Builds the image, starts commandcenter, then uses real git and git-lfs
# (alpine/git) in two containers that share only the bare git repositories:
#   1. the first tracks two binary files (one larger than the server's
#      memory) and pushes: the commits go to the bare repository, the objects
#      to lfs-hosted. A push without credentials, with a wrong password or
#      without the add privilege is refused first; a push of the same objects
#      from a second repository uploads nothing; git lfs lock is refused
#      (locks are not supported); a private repository takes an API token
#   2. the second clones (the smudge filter downloads), clones without the
#      filter and runs git lfs pull, is told about an object the server never
#      got, and reads the private repository only when signed in
# Finally prints the container's memory usage.
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-gitlfs-$$"
net=commandcenter-gitlfs-smoke-$$
name=commandcenter-gitlfs-smoke-$$
. test/postgres.sh
big=160 # MiB of the large object: more than the server's 128 MiB

cleanup() {
  docker rm -fv "$name" "$name-pg" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup
rm -rf "$work" && mkdir -p "$work/git"

docker build -q -t commandcenter:smoke-gitlfs . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i commandcenter:smoke-gitlfs hash-password)"

cat > "$work/config.yaml" <<EOF
anonymousRead: true
baseURL: "http://commandcenter:8081"
users:
  - { name: ci, passwordHash: "$hash", roles: [deployer] }
  - { name: reader, passwordHash: "$hash" }
roles:
  deployer: { write: [lfs-hosted, lfs-private] }
repositories:
  - { name: lfs-hosted,  format: gitlfs, type: hosted }
  - { name: lfs-private, format: gitlfs, type: hosted, anonymousRead: false }
EOF

cat > "$work/common.sh" <<'EOF'
CC=http://commandcenter:8081
export GIT_TERMINAL_PROMPT=0
git config --global user.name smoke
git config --global user.email smoke@example.com
git config --global init.defaultBranch main
git config --global credential.helper store
git lfs install >/dev/null
# cred <user> <password>: what the credential helper answers from now on.
cred() {
  rm -f "$HOME/.git-credentials"
  printf 'protocol=http\nhost=commandcenter:8081\nusername=%s\npassword=%s\n\n' "$1" "$2" | git credential approve
}
# refused <message> <git arguments>: the command must fail and say message.
refused() {
  msg=$1; shift
  if git "$@" >/tmp/refused.log 2>&1; then echo "FAIL: git $* succeeded"; cat /tmp/refused.log; exit 1; fi
  grep -q "$msg" /tmp/refused.log || { echo "FAIL: git $* did not say \"$msg\":"; cat /tmp/refused.log; exit 1; }
}
EOF

cat > "$work/push.sh" <<'EOF'
set -eu
. /work/common.sh
lfs=$CC/repository/lfs-hosted/info/lfs

echo "== $(git lfs version | cut -d' ' -f1): track two binary files and commit them"
git init -q --bare /git/project.git
git clone -q /git/project.git /tmp/a 2>/dev/null
cd /tmp/a
git config -f .lfsconfig lfs.url "$lfs"
git lfs track '*.bin' >/dev/null
head -c 1048576 /dev/urandom > model.bin
head -c "$((BIG << 20))" /dev/urandom > big.bin
sha256sum model.bin big.bin > /git/expected.sha256
git add .lfsconfig .gitattributes model.bin big.bin
git commit -qm 'two LFS objects'
git cat-file -p HEAD:big.bin | grep -q '^oid sha256:' # the commit holds pointers

echo "== a push without credentials, with a wrong password or without the add privilege is refused"
refused "Git credentials for $lfs not found" push origin main
cred ci wrong-pass
refused "Git credentials for $lfs not found" push origin main # git-lfs drops what the server refused
cred reader smoke-pass
refused 'user "reader" is not allowed' push origin main

echo "== push: the objects go to lfs-hosted, the commits to the git remote"
cred ci smoke-pass
git push origin main >/tmp/push.log 2>&1 || { cat /tmp/push.log; exit 1; }
# git-lfs asked for the locks of the branch, was told 501 and now skips that.
test "$(git config --get "lfs.$lfs.locksverify")" = false

echo "== a second repository pushes the same objects: nothing is uploaded"
git init -q --bare /git/second.git
git remote add second /git/second.git
GIT_TRACE=1 git push second main >/dev/null 2>/tmp/trace.log
grep -q 'HTTP: POST .*/objects/batch' /tmp/trace.log || { cat /tmp/trace.log; exit 1; }
if grep -q 'HTTP: PUT' /tmp/trace.log; then echo "FAIL: an object the server has was uploaded again"; exit 1; fi

echo "== locks are not supported"
refused "locks are not supported" lfs lock model.bin
refused "locks are not supported" lfs locks

echo "== a commit whose object never reached the server (pushed without the hook)"
git checkout -q -b ghost
printf 'version https://git-lfs.github.com/spec/v1\noid sha256:%064d\nsize 5\n' 0 > ghost.bin
git add ghost.bin && git commit -qm 'a pointer without its object'
git push -q --no-verify origin ghost

echo "== a private repository, pushed with an API token"
git init -q --bare /git/private.git
git clone -q /git/private.git /tmp/p 2>/dev/null
cd /tmp/p
git config -f .lfsconfig lfs.url "$CC/repository/lfs-private/info/lfs"
git lfs track '*.bin' >/dev/null
head -c 300000 /dev/urandom > secret.bin
sha256sum secret.bin > /git/expected-private.sha256
git add .lfsconfig .gitattributes secret.bin
git commit -qm 'a private LFS object'
cred ci "$TOKEN"
git push origin main >/tmp/push.log 2>&1 || { cat /tmp/push.log; exit 1; }
EOF

cat > "$work/pull.sh" <<'EOF'
set -eu
. /work/common.sh

echo "== clone elsewhere: the smudge filter downloads both objects"
git clone -q /git/project.git /tmp/b
cd /tmp/b
sha256sum -c /git/expected.sha256 >/dev/null

echo "== clone without the smudge filter, then git lfs pull"
GIT_LFS_SKIP_SMUDGE=1 git clone -q /git/project.git /tmp/c
cd /tmp/c
grep -q '^oid sha256:' model.bin
git lfs pull --include model.bin >/dev/null 2>&1
grep model.bin /git/expected.sha256 | sha256sum -c >/dev/null
grep -q '^oid sha256:' big.bin # not asked for

echo "== an object the server does not have: the pull says so"
GIT_LFS_SKIP_SMUDGE=1 git checkout -q ghost
refused "object does not exist" lfs pull --include ghost.bin

echo "== the private repository: no anonymous pull, a signed-in user pulls"
GIT_LFS_SKIP_SMUDGE=1 git clone -q /git/private.git /tmp/p
cd /tmp/p
refused "Git credentials for $CC/repository/lfs-private/info/lfs not found" lfs pull
cred reader smoke-pass
git lfs pull >/dev/null 2>&1
sha256sum -c /git/expected-private.sha256 >/dev/null
echo "GIT LFS CLIENT OK"
EOF

docker network create "$net" >/dev/null
start_postgres
docker run -d --name "$name" --network "$net" -e COMMANDCENTER_DATABASE_URL="$pg_url" --network-alias commandcenter --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" commandcenter:smoke-gitlfs >/dev/null

ready=0
for i in $(seq 1 30); do
  if docker exec "$name" commandcenter healthcheck >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "FAIL: commandcenter did not become ready"; docker logs "$name"; exit 1
fi
token="$(docker exec "$name" commandcenter token create -config /etc/commandcenter/config.yaml -user ci)"

# git <script>: git and git-lfs as the caller's user (the bare repositories
# in $work/git are the caller's to remove), with a home of its own.
git_client() {
  docker run --rm --network "$net" --memory 1g --user "$(id -u):$(id -g)" -e HOME=/tmp -e BIG="$big" -e TOKEN="$token" \
    -v "$root/$work:/work:ro" -v "$root/$work/git:/git" --entrypoint sh alpine/git:v2.54.0 "/work/$1"
}
git_client push.sh
git_client pull.sh

# Three objects were stored, each uploaded once.
uploads="$(docker logs "$name" 2>&1 | grep -c '"method":"PUT","path":"/repository/lfs-[a-z]*/objects/[0-9a-f]*","status":200' || true)"
test "$uploads" = 3 || { echo "FAIL: $uploads uploads, want 3"; exit 1; }

echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "GIT LFS SMOKE OK"
