# Run a go command inside the golang:1.27 container (no local Go install needed).
# Usage: .\scripts\go.ps1 test ./...
# For "test" it also starts a PostgreSQL container (commandcenter-test-pg, on the
# docker network commandcenter-test) unless COMMANDCENTER_TEST_POSTGRES_URL is set.
$root = Split-Path -Parent $PSScriptRoot
$extra = @()
if ($args.Count -gt 0 -and $args[0] -eq "test") {
  if ($env:COMMANDCENTER_TEST_POSTGRES_URL) {
    $extra = @("-e", "COMMANDCENTER_TEST_POSTGRES_URL", "--network", "host")
  } else {
    $net = "commandcenter-test"; $pg = "commandcenter-test-pg"; $pw = "commandcenter-test"
    docker network inspect $net *> $null
    if ($LASTEXITCODE -ne 0) { docker network create $net > $null }
    if (-not (docker ps -q --filter "name=^$pg`$")) {
      docker rm -f $pg *> $null
      docker run -d --name $pg --network $net -e POSTGRES_PASSWORD=$pw postgres:17-alpine > $null
    }
    for ($i = 0; $i -lt 60; $i++) {
      docker exec $pg pg_isready -U postgres *> $null
      if ($LASTEXITCODE -eq 0) { break }
      Start-Sleep -Seconds 1
    }
    $extra = @("--network", $net, "-e", "COMMANDCENTER_TEST_POSTGRES_URL=postgres://postgres:${pw}@${pg}:5432/postgres?sslmode=disable")
  }
}
docker run --rm -v "${root}:/src" -w /src `
  -v commandcenter-gocache:/root/.cache/go-build `
  -v commandcenter-gomod:/go/pkg/mod `
  -e CGO_ENABLED=0 `
  @extra `
  golang:1.27 go @args
exit $LASTEXITCODE
