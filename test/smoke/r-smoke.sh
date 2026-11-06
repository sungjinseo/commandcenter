#!/usr/bin/env bash
# Builds the image, starts commandcenter, then uses real R (r-base) and curl to:
#   1. build source packages with R CMD build; upload them to r-hosted at CRAN's
#      src/contrib and at the paths Nexus also takes (Archive/<pkg>, a prefix,
#      a binary layout of its own), through the REST upload, and a real Windows
#      binary of CRAN
#   2. read and install from the hosted repository (PACKAGES.rds, every version)
#   3. install through r-proxy of CRAN: packages, an archived version with
#      remotes (Meta/archive.rds), tools::CRAN_package_db (web/packages/packages.rds)
#   4. install through the r-all group: an internal package named like a public
#      one wins, whatever version the public one has, in the source and the
#      binary index; a group over a remote with only PACKAGES.gz lists its packages
#   5. restore an renv lockfile and install with pak through the group
#   6. delete a package through the REST API, lose the index files and run the
#      repository.rebuild-metadata task
# Finally prints the container's memory usage.
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-r-$$"
net=commandcenter-r-smoke-$$
name=commandcenter-r-smoke-$$
. test/postgres.sh

cleanup() {
  docker rm -f -v "$name" "$name-pg" "$name-r" "$name-curl" "$name-mirror" >/dev/null 2>&1 || true
  docker volume rm "$name-share" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup

# Built before the work directory exists: a new directory in the build
# context would compile the binary again on every run.
docker build -q -t commandcenter:smoke-r . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i commandcenter:smoke-r hash-password)"

mkdir -p "$work"
cat > "$work/config.yaml" <<EOF
anonymousRead: true
baseURL: "http://commandcenter:8081"
users:
  - { name: admin, passwordHash: "$hash", roles: [admin] }
  - { name: ci, passwordHash: "$hash", roles: [deployer] }
roles:
  deployer: { write: [r-hosted] }
repositories:
  - { name: r-proxy,   format: r, type: proxy,  remote: "https://cloud.r-project.org/" }
  - { name: r-hosted,  format: r, type: hosted }
  - { name: r-all,     format: r, type: group,  members: [r-hosted, r-proxy], writeMember: r-hosted }
  - { name: r-gzonly,  format: r, type: proxy,  remote: "http://mirror:8000/" }
  - { name: r-migrate, format: r, type: group,  members: [r-gzonly, r-hosted] }
EOF

# What every R step starts with: the repositories, and check(), which fails the step.
cat > "$work/common.R" <<'EOF'
options(warn = 1, timeout = 300)
BASE <- "http://commandcenter:8081/repository/"
H <- paste0(BASE, "r-hosted"); P <- paste0(BASE, "r-proxy"); G <- paste0(BASE, "r-all")
tools_lib <- "/tmp/tools"
dir.create(tools_lib, showWarnings = FALSE)
.libPaths(c(tools_lib, .libPaths()))
newlib <- function() { d <- tempfile("lib", "/tmp"); dir.create(d); d }
ver <- function(pkg, lib) as.character(packageVersion(pkg, lib.loc = lib))
check <- function(what, got, want) {
  if (!identical(as.character(got), as.character(want))) {
    stop(sprintf("%s: got [%s], want [%s]", what, paste(got, collapse = " "), paste(want, collapse = " ")))
  }
  cat("ok:", what, "\n")
}
EOF

docker network create "$net" >/dev/null
docker volume create "$name-share" >/dev/null
start_postgres
docker run -d --name "$name" --network "$net" -e COMMANDCENTER_DATABASE_URL="$pg_url" --network-alias commandcenter --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" commandcenter:smoke-r >/dev/null

# No port is published: the image's own health check asks the server.
ready=0
for i in $(seq 1 30); do
  if docker exec "$name" commandcenter healthcheck 2>/dev/null; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "FAIL: commandcenter did not become ready"; docker logs "$name"; exit 1
fi

# The clients keep running: uploads (curl) and installs (R) take turns.
docker run -d --name "$name-r" --network "$net" --memory 2g -v "$name-share:/share" -v "$root/$work:/work:ro" r-base:4.6.2 sleep infinity >/dev/null
docker run -d --name "$name-curl" --network "$net" --memory 128m -v "$name-share:/share:ro" --entrypoint sleep curlimages/curl:8.23.0 infinity >/dev/null
r() { docker exec -i "$name-r" sh -c "cd /tmp && { echo 'source(\"/work/common.R\")'; cat; } | R --no-echo --no-save --no-restore -f /dev/stdin"; }
BASE=http://commandcenter:8081
HOSTED=$BASE/repository/r-hosted
REST=$BASE/service/rest/v1
# want STATUS CURL-ARGS...: curl answers STATUS.
want() {
  local status=$1 got; shift
  got="$(docker exec "$name-curl" curl -s -o /tmp/out -w '%{http_code}' "$@")"
  if [ "$got" != "$status" ]; then
    echo "FAIL: curl $* answered $got, want $status:"; docker exec "$name-curl" cat /tmp/out; return 1
  fi
}
put() { want "$1" -u ci:smoke-pass -T "/share/pkgs/$2" "$HOSTED/$3"; }
rest() { docker exec "$name-curl" curl -sS -u admin:smoke-pass "$@"; }

echo "== build source packages with R CMD build"
docker exec -i "$name-r" sh -s <<'EOF'
set -e
mkdir -p /share/pkgs /share/mirror/src/contrib
mk() { # name version imports
  d=/tmp/src/$1-$2/$1; mkdir -p "$d/R"
  { printf 'Package: %s\nVersion: %s\nTitle: Smoke Package %s\n' "$1" "$2" "$1"
    printf 'Description: A package for the Command Center smoke test.\nAuthor: Smoke Test\nMaintainer: Smoke Test <smoke@example.com>\n'
    printf 'License: MIT + file LICENSE\nEncoding: UTF-8\n'
    [ -z "${3:-}" ] || printf 'Imports: %s\n' "$3"; } > "$d/DESCRIPTION"
  printf 'YEAR: 2026\nCOPYRIGHT HOLDER: Smoke Test\n' > "$d/LICENSE"
  printf 'export(version_of_%s)\n' "$1" > "$d/NAMESPACE"
  printf 'version_of_%s <- function() "%s"\n' "$1" "$2" > "$d/R/$1.R"
  (cd /share/pkgs && R CMD build --no-manual --no-build-vignettes "$d" >/dev/null)
}
mk foo 0.8; mk foo 1.0; mk foo 1.1; mk bar 0.9-2 foo; mk qux 1.0; mk legacy 1.0
mk R6 0.0.1 # an internal package named like a public one
cp /share/pkgs/legacy_1.0.tar.gz /share/mirror/src/contrib/
# A remote that publishes only PACKAGES.gz, as a Nexus hosted repository does.
Rscript -e 'tools::write_PACKAGES("/share/mirror/src/contrib", type = "source")'
rm /share/mirror/src/contrib/PACKAGES /share/mirror/src/contrib/PACKAGES.rds
EOF
docker run -d --name "$name-mirror" --network "$net" --network-alias mirror --memory 128m -v "$name-share:/share:ro" \
  python:3.12-slim python -m http.server 8000 --directory /share/mirror >/dev/null

echo "== upload"
for f in foo_1.0 foo_1.1 bar_0.9-2 R6_0.0.1; do put 201 "$f.tar.gz" src/contrib/; done
want 401 -T /share/pkgs/qux_1.0.tar.gz "$HOSTED/src/contrib/"                    # no credentials
put 409 foo_1.0.tar.gz src/contrib/                                              # allow_once
put 400 foo_1.0.tar.gz src/contrib/foo_1.2.tar.gz                                # the DESCRIPTION says 1.0
put 400 foo_1.0.tar.gz src/contrib/foo_1.0.zip                                   # not a source package's name
put 400 foo_1.0.tar.gz one/foo_1.0.tar.gz                                        # one directory level, as Nexus refuses
put 201 foo_0.8.tar.gz src/contrib/Archive/foo/                                  # CRAN's Archive, as CI that keeps old versions does
put 201 foo_1.0.tar.gz prod/src/contrib/                                         # a sub-repository below a prefix
put 201 foo_1.0.tar.gz bin/linux/jammy/contrib/4.6/                              # a binary layout of its own
want 204 -u ci:smoke-pass -F r.asset=@/share/pkgs/qux_1.0.tar.gz -F r.asset.pathId=src/contrib "$REST/components?repository=r-hosted"
# A real Windows binary, as CRAN built it, taken through the proxy.
r <<'EOF'
download.file(paste0(P, "/bin/windows/contrib/4.6/praise_1.0.0.zip"), "/share/pkgs/praise_1.0.0.zip", quiet = TRUE, mode = "wb")
EOF
put 201 praise_1.0.0.zip bin/windows/contrib/4.6/
put 400 foo_1.0.tar.gz bin/windows/contrib/4.6/foo_1.0.tar.gz                    # a binary directory's extension is .zip

echo "== R: hosted"
r <<'EOF'
all <- available.packages(repos = H, filters = list())
check("every version is listed", sort(paste(all[, "Package"], all[, "Version"])), sort(c("R6 0.0.1", "bar 0.9-2", "foo 1.0", "foo 1.1", "qux 1.0")))
check("R picks the newest", available.packages(repos = H)["foo", "Version"], "1.1")
lib <- newlib()
install.packages("bar", repos = H, lib = lib, quiet = TRUE)
check("bar and its dependency from hosted", c(ver("bar", lib), ver("foo", lib)), c("0.9.2", "1.1"))
lib <- newlib()
install.packages("foo", repos = paste0(H, "/prod"), lib = lib, quiet = TRUE)
check("a repository below a prefix", ver("foo", lib), "1.0")
own <- available.packages(contriburl = paste0(H, "/bin/linux/jammy/contrib/4.6"), filters = list())
check("a directory of its own has its own index", paste(own[, "Package"], own[, "Version"]), "foo 1.0")
win <- available.packages(contriburl = contrib.url(H, type = "win.binary"), filters = list(), fields = "Built")
check("a Windows binary is listed with Built, as CRAN lists it", startsWith(win["praise", "Built"], "R 4.6"), TRUE)
got <- download.packages("praise", destdir = tempdir(), contriburl = contrib.url(H, type = "win.binary"), type = "win.binary", quiet = TRUE)
check("and downloads", basename(got[1, 2]), "praise_1.0.0.zip")
EOF

echo "== R: proxy of CRAN"
r <<'EOF'
install.packages(c("remotes", "renv"), repos = P, lib = tools_lib, quiet = TRUE)
cat("remotes", as.character(packageVersion("remotes")), "renv", as.character(packageVersion("renv")), "\n")
lib <- newlib()
remotes::install_version("R6", "2.5.0", repos = P, lib = lib, quiet = TRUE, upgrade = "never")
check("an archived version (Meta/archive.rds, src/contrib/Archive)", ver("R6", lib), "2.5.0")
Sys.setenv(R_CRAN_WEB = P)
db <- tools::CRAN_package_db()
check("tools::CRAN_package_db through the proxy", nrow(db) > 1000, TRUE)
EOF

echo "== R: group"
r <<'EOF'
all <- available.packages(repos = G, filters = list())
check("the public R6 is not listed", all[all[, "Package"] == "R6", "Version"], "0.0.1")
check("public packages are", "praise" %in% rownames(all), TRUE)
lib <- newlib()
install.packages(c("R6", "bar", "praise"), repos = G, lib = lib, quiet = TRUE)
check("the internal R6 wins, a dependency and a public package come along",
      c(ver("R6", lib), ver("bar", lib), ver("foo", lib), ver("praise", lib)), c("0.0.1", "0.9.2", "1.1", "1.0.0"))
# Windows and macOS clients read a binary directory too, and take a newer binary over a source package.
win <- available.packages(contriburl = contrib.url(G, type = "win.binary"), filters = list())
check("the binary index is CRAN's", nrow(win) > 1000 && "praise" %in% rownames(win), TRUE)
check("without the binary of the internal name", "R6" %in% rownames(win), FALSE)
# A group over a remote that has only PACKAGES.gz.
M <- paste0(BASE, "r-migrate")
check("a member with only PACKAGES.gz is listed", available.packages(repos = M)["legacy", "Version"], "1.0")
lib <- newlib()
install.packages("legacy", repos = M, lib = lib, quiet = TRUE)
check("and installs", ver("legacy", lib), "1.0")
EOF

echo "== renv: restore a lockfile through the group"
r <<'EOF'
options(renv.consent = TRUE)
Sys.setenv(RENV_PATHS_ROOT = "/tmp/renv-root", RENV_CONFIG_SANDBOX_ENABLED = "FALSE")
proj <- tempfile("proj", "/tmp"); dir.create(proj); setwd(proj)
rv <- paste(R.version$major, R.version$minor, sep = ".")
writeLines(sprintf('{"R":{"Version":"%s","Repositories":[{"Name":"CC","URL":"%s"}]},"Packages":{
  "foo":{"Package":"foo","Version":"0.8","Source":"Repository","Repository":"CC"},
  "praise":{"Package":"praise","Version":"1.0.0","Source":"Repository","Repository":"CC"}}}', rv, G), "renv.lock")
lib <- newlib()
renv::restore(lockfile = "renv.lock", library = lib, prompt = FALSE, clean = FALSE)
check("renv finds an old version in Archive and a public one", c(ver("foo", lib), ver("praise", lib)), c("0.8", "1.0.0"))
EOF

echo "== pak: through the group"
r <<'EOF'
# pak's own binary build: CRAN's source package needs compilers and libcurl headers.
pak_lib <- newlib()
install.packages("pak", repos = sprintf("https://r-lib.github.io/p/pak/stable/%s/%s/%s", .Platform$pkgType, R.Version()$os, R.Version()$arch), lib = pak_lib, quiet = TRUE)
.libPaths(c(pak_lib, .libPaths()))
cat("pak", as.character(packageVersion("pak")), "\n")
Sys.setenv(R_USER_CACHE_DIR = "/tmp/pak-cache", PKG_USE_BIOCONDUCTOR = "false")
options(repos = c(CRAN = G), pkg.use_bioconductor = FALSE, pkg.show_progress = FALSE, pak.no_extra_messages = TRUE)
lib <- newlib()
pak::pkg_install(c("bar", "R6", "praise"), lib = lib, ask = FALSE, upgrade = FALSE)
check("pak (PACKAGES.gz): the newest dependency, the internal R6, a public package",
      c(ver("bar", lib), ver("foo", lib), ver("R6", lib), ver("praise", lib)), c("0.9.2", "1.1", "0.0.1", "1.0.0"))
EOF

echo "== REST: components and delete"
components="$(rest "$REST/components?repository=r-hosted")"
for g in src/contrib src/contrib/Archive/foo prod/src/contrib bin/linux/jammy/contrib/4.6; do
  case "$components" in *"\"group\":\"$g\""*) ;; *) echo "FAIL: no component of group $g: $components"; exit 1;; esac
done
found="$(rest "$REST/search?repository=r-hosted&group=prod/src/contrib&name=foo")"
id="$(printf '%s' "$found" | sed -E 's/.*"id":"([^"]*)","name":"foo".*/\1/')"
[ -n "$id" ] && [ "$id" != "$found" ] || { echo "FAIL: no component id in $found"; exit 1; }
want 204 -u admin:smoke-pass -X DELETE "$REST/components/$id"
r <<'EOF'
check("the deleted package left the index", nrow(available.packages(repos = paste0(H, "/prod"), filters = list())), 0)
check("the others stayed", "foo" %in% rownames(available.packages(repos = H)), TRUE)
EOF

echo "== repository.rebuild-metadata after the index files were lost"
docker exec "$name-pg" psql -q -U postgres -d cc -c "DELETE FROM assets WHERE repo = 'r-hosted' AND path LIKE 'src/contrib/PACKAGES%'"
r <<'EOF'
check("the lost index reads as empty", nrow(available.packages(repos = H, filters = list())), 0)
EOF
task="$(rest -X POST -H 'Content-Type: application/json' "$REST/tasks" \
  -d '{"type":"repository.rebuild-metadata","name":"rebuild-r","enabled":true,"alertEmail":"","notificationCondition":"FAILURE","frequency":{"schedule":"manual"},"properties":{"repository":"r-hosted"}}')"
tid="$(printf '%s' "$task" | sed -E 's/.*"id":"([^"]*)".*/\1/')"
want 204 -u admin:smoke-pass -X POST "$REST/tasks/$tid/run"
result=""
for i in $(seq 1 30); do
  state="$(rest "$REST/tasks/$tid")"
  case "$state" in *'"lastRunResult":"OK"'*) result=ok; break;; *'"lastRunResult":"FAILED"'*) result=failed; break;; esac
  sleep 1
done
[ "$result" = ok ] || { echo "FAIL: the rebuild task: $state"; exit 1; }
r <<'EOF'
check("the task rebuilt the index", available.packages(repos = H)["foo", "Version"], "1.1")
EOF

echo "== what the clients asked for"
docker logs "$name" > "$work/server.log" 2>&1
asked() {
  grep -q "\"method\":\"$1\",\"path\":\"/repository/$2\",\"status\":$3," "$work/server.log" ||
    { echo "FAIL: the client made no $1 $2 answered $3"; exit 1; }
}
asked GET r-hosted/src/contrib/PACKAGES.rds 200                  # R reads the .rds first
asked GET r-all/src/contrib/PACKAGES.rds 200
asked GET r-all/src/contrib/PACKAGES.gz 200                      # pak reads the .gz
asked GET r-all/bin/windows/contrib/4.6/PACKAGES.rds 200
asked GET r-migrate/src/contrib/PACKAGES.rds 200
asked GET r-proxy/src/contrib/Meta/archive.rds 200               # remotes::install_version
asked GET r-proxy/src/contrib/Archive/R6/R6_2.5.0.tar.gz 200
asked GET r-proxy/web/packages/packages.rds 200                  # tools::CRAN_package_db
asked GET r-all/src/contrib/Archive/foo/foo_0.8.tar.gz 200       # renv::restore
asked GET r-all/src/contrib/R6_0.0.1.tar.gz 200

echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "R SMOKE OK"
