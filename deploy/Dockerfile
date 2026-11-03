# The server as a container. Build from the repository root:
#
#   docker build -f deploy/Dockerfile --build-arg VERSION=1.2.3 -t raccoonci-server .
#
# The binary is static (the SQLite driver is pure Go), so the image holds the
# binary and the CA certificates the server needs to reach GitHub and the
# compute provider, and nothing else. Data (the database, artifacts and caches
# kept on disk) lives in the /data volume. Secrets are given as files, never
# baked in: see docs/deploy.md.
#
# Until the tag github.com/Raccoon-CI/lib v0.1.0 exists, `go mod
# download` below cannot fetch that dependency, so this build fails inside a
# container (outside one, the go.work in the parent directory covers it).

FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/raccoonci-server ./cmd/raccoonci-server
# The image has no shell, so the data directory is made here and copied below.
RUN mkdir /out/data

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /out/raccoonci-server /raccoonci-server
# Owned by the nonroot user (65532), so that a fresh volume mounted on /data,
# which takes this directory's owner, is writable. A directory of the host
# mounted there instead must be chowned to 65532 by whoever mounts it.
COPY --from=build --chown=65532:65532 /out/data /data
VOLUME /data
EXPOSE 18080
HEALTHCHECK --interval=30s --timeout=6s --start-period=20s --retries=3 \
  CMD ["/raccoonci-server", "-check", "http://127.0.0.1:18080/readyz"]
ENTRYPOINT ["/raccoonci-server", "-data-dir", "/data", "-listen", "0.0.0.0:18080"]
