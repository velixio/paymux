#!/usr/bin/env bash
# Full CI gate. Run from anywhere: bash scripts/ci.sh
set -euo pipefail
cd "$(dirname "$0")/.."

STATICCHECK_VERSION=v0.8.2
GOVULNCHECK_VERSION=v1.9.0

step() { printf '\n==> %s\n' "$*"; }

step "gofmt"
test -z "$(git ls-files -z "*.go" | xargs -0 gofmt -l)" || { git ls-files -z "*.go" | xargs -0 gofmt -l; echo "gofmt: files need formatting" >&2; exit 1; }

step "go vet"
go vet ./...

step "staticcheck ${STATICCHECK_VERSION}"
go run "honnef.co/go/tools/cmd/staticcheck@${STATICCHECK_VERSION}" ./...

step "govulncheck ${GOVULNCHECK_VERSION}"
go run "golang.org/x/vuln/cmd/govulncheck@${GOVULNCHECK_VERSION}" ./...

step "make test (SQLite)"
make test

step "make test-pg (Postgres required)"
PAYMUX_REQUIRE_PG=1 make test-pg

step "cross builds (CGO_ENABLED=0)"
mkdir -p dist
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -o "dist/paymux-linux-$arch" ./cmd/paymux
done

step "CI OK"
