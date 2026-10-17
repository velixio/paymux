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
go vet -tags loadtest ./...

step "API snapshot (pkg/... exported API == api/pkg.txt)"
go run ./scripts/apisnap -check

step "staticcheck ${STATICCHECK_VERSION}"
go run "honnef.co/go/tools/cmd/staticcheck@${STATICCHECK_VERSION}" ./...

step "govulncheck ${GOVULNCHECK_VERSION}"
go run "golang.org/x/vuln/cmd/govulncheck@${GOVULNCHECK_VERSION}" ./...

step "make test (SQLite)"
make test

step "make test-pg (Postgres required)"
PAYMUX_REQUIRE_PG=1 make test-pg

step "smoke test of the real binary (SQLite; scripts/smoke.sh runs PostgreSQL too when asked)"
PAYMUX_SMOKE_SQLITE_ONLY=1 bash scripts/smoke.sh

step "cross builds (CGO_ENABLED=0)"
mkdir -p dist
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -o "dist/paymux-linux-$arch" ./cmd/paymux
done

step "docker build (skipped when docker is not usable)"
if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
  docker build --build-arg VERSION=ci -t paymux:ci .
else
  echo "docker is not installed or its daemon is not reachable: skipping the image build"
fi

step "CI OK"
