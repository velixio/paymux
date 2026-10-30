# syntax=docker/dockerfile:1
#
# Build from the repository root: docker build -f deploy/docker/Dockerfile .
# (make docker). The ignore rules are in Dockerfile.dockerignore beside this file.

# Every base image is pinned by digest, with its tag kept for the reader: a tag can
# be moved to other content, a digest cannot. Dependabot (docker) proposes both
# together. Changing GO_VERSION also needs the digest of that golang tag.
# Declared before the first FROM so every FROM line can use it.
ARG GO_VERSION=1.27

# Frontends are compiled once; the runtime images remain static Go binaries.
FROM oven/bun:1.4.4@sha256:1d22548f177f4ff38e0c15baa3c2c46c6c2060ba2d1bc573bb4720b407548f1d AS frontend
WORKDIR /src/frontend
COPY frontend/package.json frontend/bun.lock ./
RUN bun install --frozen-lockfile
COPY frontend ./
# The website's guide catalog is shared with its Go server; its output goes to
# /src/cmd/paymux-home/dist and the dashboard's to /src/web/static/dashboard.
COPY cmd/paymux-home/docs-catalog.json /src/cmd/paymux-home/docs-catalog.json
COPY llms.txt llms-full.txt /src/
RUN bun run typecheck && bun run build

# ---- build -------------------------------------------------------------------
FROM golang:${GO_VERSION}@sha256:f20d83a83e18dbd2a4714b8ffd4b30b287b6243962dd75608e86abcc63ba4efc AS build
WORKDIR /src

# Dependencies first, so a code change does not download them again.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
COPY --from=frontend /src/web/static/dashboard ./web/static/dashboard
COPY --from=frontend /src/cmd/paymux-home/dist ./cmd/paymux-home/dist
ARG VERSION=dev
# Static, CGO-free, no debug info or build paths in the binary.
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/velixio/paymux/pkg/hub.Version=${VERSION}" \
      -o /out/paymux ./cmd/paymux \
 && mkdir -p /out/data \
 && chmod 0700 /out/data

# ---- run ---------------------------------------------------------------------
# distroless static: no shell, no package manager, only the binary and CA certificates.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

COPY --from=build /out/paymux /paymux
# The data directory is owned by the non-root user (65532) so SQLite can write to it,
# and closed to everyone else (mode 0700): it holds the databases.
COPY --from=build --chown=65532:65532 /out/data /data

ENV PAYMUX_DATA_DIR=/data \
    PAYMUX_LISTEN=:8080

USER 65532:65532
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/paymux"]
CMD ["serve"]
