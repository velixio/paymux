# syntax=docker/dockerfile:1

# ---- build -------------------------------------------------------------------
ARG GO_VERSION=1.27
FROM golang:${GO_VERSION} AS build
WORKDIR /src

# Dependencies first, so a code change does not download them again.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
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
