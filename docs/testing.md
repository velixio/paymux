# Testing and coverage

Paymux has Go package tests, frontend unit/contract tests, Chromium browser checks, and executable integration examples. They test different failure boundaries; a browser route passing is not a statement-coverage percentage.

The [measured coverage snapshot](coverage-status.md) records the current gaps; repository-wide 100% has not been reached.

## Run the checks

```sh
bun install --cwd frontend --frozen-lockfile
bun run --cwd frontend check:docs
bun run --cwd frontend typecheck
bun run --cwd frontend test:frontend
(cd frontend && bunx playwright install chromium)
bun run --cwd frontend test:home
go test ./...
python3 -m unittest discover -s examples/webhook-receiver -p 'test_*.py'
```

The full CI gate is `bash scripts/ci.sh`. It also runs static analysis, API/capability snapshot checks, database tests, smoke checks, and release/build checks. See the Makefile for database-specific targets.

## Go coverage across the repository

```sh
bun run --cwd frontend coverage:go
```

This instruments `./...` with `-coverpkg=./...` so an integration test can cover production code in another package. It emits a Go profile, function report, HTML report, and JSON statement totals under `coverage/go/`.

The command enforces a **100% target** by default and returns nonzero when that target is not met or a test fails. It does not mean the repository has reached 100%. To collect a baseline without enforcing the target, use `PAYMUX_COVERAGE_MIN=0 bun run --cwd frontend coverage:go`; test failures still fail the command.

A cross-package profile can contain the same source block from several test binaries. The summary counts each source block's statements once and marks it covered if any test hit it, matching the union represented by `go tool cover` rather than inflating the denominator.

## Database coverage

SQLite is the default local database. PostgreSQL paths need an actual test database:

```sh
export PAYMUX_TEST_POSTGRES_URL=postgres://user:password@localhost:5432/paymux_test?sslmode=disable
PAYMUX_REQUIRE_PG=1 bun run --cwd frontend coverage:go
```

Use an isolated test database. Without a configured server, PostgreSQL-specific tests may skip; that is a limitation to report, not a production code exclusion. CI provisions PostgreSQL for its database gate. A passed SQLite run does not prove PostgreSQL fault/recovery behavior.

## Frontend coverage

```sh
bun run --cwd frontend coverage:frontend
```

Vitest/V8 reports statements, branches, functions, and lines across shared frontend components, the dashboard source, and the public website source, including files that no unit test loads. Test files are excluded from production-code metrics. HTML, LCOV, and JSON summaries are written under `coverage/frontend/`.

The frontend target is 100% for all four metrics. Browser tests separately exercise the actual built site, CSP, theme persistence, keyboard behavior, copy controls, mobile layout, every published documentation route, and payment demo outcomes. Browser results are not silently added to an unrelated unit-coverage report.

## Example tests

The Go HTTP client has an `httptest` server that verifies method, path, headers, responses, and errors. The Python receiver tests valid/tampered/stale signatures, durable deduplication, and HTTP success/error handling.

The receiver is a local learning example. Its inbox records received events but does not implement your fulfillment transaction or production application worker.

## Interpreting the 100% target

Measure before claiming completion. A clean test run, 100% coverage of a helper, and 100% coverage of the entire repository are three different claims. Keep uncovered error paths, CLI entrypoints, browser code, transport failures, and database-specific behavior visible. Do not remove code from the metric or weaken assertions to meet the target.

Go statement coverage, JavaScript branch coverage, Python coverage, and shell workflow execution do not share a meaningful combined percentage. Report each measured language/suite and its gaps. Provider sandbox tests remain conditional on credentials; passing fakes is not proof of live-provider compatibility.
