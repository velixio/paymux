# syntax=docker/dockerfile:1
#
# Build from the repository root: docker build -f deploy/docker/Dockerfile .
# (make docker). The ignore rules are in Dockerfile.dockerignore beside this file.

# Declared before the first FROM so every FROM line can use it.
ARG GO_VERSION=1.27

# Frontends are compiled once; the runtime images remain static Go binaries.
FROM oven/bun:1.4.3 AS frontend
WORKDIR /src/frontend
COPY frontend/package.json frontend/bun.lock ./
RUN bun install --frozen-lockfile
COPY frontend ./
# The website's guide catalog is shared with its Go server; its output goes to
# /src/web-home/dist and the dashboard's to /src/web/static/dashboard.
COPY web-home/docs-catalog.json /src/web-home/docs-catalog.json
RUN bun run typecheck && bun run build

# ---- build -------------------------------------------------------------------
FROM golang:${GO_VERSION} AS build
WORKDIR /src

# Dependencies first, so a code change does not download them again.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
COPY --from=frontend /src/web/static/dashboard ./web/static/dashboard
COPY --from=frontend /src/web-home/dist ./web-home/dist
ARG VERSION=dev
# Static, CGO-free, no debug info or build paths in the binary.
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/paymux/paymux/hub.Version=${VERSION}" \
      -o /out/paymux ./cmd/paymux \
 && mkdir -p /out/data

# ---- run ---------------------------------------------------------------------
# distroless static: no shell, no package manager, only the binary and CA certificates.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/paymux /paymux
# The data directory is owned by the non-root user (65532) so SQLite can write to it.
COPY --from=build --chown=65532:65532 /out/data /data

ENV PAYMUX_DATA_DIR=/data \
    PAYMUX_LISTEN=:8080

USER 65532:65532
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/paymux"]
CMD ["serve"]
