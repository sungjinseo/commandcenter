# syntax=docker/dockerfile:1
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/commandcenter ./cmd/commandcenter \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/commandcenter /usr/local/bin/commandcenter
# The licenses and notices of the project and of everything linked into the binary.
COPY LICENSE-MIT LICENSE-APACHE NOTICE THIRD_PARTY_LICENSES.txt /usr/share/doc/commandcenter/
# /data is owned by the nonroot user so a new named volume inherits that ownership.
COPY --from=build --chown=65532:65532 /out/data /data
EXPOSE 8081
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/usr/local/bin/commandcenter", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/commandcenter"]
# No -config: the default is /etc/commandcenter/config.yaml, with the repolite fallback.
CMD ["serve"]
