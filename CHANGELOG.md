# Changelog

All notable changes to Command Center are documented in this file. Each entry says what changed
for the people who run it and what, if anything, they have to do.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Upgrade notes

- Back up before you upgrade. Downgrades are not supported: an older version can misbehave
  silently on data a newer one has written, so the way back is the backup taken before the upgrade,
  restored together with the old image ("업그레이드" in the operations guide).
- After upgrading, run `repository.rebuild-metadata` once for each Maven hosted repository (a task
  with the schedule `manual`, then "Run now"). It removes the versions, `latest`/`release` values
  and foreign `groupId`/`artifactId` that older builds took from uploaded `maven-metadata.xml`
  files; without it they stay until the next upload to the same `groupId:artifactId` (QA L-11).
- Review npm and PyPI groups that relied on mixing hosted and proxy versions of the same name. Such
  a group now answers only from the first member that has the name, so versions, dist-tags and
  files that only a later member has disappear from it, and an offline member makes it answer 503
  for names that no member ahead of it has (see Changed).
- Look for the new startup warnings `local user cannot sign in with a password` and `the stored
  baseURL is no longer accepted`, and fix what they name (see Changed).
- Health checks that call `npm ping` on a repository without anonymous read now get 401, because
  ping needs read access. Point them at `GET /service/rest/v1/status`, which answers 200 without
  credentials.
- If browsers reach the web UI over plain HTTP through a reverse proxy that rewrites `Host`
  (nginx's default), make `baseURL` exactly the address they open (scheme, host and port, without a
  default port such as `:80`), or the web UI's writes are refused with 403 (see Changed).

### Added

- A production deployment section in the operations guide (`docs/guide.md`): TLS through a reverse
  proxy, storage for `meta.db` (local or block storage, never NFS or SMB), a single instance,
  resource sizing, graceful shutdown, consistent and online backups, and why downgrades are unsafe.
- Reference Kubernetes manifests in `deploy/kubernetes/`. Liveness is a TCP connect, so a full
  connection table does not restart the pod; startup and readiness ask
  `GET /service/rest/v1/status`.
- `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, this changelog, and issue and pull request templates.
- Continuous integration (`.github/workflows/ci.yml`) on every push to `main` and every pull
  request: gofmt, `go vet`, `go test` (also with the race detector), govulncheck, a syntax check of
  the web UI scripts, and an image build for linux/amd64 and linux/arm64. It also fails when
  `go-version` differs from the Dockerfile's `golang` tag, or when `THIRD_PARTY_LICENSES.txt` does
  not match the linked modules and Go version (`scripts/third-party-licenses.sh --check`), so a
  Dependabot update cannot skip either.
- A release pipeline (`.github/workflows/release.yml`). A `v*` tag runs CI again, then publishes
  `ghcr.io/sungjinseo/commandcenter` for linux/amd64 and linux/arm64 with an SBOM and provenance,
  and a GitHub Release with linux tarballs (binary and license files) and `SHA256SUMS`. The
  floating image tags `<major>.<minor>`, `<major>` and `latest`, and the GitHub "Latest" release,
  move only to the highest stable release in their range: v1.2.5 after v1.3.0 gets `1.2.5` and
  `1.2` and leaves `1` and `latest` on 1.3.0. Prereleases get only their own tag and do not count.
- Dependabot: weekly updates for Go modules, the Docker base images and GitHub Actions.
- `COMMANDCENTER_VERSION` sets the version a compose build reports (`commandcenter version`, web
  UI); without it the version is `dev`.

### Changed

- **npm and PyPI groups answer from one member and fail closed** (QA N-1, N-11). The first member
  in order that has the package answers alone: npm packuments, version documents, dist-tags and
  tarballs, PyPI project pages and files. Later members' versions, tags and files are not mixed in,
  and their direct URLs return 404 through the group. A member ahead of the owner that is offline
  makes the group answer 503 `group member "<name>" is offline`, and one that fails with anything
  but "not found" makes it answer 502 or that member's 5xx, also when no member has the name. Names
  that only a later member has are still served from it. Maven, Go and Cargo groups still skip
  offline members. Merged npm packuments stored by older builds are no longer read. A tarball or
  file request asks the members for the file first: the first member that has it answers (a proxy
  from its cache, also while its upstream is down or after the upstream removed the package), and
  a member that has the name but not the file ends the search with 404, so lockfile installs
  (`npm ci`) need no packuments.
- **A group whose members are all offline answers reads with 503** `repository is offline` in
  every format, instead of 404 (QA N-27).
- **The server owns GA-level `maven-metadata.xml`** (QA L-11). Versions, `latest`, `release`,
  `lastUpdated`, `groupId` and `artifactId` are rebuilt from the stored artifacts on every artifact
  upload or delete. An uploaded document still gets 201, but only its `<plugins>` are kept; a GA
  document whose `groupId`/`artifactId` name the GA of its path has that GA's document rebuilt
  from the stored artifacts, so the last step of `mvn deploy` repairs a rebuild that failed after
  an artifact upload. Uploaded metadata is capped at 1 MiB (413), and at most two uploads are
  processed at once (503 after a 30 s wait); an upload holding a slot must send its body within
  10 s, also when it keeps sending (408 `maven-metadata.xml not received within 10s`).
- **Request bodies have deadlines before authentication, and connections are capped** (QA N-5,
  N-8). Every request body, also on requests answered 401, 403, 404 or 503 and on `/-/api/`, is cut
  when no data arrives for 60 s, with 408 instead of 500. When the server answers without reading a
  body to its end, it waits at most 5 s for the rest, then closes the connection. At most 1024
  connections are served at once; when all are taken, the one that has been idle between requests
  or has not finished its first request's headers the longest (at least 1 s) is closed for the new
  one, and only while none qualifies does a new connection wait in the listen backlog.
- **Bodies from callers who are not signed in must arrive within 5 s** (QA N-5): the web UI login
  and `npm login` bodies are cut 5 s after the request headers however steadily they trickle in,
  and the connection is closed. Uploads always need a sign-in and keep the 60 s idle rule.
- **Request headers are limited to 16 KiB**; larger ones get 431 Request Header Fields Too Large.
- **Proxy egress policy.** A proxy repository connects only to public addresses and to the address
  classes its own `remote` resolves to (an internal remote may also reach other private hosts). The
  check runs on every resolved address at connect time, which covers redirects, index-supplied URLs
  and DNS rebinding, and at most 5 redirects are followed. A refused fetch answers 502 (or serves
  the stale copy) and logs `address refused by the remote egress policy`. Through `HTTP(S)_PROXY`
  only IP-literal destinations are checked. A host that spells an IPv4 address other than in
  dotted-decimal form (`10.1`, `167772161`, `0x0a000001`, `012.0.0.1`, `10.0.0.1.`) is always
  refused, also through `HTTP(S)_PROXY` and for internal remotes, because resolvers and forward
  proxies read it as an address the check did not see. NAT64 addresses (`64:ff9b::/96`,
  `64:ff9b:1::/48`) are classified by the IPv4 address in their last 32 bits. When the DNS lookup
  of the remote's host fails, what its last successful lookup in the past 10 minutes found is used,
  so an internal remote keeps working through a DNS blip instead of counting as public. A read of a
  remote body that waits 60 s for data aborts the fetch; time spent writing to the blob store does
  not count.
- **`baseURL` must be an `http(s)` URL without user info, query or fragment** (QA N-24), in
  `config.yaml` and in the settings API (400). A value stored by an older build keeps working and
  logs a warning at every start; error messages no longer repeat the value.
- **Cross-origin browser writes are refused with 403** on every route, `/repository/` and the web
  UI's `/-/api/` included, using Go's `CrossOriginProtection`: a request other than GET, HEAD or
  OPTIONS from a page of another origin (by `Sec-Fetch-Site`, or else by `Origin` against `Host`),
  unless its `Origin` is the origin of `baseURL`. Command-line clients send neither header and are
  unaffected.
- **A missing repository answers like an unreadable one** to callers who could not read a
  repository of that name: 401 with a challenge, or 403, with the same body. Callers who could read
  it still get 404. npm `ping` and `audit` need read access, so an anonymous `ping` without anonymous
  read answers 401. A denial to a caller who cannot read the repository has a plain-text body in
  every format; npm and Cargo used their own JSON.
- **The session cookie is `__Host-cc_session` under an `https` `baseURL`** (Secure, `Path=/`, no
  `Domain`); `http` keeps `cc_session`. Only the current scheme's name signs in, so switching
  `baseURL` between `http` and `https` signs web UI users out. Among several cookies of that name,
  unknown ids are skipped and live sessions of two different users are refused; signing in again
  ends the sessions of all of them.
- **Session eviction starts with the signing-in user's own sessions.** At the 1000-session cap,
  expired sessions go first, then the signing-in user's own least recently used session; another
  user's least recently used session is ended only when the signing-in user holds none. The
  per-user cap of 20 ends the least recently used session instead of the oldest.
- **A `passwordHash` that is not a bcrypt hash counts as no password** (QA L-2), such as the
  placeholder in `config.example.yaml`. That user cannot sign in with a password, the user API shows
  `hasPassword: false`, lockout protection does not count such an administrator (deleting,
  disabling or demoting the last real one is 409), and every start logs a warning for each such
  active user.
- **`commandcenter user reset-password` works next to a running server** (QA N-2): the server
  reloads users, roles and settings every 5 s and ends the sessions of users whose password changed
  or who were disabled or removed. Use `docker compose exec -T commandcenter commandcenter user
  reset-password …`; stopping the server is no longer needed.
- `docker-compose.yml` waits 60 s before it kills the container (`stop_grace_period`), so the 30 s
  request drain and the final writes finish. Give `docker run` (`--stop-timeout 60`) and Kubernetes
  (`terminationGracePeriodSeconds: 60`) the same.
- The Docker image pins its base images by digest and is cross-compiled for linux/amd64 and
  linux/arm64.
- The README's "Honest status" section no longer claims that every High and Medium QA finding is
  fixed. Known limitations are listed in the operations guide, and changes in this file.

### Fixed

- npm and PyPI groups served a later member's same-named public package while an earlier hosted
  member was offline (QA N-1), PyPI groups also while it failed.
- npm groups mixed dist-tags that only a later member had, such as `beta`, into an internal package
  (QA N-11).
- npm publish accepted `3.0.0+a` next to `3.0.0`, which npm treats as the same version, so installs
  failed. New versions with build metadata are rejected with 400 (QA N-12).
- `npm unpublish` answered 405 with a message pointing to a web UI and an admin route that do not
  exist for npm. It now says to use `npm deprecate` (QA H-2, N-10).
- Clients that sent headers and then stalled their bodies, even without credentials, could fill the
  128 MiB container with idle connections (QA N-5).
- A body that stalled past the idle timeout answered 500 and logged an ERROR (QA N-8).
- A group whose members were all offline answered 404, which clients such as Maven cache (QA N-27).
- Uploading a GA `maven-metadata.xml` could add versions without artifacts, a `<release>` or
  another artifact's IDs, and so steer version ranges such as `[1.0,)` (QA L-11).
- `baseURL` accepted user info, a query or a fragment, so a credential in it reached every client in
  download URLs, and the config error repeated the value (QA N-24).
- A placeholder `passwordHash` counted as a password: the last administrator who could really sign
  in could be deleted, disabled or demoted, and nothing said that the placeholder user could never
  sign in (QA L-2).
- The Maven `settings.xml` the web UI shows for proxy and group repositories had no profile that
  enables snapshots for `central`, so SNAPSHOT dependencies failed to resolve. The snippet and the
  guide now include it (QA M-17).
- `user reset-password` next to a running server had no effect until a restart, and the user's
  sessions survived (QA N-2).
- A proxy download whose remote stalled mid-body held one of the 32 fetch slots for up to 30
  minutes.
- Expired negative-cache rows were never deleted. They are now pruned at start and then daily.
- Temp files of writes cut off by SIGKILL, OOM or a stop timeout stayed in each blob store's `tmp/`.
  Files untouched for an hour are now removed at start and by every compact run.
- Concurrent `maven-metadata.xml` uploads of up to 10 MiB each could exhaust memory.
- LDAP: the 10 s back-off after a failed re-check did not hold when the directory hung, because it
  ran from the start of the attempt; it now runs from its end. A client that disconnects no longer
  starts it, and a request waiting behind another's re-check of the same user ends when its client
  disconnects.
- `commandcenter hash-password` accepted passwords that the web UI and `user reset-password`
  refuse. It now applies the same rules (at least 8 characters, at most 72 bytes).
- LDAP users saw none of their own API tokens on the token page. `/-/api/me` now returns `owner`
  (`ldap:<id>` for them), which the page matches.
- `scripts/third-party-licenses.sh` wrote a `THIRD_PARTY_LICENSES.txt` without any module licenses,
  and reported success, wherever root in its container does not own the checkout (GitHub runners,
  Linux with rootful Docker). It now works there, fails when `go list` fails, and runs in the
  Dockerfile's pinned golang image instead of the floating `golang:1.26`.

### Security

- Server-side request forgery: an upstream redirect or a file URL in an upstream index could lead a
  proxy repository to internal addresses, such as cloud metadata at `169.254.169.254`. See the
  egress policy under Changed.
- Dependency confusion: npm and PyPI groups could serve a same-named public package in place of an
  internal one (QA N-1, N-11).
- Denial of service: stalled or slowly trickled request bodies, even without credentials (QA N-5),
  and unbounded growth of the proxy negative cache in `meta.db` through requests for missing paths.
- Denial of service: with Go's default 1 MiB header limit, a connection still sending its headers
  could use about 1.5 MiB, so a few dozen such connections could exhaust the 128 MiB container.
  Headers are now limited to 16 KiB.
- Cross-site request forgery: a page on another site could make a browser send writes, such as
  uploads, with Basic credentials the browser had cached for this server (see Changed).
- Repository names could be probed: a hidden repository answered 401 or 403 where a missing one
  answered 404, npm `ping` and `audit` answered without read access, and npm and Cargo denials used
  their own JSON body (see Changed). Two exceptions remain, listed under "읽기 권한" in the
  operations guide.
- Session cookies: the server read the first cookie of its name, which another host under the same
  parent domain can plant ahead of the real one, so with an account of its own that host could
  sign a victim's browser in as itself. At the 1000-session cap, each account could also end up to
  20 other users' sessions just by signing in repeatedly (see Changed).
- A user allowed to deploy to a Maven hosted repository could steer version-range resolution with
  an uploaded `maven-metadata.xml` (QA L-11).
- A credential in `baseURL` was sent to every client (QA N-24).
- A password reset with `user reset-password` did not reach a running server, so the old password
  and the user's sessions kept working until a restart (QA N-2). Response times no longer reveal
  accounts whose stored hash is not a bcrypt hash.

[Unreleased]: https://github.com/sungjinseo/commandcenter/commits/main
