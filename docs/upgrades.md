# Upgrades

## The guarantee

**Any release can upgrade the database of the release before it (N-1 to N),
on SQLite and on PostgreSQL, without losing data.** Upgrading means starting
the new binary: `paymux serve` (or `paymux migrate`) applies the pending
migrations in order, each in its own transaction, and then runs.

What this covers:

- Every table, row and column of the previous release survives. Rows keep their
  ids, amounts, timestamps, JSON documents and binary blobs bit for bit.
- Columns and tables added by the new release start with defaults that keep old
  rows valid (for example an invoice number's new sequence column is filled in
  from the number, and a payment attempt has no stored checkout yet).
- Settings, API keys, provider accounts, webhook endpoints, users and queued
  jobs keep working with no action from you.
- An upgraded database has exactly the schema a fresh install has.

What it does not cover:

- **Downgrades.** Migrations only go forward. To go back, restore the backup you
  took before upgrading (below).
- **Skipping releases** is expected to work, because migrations are applied one
  after another, but only N-1 to N is tested on every change. If you are more
  than one release behind, upgrade through the releases in between or take a
  backup first and check the result.
- Data that was already inconsistent before the upgrade.

## Upgrading safely

1. Read the release notes for anything that needs configuration.
2. Take a backup.
   - SQLite: `paymux backup --to ./backup-before-upgrade.db` (stop `paymux
     serve` first; SQLite allows one process). On SQLite, Paymux also writes a
     snapshot of the database next to it (`VACUUM INTO`) before applying
     migrations to a database that already has data.
   - PostgreSQL: `pg_dump --format=custom "$PAYMUX_DATABASE_URL" > before.dump`.
3. Replace the binary (`paymux update`, or your package manager, or a new
   container image) and restart. The first start applies the migrations; logs
   show each one, and `/readyz` reports `migrations` as failing until none are
   pending.
4. If something is wrong, stop, restore the backup and start the previous
   binary.

Run more than one instance against PostgreSQL? Start the new version on one
instance first: migrations take an advisory lock, so a second instance waits
instead of racing, but old instances keep running against a schema they do not
know until you restart them. Migrations are written to be additive for that
reason: they add columns and tables and avoid renaming or dropping what the
previous release reads.

## `paymux update`

```sh
paymux update --check     # report only: current version, latest release
paymux update             # install the latest release over the running binary
paymux update --version v1.4.0
```

It asks the GitHub releases API of `velixio/paymux` (change with `--repo
owner/name` or `PAYMUX_UPDATE_REPO`; `GITHUB_TOKEN` is used, only for the API
host, if you hit rate limits) and then:

1. downloads `paymux_<version>_<os>_<arch>.tar.gz` for this machine;
2. checks its SHA-256 against the release's `SHA256SUMS` and refuses to install
   anything it cannot verify (no `SHA256SUMS`, no entry for the file, or a
   different hash);
3. if the release has a `SHA256SUMS.sig` and `cosign` is installed, verifies
   that signature as well. Give it what to verify against: `--cosign-key
   cosign.pub`, or `--cosign-identity <signer> --cosign-issuer <oidc issuer>`
   for keyless releases (`SHA256SUMS.pem` is used when present). A signature
   that does not verify aborts the update. With cosign missing, or nothing to
   verify against, it says "signature not verified" and the checksum alone
   decides;
4. runs the new binary's `version` and requires it to report the release's
   version;
5. replaces the binary atomically (a rename in the same directory), keeping the
   old one as `<binary>.prev` for a rollback, and prints how to restart.

It never restarts the server itself and does not touch the database; pending
migrations run on the next start (see above). A development build (version
`dev`) cannot tell whether it is current, so it needs `--force`. The directory
of the binary must be writable by you; otherwise run it as a user who can, or
update elsewhere with `--dest` and move the file.

Release layout it expects, per release tag `vX.Y.Z`: one `.tar.gz` per platform
named `paymux_X.Y.Z_<os>_<arch>.tar.gz` holding a file called `paymux`, and a
`SHA256SUMS` file in `sha256sum` format covering them; `SHA256SUMS.sig` (and
`SHA256SUMS.pem`) are optional.

## How the guarantee is tested

`internal/store/upgrade_test.go` takes a database as release N-1 left it and
upgrades it with the current code:

- `testdata/upgrade/v0005.sqlite` is a SQLite database file and
  `testdata/upgrade/v0005.postgres.sql` a `pg_dump` of a PostgreSQL database,
  both made at migration 0005 with rows in every table that existed then
  (payments, attempts, refunds, ledger, keys, provider accounts with binary
  config, settings, webhook endpoints, users, jobs, ...).
- The test runs the current migrations over each, then checks that every row
  count is unchanged, that representative rows read back exactly (including
  non-ASCII text, JSON, big integers and blobs), that settings still resolve,
  that the schema equals a fresh install's, and that a smoke flow works on the
  result: a new payment is created and captured, a refund succeeds, and a
  payment that predates the upgrade is partly refunded.
- It runs in CI on both databases.

The fixtures are frozen on purpose. Migrations never change once released, so
the baseline keeps proving that the *whole chain* from that point still works.
When the oldest release you still promise to upgrade from moves forward, add a
new pair of fixtures at that migration and keep the old ones for as long as
that release is supported. Do not regenerate an existing fixture to make a test
pass.

### Making a fixture

`TestGenerateUpgradeFixtures` builds the schema with the migrations up to the
baseline and loads the rows in `testdata/upgrade/data_*.sql` (one shared file
plus one per dialect for blob syntax). It does nothing unless asked:

```sh
# SQLite: writes testdata/upgrade/v0005.sqlite
PAYMUX_UPGRADE_FIXTURE=sqlite go test ./internal/store -run GenerateUpgradeFixtures

# PostgreSQL: fills an empty database, then dump it
createdb upgrade_fixture
PAYMUX_UPGRADE_FIXTURE_PG_URL=postgres://localhost/upgrade_fixture go test ./internal/store -run GenerateUpgradeFixtures
pg_dump --no-owner --no-privileges --no-comments --column-inserts upgrade_fixture \
  > internal/store/testdata/upgrade/v0005.postgres.sql
```

For a new baseline, change `fixtureVersion` and the file names, and extend the
data files to cover tables added since.

## Changes to the Go API (framework mode)

The database guarantee above does not cover Go source. These changes break code
that imports Paymux as a library:

- **The root package moved to `hub`.** `github.com/paymux/paymux` (package
  `paymux`) is now `github.com/paymux/paymux/pkg/hub` (package `hub`), so the
  repository root holds no Go code. In a framework-mode `main` or a plugin
  module, change the import and the qualifier:

  ```go
  // before
  import "github.com/paymux/paymux"
  func(app *paymux.App) { ... paymux.PaymentEvent ... }

  // after
  import "github.com/paymux/paymux/pkg/hub"
  func(app *hub.App) { ... hub.PaymentEvent ... }
  ```

  The module path (`github.com/paymux/paymux`, used in `go.mod` and in
  `paymux build --with`) did not change. The build-version flag is now
  `-X github.com/paymux/paymux/pkg/hub.Version=...`. Nothing else in the package
  changed, so a find-and-replace is the whole migration. This was done before
  there were framework-mode users.

## Writing migrations that keep the promise

- Forward only, in lockstep for `internal/migrations/sqlite` and `internal/migrations/postgres`;
  never edit a released migration.
- Prefer adding (columns with defaults or nullable, tables, indexes). A change
  that rewrites existing data must be correct for rows of every earlier shape,
  which is what the fixtures exercise.
- If you add a table whose rows an old database should already have in some
  form, extend the fixture data and the test's row-count table.
