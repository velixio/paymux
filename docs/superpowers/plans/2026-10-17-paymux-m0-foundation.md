# Paymux M0 — Foundation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. **Do not start until the user explicitly says to begin implementation.**

**Goal:** A `paymux` binary that boots on SQLite or Postgres with identical behavior. It migrates the database, runs a durable job queue and scheduler, and serves `/healthz`, `/readyz` and an internal metrics listener. Config comes from the environment or `.env`, with a guarded dev mode. `make ci` checks the architectural seams on both databases.

**Architecture:**
- Thin `cmd/paymux/main.go` → `internal/cli` → public `paymux.New(cfg).Start(ctx)`.
- All SQL lives in `internal/store`. Every SQLite/Postgres difference lives in `internal/store/dialect`.
- The job queue is an in-database outbox: `Enqueue` takes the caller's `store.Tx`, so a rolled-back transaction never leaves a job behind.

**Tech Stack:**
- Go 1.27 (`toolchain go1.27.2`, auto-downloaded)
- `modernc.org/sqlite`
- `github.com/jackc/pgx/v5/stdlib`
- stdlib `net/http`, `log/slog`, `crypto/aes`/`cipher`

**Spec:** `docs/superpowers/specs/2026-10-17-paymux-design.md` v2 (§3, §4, §7, §12, §13, §14). Roadmap: `docs/superpowers/plans/2026-10-17-paymux-roadmap.md`.

## Global Constraints

- **Toolchain:** `go.mod` declares `go 1.27` and `toolchain go1.27.2`. Never require a manual Go install: any installed Go ≥1.21 with `GOTOOLCHAIN=auto` (the default) must work through `make build`.
- **Build:** `CGO_ENABLED=0`. Builds must succeed for `linux/amd64` and `linux/arm64`.
- **Dependencies allowed in M0:**
  - `modernc.org/sqlite`;
  - `github.com/jackc/pgx/v5` (stdlib adapter only);
  - `github.com/google/uuid`, only if stdlib `uuid` lacks v7 (Task 4).

  Each one gets a line in `docs/dependencies.md`.
- **Module path:** `github.com/paymux/paymux` (placeholder).
- **Storage types:**
  - timestamps `BIGINT` unix **milliseconds** UTC;
  - money `int64` minor units;
  - IDs `TEXT`;
  - JSON `TEXT`.
- **Env vars (exact names and defaults):**

  | Variable | Default / rule |
  |---|---|
  | `PAYMUX_ENV` | `production`; allowed values `production`, `development` |
  | `PAYMUX_BASE_URL` | required |
  | `PAYMUX_LISTEN` | `:8080` |
  | `PAYMUX_INTERNAL_LISTEN` | `127.0.0.1:9100` |
  | `PAYMUX_DATA_DIR` | `./data` |
  | `PAYMUX_DATABASE_URL` | `sqlite://<PAYMUX_DATA_DIR>/paymux.db`; also accepts `sqlite:///abs/path.db` or `postgres://…` |
  | `PAYMUX_MASTER_KEY` / `PAYMUX_MASTER_KEY_FILE` | exactly one is required |
  | `PAYMUX_TZ` | `Asia/Kolkata` |
  | `PAYMUX_DEV_SKIP_2FA` | `false` |
  | `PAYMUX_ENV_FILE` | `.env` |
  | `PAYMUX_TEST_POSTGRES_URL`, `PAYMUX_REQUIRE_PG` | tests only |

- **IDs:**
  - public: `<prefix>_<26 lowercase Crockford base32 of a UUIDv7>`;
  - `provider_ref`: 20 uppercase Crockford chars.
- **Seams:**
  - No file outside `internal/store/` imports `database/sql`, `modernc.org/sqlite` or `github.com/jackc/pgx`.
  - `cmd/paymux/main.go` is ≤ 25 non-blank lines.
- **Errors and logs never contain the database password or the master key.**
- **Commits:** plain conventional-commit messages. **Never** add `Co-Authored-By` or any attribution trailer, and never put a private email anywhere (code, fixtures, docs, commits, git config). Tests use `example.com` addresses.

## Review Focus

1. **Process killed while a job runs:** the lease expires and the job is rerun, on both DBs. Pinned in Task 8 by `TestExpiredLeaseIsReclaimed`.
2. **Postgres unreachable or wrong password at startup:** the error names `PAYMUX_DATABASE_URL` and the host but **not** the password. Pinned in Task 6 by `TestOpenPostgresErrorRedactsPassword`.
3. **Two processes on the same database:**
   - SQLite: the second refuses with `store.ErrLocked`.
   - Postgres: both run, and each scheduled job runs exactly once.

   Pinned in Task 6 by `TestSecondSQLiteOpenIsLocked` and Task 9 by `TestTwoInstancesScheduleOnce`.
4. **Migration fails halfway:** earlier versions stay applied and the failing migration leaves no partial tables, on both DBs. The error names the file. On SQLite, the pre-migration snapshot exists. Pinned in Task 7 by `TestFailedMigrationRollsBack`.
5. **Master key file with a trailing newline, or `.env` with quotes, comments or CRLF line endings:** parsed correctly. A wrong key length gives an error naming the variable. Pinned in Task 2 by `TestMasterKeyFileTrimmed` and `TestDotEnvParsing`.

---

### Task 1: Repository, module, Makefile, thin main, `version`

**Files:**
- Create:
  - `go.mod`
  - `.gitignore` (`/data/`, `/dist/`, `.env`)
  - `.env.example`
  - `Makefile`
  - `cmd/paymux/main.go`
  - `paymux.go`
  - `internal/cli/cli.go`
  - `docs/dependencies.md`
- Test: `internal/cli/cli_test.go`

**Interfaces:**
- Produces:
  - `paymux.Version string` (default `"dev"`, set via `-ldflags -X`)
  - `cli.Run(ctx context.Context, args []string, lookupEnv func(string) (string, bool), stdout, stderr io.Writer) int`

- [ ] **Step 1:** Run `git init`, then `go mod init github.com/paymux/paymux` and `go mod edit -go=1.27 -toolchain=go1.27.2`. Then run `go version` in the repo. Expected: `go1.27.2` (auto-downloaded; the machine has 1.26.0).
- [ ] **Step 2: Write failing tests:**
  - `TestRunVersion`: `["version"]` returns 0, and stdout is `"paymux dev\n"`.
  - `TestRunUnknownCommand`: returns 2, and stderr contains `unknown command "bogus"` and `usage: paymux <serve|migrate|version>`.
  - `TestRunNoArgs`: returns 2 and prints usage.
- [ ] **Step 3:** Run `go test ./internal/cli/`. Expected: FAIL.
- [ ] **Step 4: Implement `cli.Run`** as a stdlib switch. `serve` and `migrate` return 1 with "not implemented" until Tasks 7 and 10. `main.go` uses `signal.NotifyContext(SIGINT, SIGTERM)`, blank-imports `time/tzdata`, and calls `os.Exit(cli.Run(...))`.
- [ ] **Step 5: Write the `Makefile`.** Targets:
  - `build`: `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/paymux/paymux.Version=$(VERSION)" -o dist/paymux ./cmd/paymux`, where `VERSION ?= $(shell git describe --always --dirty 2>/dev/null || echo dev)`.
  - `test`: `go test -race ./...`.
  - `test-pg`: runs `go test -race ./...` with `PAYMUX_REQUIRE_PG=1`. If `PAYMUX_TEST_POSTGRES_URL` is unset and `docker` exists, it starts `postgres:17` on a random port, exports the URL, and removes the container afterwards. Otherwise it fails with `set PAYMUX_TEST_POSTGRES_URL or install docker`.
  - `test-all`: runs `test` and `test-pg`.
  - `fmt`: `gofmt -w .`.
  - `ci`: `scripts/ci.sh` (Task 11).
  - `run-dev`: `go run ./cmd/paymux serve`, which picks up `.env`.

  `.env.example` lists every variable from Global Constraints with dev-safe values, including `PAYMUX_ENV=development`, `PAYMUX_BASE_URL=http://localhost:8080` and `PAYMUX_DEV_SKIP_2FA=true`. The master key line holds a placeholder plus a comment showing how to generate one: `head -c32 /dev/urandom | base64`.
- [ ] **Step 6:** Run `make build && ./dist/paymux version && make test`. Expected: the binary prints `paymux <version>`, and the tests pass.
- [ ] **Step 7: Commit** `chore: bootstrap module, Makefile and CLI skeleton`.

### Task 2: Config from env and `.env`, with the dev-mode guard

**Files:**
- Create: `internal/config/config.go`, `internal/config/dotenv.go`
- Test: `internal/config/config_test.go`, `internal/config/dotenv_test.go`

**Interfaces:**
- Produces:

```go
type Env string // "production" | "development"
type Config struct {
    Env            Env
    BaseURL        *url.URL
    Listen, InternalListen, DataDir string
    DatabaseURL    string           // normalised; sqlite default resolved against DataDir
    MasterKey      []byte           // 32 bytes
    Location       *time.Location
    DevSkip2FA     bool
}
func Load(lookupEnv func(string) (string, bool)) (Config, error)
func LoadDotEnv(path string) (map[string]string, error)          // missing file => empty map, nil error
func WithDotEnv(lookupEnv func(string) (string, bool), file map[string]string) func(string) (string, bool) // real env wins
func (c Config) RedactedDatabaseURL() string                      // password replaced by "xxxxx"
```

- [ ] **Step 1: Write failing tests:**
  - `TestDefaults`: only `BASE_URL` and a valid `MASTER_KEY` are set. Expected: Env `production`, Listen `:8080`, InternalListen `127.0.0.1:9100`, DataDir `./data`, DatabaseURL `sqlite://data/paymux.db` (cleaned path) and Location `Asia/Kolkata`.
  - `TestMissingBaseURL`: error contains `PAYMUX_BASE_URL is required`.
  - `TestBaseURLRules`: `http://` is allowed only for `localhost`, `127.0.0.1`, `*.localhost` and `*.test`. `ftp://x` and `pay.example.com` give errors naming the variable.
  - `TestMasterKeyBothSet`, `TestMasterKeyWrongLength` (message `PAYMUX_MASTER_KEY must decode to 32 bytes, got 16`), `TestMasterKeyFileTrimmed` (content plus `"\n"` loads).
  - `TestBadTZ`: `Mars/Base` gives an error naming `PAYMUX_TZ`.
  - `TestBadEnv`: `PAYMUX_ENV=staging` gives `PAYMUX_ENV must be production or development`.
  - `TestDatabaseURLForms`:
    - `sqlite:///var/lib/paymux/p.db` is accepted;
    - `postgres://u:p@h:5432/db?sslmode=disable` is accepted;
    - `mysql://x` gives `PAYMUX_DATABASE_URL must start with sqlite:// or postgres://`.
  - `TestRedactedDatabaseURL`: `postgres://u:s3cret@h/db` becomes `postgres://u:xxxxx@h/db`.
  - `TestDevSkip2FAGuard` (table):

    | `PAYMUX_ENV` | Base URL | `DEV_SKIP_2FA` | Result |
    |---|---|---|---|
    | development | `http://localhost:8080` | true | ok, `DevSkip2FA=true` |
    | development | `https://pay.example.com` | true | error `PAYMUX_DEV_SKIP_2FA is only allowed in development on a local host` |
    | production | `http://localhost:8080` | true | same error |
    | production | any | unset or false | ok, `DevSkip2FA=false` |
    | any | any | `maybe` | error naming the variable (must be true or false) |

  - `TestDotEnvParsing` (`dotenv_test.go`):
    - `KEY=value`, `KEY="quoted value"`, `KEY='single'` and `export KEY=v` all parse;
    - `# comment` lines and blank lines are skipped;
    - CRLF endings work, and an inline ` # comment` after an unquoted value is dropped;
    - `KEY=` gives an empty string;
    - a malformed line `NOEQUALS` gives an error with its line number.
  - `TestRealEnvWinsOverDotEnv`.
- [ ] **Step 2:** Run `go test ./internal/config/`. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - Use `errors.Join` so every problem is reported at once.
  - `cli` builds `lookupEnv` as `WithDotEnv(os.LookupEnv, LoadDotEnv(env PAYMUX_ENV_FILE or ".env"))`.
- [ ] **Step 4:** Run the tests. Expected: PASS.
- [ ] **Step 5: Commit** `feat(config): env and .env loading with dev-mode guard`.

### Task 3: Structured logging with redaction

**Files:**
- Create: `internal/logging/logging.go`
- Test: `internal/logging/logging_test.go`

**Interfaces:**
- Produces: `func New(w io.Writer, level slog.Level) *slog.Logger`. It writes JSON lines with redaction through `ReplaceAttr`.

- [ ] **Step 1: Write failing tests:**
  - `TestRedactsSecretKeys`: keys whose lowercase form ends in `secret`, `password`, `token`, `authorization`, `cookie`, `signature`, `api_key` or `master_key`, including nested groups, come out as `"[REDACTED]"`. `request_id` and `idempotency_key` are unchanged.
  - `TestMasksEmail`: `riya.sharma@example.com` becomes `r***@example.com`.
  - `TestMasksCardLikeDigits`: `4111 1111 1111 1111` and `4111111111111111` become `[PAN]`.
  - `TestMasksPhone`: `+91 98765 43210` becomes `******3210`.
  - `TestMasksGSTIN`: `27AAPFU0939F1ZV` becomes `27**********1ZV`.
  - `TestMasksURLPassword`: `postgres://u:s3cret@h/db` becomes `postgres://u:xxxxx@h/db` inside any string value.
  - `TestLeavesIDsAlone`: `pay_01jbx…` and a 20-char provider ref are unchanged.
- [ ] **Step 2:** Run the tests. Expected: FAIL.
- [ ] **Step 3: Implement** with package-level compiled regexes, in this order: URL credentials, PAN (13–19 digits with optional separators), GSTIN (`\b\d{2}[A-Z]{5}\d{4}[A-Z][1-9A-Z]Z[0-9A-Z]\b`), phone (10–12 digits), email.
- [ ] **Step 4:** Run the tests. Expected: PASS.
- [ ] **Step 5: Commit** `feat(logging): JSON slog with secret and PII redaction`.

### Task 4: IDs and provider references

**Files:**
- Create: `internal/ids/ids.go`
- Test: `internal/ids/ids_test.go`

**Interfaces:**
- Produces:

```go
func NewGenerator(clock func() time.Time, rand io.Reader) *Generator
func (g *Generator) New(prefix string) string   // prefix must match ^[a-z]{2,5}$, else panic
func (g *Generator) ProviderRef() string        // 20 chars
func New(prefix string) string; func ProviderRef() string // Default generator (time.Now, crypto/rand)
```

- [ ] **Step 1:** Run `go doc uuid`. If it exposes v7, use it. Otherwise add `github.com/google/uuid` with a justification line. In both cases, assemble v7 from the injected clock and rand per RFC 9562, so tests are deterministic.
- [ ] **Step 2: Write failing tests:**
  - `TestNewFormat`: `^pay_[0-9a-hjkmnp-tv-z]{26}$`.
  - `TestNewPanicsOnBadPrefix`: `""`, `"Pay"` and `"pa_y"` each panic.
  - `TestProviderRefFormat`: `^[0-9A-HJKMNP-TV-Z]{20}$`.
  - `TestSortableByTime`: with a clock stepping 1ms, 1,000 values are strictly increasing, for both generators.
  - `TestUniqueSameMillisecond`: 100,000 `ProviderRef()` values with a frozen clock have no duplicates.
- [ ] **Step 3:** Run the tests. Expected: FAIL.
- [ ] **Step 4: Implement.** `ProviderRef` is a 48-bit ms timestamp (10 chars) followed by 50 random bits (10 chars).
- [ ] **Step 5:** Run the tests. Expected: PASS.
- [ ] **Step 6: Commit** `feat(ids): prefixed UUIDv7 IDs and short provider refs`.

### Task 5: Money helpers and secrets box

**Files:**
- Create: `pkg/connector/money/money.go`, `internal/secrets/box.go`
- Test: `pkg/connector/money/money_test.go`, `internal/secrets/box_test.go`

**Interfaces:**
- Produces:

```go
// money
func Exponent(currency string) (int, error)
func ParseDecimal(s, currency string) (int64, error)
func FormatDecimal(minor int64, currency string) (string, error)
var ErrUnknownCurrency, ErrInvalidAmount error
// secrets
func NewBox(active []byte, older ...[]byte) (*Box, error)
func KeyID(key []byte) string // first 8 hex of sha256(key)
func (b *Box) Seal(plaintext, aad []byte) ([]byte, error)
func (b *Box) Open(ciphertext, aad []byte) ([]byte, error)
var ErrUnknownKey, ErrDecrypt error
```

Secrets format: `0x01 | len(keyID) | keyID | 12-byte nonce | GCM ciphertext+tag`.

- [ ] **Step 1: Write failing money tests.**
  - `TestParseDecimal`:

    | Input | Currency | Result |
    |---|---|---|
    | `"499.00"` | INR | 49900 |
    | `"499"` | INR | 49900 |
    | `"499.5"` | INR | 49950 |
    | `"0.01"` | INR | 1 |
    | `"500"` | JPY | 500 |
    | `"1.234"` | KWD | 1234 |
    | `"499.005"` | INR | `ErrInvalidAmount` |
    | `"500.1"` | JPY | `ErrInvalidAmount` |
    | `""`, `" 1"`, `"1e3"`, `"-1"`, `"+1"`, `"1,000"`, `"NaN"` | INR | `ErrInvalidAmount` |
    | `"92233720368547758.08"` | INR | `ErrInvalidAmount` (overflow) |
    | `"1"` | `"XYZ"` | `ErrUnknownCurrency` |
    | `"1"` | `"inr"` | `ErrUnknownCurrency` |
    | `"1"` | BDT | 100 |

  - `TestFormatDecimal`: 49900 INR gives `"499.00"`, 500 JPY gives `"500"`, 1234 KWD gives `"1.234"`, and a negative input gives `ErrInvalidAmount`.
  - `TestRoundTrip`: every currency at 0, 1, 99 and 123456789.
- [ ] **Step 2: Write failing secrets tests:**
  - `TestRoundTrip`, `TestTamperedCiphertextFails`, `TestWrongAADFails`, `TestUnknownKeyID`.
  - `TestRotationOpensOld`: `NewBox(B, A)` opens data sealed with A, and new seals use B.
  - `TestNonceUnique`, `TestKeyLength` (31 bytes gives an error).
- [ ] **Step 3:** Run `go test ./pkg/connector/money/ ./internal/secrets/`. Expected: FAIL.
- [ ] **Step 4: Implement.**
  - Money uses integer digit arithmetic, never floats.
  - Zero-decimal set: BIF CLP DJF GNF ISK JPY KMF KRW PYG RWF UGX UYI VND VUV XAF XOF XPF.
  - Three-decimal set: BHD IQD JOD KWD LYD OMR TND.
  - Every other active ISO-4217 code uses 2.
  - Secrets use `crypto/aes`, `cipher.NewGCM` and `crypto/rand`.
- [ ] **Step 5:** Run the tests. Expected: PASS.
- [ ] **Step 6: Commit** `feat: money helpers and AES-GCM secrets box`.

### Task 6: Store — open both dialects, lock, transactions

**Files:**
- Create:
  - `internal/store/store.go`
  - `internal/store/open_sqlite.go`
  - `internal/store/open_postgres.go`
  - `internal/store/lock_unix.go` (`//go:build unix`)
  - `internal/store/dialect/dialect.go`
  - `internal/store/dialect/sqlite.go`
  - `internal/store/dialect/postgres.go`
  - `internal/store/storetest/storetest.go`
- Test: `internal/store/store_test.go`, `internal/store/dialect/dialect_test.go`
- Modify: `docs/dependencies.md`. Add `modernc.org/sqlite` (pure-Go SQLite; keeps `CGO_ENABLED=0`; required by Litestream library mode) and `github.com/jackc/pgx/v5` (maintained pure-Go Postgres driver via `database/sql` adapter).

**Interfaces:**
- Produces:

```go
// store
type DB struct{ /* writer, reader *sql.DB; d dialect.Dialect; unlock func() */ }
type Tx struct{ /* unexported */ }       // opaque outside internal/store
type Querier struct{ /* unexported */ }
func Open(ctx context.Context, databaseURL, dataDir string, logger *slog.Logger) (*DB, error)
func (db *DB) Dialect() dialect.Dialect
func (db *DB) Write(ctx context.Context, fn func(tx Tx) error) error   // commit on nil; rollback on error or panic (panic re-raised)
func (db *DB) Read(ctx context.Context, fn func(q Querier) error) error
func (db *DB) Ping(ctx context.Context) error                          // write-path probe
func (db *DB) Close() error
var ErrLocked error

// dialect
type Dialect interface {
    Name() string                    // "sqlite" | "postgres"
    Rebind(query string) string      // '?' -> '$1..$n' for postgres (ignores '?' inside single-quoted literals)
    BlobType() string                // "BLOB" | "BYTEA"
    ForUpdate() string               // "" | " FOR UPDATE"
    SkipLocked() string              // "" | " FOR UPDATE SKIP LOCKED"
    SupportsSnapshot() bool          // sqlite true
}

// storetest
func Each(t *testing.T, fn func(t *testing.T, db *store.DB)) // runs fn as subtests "sqlite" and "postgres"
```

- **SQLite:**
  - Writer DSN: `file:<path>?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate`, with `MaxOpenConns(1)`.
  - Reader pool: same pragmas, `MaxOpenConns(4)`.
  - Takes a `flock` on `<dir of db file>/paymux.lock`.
- **Postgres:**
  - One pool (`MaxOpenConns(10)`) used for both reads and writes, no file lock.
  - Transactions use `ReadCommitted`, with explicit locks where needed.
- **`storetest.Each`:**
  - The `postgres` subtest creates a schema `t_<random>`, opens with `search_path=t_<random>` added to `PAYMUX_TEST_POSTGRES_URL`, and drops the schema in cleanup.
  - Without the URL, it calls `t.Skip`, unless `PAYMUX_REQUIRE_PG=1`, in which case it calls `t.Fatal("PAYMUX_REQUIRE_PG=1 but PAYMUX_TEST_POSTGRES_URL is unset")`.

- [ ] **Step 1: Write failing tests:**
  - `TestRebind`:
    - `SELECT ? , ?` becomes `SELECT $1 , $2`;
    - `WHERE a = '?' AND b = ?` becomes `WHERE a = '?' AND b = $1`;
    - SQLite leaves it unchanged.
  - `TestOpenSQLiteCreatesDirAndPragmas`: dir created with mode 0700; `journal_mode=wal`, `foreign_keys=1`.
  - `TestSecondSQLiteOpenIsLocked`: `errors.Is(err, ErrLocked)`; succeeds after the first `Close`.
  - `TestOpenUnwritableDir`: dir 0500 gives an error containing the path (skipped as root).
  - `TestOpenPostgresErrorRedactsPassword`: `postgres://u:s3cret@127.0.0.1:1/db` gives an error that contains `PAYMUX_DATABASE_URL` and `127.0.0.1:1`, and does not contain `s3cret`.
  - Using `storetest.Each`:
    - `TestWriteCommits`;
    - `TestWriteRollsBackOnError`;
    - `TestWriteRollsBackOnPanic` (panic re-raised);
    - `TestPing`.

    These need a scratch table created in-test through `Write`.
- [ ] **Step 2:** Run `go test ./internal/store/...`. Expected: FAIL. Then run `make test-pg`. Expected: FAIL.
- [ ] **Step 3: Implement.** URL scheme dispatch happens in `Open`. Errors are wrapped as `fmt.Errorf("open %s: %w", redacted, err)`, using `config`'s redaction rule copied into `store`. Use a small shared helper; do not import `config` into `store`.
- [ ] **Step 4:** Run `make test-all`. Expected: PASS on both.
- [ ] **Step 5: Commit** `feat(store): SQLite and Postgres open, locking, transactions, dialects`.

### Task 7: Migrations in lockstep, schema parity, SQLite snapshots

**Files:**
- Create:
  - `internal/store/migrate.go`
  - `internal/store/schema.go`
  - `migrations/embed.go` (`//go:embed sqlite/*.sql postgres/*.sql`; `var FS embed.FS`)
  - `migrations/sqlite/0001_init.sql`
  - `migrations/postgres/0001_init.sql`
- Test: `internal/store/migrate_test.go`, `internal/store/schema_test.go`
- Modify: `internal/cli/cli.go` (wire `migrate`)

**Interfaces:**
- Produces:

```go
func (db *DB) Migrate(ctx context.Context, fsys fs.FS) error              // fsys root has sqlite/ and postgres/; picks Dialect().Name()
func (db *DB) PendingMigrations(ctx context.Context, fsys fs.FS) (int, error)
type Schema struct{ Tables map[string]Table }                              // normalised
type Table struct{ Columns map[string]Column; Indexes map[string]Index }
type Column struct{ Type string; NotNull bool }                            // Type in {"int","text","bytes"}
type Index struct{ Columns []string; Unique bool; Partial bool }
func (db *DB) DescribeSchema(ctx context.Context) (Schema, error)          // excludes schema_migrations
```

- **`0001_init.sql`** (both dialects, same names) creates:
  - `orgs(id TEXT PRIMARY KEY, name TEXT NOT NULL, created_at BIGINT NOT NULL)`;
  - `jobs` and `schedules`, with columns as in Task 8 and Task 9.

  `schema_migrations(version BIGINT PRIMARY KEY, name TEXT NOT NULL, applied_at BIGINT NOT NULL)` is created by `Migrate`.
- **Type normalization:**
  - `INTEGER`, `BIGINT`, `INT8` → `int`;
  - `TEXT`, `VARCHAR` → `text`;
  - `BLOB`, `BYTEA` → `bytes`.

- [ ] **Step 1: Write failing tests:**
  - With `storetest.Each`:
    - `TestMigrateFreshAndIdempotent` (MapFS with `sqlite/0001_a.sql, 0002_b.sql` and the Postgres equivalents);
    - `TestFailedMigrationRollsBack`: 0003 has a valid `CREATE TABLE x` and then invalid SQL. Expected: error contains `0003_`, max version stays 2, and table `x` does not exist.
    - `TestMigrationFilenameValidation`: `1_x.sql`, `0002-x.sql` and duplicate versions give errors;
    - `TestDialectDirsMustMatch`: version or name mismatch between `sqlite/` and `postgres/` gives `migration sets differ: …`.
  - SQLite only:
    - `TestSnapshotBeforeMigration`: once 0001 is applied, adding 0002 creates exactly one `<dir>/backups/pre-0002-*.db`. A fresh DB creates none.
    - `TestSnapshotFailureAbortsMigration`: a read-only backups dir means `Migrate` errors and applies nothing.
  - `TestSchemaParity` (needs both DBs; obeys `PAYMUX_REQUIRE_PG`): migrate both with the real `migrations.FS`. `DescribeSchema` must be deep-equal, and the failure message prints a diff of the table and column that differ.
- [ ] **Step 2:** Run `make test-all`. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - Each migration runs in its own `Write`, so Postgres DDL is transactional.
  - SQLite runs `VACUUM INTO` before the first pending migration when at least one migration is already applied.
  - `DescribeSchema`:
    - SQLite: `sqlite_master` plus `pragma_table_info` and `pragma_index_list`/`index_info`.
    - Postgres: `information_schema.columns` plus `pg_indexes`/`pg_index` for the current schema.
  - Wire `paymux migrate` to print `applied N migrations (<dialect>)`.
- [ ] **Step 4:** Run `make test-all`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(store): lockstep migrations, schema parity, SQLite snapshots`.

### Task 8: Durable job queue (both dialects)

**Files:**
- Create: `internal/store/jobs.go`, `internal/jobs/queue.go`, `internal/jobs/backoff.go`
- Test: `internal/jobs/queue_test.go`, `internal/jobs/backoff_test.go`

**`jobs` table:**
- Columns: `id TEXT PK`, `kind TEXT NOT NULL`, `payload TEXT NOT NULL`, `state TEXT NOT NULL CHECK (state IN ('pending','running','done','dead'))`, `run_at BIGINT NOT NULL`, `attempts INTEGER NOT NULL DEFAULT 0`, `max_attempts INTEGER NOT NULL`, `locked_until BIGINT`, `last_error TEXT`, `dedupe_key TEXT`, `created_at BIGINT NOT NULL`, `updated_at BIGINT NOT NULL`.
- Index `jobs_state_run_at(state, run_at)`.
- Unique partial index `jobs_dedupe(dedupe_key) WHERE dedupe_key IS NOT NULL AND state IN ('pending','running')`.

**Interfaces:**
- Consumes: `store.DB`, `store.Tx`, `storetest.Each`, `ids.New`.
- Produces:

```go
// store
type JobRow struct{ ID, Kind string; Payload json.RawMessage; State string; RunAt time.Time; Attempts, MaxAttempts int; LockedUntil time.Time; LastError, DedupeKey string }
func (db *DB) InsertJob(ctx context.Context, tx Tx, j JobRow) error   // dedupe conflict => nil
func (db *DB) ClaimJobs(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]JobRow, error)
func (db *DB) CompleteJob(ctx context.Context, id string, now time.Time) error
func (db *DB) FailJob(ctx context.Context, id string, now, nextRunAt time.Time, errMsg string, dead bool) error
func (db *DB) JobStats(ctx context.Context, now time.Time) (depth map[string]int, oldestPending time.Duration, err error)
// jobs
type Job struct{ Kind string; Payload any; RunAt time.Time; MaxAttempts int; DedupeKey string } // MaxAttempts 0 => 10
type Handler func(ctx context.Context, payload json.RawMessage) error
type Options struct{ Workers int; PollInterval, Lease time.Duration; Clock func() time.Time; Jitter func() float64; Logger *slog.Logger } // 4, 1s, 5m, time.Now, rand in [-0.2,0.2]
func New(db *store.DB, opts Options) *Queue
func (q *Queue) Register(kind string, h Handler)   // panics on duplicate
func (q *Queue) Enqueue(ctx context.Context, tx store.Tx, j Job) error
func (q *Queue) RunOnce(ctx context.Context) (int, error)
func (q *Queue) Start(ctx context.Context) error   // drains in-flight up to 30s on cancel
func (q *Queue) Running() bool
func Permanent(err error) error
func Backoff(attempt int, jitter float64) time.Duration // 5s·2^(attempt-1), cap 1h, ×(1+jitter)
```

**Claim** (inside `Write`; the inner select gets `db.Dialect().SkipLocked()` appended):

```sql
UPDATE jobs SET state='running', attempts=attempts+1, locked_until=?, updated_at=?
WHERE id IN (SELECT id FROM jobs
             WHERE (state='pending' AND run_at<=?) OR (state='running' AND locked_until<=?)
             ORDER BY run_at LIMIT ?{SKIP_LOCKED})
RETURNING id, kind, payload, attempts, max_attempts
```

- [ ] **Step 1: Write failing tests.** All of them use a fake clock, `Jitter: func() float64 { return 0 }` and `RunOnce`. Every DB test runs under `storetest.Each`.
  - `TestBackoff`:
    - `(1,0)` = 5s, `(2,0)` = 10s, `(3,0)` = 20s;
    - `(20,0)` = 1h;
    - `(1,0.2)` = 6s, `(1,-0.2)` = 4s.
  - `TestJobRuns`: the exact payload is received, and the state becomes `done`.
  - `TestRolledBackEnqueueNeverRuns`.
  - `TestRetryThenDead`: `MaxAttempts` 3; the first retry is at +5s (not runnable at +4s, runnable at +5s); after attempt 3 the job is `dead` with `last_error`.
  - `TestPermanentError`: `dead` after 1 attempt.
  - `TestDedupe`: second enqueue while pending is a no-op; after `done`, the same key inserts again.
  - `TestExpiredLeaseIsReclaimed`: `ClaimJobs` without completing, advance the lease, then `RunOnce` runs it with attempts = 2.
  - `TestUnknownKindIsDead`: `last_error` = `no handler registered for "x.y"`.
  - `TestHandlerPanicIsRetried`.
  - `TestStartStopsOnCancel`: returns within 1s.
  - `TestConcurrentClaimersRunEachJobOnce` (Postgres only): 2 queues on the same DB, 50 jobs, 4 workers each, `RunOnce` called concurrently in loops until empty. Every job ran exactly once, counted by a handler that inserts into a test table with `UNIQUE(job_id)`.
- [ ] **Step 2:** Run `make test-all`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4:** Run `make test-all`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(jobs): durable outbox job queue on SQLite and Postgres`.

### Task 9: Scheduler (interval and daily-at-local-time, leased)

**Files:**
- Create: `internal/jobs/schedule.go`, `internal/store/schedules.go`
- Test: `internal/jobs/schedule_test.go`

**`schedules` table:** `name TEXT PK`, `next_run_at BIGINT NOT NULL`, `locked_until BIGINT`, `updated_at BIGINT NOT NULL`.

**Interfaces:**
- Produces:

```go
type Schedule struct{ Name, Kind string; Every time.Duration; DailyAt string; Location *time.Location } // exactly one of Every/DailyAt
func (q *Queue) AddSchedule(s Schedule) error
func NextRun(s Schedule, after time.Time) time.Time
// store
func (db *DB) ClaimDueSchedule(ctx context.Context, tx Tx, name string, now, defaultNext time.Time) (due bool, runAt time.Time, err error) // Postgres row lock via ForUpdate()
func (db *DB) SetScheduleNext(ctx context.Context, tx Tx, name string, next, now time.Time) error
```

Due schedules are handled at the start of each `RunOnce`, inside one `Write`: claim, enqueue with `DedupeKey "sched:<name>:<runAt unixms>"`, then set next.

- [ ] **Step 1: Write failing tests:**
  - `TestNextRunEvery`.
  - `TestNextRunDaily`: `"03:00"` in Asia/Kolkata. 02:59 IST gives 03:00 the same day. Exactly 03:00 gives the next day.
  - `TestNextRunDailyDST`, America/New_York:
    - `"02:30"` after 2026-03-07 03:00 gives 2026-03-08 03:30 EDT;
    - `"01:30"` on 2026-11-01 gives one run (the first 01:30, EDT), and the next run is 2026-11-02 01:30 EST.
  - `TestScheduleEnqueuesOncePerDue` (Each).
  - `TestScheduleCatchUpDoesNotBurst` (Each): after 1h down on an `Every 1m` schedule, the result is 1 job, and next = now+1m.
  - `TestTwoInstancesScheduleOnce` (Postgres): two queues with the same schedule call `RunOnce` concurrently 20 times at the same fake time. Exactly 1 job is enqueued.
  - `TestAddScheduleValidation`: both fields set, neither set, `"25:00"`, or a nil Location give errors.
- [ ] **Step 2:** Run `make test-all`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4:** Run `make test-all`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(jobs): leased interval and daily schedules`.

### Task 10: HTTP server, health, internal listener, `serve`

**Files:**
- Create: `internal/metrics/metrics.go`, `internal/server/server.go`, `internal/server/middleware.go`, `internal/server/health.go`
- Modify: `paymux.go`, `internal/cli/cli.go`
- Test: `internal/metrics/metrics_test.go`, `internal/server/server_test.go`, `paymux_test.go`, `internal/cli/cli_test.go`

**Interfaces:**
- Produces:

```go
// metrics (no dependency)
func NewRegistry() *Registry
func (r *Registry) Counter(name, help string, labels ...string) *CounterVec
func (r *Registry) Gauge(name, help string, labels ...string) *GaugeVec
func (r *Registry) GaugeFunc(name, help string, fn func() float64)
func (r *Registry) Handler() http.Handler
// paymux
func New(cfg config.Config, opts ...Option) (*App, error)
func WithLogger(l *slog.Logger) Option
func (a *App) Start(ctx context.Context) error   // open store, migrate, queue + schedules, main + internal listeners; graceful 10s
func (a *App) Addr() string
func (a *App) InternalAddr() string
```

- **Middleware:**
  1. request ID: honour `X-Request-Id` if it matches `^[A-Za-z0-9-]{1,64}$`, otherwise `ids.New("req")`; echo it in the response;
  2. access log;
  3. recover: 500 with `{"error":{"type":"internal","request_id":"…"}}`;
  4. `TimeoutHandler` 30s.
- **Main listener:**
  - `GET /healthz` returns `{"status":"ok"}`.
  - `GET /readyz` returns 200 `{"status":"ready"}`, or 503 with `failing` listing any of `db`, `migrations`, `queue` or `scheduler`. The scheduler check uses the built-in `system.heartbeat` schedule (`Every 1m`), which must have run within 3m once uptime is 3m or more.
- **Internal listener** (`PAYMUX_INTERNAL_LISTEN`):
  - `/metrics`:
    - `paymux_build_info{version,db}`
    - `paymux_http_requests_total{route,code}`
    - `paymux_jobs_depth{state}`
    - `paymux_jobs_oldest_pending_seconds`
    - `paymux_heartbeat_last_run_timestamp_seconds`
  - `/debug/pprof/`.
- **Startup logs:**
  - one line with version, dialect, `RedactedDatabaseURL` and env;
  - when `cfg.DevSkip2FA` is true, a `WARN` line `2FA is disabled (development)`.

- [ ] **Step 1: Write failing tests:**
  - `TestMetricsTextFormat`: HELP and TYPE lines, label escaping, sorted output.
  - `TestRequestIDHonouredAndGenerated`.
  - `TestPanicReturns500JSON`.
  - `TestReadyzNotReadyBeforeQueue`.
  - `TestServeBoots`, under `storetest`-style dual runs. Build the config with `DatabaseURL` pointing at SQLite in a temp dir, or at a fresh Postgres schema. Then:
    - start, and poll `/readyz` until 200 (10s timeout);
    - `/metrics` has `paymux_build_info` with the right `db` label;
    - cancel, and `Start` returns nil within 11s.

    SQLite only: a second `App` on the same dir fails with `store.ErrLocked`.
  - `TestStartupLogRedactsPassword` (Postgres): the captured log does not contain the password.
  - `TestDevSkipWarningLogged`.
  - `TestServeMissingConfigExit1` (cli): stderr contains `PAYMUX_BASE_URL is required`.
- [ ] **Step 2:** Run `make test-all`. Expected: FAIL.
- [ ] **Step 3: Implement.** Use stdlib `ServeMux` patterns, with a separate mux for the internal listener.
- [ ] **Step 4:** Run `make test-all`. Expected: PASS. Then manually: copy `.env.example` to `.env`, set a master key, run `make run-dev` and `curl localhost:8080/readyz`. Expected: 200 within 2s, and the WARN line appears.
- [ ] **Step 5: Commit** `feat(server): HTTP server, health, internal metrics listener, serve`.

### Task 11: CI script and seam checks

**Files:**
- Create:
  - `internal/seams/seams.go`
  - `scripts/ci.sh`
  - fixtures under `internal/seams/testdata/{clean,sqlleak,pgxleak,connleak,fatmain}/…`
- Test: `internal/seams/seams_test.go`

**Interfaces:**
- Produces: `type Violation struct{ File, Rule, Detail string }`, and `func Check(root, modulePath string) ([]Violation, error)`. It uses `go/parser` with `ImportsOnly` and skips `testdata/` and `_test.go`.

**Rules:**

| Rule | Check |
|---|---|
| `sql-outside-store` | `database/sql`, `modernc.org/sqlite` and `github.com/jackc/pgx/...` may be imported only under `internal/store/`. |
| `connector-imports` | Files under `connectors/` import only the stdlib and `<module>/pkg/...`. |
| `thin-main` | `cmd/paymux/main.go` has ≤ 25 non-blank, non-comment lines. |

- [ ] **Step 1: Write failing tests:**
  - `TestCleanFixture`: 0 violations.
  - `TestSQLLeak` and `TestPgxLeak`: one `sql-outside-store` each.
  - `TestConnectorLeak`: one `connector-imports`, naming the import.
  - `TestFatMain`: one `thin-main`.
  - `TestRepoHasNoViolations`: `Check("../..", "github.com/paymux/paymux")` gives 0.
- [ ] **Step 2:** Run the tests. Expected: FAIL.
- [ ] **Step 3: Implement `Check`.** Then write `scripts/ci.sh` (`set -euo pipefail`), which runs:
  1. `test -z "$(gofmt -l .)"`
  2. `go vet ./...`
  3. staticcheck at a pinned version
  4. govulncheck at a pinned version
  5. `make test`
  6. `PAYMUX_REQUIRE_PG=1 make test-pg`
  7. linux/amd64 and linux/arm64 builds with `CGO_ENABLED=0` into `dist/`

  Pin the staticcheck and govulncheck versions to their latest tags at implementation time, and write them in the script.
- [ ] **Step 4:** Run `make ci`. Expected: exit 0, and `file dist/paymux-linux-amd64` reports statically linked.
- [ ] **Step 5: Commit** `ci: CI script and architectural seam checks`.

---

## M0 exit checklist

- [ ] `make build` works with only Go 1.26 installed: the toolchain auto-downloads (Task 1).
- [ ] `paymux serve` boots on SQLite and on Postgres, and `/readyz` returns 200 (Task 10 `TestServeBoots`).
- [ ] A rolled-back enqueue never runs, on both DBs (Task 8).
- [ ] Two Postgres instances run each job and each schedule exactly once (Tasks 8 and 9).
- [ ] A second SQLite instance refuses to start (Task 6).
- [ ] The schema parity test passes (Task 7).
- [ ] The dev 2FA skip is refused outside development or localhost (Task 2).
- [ ] `make ci` is green (Task 11).
