# Contributing to Command Center

**SCVs wanted.** Bug reports, documentation fixes, bug fixes and support for new formats are all
welcome. Write issues and pull requests in Korean or English, whichever you prefer.

- **Security vulnerabilities** never go into a public issue or pull request. Follow
  [SECURITY.md](SECURITY.md).
- Everyone who takes part follows the [Code of Conduct](CODE_OF_CONDUCT.md).

## Before you start

- **Bugs**: open an issue with the bug report form. The `commandcenter version` output and how you
  deploy it (Docker or Kubernetes, reverse proxy, storage for `/data`) save a round trip.
- **Larger changes** (a new format or repository type, a change to `meta.db` or the blob layout, a
  new config key, a new dependency): open an issue first, so the design is agreed before you write
  the code.
- **Small fixes** (documentation, an obvious bug together with its test) can go straight to a pull
  request.

## Build and test

You need Docker and bash. The scripts run Go in the `golang:1.26` container, so there is no local Go
to install. In Windows PowerShell, `.\scripts\go.ps1` takes the same arguments as `scripts/go.sh`.

```bash
bash scripts/go.sh test ./...                                      # unit and integration tests
bash scripts/go.sh vet ./...
docker run --rm -v "$PWD":/src -w /src golang:1.26 gofmt -l .      # must print nothing
```

The unit tests must pass without network access. The end-to-end checks build the image and drive it
with the real clients (mvn, npm, pip, go, cargo) in containers. They need Docker and internet access,
because the proxy repositories fetch from Maven Central, npmjs, PyPI, proxy.golang.org and crates.io.

```bash
bash test/e2e.sh                  # every smoke test plus the 128 MiB load test, about 20 minutes
bash test/smoke/maven-smoke.sh    # one smoke test (also npm, pypi, go, cargo, ui, ldap, mail)
bash test/load/load.sh            # the memory load test on its own
```

Run the matching smoke test when you change a format, and the load test when you change how
requests, uploads or proxy downloads are handled.

## How the code is written

- **Standard library first.** The binary has four direct dependencies (SQLite, YAML,
  `golang.org/x/crypto`, `golang.org/x/sync`); even the LDAP client is built on the standard
  library. A new module needs a reason in the pull request, a row in [ATTRIBUTION.md](ATTRIBUTION.md)
  and a regenerated `THIRD_PARTY_LICENSES.txt` (`scripts/third-party-licenses.sh`). The build stays
  `CGO_ENABLED=0`.
- **128 MiB is the budget.** Stream request and response bodies; never read an upload, an archive or
  a remote response into memory whole. Everything that grows with input gets a limit: sizes, counts,
  list lengths, concurrency and time.
- **Every input is hostile**: uploads, archives, metadata documents, version strings, proxied
  responses, LDAP and SMTP replies. Validate at the boundary, fail closed, and answer a client error
  with a 4xx and a clear message (`repo.Errorf(status, ...)`).
- **Secrets stay secret.** Passwords and tokens never reach responses, logs or activity events; the
  API only says whether one is set. Log request paths and errors through `internal/logsafe`.
- **Logs** are `log/slog` JSON lines on stdout.
- **Comments say why**, not what, and exported identifiers have doc comments. Point to public issues
  (`#123`) rather than to documents outside the repository.
- **Tests** use the standard `testing` and `net/http/httptest` packages, without test frameworks.
  Every bug fix comes with a regression test that fails without the fix.
- **Web UI** (`internal/ui/static`): plain ES modules with no build step, framework or external URL;
  no `innerHTML` from data; every string in both languages in `i18n.js`.
  `internal/ui/static_test.go` enforces these rules and the size budget of the static files.
- **Formatting**: `gofmt` and `go vet` report nothing.

## Pull requests

- One topic per pull request. Keep refactoring apart from behaviour changes.
- Say what changed and why, link the issue, and say how you tested it; the template asks for each.
- Document user-visible behaviour in [docs/guide.md](docs/guide.md) (Korean) and, where it matters,
  in both READMEs, and add a line under "Unreleased" in [CHANGELOG.md](CHANGELOG.md).
- A maintainer reviews every change. Expect questions about memory use, limits and failure modes.
- Maintainers tag releases on `main` and add the commit that Dependabot's Go module and golang
  updates need before CI passes: see [RELEASING.md](RELEASING.md).

## License

Command Center is licensed under MIT OR Apache-2.0. Unless you explicitly state otherwise, any
contribution intentionally submitted for inclusion in Command Center by you, as defined in the
Apache-2.0 license, shall be dual licensed as above, without any additional terms or conditions.
