# Releasing Command Center

How maintainers publish a release and complete Dependabot's pull requests. A release is a tag:
pushing `vX.Y.Z` to GitHub runs [`.github/workflows/release.yml`](.github/workflows/release.yml),
and nothing else publishes anything.

## Versions

Versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Tags are
`vMAJOR.MINOR.PATCH`, or `vMAJOR.MINOR.PATCH-rc.N` for a prerelease. Every tag that starts with `v`
starts the release workflow, so push no other `v` tags (no `v1`, no `v0.1`).

**The first release is v0.1.0.** The initial open-source commit (cc41abc) has no tag, so v0.1.0 is
the first version anyone can pin. A major version of zero says that config keys, the REST API and
the database schema may still change in a minor release, and the workflow gives 0.x releases no
floating `0` image tag. Release v1.0.0 once the project commits to in-place upgrades without
breaking changes within a major version.

While the major version is 0, a minor release may break compatibility and a patch release only
fixes things. Whatever needs an action from the people who run Command Center goes under "Upgrade
notes" in [CHANGELOG.md](CHANGELOG.md). This includes a new database schema version, because the
only way back from one is the backup.

## Cutting a release

1. **Merge everything for the release into `main`** through pull requests with green CI.
2. **Prepare the CHANGELOG** in a pull request, and merge it:
   - Rename `## [Unreleased]` to `## [X.Y.Z] - YYYY-MM-DD`, using the day you tag. Put a new,
     empty `## [Unreleased]` above it.
   - Read the section as someone who runs Command Center: every step they have to take must be
     under "Upgrade notes".
   - Update the links at the end of the file:

     ```markdown
     [Unreleased]: https://github.com/sungjinseo/commandcenter/compare/vX.Y.Z...HEAD
     [X.Y.Z]: https://github.com/sungjinseo/commandcenter/compare/vPREVIOUS...vX.Y.Z
     ```

     v0.1.0 covers everything since the initial commit, so its link is
     `https://github.com/sungjinseo/commandcenter/compare/cc41abc35f8ba99d6911d0a8e947767dc79d67b9...v0.1.0`.

   No other file carries the version. The binary gets it from the tag (`commandcenter version`
   prints `vX.Y.Z`). The workflow sets the chart's `version` and `appVersion` when it packages the
   chart, so `Chart.yaml` needs no bump.
3. **Run the end-to-end checks on `main`**: `bash test/e2e.sh` (about 20 minutes, needs Docker and
   internet access). They drive the image with the real clients, and CI does not run them.
4. **Tag `main` and push only that tag.** Don't use `git push --tags`, which pushes every local
   tag:

   ```bash
   git fetch origin
   git log -1 origin/main        # the CHANGELOG pull request's merge, with green CI
   git tag -a vX.Y.Z -m "Command Center vX.Y.Z" origin/main
   git push origin vX.Y.Z
   ```

   The workflow does not check that the tag is on `main`, so check it yourself. To release two
   versions at once, for example v1.3.0 and a maintenance v1.2.5, push both tags in one command:
   `git push origin v1.2.5 v1.3.0`. Each run picks the floating tags from the tags it sees when it
   starts. If the v1.2.5 run starts before v1.3.0 exists, `latest` can move back to 1.2.5.
5. **Watch the run**, with `gh run watch` or in the Actions tab. The workflow runs CI again first,
   and publishes nothing if CI fails.
6. **Check the release as a user would** (see [Verifying a release](#verifying-a-release)). Then
   edit the GitHub Release and put the version's CHANGELOG section, or at least its Upgrade notes,
   above the generated notes, which only list the merged pull requests.

## What the release workflow publishes

| What | Where | Details |
|---|---|---|
| Image | `ghcr.io/sungjinseo/commandcenter:X.Y.Z` | linux/amd64 and linux/arm64, with an SBOM and provenance. Signed with cosign, without a key, on the multi-platform index. |
| Helm chart | `oci://ghcr.io/sungjinseo/charts/commandcenter`, version `X.Y.Z` | `version` and `appVersion` come from the tag, so by default the chart deploys this release's image. |
| Binaries | GitHub Release: `commandcenter-vX.Y.Z-linux-amd64.tar.gz` and `commandcenter-vX.Y.Z-linux-arm64.tar.gz` | The image's binary, with `LICENSE-MIT`, `LICENSE-APACHE`, `NOTICE` and `THIRD_PARTY_LICENSES.txt`. |
| `SHA256SUMS` | GitHub Release | The SHA-256 of both tarballs. |
| GitHub Release | `vX.Y.Z` | Generated notes. A prerelease when the tag contains a `-`. Marked Latest when it is the highest stable release. |

The image also gets floating tags: `X.Y`, `X` (never for 0.x) and `latest`. Each one moves to this
release only when it is the highest stable release in that tag's range. For example, v1.2.5 after
v1.3.0 moves `1.2`, but leaves `1` and `latest` on 1.3.0. A prerelease gets only its own tag.
v0.1.0 gets `0.1.0`, `0.1` and `latest`.

### When the run fails

- **CI failed:** nothing was published. Delete the tag with `git push origin :refs/tags/vX.Y.Z` and
  `git tag -d vX.Y.Z`, fix `main` through a pull request, and tag again.
- **The release job failed on something transient,** such as the registry or Sigstore: click
  "Re-run failed jobs".
- **Anything else after the image was pushed:** never move or recreate a tag that published
  something, because users, the floating tags and the transparency log already point at that
  build. Fix `main` and release the next patch version.

## The first release

The workflow needs no secrets: it pushes with the run's `GITHUB_TOKEN` and signs with the run's
OIDC identity. Before you push v0.1.0, change these repository settings:

- Add a tag ruleset (Settings → Rules → Rulesets → New tag ruleset): target `v*`, restrict
  creations, updates and deletions, and let only maintainers bypass it. Anyone who can push a `v`
  tag can publish an image with a valid signature.
- Add a branch ruleset for `main` that requires CI's checks (today `test` and `docker`). Without
  it, a failing CI doesn't stop a merge. This matters for the Dependabot pull requests below.
- Turn on private vulnerability reporting in the repository's security settings.
  [SECURITY.md](SECURITY.md) tells reporters to use it.

After the run finishes:

1. **Make both packages public.** The workflow creates them on ghcr.io, and GitHub makes every new
   package private, so nobody else can pull the image or the chart yet. On github.com, go to
   sungjinseo → Packages and open each package. Then open Package settings → Danger Zone →
   Change visibility, and choose Public:
   - `commandcenter`, the image:
     <https://github.com/users/sungjinseo/packages/container/commandcenter/settings>
   - `charts/commandcenter`, the chart:
     <https://github.com/users/sungjinseo/packages/container/charts%2Fcommandcenter/settings>

   You can't make a public package private again. If the repository moves to an organization, its
   owners must first allow public packages under the organization's Settings → Packages.
2. If a package page shows "Connect repository", connect the package to
   `sungjinseo/commandcenter` so that it is listed on the repository's page.
3. Check both packages while signed out, as described in the next section.

Later releases push new versions into the same packages, and the packages stay public. If a push
is refused with 403, for example because someone pushed the package by hand first, open the
package's Manage Actions access and give the repository the Write role.

## Verifying a release

Maintainers run these checks after every release, and users can run the same ones. The operations
guide ([docs/guide.md](docs/guide.md), "릴리스 이미지와 바이너리") gives them in Korean. Sign out
first with `docker logout ghcr.io` and `helm registry logout ghcr.io`, so that you see what the
public sees.

**The image signature.** Use cosign 3 or later:

```bash
cosign verify ghcr.io/sungjinseo/commandcenter:X.Y.Z \
  --certificate-identity https://github.com/sungjinseo/commandcenter/.github/workflows/release.yml@refs/tags/vX.Y.Z \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The command succeeds only for an image that this repository's release workflow signed at that
tag. The signature belongs to the multi-platform index that every tag of the release points to. So
verify the tag or the index digest, because the per-platform digests carry no signature. A policy
that should accept every release, such as an admission controller's, can match the identity with
the regular expression
`^https://github\.com/sungjinseo/commandcenter/\.github/workflows/release\.yml@refs/tags/v`.

**The image itself.** `docker buildx imagetools inspect ghcr.io/sungjinseo/commandcenter:X.Y.Z`
lists both platforms. Add `--format '{{ json .SBOM }}'` or `--format '{{ json .Provenance }}'` to
see the SBOM or the provenance. `docker run --rm ghcr.io/sungjinseo/commandcenter:X.Y.Z version`
prints `vX.Y.Z`.

**The chart.** `helm show chart oci://ghcr.io/sungjinseo/charts/commandcenter --version X.Y.Z`
prints `version` and `appVersion` X.Y.Z, so by default the chart deploys the image you verified
above.

**The binaries.** Download the tarballs and `SHA256SUMS` from the GitHub Release, then run
`sha256sum -c --ignore-missing SHA256SUMS`. `SHA256SUMS` is not signed: it catches a broken
download, not a replaced one. If you need a verified binary, copy it out of the verified image:
run `docker create`, then `docker cp <container>:/usr/local/bin/commandcenter .`.

## Dependabot pull requests

Dependabot opens pull requests every week for Go modules, the Dockerfile's base images and GitHub
Actions (see [`.github/dependabot.yml`](.github/dependabot.yml)). Two kinds of pull request fail
CI until a maintainer adds a commit to their branch. This is on purpose: CI checks files that
Dependabot does not update. Don't merge them while they are red.

### Go modules

`THIRD_PARTY_LICENSES.txt` names the version of every module linked into the binary, and the
image and the release tarballs ship it. Nearly every module in `go.mod` is linked, so an update
of one makes CI's `docker` job fail at "THIRD_PARTY_LICENSES.txt is current"
(`scripts/third-party-licenses.sh --check`). Regenerate the file on the pull request's branch.
The script needs Docker and bash, because it runs in the Dockerfile's golang image, which also
makes its output the same as CI's:

```bash
gh pr checkout <number>
bash scripts/third-party-licenses.sh
git diff THIRD_PARTY_LICENSES.txt
# update the module's version in ATTRIBUTION.md
git add THIRD_PARTY_LICENSES.txt ATTRIBUTION.md
git commit -m "Regenerate THIRD_PARTY_LICENSES.txt"
git push
```

Read the diff before you commit. A new module or a changed license text needs a look, because
third-party code must stay permissively licensed. A new module also needs a row in
[ATTRIBUTION.md](ATTRIBUTION.md). CI does not check that file, so its versions are easy to
forget.

### The golang image

An update of the golang image changes the Go version that releases are built with. The license
check fails for this too, because the file's standard-library section names the Go version. CI's
`test` job also fails, at "Go version matches the Dockerfile". Set `go-version` in
[`.github/workflows/ci.yml`](.github/workflows/ci.yml) to the new golang tag, regenerate the
license file, and commit both. The script reads the golang image from the pull request's
Dockerfile:

```bash
gh pr checkout <number>
sed -n 's/^FROM .*golang:\([0-9.]*\).*/\1/p' Dockerfile   # the new tag, for example 1.27.3
# set go-version: "1.27.3" in .github/workflows/ci.yml
bash scripts/third-party-licenses.sh
git add .github/workflows/ci.yml THIRD_PARTY_LICENSES.txt
git commit -m "Build and test with Go 1.27.3"
git push
```

A new Go minor version, such as 1.26 to 1.27, is a toolchain upgrade rather than a routine update,
so read the Go release notes. In the same pull request or right after it, also move these to the
new version, because nothing checks them:

- the floating `golang:1.26` image in `scripts/go.sh`, `scripts/go.ps1` and the tests in `test/`
- the "Go 1.26" in CONTRIBUTING.md, ATTRIBUTION.md, the badges of both READMEs and the
  development section of docs/guide.md

Updates of the distroless runtime image's digest need nothing extra.

### GitHub Actions

These updates need nothing extra. CI runs the actions of `ci.yml` on the pull request. The actions
that only `release.yml` uses, such as `sigstore/cosign-installer`, `docker/login-action` and
`docker/metadata-action`, first run at the next tag, so read their release notes before you merge.
A cosign-installer update can change the cosign version that signs the image, and with it the
version that users need to verify it.

### Working on Dependabot's branches

- Once you push to a pull request's branch, Dependabot stops rebasing that pull request.
  `@dependabot recreate` rebuilds the pull request without your commit.
- Add your commit last, just before you merge. Merge Go module pull requests one at a time: each
  one rewrites `THIRD_PARTY_LICENSES.txt`, so the next one conflicts until Dependabot rebases it.
  If you already pushed to that pull request, you have to rebase it yourself.
