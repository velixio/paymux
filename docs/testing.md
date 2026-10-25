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
python3 -m unittest discover -s examples/webhook-receiver -p 'test_*.py'   # optional: the Python receiver example
node --test examples/webhook-receiver-js/receiver.test.mjs                  # optional: the Node receiver example
```

The Go receiver example (`examples/webhook-receiver-go`) is tested by `go test ./...`. The Python and Node examples are for customers and need their interpreters, which the project does not: `scripts/ci.sh` runs them when `python3` or `node` is installed and skips them, with a message, when it is not. The smoke test (`bash scripts/smoke.sh`) is a Go program, `scripts/smoke`; it needs only the Go toolchain and, for the PostgreSQL run, `psql`.

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

### Why the suite is fast enough for a small runner

Under the race detector, three costs dominated, and each has one remedy:

- **Migrating a database for every test.** `storetest.EachMigrated` (and `SQLiteMigrated`, `PostgresMigrated`) hand out copies of a template migrated once per migration set: a file copy on SQLite (kept under the system temp directory, named after a hash of the migration files), `CREATE DATABASE ... TEMPLATE` on PostgreSQL (a `paymux_tpl_<hash>` database; the test user needs the right to create databases, or the helper migrates a fresh schema per test as before). A test that has to see an empty database, or its own migrations, uses `storetest.Each`.
- **Password hashing.** In a test binary only (`testing.Testing()`), new argon2id hashes use a tiny cost; the hub, the CLI and custom builds always use 64 MiB and 3 passes, which `internal/auth` tests assert. A test about the cost or the concurrency bound calls `auth.UseProductionCost(t)`.
- **Sizes.** Tests of limits use the smallest size at which the property holds.
- **Wall clocks.** A test about one limit (memory, say) gives every other limit (the script time limit) a generous value, and asserts the property (stopped, failed closed, process alive) with a hang guard of a minute or more rather than how quickly it happened; on a busy shared runner speed is not a property of the code.

On four cores with `-race`, `make test` takes about two minutes and `make test-pg` about three.

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
