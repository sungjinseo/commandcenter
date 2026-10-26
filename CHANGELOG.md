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

### Added

- A production deployment section in the operations guide (`docs/guide.md`): TLS through a reverse
  proxy, storage for `meta.db` (local or block storage, never NFS or SMB), a single instance,
  resource sizing, graceful shutdown, consistent and online backups, and why downgrades are unsafe.
- Reference Kubernetes manifests in `deploy/kubernetes/`.
- `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, this changelog, and issue and pull request templates.
- Continuous integration (`.github/workflows/ci.yml`) on every push to `main` and every pull
  request: gofmt, `go vet`, `go test` (also with the race detector), govulncheck, a syntax check of
  the web UI scripts, and an image build for linux/amd64 and linux/arm64.
- A release pipeline (`.github/workflows/release.yml`). A `v*` tag runs CI again, then publishes
  `ghcr.io/sungjinseo/commandcenter` for linux/amd64 and linux/arm64 with an SBOM and provenance,
  and a GitHub Release with linux tarballs (binary and license files) and `SHA256SUMS`.
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
  offline members. Merged npm packuments stored by older builds are no longer read.
- **A group whose members are all offline answers reads with 503** `repository is offline` in
  every format, instead of 404 (QA N-27).
- **The server owns GA-level `maven-metadata.xml`** (QA L-11). Versions, `latest`, `release`,
  `lastUpdated`, `groupId` and `artifactId` are rebuilt from the stored artifacts on every artifact
  upload or delete. An uploaded document still gets 201, but only its `<plugins>` are kept.
  Uploaded metadata is capped at 1 MiB (413), and at most two uploads are processed at once (503
  after a 30 s wait).
- **Request bodies have deadlines before authentication, and connections are capped** (QA N-5,
  N-8). Every request body, also on requests answered 401, 403, 404 or 503 and on `/-/api/`, is cut
  when no data arrives for 60 s, with 408 instead of 500. When the server answers without reading a
  body to its end, it waits at most 5 s for the rest, then closes the connection. At most 1024
  connections are served at once; more wait in the listen backlog.
- **Proxy egress policy.** A proxy repository connects only to public addresses and to the address
  classes its own `remote` resolves to (an internal remote may also reach other private hosts). The
  check runs on every resolved address at connect time, which covers redirects, index-supplied URLs
  and DNS rebinding, and at most 5 redirects are followed. A refused fetch answers 502 (or serves
  the stale copy) and logs `address refused by the remote egress policy`. Through `HTTP(S)_PROXY`
  only IP-literal destinations are checked. A remote body that sends nothing for 60 s aborts the
  fetch.
- **`baseURL` must be an `http(s)` URL without user info, query or fragment** (QA N-24), in
  `config.yaml` and in the settings API (400). A value stored by an older build keeps working and
  logs a warning at every start; error messages no longer repeat the value.
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

### Security

- Server-side request forgery: an upstream redirect or a file URL in an upstream index could lead a
  proxy repository to internal addresses, such as cloud metadata at `169.254.169.254`. See the
  egress policy under Changed.
- Dependency confusion: npm and PyPI groups could serve a same-named public package in place of an
  internal one (QA N-1, N-11).
- Denial of service: stalled request bodies, even without credentials (QA N-5), and unbounded
  growth of the proxy negative cache in `meta.db` through requests for missing paths.
- A user allowed to deploy to a Maven hosted repository could steer version-range resolution with
  an uploaded `maven-metadata.xml` (QA L-11).
- A credential in `baseURL` was sent to every client (QA N-24).
- A password reset with `user reset-password` did not reach a running server, so the old password
  and the user's sessions kept working until a restart (QA N-2). Response times no longer reveal
  accounts whose stored hash is not a bcrypt hash.

[Unreleased]: https://github.com/sungjinseo/commandcenter/commits/main
