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
- To encrypt the stored SMTP password, LDAP bind password and proxy `remoteAuth` credentials, give
  the server a key: 32 random bytes in base64 (`head -c 32 /dev/urandom | base64`) in
  `COMMANDCENTER_SECRET_KEY`, or in a file named by the new `secretKeyFile` key. Back the key up
  apart from `meta.db`: once the secrets are encrypted the server does not start without it, and
  versions before this one cannot use them ("저장된 비밀값 암호화" in the operations guide).
- Uploads are now checked against what is inside them: npm tarballs, PyPI wheels and sdists, Maven
  POMs, Go `go.mod` files, and the `Cargo.toml` and dependencies of crates (see Changed and Fixed).
  Automation that uploads placeholder or hand-made files can now get 400; files already stored are
  not re-checked.
- Build tools and scripts that sign in with a password get `429 Too Many Requests` after five
  failures of that name in a row; give CI an API token. Scripts that create a role with a `repo:`
  privilege before its repository, a blob store with a relative path, or a user or role whose id
  differs from an existing one only in case now get 400 or 409 (see Changed and Security).
- LDAP: a login name that is not printable ASCII, or that has leading, trailing or repeated spaces,
  is now refused like a wrong password (see Security). If your `userFilter` finds users by a
  non-ASCII value such as a Korean `cn`, they can no longer sign in with it; make the filter match
  an ASCII attribute such as `uid`, `sAMAccountName` or `mail`. Local users are not affected. The
  LDAP connection test reports such a name as a failing `name` step.
- A proxy's `remoteAuth` credentials no longer follow a redirect to another scheme, port or
  subdomain (a same-host move from `http` on port 80 to `https` on port 443 keeps them), where such
  a remote typically answers 401 (`remote status 401` in the log): make the
  remote redirect within its own `https` origin, or point `remote` at the final `https` address. A
  proxy whose `remote` host is not ASCII must be changed to its punycode form (`xn--…`), or all its
  fetches are refused (see Changed and Security).

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
- `npm unpublish pkg@version` and `npm unpublish pkg --force` delete from a hosted repository, or
  through a group's `writeMember`, with the delete privilege (QA H-2, N-10). Dist-tags of removed
  versions go, `latest` moves to the newest remaining release, and an unpublish that would delete a
  version published, or published again, after npm read the package is refused with 409 instead:
  the PUT checks the document npm sends back, and every unpublish request checks the versions it
  would delete against the `_rev` that hosted package documents now carry (the publish time of
  their newest version, in milliseconds). Versions removed meanwhile and dist-tag or deprecate
  edits do not refuse it, so concurrent unpublishes of different versions all succeed. A document
  written by an older build has no `_rev`; npm then sends `undefined`, which, like any rev that is
  not a number, is not checked, until the package next changes. A new version is stamped at least
  1 ms after the package's newest stored one, so the rule holds within one millisecond.
- Go hosted repositories accept `DELETE` of a version's `.mod`, `.info` or `.zip`. It removes the
  whole version, also one that has only a `.mod`, and needs the delete privilege (QA M-11).
- `DELETE /-/api/repositories/{name}/components?group=&name=&version=` deletes one component
  version of a hosted repository in any format with the delete privilege, so PyPI and Cargo
  versions, which their clients cannot delete, can be removed. Cleanup policies still skip Cargo
  hosted repositories.
- The web UI offers "Delete component" in npm, PyPI, Go and Cargo hosted repositories, and the
  server deletes the whole version; Maven still deletes file by file. The repository API reports
  `canDelete`.
- "Revoke all tokens" on a user's page (`DELETE /-/api/users/<id>/tokens`) revokes every API token
  of that user, including those of the LDAP user with the same id, and reports how many (QA N-16).
  Password changes and resets still leave tokens valid; the reset message now says so.
- Paging for the component search (QA L-23): `GET /-/api/search/components` returns a
  `continuationToken` when more than 100 components match; send it back with the same criteria for
  the next 100, in repository, group, name and version order. The Search screen has a "Show the
  next 100" button, and `truncated` still means the response does not hold every match.
- Optional encryption at rest for stored secrets (QA N-29): with a key in
  `COMMANDCENTER_SECRET_KEY` or in the file the new `secretKeyFile` key names, the SMTP password,
  the LDAP bind password and proxy `remoteAuth` passwords and bearer tokens are stored with
  AES-256-GCM, and plain-text ones are encrypted on the next start. Without a key they stay plain
  text and every start logs the warning `no secret key is configured`. A user name, password or
  query in a proxy's `remote` URL is not encrypted: with a key, every start logs a warning that
  names those proxies (`the remote URL of these proxies holds a user name, password or
  query`); move user names and passwords to `remoteAuth`.

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
  one, and only while none qualifies does a new connection wait in the listen backlog. On shutdown a
  connection waiting for a slot is closed, so the 30 s drain ends on time also while every slot is
  busy.
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
  proxies read it as an address the check did not see. A host name that is not ASCII is refused the
  same way, because Go maps it to ASCII only when it connects, which can turn fullwidth digits and
  dots into such an address; write internationalized names in punycode (`xn--…`). Creating or
  updating a proxy whose `remote` host is not ASCII is refused (400), and so is a first-start import
  of one; a stored one still loads, but its fetches are refused until its `remote` is changed. NAT64
  addresses (`64:ff9b::/96`, `64:ff9b:1::/48`) are classified by the IPv4 address in their last 32
  bits. When the DNS lookup of the remote's host fails, what its last successful lookup in the past
  10 minutes found is used, so an internal remote keeps working through a DNS blip instead of
  counting as public. A read of a remote body that waits 60 s for data aborts the fetch; time spent
  writing to the blob store does not count.
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
- **New npm package names follow npm's rules for new packages** (QA L-15): the first publish of a
  name with upper case, or of `node_modules` or `favicon.ico`, is refused with 400. Packages
  already stored, including legacy upper-case names, still accept new versions, and such names stay
  readable through proxies and groups.
- **PyPI checks what is inside an upload** (QA M-9): a wheel must be a zip with one `*.dist-info`
  directory whose `METADATA` names the upload's project and version, and an sdist (`.tar.gz` or
  `.zip`) must keep a matching `PKG-INFO` in the release's `<name>-<version>/` directory. Anything
  else, such as a text file or another project's metadata, is a 400, as are zips with over 10,000
  entries or a central directory over 2 MiB, and `.tar.gz` sdists without `PKG-INFO` in their first
  512 MiB uncompressed.
- **PyPI accepts every PEP 440 version spelling and stores it normalized** (QA N-7): `1.0-1`,
  `01.2`, `2.0a`, `1.0post2`, `1.0.dev` and `1.0RC2` upload as `1.0.post1`, `1.2`, `2.0a0`,
  `1.0.post2`, `1.0.dev0` and `1.0rc2`. A leading `v` is still refused, now as an invalid version
  instead of "does not belong to project".
- **PyPI project pages link files with host-relative URLs** (QA L-5):
  `/repository/<repo>/packages/…` behind the path of `baseURL`, in the HTML and PEP 691 JSON pages,
  so pip downloads from the host it used for the index and keeps sending its credentials. For PyPI
  only the path of `baseURL` matters now.
- **Go `.mod` files and the `go.mod` in module zips must parse** (QA L-22): balanced blocks, `//`
  comments only, quoted strings that end on their line, exactly one `module` directive, at most one
  well-formed `go` directive, and only directives Go 1.26 knows unless the `go` directive names a
  newer Go. Anything else is 400; the arguments of other directives are not checked.
- **A Go module zip uploaded after its `.mod` must carry the same `go.mod`** (QA L-22), byte for
  byte; a zip without `go.mod` matches only the one-line `module <path>` stub. Otherwise the zip
  gets 409. Under `writePolicy: allow` the zip's `go.mod` replaces the `.mod` instead, which needs
  the edit privilege (403 without it).
- **Maven hosted repositories check uploaded POMs** (QA N-6): a `.pom` without a classifier must be
  well-formed XML of at most 1 MiB (413 beyond) with one `<project>` root. Its `groupId` (or the
  parent's) and `artifactId`, with `${...}` references replaced from its own `<properties>`, and
  its `version` (or the parent's) must match its path; anything else gets 400 and stores nothing. A
  reference left in `groupId` or `artifactId`, such as a `${scala.binary.version}` the parent sets,
  stands for one or more characters other than `/` and `:`, and the rest must match. A version that
  holds a `${...}` reference is not compared, so a CI-friendly `${revision}` set with
  `mvn -Drevision=… deploy` is accepted without flatten-maven-plugin. As in Maven, bytes that are
  not UTF-8 in a POM that declares no other encoding are read as U+FFFD instead of refusing the POM.
- **Maven proxies fetch and cache only Maven files** (QA L-12): artifacts, signatures,
  `maven-metadata.xml`, `.meta/prefixes.txt`, `archetype-catalog.xml`, sbt plugin files in sbt's
  legacy layout (`…/sbt-assembly_2.12_1.0/2.1.1/sbt-assembly-2.1.1.jar`) and their checksums. Other
  paths, such as directories, answer 404 through proxies and groups without asking the remote; a
  `text/html` answer for a file not named `.html` is taken as not found and negative-cached, or, for
  a file already cached, as a failed check that serves the cached copy; and directory listings
  cached by older builds are no longer served but stay in Browse until the proxy is re-created.
- **Cargo proxy and group index responses carry an `ETag`** (QA L-21) and answer a matching
  `If-None-Match` with 304, like hosted ones, so cargo stops downloading them again on every
  resolve. No Cargo index response sends `Last-Modified`.
- **A version refuses to start on a `meta.db` written by a newer version** (QA N-30): the database
  now records its schema version (SQLite `user_version`), and the error names both versions and
  points to restoring the pre-upgrade backup. Versions before this one do not check.
- **Garbage collection of the default blob store keeps every blob any repository references** (QA
  N-30), including blobs that only a repository on a named store references, which a version
  without named stores may have written there.
- User and role ids that differ from an existing one only in case are refused with 409 `already
  exists as "<id>"`, and `admin` is a reserved role id in any case (QA N-18). Ids already stored
  are kept; a first-start import of a `config.yaml` with such ids fails.
- User names, e-mail addresses and role descriptions refuse control characters such as CR and LF, a
  user's e-mail must be one plain address, and the SMTP host refuses spaces and control characters
  even while e-mail is off (QA N-17). Values stored by older versions are only checked when they
  change.
- A password made only of spaces is refused everywhere (web UI, `hash-password`, `user
  reset-password`), and the self-service change refuses the current password as the new one (QA
  N-21).
- A role privilege on `repo:<name>` needs an existing repository (400 `unknown repository`);
  targets the stored role already has may stay. A role cannot name the same target twice (QA N-25,
  L-9).
- Blob store paths must be absolute: a relative path is refused instead of silently becoming
  `<dataDir>/blobstores/<name>`, and a store outside the data directory carries a `warning` in the
  API and on its page (QA N-22, N-23).
- `remote` and `remoteAuth` are refused on hosted and group repositories, and `versionPolicy` on
  groups (QA L-9). Stored definitions that have them load with those unused values dropped and a
  startup warning. The Maven group form has no version policy field, and web UI input limits match
  the server (repository, cleanup policy and blob store names 64, task names 100, LDAP timeout
  1-60 s).
- The web UI's delete buttons follow the delete privilege (`canDelete`) instead of the add
  privilege, also for Maven.
- Deletes (`npm unpublish`, Go `DELETE`, the components API) are refused with 400 in repositories
  with `writePolicy: deny`, as Maven `DELETE` is.
- Hosted npm package documents are sent with `Cache-Control: no-cache`, so npm does not offer an
  unpublished version from its cache.
- Notification e-mail carries its own `Message-ID` (random, at the From address's domain) next to
  `Date` (QA N-28).
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
- `npm unpublish` answered 405 for every caller, with a message pointing to a web UI and an admin
  route that did not exist for npm. It now deletes (see Added) (QA H-2, N-10).
- npm publish stored any attachment as the tarball: an empty file, one that was not gzip or not
  tar, one without `package.json`, or one packed for another name or version (QA L-15). The server
  now reads the stored tarball and answers 400 unless exactly one regular `package.json` of at most
  1 MiB sits in its top directory (`package/` from `npm pack`; any name is accepted) with the
  published name and version, and it reads at most 1 GiB uncompressed for all new tarballs of one
  publish document together, only after the version, name and checksum checks pass, and stops when
  the client goes away.
- npm publish accepted a new version whose manifest `name` differed from the package name; it now
  answers 400 (QA L-15).
- Scoped npm names whose scope or package part starts with `_`, such as `@acme/_internal`, were
  refused with 400 on every route, so they could not be published, nor installed from npmjs through
  a proxy or group. npm accepts them, and now so does Command Center; a scope or package part that
  starts with `.` is still refused.
- PyPI duplicate detection missed wheels that only respelled their compressed tag set (`py3.py3`)
  or build number (`01`), and a `.zip` sdist next to the `.tar.gz` of a release. They now get 400
  `File already exists` naming the existing file, a release has one sdist, and malformed wheel file
  names are a 400 (QA M-8).
- PyPI uploads ignored `blake2_256_digest`; a mismatch is now a 400 like `sha256_digest` and
  `md5_digest` (QA L-18).
- An invalid `Requires-Python` in a wheel's `METADATA` or an sdist's `PKG-INFO` was silently
  dropped; it is now a 400 like an invalid form value (QA N-9).
- Go module zips with a corrupt entry (wrong CRC-32) or forged sizes were accepted, and the 500 MiB
  uncompressed limit trusted the sizes in the headers. Every entry is now read back, and the limit
  counts the bytes really read (QA N-3).
- Go module zips with directory entries, as `zip -r` writes them (`module@version/`, `sub/`), were
  rejected with a contradictory message (QA N-13).
- A Go module zip could hold a root `LICENSE` over 16 MiB, and a `go.mod` over 16 MiB when the
  `.mod` had been uploaded first; the go command refuses both (QA N-14).
- Go module paths ending in `/v0`, `/v1`, `/v01` or `/v1.2`, which the go command rejects, were
  accepted (QA L-22).
- The `.info` of a Go version without its `.zip` answered 200 while `list` and `@latest` hid the
  version. It is now 404, and a group asks its next member (QA N-15).
- Cargo publish accepted a `.crate` whose `Cargo.toml` declares another `[package]` name or version
  than the publish metadata, or leaves either to workspace inheritance (`cargo publish` always
  resolves it). It is now a 400; the manifest is read up to 1 MiB (QA N-4).
- Cargo publish copied dependency fields into the index unchecked, and new crates could take the
  names of Windows devices (`con`, `nul`, `com1`, …) or standard library crates (`std`, `core`,
  `alloc`, `proc_macro`, `test`). Each dependency's name, `version_req`, `kind`, `target` and
  `registry` are now checked, and such new crates get 400; crates that already exist under such a
  name keep publishing (QA L-19).
- Mixed-case crate names showed in lower case in the web UI (search results and `cargo add`
  snippets), the packages API and upload events. They keep their published spelling now, while
  lookups stay case-insensitive (QA L-20).
- A Maven hosted repository accepted any content as a `.pom`, such as plain text or a POM naming
  other coordinates, while it rejected a broken `maven-metadata.xml` (QA N-6).
- Maven proxies cached upstream HTML directory listings under the directory's name, served them as
  files forever and listed them in Browse as both a file and a folder (QA L-12).
- Rebuilding `maven-metadata.xml`, which every Maven artifact upload and delete does, loaded the
  database record of every file of the `groupId:artifactId` (and of the SNAPSHOT version) at once:
  over 90 MiB for 100,000 files, which a SNAPSHOT that CI has deployed for years without a cleanup
  policy can reach, so two such uploads at once could exceed the 128 MiB container. The rebuild now
  reads one file path at a time.
- Maven uploads accepted paths that are not valid UTF-8 (only a hand-made request such as `caf%E9`
  sends one), names that JSON responses such as the web UI's listings cannot carry unchanged. They
  now get 400 `invalid path`; files stored earlier stay readable.
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
- The user list merged an LDAP user into the local user with the same id, so the local user showed
  the directory user's roles. They are now separate rows, an LDAP user is merged only into its
  role-override record, and an LDAP user's account page shows the owner key `ldap:<id>` (QA N-19).
- Deleting the disabled role-override record of an LDAP user let that user sign in again. The delete
  is now refused with 409 until the record is set active (QA N-20).
- `scripts/third-party-licenses.sh` wrote a `THIRD_PARTY_LICENSES.txt` without any module licenses,
  and reported success, wherever root in its container does not own the checkout (GitHub runners,
  Linux with rootful Docker). It now works there, fails when `go list` fails, and runs in the
  Dockerfile's pinned golang image instead of the floating `golang:1.26`.

### Security

- Server-side request forgery: an upstream redirect or a file URL in an upstream index could lead a
  proxy repository to internal addresses, such as cloud metadata at `169.254.169.254`. See the
  egress policy under Changed.
- Proxy `remoteAuth` credentials followed a redirect from the remote to the same host name on
  another port or scheme, or to a subdomain, so a remote redirecting from `https` to `http` sent
  them in clear text. Redirects now keep them only on the remote's own scheme, host and port (an
  omitted port counts as the default), or on a same-host move from `http` on port 80 to `https` on
  port 443; other redirects are still followed, without credentials.
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
- Password guessing: failed sign-ins were not limited (QA L-1). After 5 failed password checks of
  one login name with no success in between (Basic auth, web UI login, `npm login`, the current
  password of a password change; local and LDAP), the name is now refused with 429 and
  `Retry-After` for 1 minute after its last failure, doubling per further failure up to 15 minutes;
  a success or an hour without failures forgets them. Names are counted case-insensitively, and a
  name that is not printable ASCII, or has leading, trailing or repeated spaces, fails like a wrong
  password, unchecked and uncounted, since a directory would match such spellings to the same
  account. An address with 20 failures in 15 minutes gets one failed attempt per name. Checks of one
  name run one at a time, tokens and existing sessions are not affected, and a revoked token sent as
  a Basic password is not counted.
- A PyPI deployer could publish a text file or another project's wheel as a release, which broke
  `pip install` for that name, or shadow a release file by respelling its wheel tags or adding a
  second sdist (QA M-8, M-9).
- Denial of service: a crafted zip uploaded as a PyPI wheel or `.zip` sdist could make the server
  allocate far more memory than the zip's size. Go's `archive/zip` reader keeps reading central
  directory headers past the entry count and directory size the archive declares, and compares the
  count only modulo 65536, so a 28 MiB zip64 file that declared one entry made it build 655,361
  entries, and one such upload grew the heap by about 137 MiB, more than the default 128 MiB
  container. Every header is now counted before the zip is opened: a wheel or zip sdist with over
  10,000 entries or 2 MiB of central directory is refused with 400 whatever it declares, and Go
  module zips get the same count against their 50,000-entry and 4 MiB limits.
- Denial of service: Go module uploads were checked with no limit on how many run at once, and
  checking a zip at the 50,000-entry limit takes about 20 MiB of memory, so six parallel uploads of
  8 MB zips grew the heap by about 126 MiB. At most two uploads (`.mod`, `.zip` or `.info`) are now
  checked at once; the others wait with their body already stored.

[Unreleased]: https://github.com/sungjinseo/commandcenter/commits/main
