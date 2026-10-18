# syntax=docker/dockerfile:1

# Declared before the first FROM so every FROM line can use it.
ARG GO_VERSION=1.27

# Frontends are compiled once; the runtime images remain static Go binaries.
FROM oven/bun:1.4.3 AS frontend
WORKDIR /src
COPY package.json bun.lock ./
RUN bun install --frozen-lockfile
COPY frontend ./frontend
COPY web-dashboard ./web-dashboard
COPY web-home ./web-home
COPY tsconfig.json tsconfig.test.json ./
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
      -ldflags "-s -w -X github.com/paymux/paymux.Version=${VERSION}" \
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
