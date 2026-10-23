# Run a go command inside the golang:1.26 container (no local Go install needed).
# Usage: .\scripts\go.ps1 test ./...
$root = Split-Path -Parent $PSScriptRoot
docker run --rm -v "${root}:/src" -w /src `
  -v commandcenter-gocache:/root/.cache/go-build `
  -v commandcenter-gomod:/go/pkg/mod `
  -e CGO_ENABLED=0 `
  golang:1.26 go @args
exit $LASTEXITCODE
