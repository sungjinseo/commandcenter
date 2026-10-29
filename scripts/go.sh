#!/usr/bin/env bash
# Run a go command inside the golang:1.26 container (no local Go install needed).
# Usage: scripts/go.sh test ./...
# For "test" it also starts a PostgreSQL container (commandcenter-test-pg, on the
# docker network commandcenter-test) unless COMMANDCENTER_TEST_POSTGRES_URL is set:
# the tests need PostgreSQL. The container is left running for the next run;
# remove it with: docker rm -f commandcenter-test-pg
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && (pwd -W 2>/dev/null || pwd))"
extra=()
if [ "${1:-}" = test ]; then
  if [ -n "${COMMANDCENTER_TEST_POSTGRES_URL:-}" ]; then
    extra=(-e COMMANDCENTER_TEST_POSTGRES_URL --network host)
  else
    net=commandcenter-test pg=commandcenter-test-pg pw=commandcenter-test
    docker network inspect "$net" >/dev/null 2>&1 || docker network create "$net" >/dev/null
    if [ -z "$(docker ps -q --filter "name=^${pg}\$")" ]; then
      docker rm -f "$pg" >/dev/null 2>&1 || true
      docker run -d --name "$pg" --network "$net" -e POSTGRES_PASSWORD="$pw" postgres:17-alpine >/dev/null
    fi
    for _ in $(seq 60); do
      docker exec "$pg" pg_isready -U postgres >/dev/null 2>&1 && break
      sleep 1
    done
    extra=(--network "$net" -e COMMANDCENTER_TEST_POSTGRES_URL="postgres://postgres:${pw}@${pg}:5432/postgres?sslmode=disable")
  fi
fi
MSYS_NO_PATHCONV=1 exec docker run --rm \
  -v "$root:/src" -w /src \
  -v commandcenter-gocache:/root/.cache/go-build \
  -v commandcenter-gomod:/go/pkg/mod \
  -e CGO_ENABLED=0 \
  ${extra[@]+"${extra[@]}"} \
  golang:1.26 go "$@"
