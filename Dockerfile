# syntax=docker/dockerfile:1
# Base images are pinned by digest and updated by Dependabot. When the golang tag
# changes, set go-version in .github/workflows/ci.yml to it and rerun
# scripts/third-party-licenses.sh; CI fails until both are done.
# The build stage runs on the build machine and cross-compiles for the target.
FROM --platform=$BUILDPLATFORM golang:1.26.10@sha256:672c944cfd07599b5f1bb179869270cdc94c6254e41db4d431259693cb286b82 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/commandcenter ./cmd/commandcenter \
 && mkdir -p /out/data /out/backup

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/commandcenter /usr/local/bin/commandcenter
# The licenses and notices of the project and of everything linked into the binary.
COPY LICENSE-MIT LICENSE-APACHE NOTICE THIRD_PARTY_LICENSES.txt /usr/share/doc/commandcenter/
# /data is owned by the nonroot user so a new named volume inherits that ownership.
COPY --from=build --chown=65532:65532 /out/data /data
# /backup (backupDir) likewise, for a named volume; a bind mount keeps the host directory's owner.
COPY --from=build --chown=65532:65532 /out/backup /backup
EXPOSE 8081
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/usr/local/bin/commandcenter", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/commandcenter"]
# No -config: the default is /etc/commandcenter/config.yaml, with the repolite fallback.
CMD ["serve"]
