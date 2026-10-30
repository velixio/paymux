#!/usr/bin/env bash
# The CI gate. Run from anywhere:
#
#   bash scripts/ci.sh                  the full gate, in sequence (for local use)
#   bash scripts/ci.sh checks           trailers, gofmt, React typecheck/docs/vitest/build, vet,
#                                       API snapshot, capability matrix, shard map, staticcheck, govulncheck
#   bash scripts/ci.sh test N/M         shard N of M of the race test suite (see below)
#   bash scripts/ci.sh smoke            the smoke test of the real binary (SQLite)
#   bash scripts/ci.sh build            cross builds and the docker build
#   bash scripts/ci.sh browser          the dashboard browser tests
#   bash scripts/ci.sh website          the public website browser tests
#   bash scripts/ci.sh shards [M]       print how the packages are split into M shards (default 6)
#
# GitHub runs the groups as parallel jobs (.github/workflows/ci.yml; the website
# tests have their own job there, so `browser` leaves them out). Together the
# groups run exactly the checks the full gate runs: nothing is skipped in a group
# that the full run does.
#
# The test shards. `test N/M` runs the race test suite for the Go packages of
# shard N. The split is deterministic: packages are placed heaviest first on the
# lightest shard, by the seconds in scripts/ci-shards.txt (measured; a package
# that is not listed counts as 1 second). `checks` fails when a package would run
# in no shard or in two, so a new package can never silently go untested.
# With PAYMUX_TEST_POSTGRES_URL set (GitHub's service container) one run covers
# SQLite and PostgreSQL, PAYMUX_REQUIRE_PG making a missing database a failure;
# without it the shard runs on SQLite (make test) and then on a PostgreSQL started
# in docker (make test-pg), as the full gate does.
set -euo pipefail
cd "$(dirname "$0")/.."

STATICCHECK_VERSION=v0.8.2
GOVULNCHECK_VERSION=v1.9.0
SHARD_WEIGHTS=scripts/ci-shards.txt
DEFAULT_SHARDS=6

step() { printf '\n==> %s\n' "$*"; }

# ---- the steps -------------------------------------------------------------

s_trailers() {
	step "no attribution trailers in commit messages"
	if git log --format=%B HEAD | grep -qiE '^(co-authored-by|signed-off-by|generated-by):'; then
		git log --format='%h %s' -i -E --grep='^(co-authored-by|signed-off-by|generated-by):' HEAD >&2
		echo "commit messages must not carry attribution trailers" >&2
		exit 1
	fi
}

s_gofmt() {
	step "gofmt"
	test -z "$(git ls-files -z "*.go" | xargs -0 gofmt -l)" || { git ls-files -z "*.go" | xargs -0 gofmt -l; echo "gofmt: files need formatting" >&2; exit 1; }
}

s_react() {
	step "React frontends (typecheck and production builds)"
	bun install --cwd frontend --frozen-lockfile
	bun run --cwd frontend typecheck
	bun run --cwd frontend check:docs
	bun run --cwd frontend test:frontend
	bun run --cwd frontend build
}

s_playwright() {
	(cd frontend && bunx playwright install chromium-headless-shell >/dev/null)
}

s_website() {
	step "public website browser tests (same as the GitHub public-browser job)"
	s_playwright
	bun run --cwd frontend test:home
}

s_dashboard() {
	step "dashboard browser tests (seeded hub on a temp data dir)"
	bun run --cwd frontend test:dashboard
}

# The Go receiver (examples/webhook-receiver-go) is tested by the Go test shards.
# The Python and Node receivers are customer-facing examples whose tests need an
# interpreter the project itself does not: they run when python3 or node is
# installed and are skipped, with a message, when it is not.
s_examples() {
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
}

s_vet() {
	step "go vet"
	go vet ./...
	go vet -tags loadtest ./...
}

s_sdk() {
	step "merchant Go SDK (independent module)"
	(cd sdk/go && go vet ./... && go test -race ./...)
}

s_js_sdk() {
	step "merchant JavaScript SDK"
	bun install --cwd sdk/js --frozen-lockfile
	bun run --cwd sdk/js typecheck
	bun run --cwd sdk/js test
	bun run --cwd sdk/js build
}

s_apisnap() {
	step "API snapshot (pkg/... exported API == scripts/apisnap/pkg.txt)"
	go run ./scripts/apisnap -check
}

s_capmatrix() {
	step "Connector capability matrix (docs/capabilities.md == what the connectors declare)"
	go run ./scripts/capmatrix -check
}

s_staticcheck() {
	step "staticcheck ${STATICCHECK_VERSION}"
	go run "honnef.co/go/tools/cmd/staticcheck@${STATICCHECK_VERSION}" ./...
}

s_govulncheck() {
	step "govulncheck ${GOVULNCHECK_VERSION}"
	go run "golang.org/x/vuln/cmd/govulncheck@${GOVULNCHECK_VERSION}" ./...
}

# shard_plan M prints "shard<TAB>import path<TAB>seconds" for every package, in the
# deterministic split described above.
shard_plan() {
	local m="$1" mod
	mod="$(go list -m)"
	go list ./... | awk -v m="$m" -v mod="$mod" -v wf="$SHARD_WEIGHTS" '
		BEGIN {
			while ((getline line < wf) > 0) {
				if (line ~ /^[ \t]*(#|$)/) continue
				split(line, f, /[ \t]+/)
				w[f[1]] = f[2]
			}
		}
		{
			rel = $0; sub("^" mod "/?", "", rel)
			if (rel == "") rel = "."
			sec = (rel in w) ? w[rel] : 1
			printf "%09.3f\t%s\t%s\n", 100000 - sec, $0, sec
		}' |
		sort -t$'\t' -k1,1 -k2,2 |
		awk -F'\t' -v m="$m" '
			BEGIN { for (i = 1; i <= m; i++) load[i] = 0 }
			{
				best = 1
				for (i = 2; i <= m; i++) if (load[i] < load[best]) best = i
				load[best] += $3
				printf "%d\t%s\t%s\n", best, $2, $3
			}'
}

s_shards() {
	local m="${1:-$DEFAULT_SHARDS}"
	step "test shard map (${m} shards: every package in exactly one)"
	local plan
	plan="$(shard_plan "$m")"
	local want got
	want="$(go list ./... | sort)"
	got="$(printf '%s\n' "$plan" | cut -f2 | sort)"
	if [ "$want" != "$got" ]; then
		echo "the shards do not cover every package exactly once" >&2
		diff <(printf '%s\n' "$want") <(printf '%s\n' "$got") >&2 || true
		exit 1
	fi
	printf '%s\n' "$plan" | awk -F'\t' '{ n[$1]++; s[$1] += $3 } END { for (i in n) printf "shard %s: %d packages, about %d s\n", i, n[i], s[i] }' | sort
}

shard_packages() {
	shard_plan "$2" | awk -F'\t' -v n="$1" '$1 == n { print $2 }'
}

# s_test_shard N/M
s_test_shard() {
	local spec="$1" n m
	n="${spec%/*}"
	m="${spec#*/}"
	if ! [[ "$n" =~ ^[0-9]+$ && "$m" =~ ^[0-9]+$ ]] || [ "$n" -lt 1 ] || [ "$n" -gt "$m" ]; then
		echo "usage: scripts/ci.sh test N/M  (1 <= N <= M)" >&2
		exit 2
	fi
	local pkgs
	pkgs="$(shard_packages "$n" "$m" | tr '\n' ' ')"
	if [ -n "${PAYMUX_TEST_POSTGRES_URL:-}" ]; then
		step "race tests, shard $n/$m (SQLite and PostgreSQL)"
		PAYMUX_REQUIRE_PG=1 go test -race -timeout 30m $pkgs
	else
		step "race tests, shard $n/$m (SQLite)"
		make test PKGS="$pkgs"
	fi
	if [[ " $pkgs " == *"/internal/redact "* ]]; then
		s_redact
	fi
	if [ -z "${PAYMUX_TEST_POSTGRES_URL:-}" ]; then
		step "race tests, shard $n/$m (PostgreSQL required)"
		PAYMUX_REQUIRE_PG=1 make test-pg PKGS="$pkgs"
	fi
}

s_redact() {
	step "redaction stays linear (timing test: must run without the race detector)"
	go test ./internal/redact -run 'TestHostileInputIsLinear' -count=1
}

s_test_sqlite() {
	step "make test (SQLite)"
	make test
}

s_test_pg() {
	step "make test-pg (Postgres required)"
	PAYMUX_REQUIRE_PG=1 make test-pg
}

s_smoke() {
	step "smoke test of the real binary (SQLite; scripts/smoke.sh runs PostgreSQL too when asked)"
	PAYMUX_SMOKE_SQLITE_ONLY=1 bash scripts/smoke.sh
}

s_build() {
	step "cross builds (CGO_ENABLED=0)"
	# The hub embeds the dashboard bundle; build it first when it is not there.
	if [ ! -f web/static/dashboard/main.js ]; then
		bun install --cwd frontend --frozen-lockfile
		bun run --cwd frontend build:dashboard
	fi
	mkdir -p bin
	local arch
	for arch in amd64 arm64; do
		CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -o "bin/paymux-linux-$arch" ./cmd/paymux
	done

	step "docker build (skipped when docker is not usable)"
	if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
		docker build -f deploy/docker/Dockerfile --build-arg VERSION=ci -t paymux:ci .
	else
		echo "docker is not installed or its daemon is not reachable: skipping the image build"
	fi
}

# ---- the groups --------------------------------------------------------------

g_checks() {
	s_trailers
	s_gofmt
	s_react
	s_examples
	s_vet
	s_sdk
	s_js_sdk
	s_apisnap
	s_capmatrix
	s_shards "$DEFAULT_SHARDS"
	s_staticcheck
	s_govulncheck
}

g_browser() {
	bun install --cwd frontend --frozen-lockfile
	s_playwright
	s_dashboard
}

g_website() {
	bun install --cwd frontend --frozen-lockfile
	s_website
}

# The full gate keeps the order it always had.
g_full() {
	s_trailers
	s_gofmt
	s_react
	s_website
	s_dashboard
	s_examples
	s_vet
	s_sdk
	s_js_sdk
	s_apisnap
	s_capmatrix
	s_shards "$DEFAULT_SHARDS"
	s_staticcheck
	s_govulncheck
	s_test_sqlite
	s_redact
	s_test_pg
	s_smoke
	s_build
}

case "${1:-full}" in
full) g_full ;;
checks) g_checks ;;
test)
	[ $# -eq 2 ] || { echo "usage: scripts/ci.sh test N/M" >&2; exit 2; }
	s_test_shard "$2"
	;;
smoke) s_smoke ;;
build) s_build ;;
browser) g_browser ;;
website) g_website ;;
shards) s_shards "${2:-$DEFAULT_SHARDS}"; shard_plan "${2:-$DEFAULT_SHARDS}" | sort -n ;;
*)
	echo "usage: scripts/ci.sh [checks | test N/M | smoke | build | browser | website | shards [M]]" >&2
	exit 2
	;;
esac

if [ "${1:-full}" = full ]; then
	step "CI OK"
else
	step "ci.sh ${1}: OK"
fi
