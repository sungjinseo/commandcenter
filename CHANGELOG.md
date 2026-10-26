# Changelog

All notable changes to Command Center are documented in this file. Each entry says what changed
for the people who run it and what, if anything, they have to do.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

<!--
Integrator: fill in the entries of this release round before tagging. One line per user-visible
change, with the QA ID where there is one (for example "QA N-1"). Remove headings that stay empty.
-->

### Added

- A production deployment section in the operations guide (`docs/guide.md`): TLS through a reverse
  proxy, storage for `meta.db` (local or block storage, never NFS or SMB), a single instance,
  resource sizing, graceful shutdown, consistent and online backups, and why downgrades are unsafe.
- Reference Kubernetes manifests in `deploy/kubernetes/`.
- `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, this changelog, and issue and pull request templates.
- <!-- integrator: features from the other work packages -->

### Changed

- The README's "Honest status" section no longer claims that every High and Medium QA finding is
  fixed. Known limitations are listed in the operations guide, and changes in this file.
- <!-- integrator: behaviour changes, each with what operators have to do -->

### Fixed

- <!-- integrator: fixes from the other work packages, for example "npm: ... (QA N-1)" -->

### Security

- <!-- integrator: security fixes; coordinate disclosure as SECURITY.md describes -->

[Unreleased]: https://github.com/sungjinseo/commandcenter/commits/main
