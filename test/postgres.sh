# Sourced by the smoke and load tests, from the repository root: the server's
# metadata database, a PostgreSQL container "$name-pg" on network "$net"
# (both set by the test). start_postgres runs it once the network exists and
# waits until it takes TCP connections; the test's cleanup removes it.
#   SMOKE_PG_IMAGE  PostgreSQL image (default postgres:17-alpine)
pg_url="postgres://postgres:smoke-pg@postgres:5432/cc?sslmode=disable"
start_postgres() {
  docker run -d --name "$name-pg" --network "$net" --network-alias postgres \
    -e POSTGRES_PASSWORD=smoke-pg -e POSTGRES_DB=cc "${SMOKE_PG_IMAGE:-postgres:17-alpine}" >/dev/null
  # TCP, not the socket: the image's init server listens on the socket only.
  for _ in $(seq 60); do
    docker exec "$name-pg" pg_isready -q -h 127.0.0.1 -U postgres -d cc 2>/dev/null && return
    sleep 1
  done
  echo "FAIL: PostgreSQL not ready after 60s" >&2
  docker logs --tail 30 "$name-pg" >&2 || true
  return 1
}
