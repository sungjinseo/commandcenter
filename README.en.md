<h1 align="center">Command Center</h1>

<p align="center"><a href="README.md">한국어</a> · <b>English</b></p>

<p align="center">
  A small, solid open-source artifact registry that takes the place of Nexus Repository.<br>
  Maven · npm · PyPI · Go · Cargo · Docker · raw · Helm · Yum · NuGet · APT in one binary, running as a single Kubernetes pod.
</p>

<p align="center">
  <a href="https://github.com/sungjinseo/commandcenter/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/sungjinseo/commandcenter/actions/workflows/ci.yml/badge.svg?branch=main"></a>
  <a href="https://github.com/sungjinseo/commandcenter/releases"><img alt="Release" src="https://img.shields.io/github/v/release/sungjinseo/commandcenter?style=flat-square"></a>
  <img alt="Go 1.26" src="https://img.shields.io/badge/Go-1.26-00add8?style=flat-square&logo=go&logoColor=white">
  <img alt="License: MIT OR Apache-2.0" src="https://img.shields.io/badge/license-MIT%20%2F%20Apache--2.0-3a3a3a?style=flat-square">
</p>

<p align="center">
  <a href="#why-command-center">Why</a> ·
  <a href="#formats">Formats</a> ·
  <a href="#features">Features</a> ·
  <a href="#quick-start">Quick start</a> ·
  <a href="#moving-off-nexus">Moving off Nexus</a> ·
  <a href="docs/guide.md">Operations guide (Korean)</a> ·
  <a href="#status-and-limits">Status</a>
</p>

---

## Why Command Center

Most in-house package repositories start as Nexus Repository OSS, and then meet a 4 GB JVM heap, a slow UI and "available in Pro". Command Center is built to take over that seat.

- **The same URL layout.** `/repository/<name>/...` and Docker's `/v2/` are unchanged, so clients only need a new host name.
- **The same operating model.** hosted, proxy and group repositories, users, roles and privileges, cleanup policies, scheduled tasks, blob stores: nothing to relearn.
- **Nexus REST API compatibility and a migration tool.** Existing scripts and the Terraform provider keep working against `/service/rest/v1`, and `import-nexus` copies hosted repositories wholesale.
- **Small.** One Go binary without CGO. It runs in a 128 MiB container; the load test peaks at 43 MiB RSS.
- **No editions.** LDAP, OIDC (SSO), S3 blob stores and PostgreSQL are all included, under MIT or Apache-2.0, your choice.

## Formats

| Format | hosted | proxy | group | Verified with |
|---|:-:|:-:|:-:|---|
| Maven | ✅ | ✅ | ✅ | mvn, Gradle |
| npm | ✅ | ✅ | ✅ | npm, pnpm, yarn |
| PyPI | ✅ | ✅ | ✅ | pip, twine, uv (PEP 503/658, JSON API) |
| Go | ✅ | ✅ | ✅ | go (GOPROXY) |
| Cargo | ✅ | ✅ | ✅ | cargo (sparse index) |
| Docker / OCI | ✅ | ✅ | ✅ | docker, skopeo, crane, helm push oci:// |
| raw | ✅ | ✅ | ✅ | curl, browsers (directory listings) |
| Helm | ✅ | ✅ | ✅ | helm repo add, helm cm-push |
| Yum / DNF | ✅ | ✅ | ✅ | dnf, yum (repodata generated) |
| NuGet | ✅ | ✅ | ✅ | dotnet nuget, nuget.exe (V3) |
| APT | ✅ | ✅ | ✅ | apt (signed Release) |

Unsupported formats are listed under [limitations](docs/guide.md#제한-사항).

## Features

- **Web UI**: browse, search, upload, component metadata (dependencies, licenses), and administration of repositories, users, roles, realms, blob stores and tasks. Korean and English, light and dark, embedded in the binary so it works offline.
- **Authentication and authorization**: local users, roles with per-repository privileges (browse, read, add, edit, delete), API tokens, **LDAP** (standard-library client), **OIDC** (Keycloak, Gitea, GitHub and others, with group-to-role mapping), Docker token auth.
- **Operational visibility**: per-repository requests, error rate, latency, upstream failures and cache hit rate; request trends, activity log, health checks, log viewer, failure e-mails.
- **Storage**: file or **S3-compatible** blob stores (several at once), content-addressed deduplication, blob GC. Metadata in **SQLite** or **PostgreSQL**, with a migration command between them.
- **Housekeeping**: cleanup policies (age, last download, name pattern), scheduled tasks, per-repository online/offline.
- **Kubernetes first**: a Helm chart (StatefulSet, values schema, PostgreSQL, NFS and HTTPRoute support, `helm test`), a reference manifest, and amd64/arm64 images with SBOM and provenance on every release.
- **Nexus compatibility**: the repositories, components, assets, search and status endpoints of `/service/rest/v1`, and `commandcenter import-nexus`.

## Quick start

Release images live at `ghcr.io/sungjinseo/commandcenter`. Replace `0.1.1` below with the [latest release](https://github.com/sungjinseo/commandcenter/releases).

Create the admin password hash first (8 characters or more).

```bash
docker run --rm -i ghcr.io/sungjinseo/commandcenter:0.1.1 hash-password
```

### Kubernetes (Helm)

```yaml
# my-values.yaml
config:
  baseURL: https://packages.example.com       # the address clients use
  adminPasswordHash: '<hash from above>'
persistence:
  storageClass: <StorageClass>                 # blob storage; NFS works
database:
  existingSecret: commandcenter-db             # omit for SQLite; PostgreSQL per the guide
```

```bash
helm install commandcenter oci://ghcr.io/sungjinseo/charts/commandcenter --version 0.1.1 \
  --namespace commandcenter --create-namespace -f my-values.yaml --wait
helm test commandcenter -n commandcenter
```

Ingress/HTTPRoute, TLS, PostgreSQL, the secret key, backups and upgrades are in the guide's [Kubernetes](docs/guide.md#kubernetes) section.

### Kubernetes without Helm (one YAML file)

Clusters that do not use Helm take the manifest attached to every release (`commandcenter-v0.1.1.yaml`, with the image pinned to that release). It is the same file as [deploy/kubernetes/commandcenter.yaml](deploy/kubernetes/commandcenter.yaml) in the repository.

```bash
curl -LO https://github.com/sungjinseo/commandcenter/releases/download/v0.1.1/commandcenter-v0.1.1.yaml
# fill in baseURL, passwordHash and storageClassName in the file, then
kubectl create namespace commandcenter
kubectl apply -n commandcenter -f commandcenter-v0.1.1.yaml
```

It holds a one-pod StatefulSet, the `config.yaml` ConfigMap, a PVC and a Service; add an Ingress or Gateway the way your cluster does it. The chart is also attached as a file (`commandcenter-helm-chart-0.1.1.tgz`) for places that cannot pull OCI charts: `helm install commandcenter ./commandcenter-helm-chart-0.1.1.tgz -f my-values.yaml`.

### Docker compose

```bash
git clone --depth 1 --branch v0.1.1 https://github.com/sungjinseo/commandcenter.git
cd commandcenter
echo COMMANDCENTER_VERSION=0.1.1 >> .env
cp config.example.yaml config.yaml     # put the hash in passwordHash
docker compose up -d
```

Open `http://localhost:8081/` and sign in as `admin`.

### Client setup

Each repository's "usage" tab shows copy-ready client configuration (Maven `settings.xml`, `.npmrc`, `pip.conf`, `GOPROXY`, Cargo `config.toml`, `docker login`, `helm repo add`, `.repo` files). The full list is in the guide under [포맷별 클라이언트 설정](docs/guide.md#포맷별-클라이언트-설정).

## Moving off Nexus

1. Create repositories of the same formats in Command Center (web UI, config file, or the Nexus-compatible `POST /service/rest/v1/repositories/<format>/<type>`).
2. Copy the hosted repositories.

   ```bash
   commandcenter import-nexus -from https://nexus.example.com -from-user admin -from-password-file nexus.pw \
     -to https://packages.example.com -to-user ci -to-token-file cc.token -all -dry-run   # check first
   commandcenter import-nexus ... -all                                                  # copy (safe to rerun)
   ```

   Maven, npm, PyPI, raw, Helm and Docker are written through each format's own upload protocol, so every server-side check applies. Proxies refill themselves on first use.
3. Change the host name in client configuration. Scripts that use the Nexus REST API stay as they are.

Details: [Nexus에서 이전하기](docs/guide.md#nexus에서-이전하기) and [Nexus REST API 호환](docs/guide.md#nexus-rest-api-호환).

## Production checklist

- Terminate TLS at the Ingress or reverse proxy and set `baseURL` to the `https://` address.
- Turn off anonymous read (`anonymousRead`) for internal use; give CI API tokens, not passwords.
- Set a key for stored secrets (SMTP, LDAP, OIDC, remote credentials, S3) with `COMMANDCENTER_SECRET_KEY` or `secretKeyFile`, and keep it apart from backups.
- Keep metadata in PostgreSQL or in SQLite on block storage (never SQLite on NFS). Run one instance.
- A 60-second stop grace period (already in the chart and compose file), regular backups, and a backup before every upgrade. Downgrades are not supported.

The full list is under [프로덕션 배포](docs/guide.md#프로덕션-배포).

## How light is it

| Measurement (`test/e2e.sh`) | Result |
|---|---|
| Smoke tests per format, including UI, LDAP and mail | 6 to 37 MiB |
| Load test: concurrent 100 MiB downloads and uploads, hostile uploads | 43 MiB peak RSS |
| Helm proxy of a bitnami-sized (30 MB) index.yaml | about 40 MiB (streamed) |

The default container limit is 128 MiB (`GOMEMLIMIT=80MiB`).

## Status and limits

- Designed for a single instance; no high availability or horizontal scaling (the same as Nexus OSS).
- RubyGems, Conan, Conda and Composer are not available yet.
- The Nexus REST API covers repositories, components, assets, search and status. User and role writes and the script API are not available.
- Known limitations are listed in the guide ([제한 사항](docs/guide.md#제한-사항)); every release's fixes and changes are in the [CHANGELOG](CHANGELOG.md).
- Report bugs as issues and security problems privately per the [security policy](SECURITY.md).

## Documentation

- [Operations guide](docs/guide.md) (Korean): installation, Kubernetes, production deployment, configuration reference, authentication, LDAP and OIDC, blob stores, per-format client setup, backup and upgrade, Nexus migration and the compatibility API
- [Helm chart](deploy/helm/commandcenter) · [Kubernetes reference manifest](deploy/kubernetes/commandcenter.yaml)
- [CHANGELOG](CHANGELOG.md) · [Contributing](CONTRIBUTING.md) · [Code of conduct](CODE_OF_CONDUCT.md) · [Security policy](SECURITY.md) · [Attribution](ATTRIBUTION.md)

## Contributing

Issues, documentation fixes, bug fixes and new formats are all welcome. The development setup and test commands are in [CONTRIBUTING.md](CONTRIBUTING.md) and the guide's [개발](docs/guide.md#개발) section.

## License

MIT or Apache-2.0, at your option ([LICENSE-MIT](LICENSE-MIT), [LICENSE-APACHE](LICENSE-APACHE)). The IBM Plex fonts in the web UI are under the SIL Open Font License.
