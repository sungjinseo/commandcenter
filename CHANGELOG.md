# Changelog

All notable changes to Command Center are documented in this file. Each entry says what changed
for the people who run it and what, if anything, they have to do.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Nexus component upload API: `POST /service/rest/v1/components?repository=` takes Nexus's multipart
  fields for maven2 (`groupId`, `artifactId`, `version`, `generate-pom`, `packaging`, `assetN`,
  `assetN.classifier`, `assetN.extension`), raw (`directory`, `assetN.filename`), apt, npm, pypi, nuget,
  rubygems, helm, yum (`directory`, `asset.filename`) and r (`asset.pathId`), plus conda, composer and
  terraform modules. Each file goes through the format's own upload (same privileges, write policy,
  validation, events and metadata); 204 on success, 400 validation lists as Nexus answers. Works with
  `curl -F`, Jenkins `nexusPublisher` and `nexus3-cli`.
- `GET /service/rest/v1/formats/upload-specs` and `/formats/{format}/upload-specs`: the fields each
  format's component upload takes.
- Nexus API: S3 blob stores (`POST /v1/blobstores/s3` 201, `GET`/`PUT /v1/blobstores/s3/{name}`, secret
  masked with Nexus's placeholder), `GET /v1/blobstores/file/{name}` and `DELETE /v1/blobstores/{name}`.
- Nexus API: `GET`/`PUT`/`DELETE /v1/email` and `POST /v1/email/verify` over the SMTP settings.
- Nexus metrics paths: `/service/rest/metrics/{ping,prometheus,data,threads}` (`name[]` filter) and
  301 redirects from `/service/metrics/*` (`healthcheck` to `/v1/status/check`).
- `/service/rest/beta/*`: Nexus's deprecated Beta paths served by their v1 twins.
- Repository bodies: format sections `docker`, `dockerProxy`, `yum` (`repodataDepth` = depth),
  `nugetProxy`, `npm`, `pypi`, `cargo`/`terraform`/`swift` (`requireAuthentication`), and
  `httpClient.authentication` of type `bearerToken`.
- System information: `system-environment`, `system-network` and `system-properties` sections.
- Nexus API search: `md5`/`sha1`/`sha256`/`sha512`, `prerelease`, `last_updated`/`lastBlobUpdated`, the
  `g:a:v[:ext[:classifier]]` form of `q`, space-separated alternatives and quotes in every value, `?` as a
  wildcard, full attribute names (`attributes.maven2.*`, `assets.attributes.*`), and the format a `maven.*`,
  `npm.*` or `docker.*` parameter implies.
- Nexus API `repository=` takes several names, `*`/`?` wildcards and groups (searched through their members,
  downloads through the group); `GET /v1/components` and `/v1/assets` on a group list its members' content.
- `DELETE /service/rest/v1/assets/{id}` for every format: a proxy's cached copy, a Maven file (metadata
  rebuilt), or a hosted file that is a whole component; 400 for part of a larger component.
- `GET /service/rest/v1/tasks?type=` filters by Nexus or Command Center task type id.
- Nexus API assets carry `"maven2": {groupId, artifactId, version, extension, classifier}` (`{}` for other
  formats), `blobUpdated` and `lastVerified`.
- Webhooks: payload format `nexus` (`payloadFormat`) sends Nexus Repository 3 webhooks: event names
  `audit`, `repository`, `component`, `asset`, `X-Nexus-Webhook-ID` `rm:<scope>:<name>`, Nexus bodies
  (timestamp, nodeId, initiator, action, component/asset/repository/audit) and the HMAC-SHA1 signature.
- Webhooks: nexus component and asset events come from the stored content of every format: hosted
  uploads, proxy caching, overwrites (UPDATED) and deletes.
- Webhooks: the Nexus event names work as aliases in commandcenter webhooks; `GET
  /-/api/webhooks/events` lists them as `nexusEvents`.
- Activity: events record the client IP and the node (`ip`, `node`); UI sign-in, sign-out and failed
  sign-in are recorded as `auth.login`, `auth.logout`, `auth.failed` (also webhook kinds).
- Nexus `POST /service/rest/v1/repositories/{name}/invalidate-cache` and `POST /-/api/repositories/{name}/
  invalidate-cache`: a proxy's cache goes stale (even with `contentMaxAge: -1`) and its negative cache is
  cleared; groups cascade to every proxy member. `POST .../rebuild-index`: a no-op (204), 400 for groups.
- Per-proxy `connection` settings (Nexus `httpClient.connection`): timeout, retries, User-Agent suffix,
  cookies. Proxy `negativeCacheEnabled` (Nexus `negativeCache.enabled`).
- Cleanup: a repository can have several cleanup policies (`cleanupPolicies`, Nexus `policyNames`); each
  applies on its own. A policy may have `pathPattern` as its only criterion, as in Nexus.
- Tasks: `cron:<Quartz expression>` schedules; weekly on several days; monthly on days 1-31 and `last`.
- Tasks: `notifyOn: success_failure` also mails finished runs (Nexus `notificationCondition: SUCCESS_FAILURE`).
- Tasks: run now while another task runs is queued (202 `queued: true`); the leader starts it after that run.
- Tasks: new databases get a nightly `cleanup-service` task (01:00); upgraded databases do not.
- Tasks: `repository.cleanup` limits `maxComponents`/`scanAssets`; config `taskRunTimeoutHours`.
- Tasks: `repository.purge-unused` deletes proxy components not downloaded for `lastUsed` days.
- Tasks: `olderThanDays` for `blobstore.delete-temp-files`; `resetProxyMetadata` (apt proxies) for rebuilds.
- Nexus API: task ids `repository.apt.rebuild.metadata`, `blobstore.metrics.reconcile`,
  `repository.purge-unused`; `(All Blob Stores)` and `blobsOlderThan` 0; 400 with a reason for
  publish-dotindex and other ids Command Center has no use for.

### Changed
- APT hosted `POST /` takes the package as the raw request body, as Nexus does (its documented
  `curl --data-binary` with a bare `multipart/form-data` type); a multipart form without a `file`
  field now uses its first file part.
- Nexus API `softQuota.limit` is in bytes, as Nexus's REST API carries it (it was read as MB).
- Repository values Command Center cannot honour (docker connector ports, v1, foreign-layer caching,
  NuGet V2, quarantine removal, PERMISSIVE yum at depth 1, yum signing) are refused with 400.
- Routing rule create answers 204; deleting a rule in use answers 400; deleting a blob store in use
  answers 400; Nexus errors carry `"id":"*"`; dates have milliseconds; `?userId=` is a prefix.
- Nexus API asset `path` and raw component `name` start with `/`, as Nexus 3.91 renders them; `checksum`
  lists only stored sums. Pull replication accepts both forms.
- Nexus API listings and search follow browse or read on the repository or a group containing it; a
  content-selector-only user lists and searches only the paths the selector matches.
- Nexus API `sort=version` orders by each format's version rules (Maven `1.0-beta` before `1.0`); an unknown
  `sort` or `direction`, a bad `prerelease` or `last_updated` answers 400.
- Activity keeps 10 000 uploads, deletes and failed sign-ins and, separately, 10 000 other events.
- `repo.delete` events name the repository's format/type; `repository.deleted` payloads carry them.
- A remote 4xx other than 401/402/407/429 for an uncached asset is a negative-cached 404, so groups move
  on past S3/CloudFront 403s (npm packuments and PyPI pages keep failing closed).
- Durations `"0"`/`"0s"` mean zero: max age 0 revalidates every request, negativeCacheTTL 0 records no
  misses. negativeCacheTTL `-1` never expires; stored `-1` is migrated to `0s` (schema 8). TTL, enabled
  switch or credential changes clear the negative cache.
- Routing rules written through the Nexus API match the whole path (stored as `^(?:m)$`).
- A blob store that fails to open no longer stops the node: its repositories answer 503 and status
  checks name it; it is retried every few seconds.
- Auto-block counts only the remote's own host and resets when remote access settings change.
- HTTP settings: timeouts up to 3600 s; nonProxyHosts takes Java-style `*` globs.
- Cleanup: age counts from when content was last written (ages, never-downloaded fallback, retain by date).
- Tasks: run now of another task no longer answers 409 while a task runs; it is queued instead.
- Nexus API: `blobstore.recalculate-size` tasks are listed as `blobstore.metrics.reconcile`.

### Fixed
- A Nexus API repository PUT no longer resets the anonymous-read override, a yum repository's depth,
  a terraform repository's GPG key or the content disposition when its body has no field for them
  (the anonymous-read reset could make a private repository readable).
- `PUT /v1/repositories/…/{name}` with another name in the body is refused instead of ignored.
- Nexus API continuation tokens stay valid after their item is deleted (cleanup scripts got 400).
- `maven.classifier=` (empty) selects files without a classifier, so `search/assets/download` serves the
  main jar instead of `-javadoc.jar`/`-sources.jar`.
- Checksum searches no longer match every component; a search naming a group no longer comes back empty.
- Raw search by `group=/dir` finds the directory's files.
- Webhook deliveries waiting to be sent were deleted once a webhook had 200 newer ones; up to 5 000
  pending deliveries per webhook are now kept beside the 200 most recent sent or failed ones.
- Nexus API: `metadataMaxAge`/`contentMaxAge`/`timeToLive` 0 and `negativeCache.enabled: false` were
  replaced by defaults; NTLM credentials were sent as Basic (now 400).
- Nexus API: repository bodies with several `cleanup.policyNames` no longer drop all but the first.

### Upgrade notes
- Terraform and scripts that send `softQuota.limit` in MB (the old reading) must multiply it by
  1048576; a GET now returns the stored bytes. Soft quotas never refuse writes. Bodies accepted
  silently before can now get 400 (docker connector ports, `v1Enabled:true`, `nugetVersion:V2`,
  `removeQuarantined:true`, ...): remove those values. Clients checking for 201 on routing-rule
  create, or 409 on deleting a rule or blob store in use, must change those checks.
- Scripts that compare the Nexus API asset `path` or raw component `name` must allow the leading `/`
  (ids are unchanged). Search/assets continuation tokens issued before the upgrade are rejected with 400:
  start the listing again. `DELETE /v1/assets/{id}` now deletes non-raw files it used to refuse.
  Browse-only users and users granted only a group now see those repositories' content in the Nexus API;
  selector-only users see less than before.
- Database schema 8, compat 7 (expand-only: 0.3.0 nodes keep running during a rolling upgrade). It adds
  `webhooks.payload_format` (existing webhooks become `commandcenter`, their payloads unchanged) and
  `events.ip`, `events.node`. During the rollout older nodes deliver nothing to `nexus` webhooks or to
  aliases they do not know. Webhooks subscribed to `*` now also receive `auth.*` events.
- Schema 8 also rewrites stored proxy `negativeCacheTTL: "-1"` to `"0s"`, keeping what it did ("no
  negative caching"); set `-1` again after the upgrade for never-expiring misses. `"0s"` in the native API
  and config.yaml now means zero rather than the default. Remote 403/400/405 for uncached assets are now a
  cached 404 instead of an uncached 502, so 502 rates in monitoring drop. A Terraform re-apply of a
  routing rule through the Nexus API changes it to whole-path matching. Saving a nonProxyHosts entry
  with a middle `*` is now refused.
- Schema 8 also adds `tasks.notify_on`, `queued_by`, `queued_at`. During the rollout an older leader
  leaves queued runs waiting until a new node leads, older nodes mail failures only, and an older node
  that saves a repository with several policies keeps only the first: don't edit such repositories until
  every node runs this version. No cleanup task is added on upgrade: create a `repository.cleanup` task if
  repositories with policies should be cleaned nightly. Cleanup ages now start at the last write, so age
  criteria delete the same or fewer components. Run now no longer refuses while another task runs:
  scripts that relied on the 409 now get 202 with `queued: true`.

## [0.3.0] - 2026-10-30

### Added
- High availability (active-active): any number of `serve` processes on one PostgreSQL
  database and one shared `dataDir` (ReadWriteMany: NFS, CephFS, EFS) serve every request together behind
  a load balancer, without session affinity. Blob store moves and blob store group member changes now
  reach every node: a node learns of a move from the database and a notification and routes the
  repository's writes to the target at once. `repository.change-blobstore` and
  `blobstore.group-remove-member` wait, before they copy, until every live node has applied the change
  and finished the writes it began before. Compaction first re-reads the stores, moves and repositories
  every node left.
- Helm chart `replicaCount`: `1` (default) renders the single-pod StatefulSet as before. Above 1 it
  renders a Deployment (rolling updates start a new pod first), one ReadWriteMany claim kept on uninstall
  (or `persistence.existingClaim`), a PodDisruptionBudget of one pod, and, while `affinity` is empty, a
  preferred anti-affinity on `kubernetes.io/hostname`. The chart refuses `replicaCount` above 1 without
  a ReadWriteMany volume (or `persistence.existingClaim`).
- `test/ha/ha-smoke.sh` (`test/e2e.sh --ha`): PostgreSQL and two nodes on one volume in Docker. It covers
  formats, sessions and Docker chunks across nodes, a task run once, webhooks delivered once, a `kill -9`
  under load and a rolling restart under load.
- Cluster-wide locks and cache invalidation (HA groundwork): every format's per-path,
  per-package and per-index locks (uploads, deletes, metadata rebuilds, repository import, Docker upload
  sessions) and repository, blob store, cleanup policy, routing rule, user, role, realm and settings
  changes are serialized across all nodes on the database (advisory locks). A node that changes
  repositories, routing rules, blob stores, webhooks or users/roles/settings notifies the others
  (LISTEN/NOTIFY), which re-read at once (the 5 s reload stays as the safety net), and a request naming a
  repository another node just created is served at once.
- UI sessions are stored in the database: they survive restarts, and every node accepts them. The
  per-name sign-in throttle is shared by all nodes, OIDC and SAML
  login state is single-use across nodes and SAML assertion replay is refused across nodes, OIDC, SAML,
  RUT and Crowd identities are shared for up to 7 days after the login, Docker and Conan tokens verify
  on every node, and the secret key is checked at start.
- Scheduling on several nodes (ahead of high availability):
  - Only the scheduler lease holder runs due tasks and prunes request statistics, the negative cache
    and events.
  - One task run at a time across all nodes (cluster lock `tasks.run`). The run shows as running on
    every node (`runningTask` in `GET /-/api/nodes`), can be stopped from any node, and is run again
    by the next leader when its node died.
  - Cleanups first ask the other nodes for their pending download counts.
  - Cleanup policy edits and `PUT /-/api/logging` reach every node.
  - Webhook deliveries are claimed by one node for 60 s and sent once (at least once after a crash).
- A node registry: `GET /-/api/nodes`, `commandcenter_node_info`, the `node` log attribute, a `Cluster`
  status check, and readiness on `/service/rest/v1/status/writable` that covers draining, schema
  compatibility and the lock and listener connections.
- The `rubygems` format: RubyGems repositories at `/repository/<repo>/` for Bundler and `gem` (the
  compact index `versions`, `names` and `info/<gem>` with the md5 `ETag`, `Repr-Digest` and byte
  ranges Bundler uses to fetch increments; the legacy `specs.4.8.gz`, `latest_specs.4.8.gz`,
  `prerelease_specs.4.8.gz`, `quick/Marshal.4.8/*.gemspec.rz` and `api/v1/dependencies`, written by
  a built-in Ruby Marshal 4.8 encoder; `gems/*.gem`). Hosted repositories take `gem push` (`POST
  api/v1/gems`, the API key being an API token sent bare in `Authorization`, or Basic auth), read
  the YAML `Gem::Specification` out of the `.gem`'s `metadata.gz`, derive every index from their
  rows and keep `versions` as an append-only asset updated in the push and delete transactions
  (`writePolicy` as for NuGet; `gem yank` = `DELETE api/v1/gems/yank` deletes the version). Proxies
  cache rubygems.org or another server as is (`metadataMaxAge` for the indexes, 256 MiB bound).
  Groups merge their members' `versions` and `names` (a gem an earlier member has hides later
  members' versions; cached 60 s) and answer `info`, gemspec and gem requests from the first member
  that has the gem; the Marshal specs indexes come from the first member. Cleanup policies (format
  `rubygems`), the web UI's component delete and metadata (summary, licenses, homepage,
  runtime/development dependencies) and `format:rubygems` role privileges cover rubygems
  repositories; a component is the gem at `<version>[-<platform>]`.
- The `composer` format: Composer v2 repositories at `/repository/<repo>/packages.json` for Composer
  2.x (per-package metadata under `p2/`, with `~dev.json` for branch versions, dist archives and
  `search.json`). Hosted repositories take `PUT packages/upload/<vendor>/<package>/<version>` with
  the package zip as the body, read `composer.json` out of it (root or first directory), normalize
  the version as Composer's VersionParser does and derive the metadata from the rows; `writePolicy`
  applies as for NuGet (409 on a re-upload under `allow_once`, overwriting under `allow` needs
  edit); `DELETE` on the same path removes the version. Proxies of repo.packagist.org, Satis or
  Private Packagist (`remote` = the repository root) cache `packages.json`, the package metadata
  (minified deltas expanded, `source` dropped, `dist.url` rewritten to the proxy) and search
  results, and fetch each archive from its upstream URL on first request, checking the `shasum`;
  groups answer from the first member that has the package, merge search results and upload to their
  `writeMember`. Cleanup policies (format `composer`), the web UI's component delete and metadata
  (`require` / `require-dev`), and `format:composer` role privileges cover composer repositories.
- The `conda` format: conda channels at `/repository/<repo>/` for conda, mamba and micromamba
  (`conda config --add channels <url>`, `conda install -c <url>`). Hosted channels take packages by
  `PUT <subdir>/<name>-<version>-<build>.tar.bz2` (or `.conda`), read `info/index.json` and
  `info/about.json` out of a `.tar.bz2` and regenerate `<subdir>/repodata.json`,
  `current_repodata.json` and `channeldata.json` in the transaction of every upload and delete;
  `writePolicy` applies as for Yum. A `.conda` package is listed with what its file name and hashes
  give only (no `depends`: its index is zstd-compressed, which the server does not read). No
  `repodata.json.bz2`/`.zst` are made; clients fall back to `repodata.json`. Proxies cache a remote
  channel (conda-forge, repo.anaconda.com) at its paths, streaming the large `repodata.json`, with
  the documents re-checked every `metadataMaxAge`; groups answer files from the first member that
  has them and merge the members' `repodata.json` and `channeldata.json` (hosted first, cached until
  a member's document changes). Components are `<subdir>/<name>@<version>-<build>`; cleanup
  policies, the component delete and `format:conda` privileges apply, and the metadata tab shows
  summary, license, home and depends/constrains.
- Webhooks (Nexus-style global and per-repository): admin API under `/-/api/webhooks`
  (create/update/delete, event-kind list, test ping, delivery history, redeliver). Every activity
  event is delivered as JSON with `X-Command-Center-Event`/`-Delivery` headers and, with a secret,
  HMAC-SHA256 (`X-Command-Center-Signature`) plus Nexus-compatible HMAC-SHA1
  (`X-Nexus-Webhook-Signature`); retries at 30 s/2 m/10 m/1 h persisted in the database and resumed
  after a restart. Secrets are sealed with the secret key. Database schema version 3 (tables
  `webhooks`, `webhook_deliveries`).
- Routing rules for proxy repositories (Nexus semantics): a named rule with mode `BLOCK` or `ALLOW`
  and RE2 matchers against the request path, connected with the repository's `routingRule`. A
  blocked path is refused before the cache (403 on the proxy, skipped as a group member, no remote
  request). Managed at `/-/api/routing-rules` (with `…/test`), imported once from `routingRules:` in
  config.yaml, and served at `/service/rest/v1/routing-rules` with the repository bodies'
  `routingRule`. Schema version 3.
- Content selectors (path-scoped privileges): a named CSEL expression (`format`, `path`,
  `coordinate.groupId`; `==`, `!=`, `=^`, `=~`; `and`/`or`/`not`) that a role privilege targets as
  `selector:<name>` with a `repos` scope; the action is granted only on matching paths (listings
  stay repository-level). Managed at `/-/api/content-selectors` (with `…/preview`), imported once
  from `contentSelectors:`, and served at `/service/rest/v1/security/content-selectors` plus
  `security/privileges/repository-content-selector`, which maps a Nexus privilege onto the role
  `priv:<name>`.
- The `conan` format: Conan remotes at `/repository/<repo>/` for Conan 2 (`conan remote add`, `conan remote login`,
  `install`, `upload`, `list`, `remove` through the v2 revisions API; `v1/ping` advertises
  `complex_search,checksum_deploy,revisions,matrix_params`) and the v1 API Conan 1.x uses without revisions
  (snapshots, `digest`, `download_urls`, `upload_urls`, package search; `remove_files` and `packages/delete` are
  not implemented). `conan remote login` sends the password or an API token as Basic to
  `v1/users/authenticate` and gets a one-hour signed token the server accepts as Bearer. Hosted repositories
  store files at their v2 paths (`v2/conans/<name>/<version>/<user>/<channel>/revisions/<rrev>/files/…` and the
  package paths under it), keep one row per recipe revision for search, and apply `writePolicy` per file of a
  revision (409 on different content under `allow_once`, overwriting under `allow` needs edit, `deny` is
  read-only); `checksum_deploy` answers 404 unless the file is already there. Proxies cache a Conan server
  (Conan Center, Artifactory): metadata revalidated after `metadataMaxAge`, files kept, `remoteAuth` exchanged
  for the remote's token; groups answer from the first member that has the recipe, merge search results and
  upload to their `writeMember`. Components are recipe revisions (`<version>#<rrev>`, group `user/channel`)
  with their binaries as assets; cleanup policies (format `conan`), the web UI's component delete and
  metadata (`conanfile.py` description, license, homepage, url and requires/tool_requires/build_requires/
  test_requires), and `format:conan` role privileges cover conan repositories.
- The `r` format: R (CRAN-style) repositories for `install.packages()`, remotes, pak and renv.
  A hosted repository takes packages with `PUT /repository/<repo>/<contrib dir>/<pkg>_<version>.tar.gz`
  (`.zip` under `bin/windows/contrib/<R.x>`, `.tgz` under `bin/macosx[/<build>]/contrib/<R.x>`), reads the
  package's `DESCRIPTION` (its `Package` and `Version` must be the file name's) and regenerates the
  directory's `PACKAGES`, `PACKAGES.gz` and `PACKAGES.rds` (an R serialization, format 2, that every R
  reads) in the same transaction as every upload and component delete; `writePolicy` applies as for Yum.
  A proxy passes a CRAN mirror or Posit Package Manager through unchanged (`PACKAGES*` and `Meta/`
  revalidated after `metadataMaxAge`, packages and `src/contrib/Archive/` kept); a group serves a file
  from the first member that has it and merges the members' `PACKAGES` of a contrib directory (hosted
  first, the first member's entry per package and version), deriving `PACKAGES.gz` and `PACKAGES.rds`
  from the merge. Components are one per contrib directory (the directory is the group), so cleanup
  policies (format `r`), the web UI's component delete and metadata (Title/Description, License, URL,
  BugReports, Depends/Imports/LinkingTo/Suggests) and `format:r` role privileges cover r repositories.
- The `gitlfs` format: a Git LFS server (Batch API with the basic transfer) as a hosted repository,
  for `lfs.url = <baseURL>/repository/<repo>/info/lfs` (or `…/repository/<repo>/`). `POST
  objects/batch` answers download hrefs for stored objects (per-object 404 otherwise) and upload +
  verify hrefs for missing ones; `PUT objects/<oid>` stores the object after checking its sha256
  (422 on mismatch) and is idempotent under every writePolicy but `deny`; `GET/HEAD objects/<oid>`
  serve it with ETag and Range; `POST objects/verify` checks the stored size. Download needs read,
  upload add; an unauthenticated batch is challenged with `LFS-Authenticate` (Basic password, API
  token as password, or Bearer token). Objects are components named by their oid (no version) for
  the web UI, component delete and cleanup policies (`gitlfs` or `all`). Object size is bounded by
  `maxUploadMiB`. The lock API answers 501 (git-lfs then skips locks); proxy and group gitlfs
  repositories are refused.
- Maintenance tasks in the style of Nexus Repository: `repository.verify-blobs` (checks that every
  asset's blob exists, with `checksum` re-reading it and comparing the SHA-256, bounded by `maxBytes`
  per run and resumed by the next; Failed when anything is missing or differs, nothing deleted),
  `maven.purge-snapshots` (removes old SNAPSHOT versions per GA by `minimumRetained`,
  `snapshotRetentionDays`, `lastDownloadedDays` and `removeIfReleased`/`gracePeriodDays`, with
  `preview`; metadata is regenerated and `delete` events recorded), `blobstore.recalculate-size`
  (reports blobs in each store against the asset rows) and `blobstore.delete-temp-files` (sweeps stale
  temp files and abandoned Docker upload sessions). `repository.rebuild-metadata` now rebuilds apt,
  yum, helm and conda hosted repositories too, and takes an empty `repo` (every repository) with an
  optional `format`. `GET /-/api/tasks` lists each type's parameters; the Nexus REST compat layer
  reports Nexus task type names.
- Prometheus metrics at `GET /-/metrics/prometheus` (text exposition): Go runtime and process gauges,
  per-repository request/error/bytes/latency/upstream/cache counters (process lifetime), repository and
  blob store sizes, task results and health checks. Admins only unless the new `metricsAnonymous`
  general setting is on.
- Thread dump at `GET /-/api/support/threads` (admin): every goroutine's stack as text, for a bug report.
- Support bundle at `GET /-/api/support/zip` (admin): a zip of version/runtime info, the effective
  settings (secrets redacted), repositories, blob stores, status checks, system information, tasks,
  webhooks, the last 500 activity events, the in-memory log, a thread dump, the Prometheus text and the
  24-hour request statistics; activity event `support.zip`.
- Runtime log level: `GET/PUT /-/api/logging` (admin) changes the root logger's level (debug, info,
  warn, error) at once, until the next start; activity event `logging.level`.
- Outbound HTTP settings in the general settings (`http`): a forward proxy with credentials and a
  non-proxy host list for all remote fetches, webhooks and OIDC, connect and response-header timeouts,
  retries of idempotent requests after network errors and a User-Agent suffix; applied without a
  restart, the `HTTP_PROXY`/`NO_PROXY` environment remains the fallback.
- SSL trust store: admin-added CA certificates (`/-/api/ssl/certificates`, retrieve a server's chain
  with `/-/api/ssl/retrieve`) trusted by remote fetches, webhooks, OIDC, LDAP and SMTP in addition to
  the system roots, with the Nexus REST compat endpoints `/service/rest/v1/security/ssl` and
  `/security/ssl/truststore`.
- The `p2` format: Eclipse update sites at `/repository/<repo>/` for Eclipse's Install New Software and
  Tycho (`<layout>p2</layout>`). Proxy and group only, as in Nexus (a hosted p2 repository is refused by
  validation). A proxy passes the site through and caches it (`p2.index`, `content.*`, `artifacts.*`,
  `site.xml` and the composite documents re-checked every `metadataMaxAge`; `features/`, `plugins/`,
  `binary/` for `contentMaxAge`) and rewrites the children of a composite repository
  (`compositeContent.*`, `compositeArtifacts.*`, `.jar` included) so they resolve through the proxy:
  children under the remote become relative paths, children elsewhere are mirrored under
  `.remote/<sha1 of the child URL>/` (table in `.remote/index.json`). A group is a composite of its
  online members (generated `p2.index`, `compositeContent.xml`, `compositeArtifacts.xml` under
  `baseURL`); other paths come from the first member that has them. Features, plugins and binaries are
  components (`plugins/<id>@<version>`) for cleanup policies (format `p2`); roles take `format:p2`; the
  metadata tab reads a cached plugin's MANIFEST.MF (Bundle-Name, Bundle-Vendor, Bundle-License,
  Require-Bundle).
- The `swift` format: Swift package registries (SE-0292) at `/repository/<repo>/` for SwiftPM 5.7+
  (`swift package-registry set/login/publish`): release lists, release metadata with the source-archive
  checksum and latest/successor/predecessor `Link`s, `Package.swift` with its `Package@swift-X.Y.swift`
  variants as alternate links, source archives with `Digest`, the `identifiers?url=` lookup and
  `POST login`, all with `Content-Version: 1` and problem+json errors. Hosted repositories take the
  multipart publish (archive, metadata, signature), validate scope, name and SemVer and the
  `archive-source` layout, and record metadata and the manifest's `.package(...)` dependencies; proxies
  rewrite release URLs and verify archives against the upstream checksum; groups answer from the first
  member that has the package. Components are `scope.name@version`; `format:swift` privileges and cleanup
  policies apply.
- The `cocoapods` format (proxy only, as in Nexus): a proxy of the CocoaPods trunk CDN
  (`https://cdn.cocoapods.org/`) for a Podfile `source '<url>/repository/<repo>/'`. The index files
  (`CocoaPods-version.yml`, `all_pods.txt`, `all_pods_versions_<a>_<b>_<c>.txt`,
  `deprecated_podspecs.txt`) are cached as metadata and
  `Specs/<a>/<b>/<c>/<Pod>/<version>/<Pod>.podspec.json` as content. A podspec's `source.http` is
  rewritten to `/repository/<repo>/pods/<Pod>/<version>/<sha1 of the URL>.<ext>`, which fetches the
  original archive on first request, checks it against the podspec's `sha256` and caches it beside the
  podspec; `source.git` passes through untouched (git is not proxied). Hosted and group repositories are
  refused. Components are `Pod@version`; cleanup policies take `format: cocoapods`, roles
  `format:cocoapods`, and the metadata tab shows summary/description, license, homepage, the source URL
  and dependencies.
- The `huggingface` format (proxy only, as in Nexus): a Hugging Face Hub proxy
  (`https://huggingface.co/`) for `HF_ENDPOINT=<url>/repository/<repo>` (huggingface_hub, transformers,
  datasets, `hf download`). `api/models|datasets|spaces/<repo id>/...` documents pass through, cached per
  query for `metadataMaxAge`; the rest of the API, such as `whoami-v2`, is 404.
  `[datasets/|spaces/]<repo id>/resolve/<revision>/<path>` follows the hub's redirect to its CDN and
  caches the file under its commit sha as immutable content; a branch or tag resolves to its commit
  through the revision document. Answers replay `X-Repo-Commit`, `ETag`/`X-Linked-Etag` (the content
  sha256, the hub's LFS etag), `X-Linked-Size` and `Content-Length`, with byte ranges, and a HEAD of an
  uncached LFS file is answered from the hub's headers without downloading. `remoteAuth.bearerToken`
  (gated and private models) is sent to the configured remote host only, never to the CDN, and a client's
  Authorization is not forwarded. Hosted and group are refused. Components are `<repo id>@<commit sha>`
  (group `datasets`/`spaces`); cleanup takes `format: huggingface`, roles `format:huggingface`, and the
  metadata tab shows pipeline_tag, library_name and license.
- The `terraform` format: Terraform/OpenTofu registries (modules.v1 and providers.v1) at
  `/repository/<repo>/`, with service discovery at the host root `/.well-known/terraform.json` (the
  general setting `terraformDefaultRepository`, else the first terraform repository by name) and per
  repository. Hosted repositories take module archives (tar.gz or zip with `*.tf` at the root or in one
  top-level directory, `PUT v1/modules/<ns>/<name>/<provider>/<version>` or
  `PUT modules/…/<version>.tgz`) and provider releases (platform zips, `SHA256SUMS`, its signature and an
  optional `terraform-registry-manifest.json`, under `PUT v1/providers/<ns>/<type>/<version>/…`), and
  list the repository's new `gpgPublicKey` (key id read from the OpenPGP key packet) in the download
  documents. `writePolicy` applies as elsewhere: allow_once accepts the same file again and refuses other
  content. Clients authenticate with `~/.terraformrc` API tokens (Bearer). Proxies cache
  registry.terraform.io: version lists as metadata, module http(s) archives and provider files fetched on
  demand under the proxy's own URLs (zips checked against `shasum`, `git::` sources passed through), and
  the last module source is kept for when the remote is down. Groups answer from the first member that
  has the module or provider. Components are `ns/name/provider@version` (modules) and `ns/type@version`
  (providers), with cleanup policies (format `terraform`), `format:terraform` privileges, the web UI's
  delete, and metadata (module README paragraph; provider protocols and platforms). Terraform fetches
  archives and zips with `~/.netrc` only and `SHA256SUMS` without credentials, so private providers need
  `anonymousRead` on their repository.
- npm audit: proxy repositories forward `npm audit` (`-/npm/v1/security/audits`, `audits/quick`,
  `advisories/bulk`) to their remote and pass the answer through uncached, with request and response
  bodies bounded at 16 MiB. Groups use their first proxy member. Hosted repositories answer an empty
  report (`{}` for bulk) instead of 501, so `npm install` no longer fails its audit step.
- Blob store soft quotas: `quota {type: spaceUsedBytes|spaceRemainingBytes, limitBytes}` on create or
  through the new `PUT /-/api/blobstores/{name}` (which also takes a group's `members` and `fillPolicy`
  in the same call; a group keeps its quota when its members change). A health check
  `Blob store quota: <name>` per store, plus `quota`/`quotaViolated` in the blob store API and a
  `blobstore.quota` event on each flip; uploads are never refused. Nexus compat: `softQuota` on
  `GET /service/rest/v1/blobstores`, `POST /blobstores/file` and `PUT /blobstores/file/{name}`, and
  `GET /service/rest/v1/blobstores/{name}/quota-status`.
- An OpenAPI 3.0 document of every `/-/api` route and the Nexus compat API at `GET /-/api/openapi.json`,
  also served at `GET /service/rest/swagger.json`, public. A test fails the build when a registered route
  has no description.
- General setting `defaultRoles`: roles granted to every signed-in user (local, LDAP, OIDC, Crowd, RUT, SAML, and
  their API tokens), validated against existing roles (`admin` refused). A role it names cannot be
  deleted, and changes record `settings.defaultRoles`.
- The `rutauth` realm (Nexus Rut Auth) authenticates the user named in a header (default `REMOTE_USER`)
  on requests that come directly from `trustedProxies` CIDRs (RemoteAddr only, never forwarded addresses)
  and carry no `Authorization`. Roles come from an optional group header with mappings, realm default
  roles and `source: "rutauth"` override rows, and API tokens are supported. Configured at
  `GET/PUT /-/api/realms/rutauth`.
- Per-repository `strictContentTypeValidation` (hosted): raw and Maven uploads whose content contradicts
  their extension or declared Content-Type are refused with 400 (npm, PyPI, NuGet and Helm already check
  every package). `contentDisposition` (`inline`/`attachment`) for raw and gitlfs downloads. Both are
  also in the Nexus compat API (`storage.strictContentTypeValidation`, `raw.contentDisposition`); hosted
  raw and Maven repositories created through it with `strictContentTypeValidation: true`, as the Nexus
  examples and the Terraform provider send, now validate.
- Proxy auto-block: after three consecutive remote failures (connection errors, timeouts or 5xx; not 404,
  401, 403 or 429), a proxy stops asking its remote for 30 s, doubling up to 5 min. It serves stale cache
  or fails fast with 502, and resets on the first answer. A manual `blocked` setting never asks the
  remote. `autoBlock`/`blocked` and the live `remoteStatus {blocked, auto, until, lastError}` (signed-in
  users only) are in the repository API, and the Nexus compat API maps them to
  `httpClient.blocked/autoBlock`.
- User tokens (Sonatype Nexus Pro's "user token", free): each user (local, LDAP, OIDC, Crowd) may hold
  one name code (8 characters, does not reveal the user name) and pass code (44 characters, shown once,
  only its SHA-256 hash stored) that replace the user name and password as Basic credentials wherever
  Basic is accepted (Maven, npm `_auth` and `npm login`, pip, `docker login`, the Nexus API). The general
  setting `userTokens` turns them on (`enabled`), sets an expiry counted from creation (`expirationDays`,
  0 = never, up to 3650; changes apply to existing tokens) and can make `/repository/*` and `/v2/*`
  refuse user name and password for everyone (`required`; tokens and API tokens only, the web UI login is
  unaffected). Self service at `GET/POST/DELETE /-/api/me/usertoken`; admins list and reset at
  `/-/api/usertokens` (reset all needs `?confirm=true`); Nexus
  `GET/PUT /service/rest/v1/security/user-tokens` (`protectContent`), `DELETE` (invalidate all),
  `.../user-tokens/tokens` and `.../security/users/{userId}/{realm}/user-token`. A failed pass code
  counts in the sign-in throttle, a disabled or deleted user's token stops at once, and the maintenance
  task `security.purge-expired-user-tokens` deletes expired ones. Events `usertoken.create`,
  `usertoken.reset`, `usertoken.delete` and `settings.userTokens`.
- An Atlassian Crowd realm (`crowd`): password sign-in (web UI, Basic, `npm login`) checked with Crowd's
  REST API as a configured application, roles from the user's nested Crowd groups through group mappings
  and default roles plus `source: crowd` override rows, successes cached for `cacheMinutes` (keyed by a
  password hash, never the password), sessions and tokens of `crowd:<id>` re-checked after that, and
  every Crowd user denied while Crowd is unreachable. Configure it at `GET/PUT /-/api/realms/crowd` (the
  application password is sealed with the secret key and never returned), check it with
  `POST /-/api/realms/crowd/test`, and watch the new "Crowd reachable" status check. Calls go through the
  outbound proxy settings and the SSL trust store.
- A SAML 2.0 realm (`saml`): SP-initiated web single sign-on (AuthnRequest over HTTP-Redirect, Response
  posted to `<baseURL>/-/sso/saml/acs`) against an identity provider described by pasted or fetched
  metadata (`idpMetadataXML` or `idpMetadataURL`, https only, re-fetched daily through the outbound proxy
  settings and the SSL trust store). Responses are verified with built-in exclusive canonicalization and
  XML Signature code against the certificates in the IdP metadata only (RSA-SHA256 and ECDSA-SHA256;
  RSA-SHA1 only with `allowSHA1Signatures`), reading only the bytes a signature covers: exactly one
  assertion, no DTD, destination, recipient, InResponseTo, issuer, audience and validity windows
  (`clockSkewSeconds`) checked, and assertion IDs remembered against replay. Encrypted assertions
  (RSA-OAEP with AES-CBC or AES-GCM) are decrypted with a service provider key generated on the first
  save (sealed with the secret key, never returned), which can also sign AuthnRequests
  (`signAuthnRequests`). The user id comes from the NameID or an attribute, roles from a groups attribute
  through group mappings and default roles plus `source: saml` override rows; sessions and API tokens of
  `saml:<id>` last until a restart or a SAML configuration change, as for OIDC. SP metadata at
  `GET /-/sso/saml/metadata` (and `GET /-/api/realms/saml/sp-metadata` for admins), configuration at
  `GET/PUT /-/api/realms/saml`. IdP-initiated logins are refused unless `allowIdpInitiated` is set;
  single logout is not implemented.
- Blob store groups (`type: "group"` in `POST /-/api/blobstores`): a store whose blobs live in member
  file or S3 stores, written round robin or to the first writable member (`fillPolicy`), with read-only
  members being drained, reads from the member that last served a blob and then every member, compaction
  and verify-blobs over every member, and per-member sizes in recalculate-size.
  `PUT /-/api/blobstores/{name}` edits members and fill policy (and the quota, in the same call),
  `POST .../convert-to-group` turns a store (the default one too) into a group keeping its repositories
  and files, the `blobstore.group-remove-member` task copies a member's blobs to the others (SHA-256
  checked) before dropping it, and the Nexus API serves `/service/rest/v1/blobstores/group`.
- S3 failover buckets (`s3.failover`): reads fall back to replica buckets in other regions while the
  primary endpoint does not answer or answers 5xx; writes stay on the primary.
  `GET /-/api/blobstores/{name}/health` shows which directory, bucket or member answers.
- S3 server-side encryption in Nexus's shape (`s3.encryption`: `s3ManagedEncryption`, or
  `kmsManagedEncryption` with an optional `kmsKeyId` sent as
  `x-amz-server-side-encryption-aws-kms-key-id`).
- Cleanup policies keep the newest N versions of every component (`retain`, sorted by `version` with each
  format's own ordering — Maven, PEP 440, RPM, Debian, Gem, Composer, conda, SemVer — or by `date`),
  never select components matching `exclude.namePattern`/`exclude.versionPattern`, and can match asset
  paths with `pathPattern`; policies take `notes`.
- `POST /-/api/cleanup/preview` previews an unsaved policy on a repository with the cleanup task's own
  evaluation over the whole repository: up to 1000 components with the reason each is selected, or every
  candidate as CSV with `?format=csv`.
- Nexus-compatible cleanup policy API: `GET/POST /service/rest/v1/cleanup-policies` and
  `GET/PUT/DELETE /service/rest/v1/cleanup-policies/{name}` with Nexus's field names.
- Nexus-compatible task API: `POST /service/rest/v1/tasks`, `GET/PUT/DELETE /service/rest/v1/tasks/{id}`
  and `POST .../{id}/run|stop`, mapping Nexus task types, properties and frequencies (cron included) to
  Command Center tasks and schedules.
- `GET /service/rest/atlas/system-information` reports status, configuration, runtime, time and blob
  store free space in Nexus's sections, without secrets.
- Component tags: named tags with free-form attributes, associated with components selected by the search
  API's parameters (`/-/api/tags`, Nexus `/service/rest/v1/tags` and `tags/associate`); searches filter
  by `tag=` and components list their tags.
- Staging: move components between hosted repositories of one format by tag or search, with metadata
  rebuilt in both, tags carried over and per-component results, and staging delete
  (`/service/rest/v1/staging/move/{destination}`, `/service/rest/v1/staging/delete`); maven, npm, raw,
  pypi, nuget, helm and yum.
- Change a repository's blob store in the background (`POST /-/api/repositories/{name}/blobstore`, the
  `repository.change-blobstore` task): reads keep working and uploads go to the new store during the
  move; blobs are sha256-checked and the switch is atomic.
- Repository export and import tasks (`repository.export`, `repository.import`; `POST /-/api/export`,
  `/-/api/import`): portable, verifiable exports under `exportRoot` (or `COMMANDCENTER_EXPORT_ROOT`;
  manifest, per-repository asset list with package rows and tags, hard-linked or copied blobs, secrets
  stripped), delta exports with `since`, and imports that keep timestamps, verify every blob, lock like
  uploads and regenerate index documents when needed.
- Vulnerability data from OSV (osv.dev): component lookups for npm, PyPI, Maven, Go, Cargo, NuGet,
  RubyGems, Composer, Conan and R, online through the OSV API (cached) or offline from the OSV dumps
  (`vulnerability.sync` task, or an all.zip upload for air-gapped sites); affected ranges evaluated with
  each ecosystem's version order, CVSS 3.x base scores computed. Settings at
  `/-/api/vulnerability/settings`.
- Proxy firewall: a proxy's `firewall` policy quarantines components with a malicious-package advisory or
  one rated at least a chosen severity, cached ones included (403 naming the advisories), with an allow
  list, a quarantine list and release, and `firewall.blocked` events.
- `vulnerability.scan` task and repository vulnerability summaries: counts by severity in the repository
  list, top components per repository, advisories on the component view.
- Pull replication (Nexus Pro "pre-emptive pull", free): a proxy whose remote is a hosted repository
  of another Command Center or Nexus instance (`<base>/repository/<name>/`) gets `replication`
  (`preemptivePullEnabled`, `assetPathRegex`, `verifyChecksums`) on the UI and Nexus repository APIs.
  The new `repository.pull-replication` task pages the source's `GET /service/rest/v1/assets` and
  fetches each component file the proxy lacks through the proxy's own endpoint (routing rule,
  firewall, content checks, blob store and quota apply), incrementally by `lastModified`, with a
  `full` option; progress is kept in the database. `POST /-/api/repositories/{name}/replication/test`
  checks the source. 17 formats are supported; docker, p2, cocoapods and huggingface are refused (400).
  `GET /service/rest/v1/assets` now pages by key and lists `blobStoreName`, `uploader` and `uploaderIp`.
- Azure Blob Storage blob stores (`type: "azure"`): blobs live in a container at `<prefix>/<sha256>`.
  Writes use Put Blob up to 64 MiB and Put Block/Put Block List above, MD5-checked; reads use ranged
  Get Blob; GC lists with prefix and marker paging. Authentication is by account key (Shared Key), SAS
  token, Microsoft Entra ID client secret, managed identity (IMDS) or Kubernetes workload identity
  (`AZURE_FEDERATED_TOKEN_FILE`), with tokens refreshed before they expire. An endpoint override serves
  Azurite, sovereign clouds and private endpoints. Secrets are sealed and never returned. Credentials
  can be replaced in place on every node (`PUT /-/api/blobstores/{name}` with `azure`), and requests are
  retried on 429/5xx. Nexus-compatible `POST /service/rest/v1/blobstores/azure` and
  `GET`/`PUT /service/rest/v1/blobstores/azure/{name}`. Azure stores take soft quotas and can be blob
  store group members.
- Google Cloud Storage blob stores (`type: "google"`): blobs in a GCS bucket under an optional prefix
  through the JSON API, authenticated with a service account key (sealed, never returned), the node's
  application default credentials (`GOOGLE_APPLICATION_CREDENTIALS` or the metadata server: GKE Workload
  Identity, GCE) or none for emulators. Multipart uploads up to 16 MiB and resumable chunked uploads
  above, MD5-checked; Range reads; retries with exponential backoff on 408/429/5xx; an endpoint override
  for emulators and private endpoints; health checks, soft quotas and group membership like S3 stores. A
  store on the same bucket and prefix as another google or S3 store (GCS S3 interoperability) is refused.
- Reconcile the database with the blob stores after a restore (Nexus "Repair - Reconcile component
  database from blob store", plan and execute). Every commit of an asset, package version or dist
  tag writes a small journal record into its repository's blob store (asynchronous; a failed write
  never fails the upload and is counted in `commandcenter_reconcile_journal_writes_total`). The
  `blobstore.reconcile-plan` task compares a window of the journal (default 24 h, `since`, or `all`)
  with the database and stores ADD, DELETE, SKIP and CONFLICT items (`GET /-/api/reconcile/plans`,
  `.../{id}/items`); `blobstore.reconcile-execute` applies a plan through the upload code paths and
  regenerates the index documents. Recovery mode (`PUT /-/api/recovery`) makes every node refuse
  uploads and deletes with 503 and stops compaction, cleanup and the other content-moving tasks.
  Compaction keeps the blobs a pending plan adds or the last day's journal puts, and prunes the
  journal after `journalRetentionDays` (default 30).
- Configuration as code: `GET /-/api/configuration` exports every admin setting and object (trust store,
  blob stores and groups with quotas, routing rules, content selectors, cleanup policies, repositories,
  roles, users, general and outbound HTTP settings, SMTP, LDAP, OIDC, SAML, Crowd, RUT, realms, tasks,
  webhooks, vulnerability settings, log level) as one YAML or JSON document, and
  `POST /-/api/configuration` applies one. An apply checks the whole document first (fields, each
  object's rules, references, the admin lockout), then goes through the admin API's own handlers in
  dependency order, so events, sealing and the other nodes' updates are those of the UI; a step that
  fails undoes the steps before it. `dryRun=true` returns the plan with field-level changes, and
  `prune=true` deletes what a listed section leaves out. Secrets are left out (an apply keeps the
  stored ones) or exported encrypted with a passphrase (AES-256-GCM, scrypt), so another instance
  restores them; `passwords=true` adds the local users' password hashes.
- `commandcenter config export` and `commandcenter config apply -f FILE [-dry-run] [-prune]`: the same
  on the metadata database directly, beside running servers or without one.
- Nexus's configuration API: `GET /service/rest/v1/configuration` (with
  `X-Nexus-Migration-Protocol-Version: 1`) exports the document; `PUT` uploads one without overwriting
  what exists.
- Repository evaluation: the `repository.evaluate` task monitors hosted repositories (`evaluateHosted`)
  and group members (`evaluateGroupMembers`) of the vulnerability settings, with a per-repository
  `evaluation` override (`enabled`, `disabled`, `inherit`). A run looks up only new components, those
  evaluated against older advisory data, stale online answers and failed lookups, stores the findings
  per component, and announces new advisories as the activity event and webhook `component.vulnerable`.
- Repository dashboard (`GET /-/api/dashboard/repositories`, `.../{name}/vulnerabilities`): components,
  assets, size (summed and per distinct blob), last upload and download, requests and evaluation status
  per repository, sorted, filtered and paged, with the vulnerable components' advisories, severities,
  fixed versions and links. Database triggers keep the counters exact without scanning; the Nexus API's
  `GET /service/rest/v1/repositories` now reports `size`.
- Monthly usage metrics (`GET /-/api/metrics/monthly`): per UTC month requests, errors, egress and
  ingress bytes, downloads, uploads, storage and component counts (latest and peak) and distinct users
  and client addresses, kept for 24 months.
- Privilege target `metrics` (action `read`): the dashboard, the evaluation results and the monthly
  metrics without the admin role.
- Web UI for the above, in Nexus's places: a Replication card and an Evaluation select in the repository
  form (with a source connection test), Azure and Google Cloud Storage in the blob store forms (Azure
  credentials replaceable in place), Administration → System → Recovery (recovery mode, reconcile plans,
  their items and execution; a banner on every admin page while recovery mode is on) and → Configuration
  as Code (export, dry run with field-level plan, apply with step results), Repository → Repository
  Dashboard with each repository's vulnerable components, Support → Usage with a monthly chart, the
  evaluation toggles in the firewall settings and the `metrics` target in the role editor. Users with only
  the `metrics` privilege see the dashboard and usage in the menu (`/-/api/me` reports `metrics`).

### Changed

- Built with Go 1.27.3, which distrusts compromised intermediate certificates in crypto/x509
  (GO-2026-6809; the LDAP StartTLS connection verifies certificates through it).
- The metadata database is PostgreSQL; `database.url` (or `COMMANDCENTER_DATABASE_URL`) is required.
  docker-compose.yml and the Helm chart run a PostgreSQL 17 alongside (`postgresql.*` values) unless an
  external one is configured (`database.existingSecret`).
- `serve` no longer refuses to start beside another live node: the single-writer guard is gone. The
  `Cluster` status check no longer reports more than one live node as unhealthy.
- The reference manifest `deploy/kubernetes/commandcenter.yaml` uses
  `GET /service/rest/v1/status/writable` for readiness, as the chart does.
- On a node other than the one running a repository's blob store move, the repository list shows the move
  as `copying` while its task runs on any node, and `interrupted` otherwise. The copy counts are only on
  the node running the task.
- `commandcenter user reset-password` and `token create` take effect at once on the running servers.
- Webhook deliveries are sent only by `serve`. CLI commands store the deliveries their events make and
  leave them to the running server, or to the next one started. Before, a CLI command beside a server
  sent them too, sometimes twice.
- `commandcenter gc` takes the task run lock. It fails with `task "<id>" is running`
  while a task runs on any node, and task runs wait while it collects (run now answers 409).

### Fixed
- A write racing a delete and re-create of its repository could land in the new repository. Repositories
  now carry a stored generation (`repositories.gen`) checked inside the write transaction, so such a
  write fails with 503 (retry).
- APT hosted: `.deb` packages whose control archive is `control.tar.xz` (Debian's `dpkg-deb` default),
  `control.tar.zst` (Ubuntu 21.10 and later), `control.tar.bz2` or `control.tar.lzma` are accepted.
  Before, only `control.tar.gz` and an uncompressed `control.tar` were read, so nearly every
  distribution package was refused with 400. Built-in xz and Zstandard decoders (standard library only)
  do the work; the decompressed archive is still bounded at 64 MiB and `control` at 1 MiB, and an
  archive that declares an xz dictionary or zstd window above 8 MiB, uses an xz BCJ filter or needs a
  zstd dictionary is refused with 400.
- Conda hosted: a `.conda` upload's `info-<name>-<version>-<build>.tar.zst` is read, so its
  `packages.conda` entry carries the package's `info/index.json` (`depends`, `constrains`, `license`,
  `timestamp`, `build_number`, ...) and conda resolves the dependencies of `.conda` packages. As for a
  `.tar.bz2`, the file name must match the index (400 otherwise); `.conda` files uploaded before keep
  their old entries until uploaded again, and a `.conda` zip with more than 64 entries is refused.
- Yum hosted repositories accept RPM files built by rpmbuild: 0.2.0 looked for the wrong header magic
  (`8d ad e8 01` instead of `8e ad e8 01`) and refused every real RPM.
- Webhooks no longer send a delivery twice when a poll read it while a worker was sending it.
- A password change that a node picks up from the database (another node, or `user reset-password`
  next to a running server) also ends the sign-in refusal of that name. Before, attempts with the new
  password made before the node loaded it counted as failures and could lock the name out for up to
  15 minutes. When ending the user's sessions fails, the next reload tries again; before, the
  sessions stayed valid.

### Upgrade notes
From 0.2.0:
- **A full stop.** The first start of 0.3.0 takes the database from schema 3 to schema 7 (compat 6), and
  0.2.0 refuses the upgraded database. Back up the database (`pg_dump`) and the blob stores, stop every
  0.2.0 instance, then start 0.3.0. The only way back is that backup.
- The database role must be able to create functions and triggers (PL/pgSQL), as the owner of the
  tables: schema 7 keeps the repository usage counters with triggers on `assets` and fills them from the
  existing assets on that first start (about 2 s per million assets). An asset write costs about 0.1 ms
  more.
- New tables arrive on that start (webhooks, routing rules, content selectors, user tokens, tags, blob
  store moves, vulnerability data and quarantine, nodes and leases, sessions and other shared auth state,
  reconcile plans, usage counters), and existing rows gain meanings 0.2.0 would misread (blob store
  groups, the cleanup criteria `retain`, `exclude` and `pathPattern`).
- Blob stores gain a `meta/` directory (file stores) or `<prefix>/meta/` keys (S3, Azure, Google Cloud
  Storage) holding the reconcile journal: back it up with the blobs. Each upload commit writes one extra
  small object there (one more PUT on object storage), proxy cache fills included. The journal starts with
  the upgrade; uploads made before it cannot be reconciled.
- UI sessions and SSO identities now live in the database and survive restarts for up to 7 days after the
  login. A start with a different secret key, or with none after one was used, is refused, and
  `secret-key rotate` needs every node stopped. Without a secret key the token signing key is stored in
  plain text.
- The Helm chart's readinessProbe asks `GET /service/rest/v1/status/writable`, and a stopping server stays
  unready for 5 s before it stops taking connections. `persistence.accessModes` accepts `ReadWriteMany`.
- From one instance to several (active-active): back up, stop, copy the whole data directory (`blobs/`,
  `blobstores/` and the journal's `meta/`) to a ReadWriteMany volume, then start every node with the same
  config, database and secret key. For the chart: `replicaCount` and `persistence.existingClaim`.

From 0.3.0 on:
- Upgrades follow the N/N-1 rule: an expand-only schema change keeps compat one version below, so the
  previous release can run beside the new one (a rolling upgrade) or be rolled back to. The release notes
  say when an upgrade needs a full stop. A node of a build older than compat is refused, and an upgrade is
  refused while such a node is live.
- Creating the first Azure or Google Cloud Storage blob store raises compat to 7, so builds before 0.3.0
  can no longer start on that database (they would open the store as an empty directory).

## [0.2.0] - 2026-10-27

NuGet, APT and Yum repositories, a Nexus Repository 3 migration tool (`import-nexus`), a Nexus REST
API v1 compatibility layer, the per-repository traffic dashboard, OIDC login and the redesigned web UI.
The release assets now include the Kubernetes manifest pinned to the release and the chart as a file.

### Added
- The `nuget` format: NuGet V3 feeds at `/repository/<repo>/index.json` for `dotnet restore`,
  `nuget.exe` and Visual Studio (service index, flat container, registration with inline pages,
  search, `PackagePublish/2.0.0`). Hosted repositories take `dotnet nuget push` (`PUT api/v2/package`,
  the API key being an API token in `X-NuGet-ApiKey`, or Basic auth), read the `.nuspec` out of the
  `.nupkg`, normalize the version as NuGet does and derive the registration and flat container from
  the rows; `writePolicy` applies as for Helm (409 on a re-push under `allow_once`, overwriting under
  `allow` needs edit); `DELETE api/v2/package/<id>/<version>` (`dotnet nuget delete`) removes the
  version; `.snupkg` symbol packages are refused with 400. Proxies of api.nuget.org or another V3
  feed (`remote` = the service index URL) cache the flat container, registration pages (split pages
  fetched when followed), search results and packages, with the documents' URLs rewritten to the
  proxy; groups answer from the first member that has the package id, merge search results and push
  to their `writeMember`. Cleanup policies (format `nuget`), the web UI's component delete and
  metadata (dependencies per target framework), and `format:nuget` role privileges cover nuget
  repositories.
- The `yum` format: Yum/DNF (RPM) repositories. A hosted repository takes RPMs with
  `PUT /repository/<repo>/<dir>/<name>-<version>-<release>.<arch>.rpm` (the file name must be the
  package's own), parses the RPM header and regenerates `repodata/` (`repomd.xml` and the sha256-named
  `primary`, `filelists` and `other` files, as createrepo_c writes them) in the same transaction as
  every upload and component delete. `depth` (0, the default, or 1) says whether one `repodata/` at the
  root lists every package or each top-level directory has its own, as in Nexus. A proxy caches a
  mirror's files at their paths (`repodata/` revalidated after `metadataMaxAge`, packages kept); a
  group answers from the first member that has the path, without merging. `writePolicy`, cleanup
  policies (`format: yum`), the component delete, `format:yum` privileges and the component metadata
  tab (summary, license, url, requires and provides) work as for Helm. `repomd.xml` is not signed:
  use `repo_gpgcheck=0` (`gpgcheck=1` still verifies signed packages) ("Yum 설정" in the
  operations guide).
- `commandcenter import-nexus`: copies the hosted repositories of a Sonatype Nexus Repository 3
  into Command Center repositories you created first (`-repo <nexus>=<cc>` or `-all`). It reads the
  Nexus REST API v1 and writes each format through its own upload protocol (Maven PUT, npm publish,
  PyPI upload, raw PUT, helm PUT, docker blob upload and manifest PUT via `-docker-from`, the
  connector URL), so every server-side check applies; maven2, npm, pypi, raw, helm and docker are
  supported, proxies, groups and other formats are listed and skipped. It is resumable: what the
  target already has is skipped (`-skip-existing`, default), `-dry-run` lists what would be copied,
  and the credentials come from files or `NEXUS_PASSWORD`/`COMMANDCENTER_TOKEN`, never from the
  command line or the output ("Nexus에서 이전하기" in the operations guide).
- apt (Debian/Ubuntu) repositories: hosted, proxy and group, under `/repository/<repo>/`. A
  hosted repository takes `.deb` uploads (`PUT pool/<any>/<file>.deb` or a multipart `POST /`
  with the file in `file`), reads the control file and keeps `dists/<distribution>/Release`,
  `<component>/binary-<arch>/Packages` and `Packages.gz` itself, regenerated in the transaction of
  every upload and delete; with an RSA signing key (repository setting `signingKey`, sealed at rest
  like the remote credentials) it also writes `Release.gpg` and `InRelease` and serves the public
  key at `public.key`. The control archive must be a `control.tar.gz` (or an uncompressed tar):
  xz- and zstd-compressed ones are refused with 400. A proxy caches whatever its remote serves
  (`dists/` as metadata, `pool/` as content, signatures untouched); a group answers from the
  first member that has the path. Cleanup policies, component delete and the metadata tab
  (description, maintainer, homepage, depends) cover apt; `format:apt` is a privilege target.
- A Sonatype Nexus Repository 3 REST API compatibility layer under `/service/rest/v1`
  (`internal/nexusapi`), so scripts, CI jobs, the Terraform provider and `nexus-cli` written
  against Nexus keep working: repositories (list, settings, create, update, delete with Nexus
  bodies and `maven2` format spelling), components and assets with continuation tokens and
  Nexus-style ids, `search`, `search/assets` and `search/assets/download` (302 to the single
  match, 400 when ambiguous), `status/check` in Nexus's shape, and read-only
  `security/users`, `security/roles`, `blobstores` and `tasks`. Writes go through the web UI's
  own API handlers, so validation, events and privileges are the same; asset delete works for
  raw files only (Command Center deletes whole components). "Nexus REST API 호환" in the guide.

## [0.1.1] - 2026-10-27

The first published release. The `v0.1.0` tag's release build failed in CI on a test that measured
allocations under the race detector; 0.1.1 is the same software with that test fixed.

## [0.1.0] - 2026-10-27

The first release: Maven, npm, PyPI, Go, Cargo, Docker, raw and Helm repositories (hosted, proxy,
group), users, roles, LDAP and OIDC, API tokens, cleanup policies and scheduled tasks, file and S3
blob stores, SQLite or PostgreSQL, a Helm chart for Kubernetes, and the web UI.

### Upgrade notes

- Back up before you upgrade. Downgrades are not supported: an older version can misbehave
  silently on data a newer one has written, so the way back is the backup taken before the upgrade,
  restored together with the old image ("업그레이드" in the operations guide).
- After upgrading, run `repository.rebuild-metadata` once for each Maven hosted repository and each
  Maven proxy (a task with the schedule `manual`, then "Run now"). In a hosted repository it removes
  the versions, `latest`/`release` values and foreign `groupId`/`artifactId` that older builds took
  from uploaded `maven-metadata.xml` files, also from documents of GAs that have no artifacts;
  without it they stay until the next upload to the same `groupId:artifactId` (QA L-11). In a proxy
  it deletes the directory listings and IDE indexes (`.index/`) that older builds cached at paths
  that are not Maven files, which are no longer served but still show in Browse; files at
  Maven-file paths are kept even when they are `text/html`, and the next `blobstore.compact`
  reclaims the space (QA L-12).
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
- Deleting a repository now also removes the role privileges on it. Scripts that delete and
  re-create a repository under the same name, for example to empty it, must grant those privileges
  again (see Changed).
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
- docker compose: `docker-compose.yml` now runs the release image and no longer builds the
  checkout, and it does not start until `.env` next to it names the version, such as
  `COMMANDCENTER_VERSION=1.2.3` (no `v`). To keep running your own build, build it with
  `docker build` and set `COMMANDCENTER_IMAGE` and `COMMANDCENTER_VERSION` to it ("빠른 시작" in the
  operations guide).
- The database schema is now version 2 (`blob_stores.type`, `blob_stores.config`). Builds before
  this one refuse nothing but would open an S3 store's path as an empty directory, so do not
  downgrade once an S3 store exists; the backup from before the upgrade is the way back.

### Added
- The `oidc` realm: browser login through an OpenID Connect provider (Gitea, Keycloak, ...)
  with the authorization code flow and PKCE, configured under Realms (`GET/PUT /-/api/realms/oidc`,
  `POST /-/api/realms/oidc/test` reads the provider's discovery document) and offered on the login
  page as an SSO button (`GET /-/api/auth/sso`). Groups from the ID token (or userinfo) map to roles
  like LDAP groups, a `source: oidc` user row overrides or blocks a provider user, and the client
  secret is stored encrypted with the secret key. OIDC users get API tokens owned by `oidc:<id>`
  for CLI clients; their sessions and tokens need a fresh browser login after a restart. The login
  state lives in memory, so the realm needs a single replica ("OIDC 설정" in the operations guide).
- Per-repository traffic on the admin dashboard: `GET /-/api/stats/repositories?range=1h|24h|7d`
  reports, for every repository, requests, errors (5xx and 429) and the error rate, bytes in and
  out, average and longest response time, and for proxies the remote fetches, their failures,
  average and longest remote time, and cache hits and misses. The hourly `request_stats` rows gain
  these counters (new columns, zero for hours before the upgrade); the request-count chart is
  unchanged.
 The `docker` format: hosted Docker/OCI registries served at `/v2/<repository>/<image>` on the
  same host and port (`docker push host/<repo>/<image>:<tag>`), with the Distribution token flow
  (`/v2/token`; Basic is accepted too), schema2 and OCI manifests and indexes stored verbatim,
  chunked, monolithic and cross-repository-mount blob uploads that survive a restart, tag and
  catalog listing, delete by digest, and `writePolicy` as for Maven. `GET /v2/` challenges
  requests without credentials so clients find the token endpoint; anonymous pull then uses an
  anonymous token. Role privileges may target `format:docker`. Cleanup policies for docker
  repositories are not supported yet.
- PostgreSQL as the metadata database: `database.url` / `COMMANDCENTER_DATABASE_URL` and
  `database.maxConns`; SQLite stays the default. Every command that opens the database honours
  it, the schema version guard exists on both, and `COMMANDCENTER_TEST_POSTGRES_URL` runs the
  whole test suite on PostgreSQL. The Helm chart takes the URL from a Secret
  (`database.existingSecret`). Names PostgreSQL cannot store (NUL, invalid UTF-8) are refused
  with 400 there, and system information reports the driver and database size.
- `GET /-/api/repositories/{name}/components/metadata`: a component's description, license,
  links and declared dependencies, read from the stored POM, npm manifest, Cargo index line or
  go.mod; the web UI shows them in a metadata tab on the component view.

- A production deployment section in the operations guide (`docs/guide.md`): TLS through a reverse
  proxy, storage for `meta.db` (local or block storage, never NFS or SMB), a single instance,
  resource sizing, graceful shutdown, consistent and online backups, and why downgrades are unsafe.
- Reference Kubernetes manifests in `deploy/kubernetes/`. Liveness is a TCP connect, so a full
  connection table does not restart the pod; startup and readiness ask
  `GET /service/rest/v1/status`.
- A Helm chart, `deploy/helm/commandcenter`, published with each release as
  `oci://ghcr.io/sungjinseo/charts/commandcenter` (chart version and appVersion are the release
  version) and signed with cosign like the image. CI lints and renders it and checks it and the
  reference manifest with kubeconform. It runs one pod: a StatefulSet with no replicas setting, a ReadWriteOnce volume for
  `/data`, `config.yaml` in a Secret, the secret key from an existing Secret
  (`secretKey.existingSecret`), a non-root user with a read-only root filesystem, and a `helm test`
  that asks the status API through the Service with the image's own
  `commandcenter healthcheck -url`. It is published through an Ingress (`ingress.*`) or a Gateway
  API HTTPRoute (`httpRoute.*`). `config.baseURL` and `config.adminPasswordHash` are required,
  and `persistence.storageClass` must name block storage, never NFS or SMB. "Kubernetes" in the
  operations guide covers installing, ingress settings for large uploads, upgrades, backups with
  the pod stopped, and uninstalling, which keeps the volume. The README quick start now uses the
  release image on Kubernetes and with docker compose; building from source is still described.
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
- Release images are signed with cosign, without a key. The signing certificate names the release
  workflow at the tag,
  `https://github.com/sungjinseo/commandcenter/.github/workflows/release.yml@refs/tags/v<version>`
  (issuer `https://token.actions.githubusercontent.com`). The signature covers the multi-platform
  index that the release's tags point to and is recorded in Sigstore's public transparency log.
  Check it with `cosign verify` and cosign 3 or later (command in the operations guide).
- Dependabot: weekly updates for Go modules, the Docker base images and GitHub Actions.
- `COMMANDCENTER_VERSION` and `COMMANDCENTER_IMAGE` choose the image docker compose runs: by
  default `ghcr.io/sungjinseo/commandcenter` at the version in `COMMANDCENTER_VERSION`, or a mirror
  or local build named by `COMMANDCENTER_IMAGE`. The compose file has no `build:` section, so a
  mistyped version or an unreachable registry fails instead of quietly building the checkout. A
  local build reports the version given as `docker build --build-arg VERSION=…`, `dev` without
  it.
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
- `commandcenter secret-key rotate -new-key-file FILE` changes the secret key. In one transaction
  it re-encrypts the stored SMTP password, LDAP bind password and proxy `remoteAuth` secrets from
  the current key (`COMMANDCENTER_SECRET_KEY` or `secretKeyFile`, as for the server) to the key in
  FILE, and encrypts the ones still in plain text; if any stored secret does not decrypt with the
  current key, it changes nothing. It prints only counts. Stop the server before running it and
  start it with the new key afterwards, since a running server keeps the key it started with.
  Backups taken before the change still need the old key.
- `repository.rebuild-metadata` also takes a Maven proxy, which it refused with 400 before
  (QA L-12). It deletes the proxy's cached files at paths a proxy no longer fetches, such as the
  upstream directory listings and IDE indexes older builds cached, keeps every Maven file whatever
  its content type, and leaves the blobs to `blobstore.compact`. The run log names the first 20
  removed paths. The web UI's task editor lists Maven proxies next to hosted repositories, with
  each repository's type.
- The `raw` format, like Nexus's raw: a hosted repository stores any file at any path
  (`PUT /repository/<repo>/<path>` with the add privilege; `writePolicy` as for Maven, overwriting an
  existing file under `allow` needs edit; the same path rules as Maven), serves it with the
  `Content-Type` the upload sent or one from the extension, and deletes it with the delete
  privilege. A path ending in `/` (or the repository root) on a hosted repository lists its children
  as an HTML index, or as JSON with `Accept: application/json`. A proxy fetches and caches any path
  of its remote, web pages included, honouring `contentMaxAge` and `negativeCacheTTL`; a group
  answers from the first member that has the path and writes to its `writeMember`. The web UI
  browses raw repositories by path; components are the files themselves (name = path, no version),
  which the component search, the UI/API component delete and cleanup policies
  (`componentAgeDays`, `lastDownloadedDays`, policy format `all` or `raw`) handle per file. Role
  privileges may target `format:raw`.
- The `helm` format: Helm chart repositories served at `/repository/<repo>/` with a server-owned
  `index.yaml`. Hosted repositories take `PUT /repository/<repo>/<chart>-<version>.tgz` (curl
  `--upload-file`) and helm cm-push's `POST api/charts` (multipart `chart` field, answered with
  `201 {"saved":true}`), read `Chart.yaml` out of the tarball and rebuild `index.yaml` in the same
  transaction as every upload or delete (digest, created, urls relative to the repository);
  `writePolicy` applies as for Maven (overwriting under `allow` needs edit). Proxies cache the
  upstream `index.yaml` for `metadataMaxAge` with its urls rewritten to the proxy and fetch charts
  from the upstream URLs verified against the index digest; groups serve a merged `index.yaml`
  where the first member that has a chart name owns it, for downloads too. Cleanup policies,
  the web UI's component delete and metadata, and `format:helm` role privileges cover helm
  repositories. Charts pushed with `helm push oci://` go to a docker repository, which already
  accepts them.
- Docker proxy and group repositories: a proxy caches pulls from an upstream registry (`remote`
  without `/v2`, Docker Hub's `library/` prefix added for unqualified names), speaking the upstream
  Distribution token flow with `remoteAuth` or anonymously; tags and tag lists are revalidated after
  `metadataMaxAge`, manifests and layers fetched by digest are verified and cached for good. A
  group answers from its members in order, a hosted member that has the image owning it, and lists
  the union of their images. Both refuse pushes with 405.
- PyPI JSON API: `GET /repository/<repo>/pypi/<project>/json` and `/pypi/<project>/<version>/json`
  answer in pypi.org's shape (`info` from the stored wheel `METADATA` / sdist `PKG-INFO`,
  `releases`, `urls` with `digests`, `size`, `upload_time`, `packagetype`). Hosted repositories
  build it from stored rows, proxies cache the upstream document as metadata and rewrite file URLs,
  groups answer from the first member that has the project.
- PEP 658 core metadata: every distribution file has a `<file>.metadata` sidecar, advertised in the
  simple index (HTML `data-core-metadata` / `data-dist-info-metadata`, JSON `core-metadata` /
  `dist-info-metadata`) with its sha256, so pip resolves dependencies without downloading wheels.
  Uploads store it with the file (a `METADATA`/`PKG-INFO` over 16 MiB is refused); files stored by
  older builds get it extracted on first request. Proxies serve the cached sidecar, extract it from
  a cached file, or fetch the upstream's advertised `.metadata` (hash-verified). The sidecar is
  deleted with its version by component delete and cleanup.
- S3-compatible blob stores. `POST /-/api/blobstores` takes `type: "s3"` with `s3.bucket`,
  `s3.prefix`, `s3.region`, `s3.endpoint`, `s3.pathStyle`, `s3.accessKeyId`/`s3.secretAccessKey`
  (or the `AWS_*` environment) and `s3.sse`; blobs are stored as `<prefix>/<sha256>`, uploads over
  64 MiB go as multipart uploads, downloads use Range GETs, and `blobstore.compact` lists the prefix
  to collect unreferenced objects. The bucket is checked when the store is created; the secret
  access key is sealed with the secret key like other stored secrets and never returned.
  `GET /-/api/blobstores` now reports `type` (`file` or `s3`) and, for S3 stores, `s3` with
  `secretSet: true/false` in place of the secret; `fsTotal`/`fsFree` are null for them.
- `commandcenter migrate-db -config PATH -to TARGET` copies an existing SQLite `meta.db` into an
  empty PostgreSQL database (or a PostgreSQL database into a new SQLite file). It runs offline with
  the server stopped, copies every table in one transaction on the target, keeps ids, timestamps,
  JSON and sealed secrets byte for byte (start the server with the same secret key), refuses a
  non-empty target or a schema-version mismatch, verifies the row count of every table before
  committing, prints per-table counts, and never prints the URL. Blobs stay where they are.

### Changed

- An up-to-date database is opened without any CREATE/ALTER, so on PostgreSQL a role with only
  SELECT/INSERT/UPDATE/DELETE on the tables (and USAGE on the schema) can run the server and the CLI.
  Creating or upgrading the schema still needs the owner of the database and its tables, and a
  refused upgrade now says so (SQLSTATE 42501).
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
  cached by older builds are no longer served; `repository.rebuild-metadata` run on the proxy
  removes them (see Added).
- **Cargo proxy and group index responses carry an `ETag`** (QA L-21) and answer a matching
  `If-None-Match` with 304, like hosted ones, so cargo stops downloading them again on every
  resolve. No Cargo index response sends `Last-Modified`.
- **A version refuses to start on a `meta.db` written by a newer version** (QA N-30): the database
  now records its schema version (SQLite `user_version`), and the error names both versions and
  points to restoring the pre-upgrade backup. Versions before this one do not check. The first
  start brings `meta.db` up to date and records the version in one transaction, so an interrupted
  start leaves it as it was and the next start upgrades it again.
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
- Deleting a repository also removes, in the same transaction, every role privilege whose target is
  exactly `repo:<name>`, so a repository created later under that name starts without them (QA
  N-25). `*`, `format:` and other repositories' targets stay, and a role left without privileges is
  kept. The server logs one warning naming the repository and the changed roles (`removed the
  privileges on a deleted repository from roles`), records a `role.update` event per role with the
  detail `removed repo:<name>`, and the web UI's delete confirmation lists those roles. Privileges
  that deletes in older versions left behind stay; the startup warning `role grants privileges on a
  repository that does not exist` names them.
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

- Docker proxy: a failure of the upstream token service (realm down, timeout, non-https realm) now
  answers 502 and is no longer negative-cached as `MANIFEST_UNKNOWN`/`BLOB_UNKNOWN` for
  `negativeCacheTTL`; a realm that refuses the credentials (401/403) still answers 404 as before.
- Docker: `GET /v2/<name>/tags/list?n=0` and `GET /v2/_catalog?n=0` return an empty page instead of
  dropping the connection (handler panic).
- Docker hosted: deleting a manifest no longer unlinks a layer that was uploaded or checked with
  `HEAD` within the last 24 hours, so a `docker push` that skipped layers already present cannot fail
  with `MANIFEST_BLOB_UNKNOWN` because another manifest of the image was deleted meanwhile; such
  layers are reclaimed by a later manifest delete.
- Docker groups: the owner rule now holds through nested groups: when a hosted member of a nested
  group has the image, the outer group answers that member's 404 instead of falling through to a
  proxy member that has the same name.
- Docker and Cargo groups: a remote's rate limit (429) reaches the client as 429 (`TOOMANYREQUESTS`)
  through a group instead of 502.
- The component metadata API no longer rebuilds the POM property map for every dependency: a POM with
  1000 properties and 2000 dependencies now costs ~3 MiB and ~10 ms per request instead of ~520 MiB
  and ~0.5 s. It expands `${project.groupId}`, `${project.artifactId}` and `${project.version}` (and
  the `${pom.*}` and bare `${version}` forms) in dependency coordinates, falling back to the parent's
  coordinates, no longer loses what follows an element nested deeper than 1000 levels, and parses
  PyPI core metadata with the pypi format's own RFC 822 parser, so the long description and folded
  continuation lines are not read as `Requires-Dist`, `License`, `Summary` or `Home-page`.
- S3 blob stores no longer follow HTTP redirects: a 301/302 from the endpoint (for example AWS's
  PermanentRedirect for a bucket in another region) is reported with S3's own message instead of
  being followed to another host. An object served without `Content-Length` is an error instead of
  an empty download, and the client no longer offers `Accept-Encoding: gzip`.
- Creating an S3 blob store on the same bucket and prefix as an existing store is refused however
  the endpoint is spelled (trailing slash, upper case, default port, or the AWS default left empty),
  so two stores' GCs cannot delete each other's blobs.
- Concurrent creates of users, roles or blob stores with the same (or look-alike) id end in a
  conflict for one of them instead of two rows or a raw constraint error.
- A Helm proxy of a large upstream (bitnami's `index.yaml` is ~30 MB) was OOM-killed in the default
  128 MiB container on its first `index.yaml` request and again on every upstream change: the index
  was decoded whole (~580 MiB). It is now read and rewritten one chart version at a time
  (~40 MiB for the same index); a single version larger than 2 MiB, or an index not laid out as
  `helm repo index` writes it and larger than 2 MiB, is a 502.
- A Helm proxy whose upstream has no `index.yaml` (a wrong `remote`) answered a fabricated empty
  index with 200, so `helm repo add` succeeded and `helm search repo` found nothing; it now answers
  the upstream's 404 (502 for a failure), and a group with such a member fails as for any failing
  member. A hosted repository nothing was published to still answers an empty index.
- PyPI hosted advertised PEP 658 core metadata (`data-core-metadata`) for every file, including
  files the initial release stored without reading them, whose `.metadata` then answered 404 and
  made pip abort the install. The simple index now extracts the metadata of such files when it
  first lists them and advertises only what it has; a file none can be read from is remembered and
  not advertised, and `/pypi/<project>/json` still lists its files (`info` with name and version
  only) instead of 404.
- A PyPI proxy's `/pypi/<project>/json` of a very large upstream document needed ~9x its size in
  memory (64 MiB were accepted: ~600 MiB); the cap is now 8 MiB (pypi.org's largest documents are
  single-digit MiB), 502 above it.
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
  lookups stay case-insensitive. Crates cached by a proxy, which records no package rows, take the
  spelling from the first line of their cached index file in the web UI: at most 4 KiB is read per
  crate shown, the upstream is never asked, and a crate whose index file is not cached keeps the
  lowercase name (QA L-20).
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
- `sbt publish` of an sbt plugin to a Maven hosted repository failed with 400. By default
  (`sbtPluginPublishLegacyMavenStyle`), sbt 1.9 and later also upload the plugin in sbt's legacy
  layout, named without the `_<scala>_<sbt>` suffix of the artifact directory
  (`…/sbt-foo_2.12_1.0/1.0.0/sbt-foo-1.0.0.jar`); sbt before 1.9 uploads only that layout. Hosted
  repositories now store such a file, and serve it also through groups, when its name is the
  directory name without the suffix, `-`, the version, an optional classifier and an extension. A
  legacy `.pom` must name that shorter `artifactId`. These files are not versions of the
  `groupId:artifactId`, so `maven-metadata.xml` lists only the versions that have Maven-layout
  files. The component search, cleanup and the components API count them, including the ones
  proxies have cached, as files of the plugin version's component, so deleting the component,
  in the web UI or through the API, deletes them too (sbt falls back to them when the
  Maven-layout files are gone).
- Clients that sent headers and then stalled their bodies, even without credentials, could fill the
  128 MiB container with idle connections (QA N-5).
- A body that stalled past the idle timeout answered 500 and logged an ERROR (QA N-8).
- A group whose members were all offline answered 404, which clients such as Maven cache (QA N-27).
- Uploading a GA `maven-metadata.xml` could add versions without artifacts, a `<release>` or
  another artifact's IDs, and so steer version ranges such as `[1.0,)` (QA L-11).
- Deleting a Maven artifact or component (also by a cleanup policy) and
  `repository.rebuild-metadata` dropped the `<plugins>` of a `maven-metadata.xml` that a GA shares
  with a plugin group (the plugins of groupId `org.example` and GA `org:example` both live in
  `org/example/`), so `mvn <prefix>:<goal>` no longer found those plugins. The plugins now stay,
  also when the GA's last version is deleted.
- `repository.rebuild-metadata` kept the versions of a GA document whose GA has no artifacts when
  the document named no `groupId`/`artifactId` or another GA, also held `<plugins>`, or sat inside
  another GA's directory (`org/example/app/` under GA `org:example`). Those versions now go too, so
  after the upgrade run no document an older build accepted adds versions to version-range,
  `LATEST` or `RELEASE` resolution, also through groups.
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
- The `blobstore.compact` task reported 0 bytes however many blobs it removed (`bytes` of
  `POST /-/api/gc`, the task's `lastBytes`). It now reports the bytes the removed blobs held, and
  its run message and log say so: `removed 3 blobs (1234 bytes)`.
- Concurrent `maven-metadata.xml` uploads of up to 10 MiB each could exhaust memory.
- LDAP: the 10 s back-off after a failed re-check did not hold when the directory hung, because it
  ran from the start of the attempt; it now runs from its end. A client that disconnects no longer
  starts it, and a request waiting behind another's re-check of the same user ends when its client
  disconnects.
- LDAP: when a sign-in's client went away, or the connection test reached its 30 s limit, between
  two directory operations, the next operation still ran for up to `timeoutSeconds` (at most 60 s)
  and held one of the directory slots. It now stops at once.
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
- The privileges a role had on a deleted repository applied again to a repository created later
  under the same name (QA N-25, see Changed).
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

[Unreleased]: https://github.com/sungjinseo/commandcenter/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/sungjinseo/commandcenter/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/sungjinseo/commandcenter/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/sungjinseo/commandcenter/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/sungjinseo/commandcenter/compare/cc41abc35f8ba99d6911d0a8e947767dc79d67b9...v0.1.0
