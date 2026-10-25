#!/usr/bin/env bash
# Full CI gate. Run from anywhere: bash scripts/ci.sh
set -euo pipefail
cd "$(dirname "$0")/.."

STATICCHECK_VERSION=v0.8.2
GOVULNCHECK_VERSION=v1.9.0

step() { printf '\n==> %s\n' "$*"; }

step "no attribution trailers in commit messages"
if git log --format=%B HEAD | grep -qiE '^(co-authored-by|signed-off-by|generated-by):'; then
  git log --format='%h %s' -i -E --grep='^(co-authored-by|signed-off-by|generated-by):' HEAD >&2
  echo "commit messages must not carry attribution trailers" >&2; exit 1
fi

step "gofmt"
test -z "$(git ls-files -z "*.go" | xargs -0 gofmt -l)" || { git ls-files -z "*.go" | xargs -0 gofmt -l; echo "gofmt: files need formatting" >&2; exit 1; }

step "React frontends (typecheck and production builds)"
bun install --cwd frontend --frozen-lockfile
bun run --cwd frontend typecheck
bun run --cwd frontend check:docs
bun run --cwd frontend test:frontend
bun run --cwd frontend build

step "public website browser tests (same as the GitHub public-browser job)"
(cd frontend && bunx playwright install chromium-headless-shell >/dev/null)
bun run --cwd frontend test:home

step "dashboard browser tests (seeded hub on a temp data dir)"
bun run --cwd frontend test:dashboard

# The Go receiver (examples/webhook-receiver-go) is tested by `make test` with
# everything else. The Python and Node receivers are customer-facing examples whose
# tests need an interpreter the project itself does not: they run when python3 or
# node is installed and are skipped, with a message, when it is not.
step "signed webhook receiver examples (Python and Node: optional)"
if command -v python3 >/dev/null 2>&1; then
  python3 -m unittest discover -s examples/webhook-receiver -p 'test_*.py'
else
  echo "python3 is not installed: skipping the Python receiver example's tests"
fi
if command -v node >/dev/null 2>&1; then
  node --test examples/webhook-receiver-js/receiver.test.mjs
else
  echo "node is not installed: skipping the JavaScript receiver example's tests"
fi

step "go vet"
go vet ./...
go vet -tags loadtest ./...

step "API snapshot (pkg/... exported API == scripts/apisnap/pkg.txt)"
go run ./scripts/apisnap -check

step "Connector capability matrix (docs/capabilities.md == what the connectors declare)"
go run ./scripts/capmatrix -check

step "staticcheck ${STATICCHECK_VERSION}"
go run "honnef.co/go/tools/cmd/staticcheck@${STATICCHECK_VERSION}" ./...

step "govulncheck ${GOVULNCHECK_VERSION}"
go run "golang.org/x/vuln/cmd/govulncheck@${GOVULNCHECK_VERSION}" ./...

step "make test (SQLite)"
make test

step "redaction stays linear (timing test: must run without the race detector)"
go test ./internal/redact -run 'TestHostileInputIsLinear' -count=1

step "make test-pg (Postgres required)"
PAYMUX_REQUIRE_PG=1 make test-pg

step "smoke test of the real binary (SQLite; scripts/smoke.sh runs PostgreSQL too when asked)"
PAYMUX_SMOKE_SQLITE_ONLY=1 bash scripts/smoke.sh

step "cross builds (CGO_ENABLED=0)"
mkdir -p bin
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -o "bin/paymux-linux-$arch" ./cmd/paymux
done

step "docker build (skipped when docker is not usable)"
if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
  docker build -f deploy/docker/Dockerfile --build-arg VERSION=ci -t paymux:ci .
else
  echo "docker is not installed or its daemon is not reachable: skipping the image build"
fi

step "CI OK"
