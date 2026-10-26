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

## Release notes that need action

- **Remote connector accounts must declare `checkout_hosts`.** The hub now
  refuses a checkout whose script, form or redirect URL is not on a host the
  connector declared (audit B-7a). The built-in connectors declare theirs; an
  account of the `remote` connector declares them in its config, and the config
  is refused without them. A remote account stored without `checkout_hosts`
  cannot be built after the upgrade, so everything that needs the connector
  fails for it, not only checkout: webhook verification, provider-refund
  handling, reconciliation and the dashboard's connection test. **Add the field
  right after upgrading** (dashboard, provider account, config; saving the
  config rebuilds the account), for example
  `"checkout_hosts": {"redirect": ["https://pay.your-provider.com"]}` for a
  service that redirects, or `{"script": ["https://sdk.your-provider.com"]}`
  for one that opens an SDK. See "Remote connectors" in `docs/connectors.md`.
  Checkouts already stored on open payments and subscriptions are checked again
  when they are shown: one outside the declared hosts is not shown (a payment
  gets a fresh checkout, a subscription page answers an error until the hosts
  are fixed).
  A connector compiled into your own build (framework mode) with no
  `CheckoutHosts` can no longer send the customer off the site; add the hosts
  to its `Info`.

- **The hosted-page Content-Security-Policy is enforced.** It was report-only.
  A script, frame, connection, image or form post to a host that is not in the
  connector account's `CheckoutHosts` (built-in connectors: their `Info`;
  remote connectors: the account's `checkout_hosts`; a custom connector in your
  own build: its `Info`) is now blocked by the browser, and the action's own
  script and form URLs are no longer allowed on top. Before upgrading, check
  that each custom or remote connector lists every host its checkout loads
  (watch `paymux_csp_violations_total` and the `csp: violation reported` log
  line on a release that still had the report-only header, or test a checkout
  in a browser with the console open). Violations are reported to
  `/csp-report` on the same host.

## Upgrades that can refuse to start

A few migrations add a uniqueness rule that data from an earlier release could
already break. They fail, with the database error, and change nothing; they
never drop or rewrite rows. Paymux has no released users yet, so these checks
are strict and there is no repair step. If one fails, fix the rows it names
(or restore the backup) and start again.

- **0031, one provider payment settles one attempt.** A unique index over
  `payment_attempts (provider_account_id, provider_payment_id)` for captured,
  non-duplicate attempts. It fails if two such attempts share a provider
  payment id. List them before upgrading:

  ```sql
  SELECT provider_account_id, provider_payment_id, COUNT(*) FROM payment_attempts
  WHERE status = 'captured' AND duplicate = 0 AND provider_payment_id IS NOT NULL
  GROUP BY 1, 2 HAVING COUNT(*) > 1;
  ```

- **0033, ledger invariants.** Amounts of payments, refunds and payouts must
  be positive and `payments.amount_refunded` not negative (CHECK constraints
  on PostgreSQL, triggers on SQLite); a refund is booked once per refund and a
  dispute releases and loses once per dispute (unique indexes); and
  `ledger_entries` refuses UPDATE and DELETE from then on. It fails on rows
  that break one of these. List them before upgrading:

  ```sql
  SELECT id FROM payments WHERE amount <= 0 OR amount_refunded < 0;
  SELECT id FROM refunds WHERE amount <= 0;
  SELECT id FROM payouts WHERE amount <= 0;
  SELECT refund_id, kind, COUNT(*) FROM ledger_entries WHERE refund_id IS NOT NULL
  GROUP BY 1, 2 HAVING COUNT(*) > 1;
  SELECT dispute_id, kind, COUNT(*) FROM ledger_entries
  WHERE dispute_id IS NOT NULL AND kind <> 'dispute_hold' GROUP BY 1, 2 HAVING COUNT(*) > 1;
  ```

  The migration also adds `ledger_entries.attempt_id` and fills it for old
  entries where the attempt is unambiguous; the rest stay empty and count as
  the sale in revenue totals. Because the ledger is now append-only, a later
  migration or script that must change ledger rows has to drop the triggers
  (`ledger_entries_no_update` and `ledger_entries_no_delete` on SQLite,
  `ledger_entries_no_change` on PostgreSQL) first and recreate them.

## Updating from the dashboard

Admins can update a server from **Update** in the dashboard menu (action
`system.update`: the built-in admin only, never given to a custom role). The
page shows the running version, the latest GitHub release with a link to its
notes (asked of GitHub's API and remembered for five minutes; *Check again*
asks again) and an **Update to vX** button. It runs the same steps as `paymux
update` (download, SHA-256 against `SHA256SUMS`, cosign signature of that file
against this repository's release workflow, a trial run of the new binary, an
atomic replace that keeps `<binary>.prev`) and then restarts Paymux onto the new
binary: the hub stops accepting connections, lets running requests and jobs
finish like on `SIGTERM`, and starts the new binary in place with the same
arguments and environment. Pending migrations run on that start, so take a
backup first (above).

What it will not do:

- **Install without the signature check.** There is no equivalent of
  `--insecure-skip-signature` here. If `cosign` is not on the server's `PATH`,
  the button is disabled and the page says why; update with the command line
  (where you can choose) instead.
- **Install where it cannot replace itself.** The directory of the binary must
  accept a new file. On a read-only file system, under systemd `ProtectSystem=`,
  in a container image whose binary belongs to another user, or when the binary
  was not found, the page says so and tells you to update with your package
  manager or image. Only the stock `paymux` binary offers the page; a custom
  binary built on the Go package never replaces itself with it.
- **Go back or repeat.** An older or the same release, or a development build,
  is installed only with the *Force* box ticked.
- **Run twice.** One update runs at a time; the page follows it (status line and
  log), and reloads when Paymux is back. If the install fails nothing has been
  replaced and the page shows the error (with secrets removed).

Starting an update needs a recent sign-in (a password and code within the last
12 hours) and the dashboard's CSRF token. Each start and each result is in the
audit log as `system.update.start` and `system.update`, with who did it, the
version it started from, the version it went to, whether it was forced and
whether it worked.

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
3. verifies the cosign signature of `SHA256SUMS` (`SHA256SUMS.sig` with the
   certificate `SHA256SUMS.pem`) with `cosign`, which must be installed. By
   default the signer has to be the release workflow of the repository on a
   version tag (`https://github.com/<owner>/<name>/.github/workflows/release.yml@refs/tags/v…`,
   issued by `https://token.actions.githubusercontent.com`); `--repo` changes
   the repository. To verify against something else give `--cosign-key
   cosign.pub`, or `--cosign-identity <signer> --cosign-issuer <oidc issuer>`.
   A release that is not signed, a signature that does not verify, a missing
   certificate or a missing `cosign` all abort the update. If you accept that
   risk (for example for an old release made before signing, or without
   `cosign`), `--insecure-skip-signature` installs on the checksum alone and
   prints a warning; it cannot be combined with `--cosign-key` or
   `--cosign-identity`;
4. runs the new binary's `version` and requires it to report the release's
   version;
5. replaces the binary atomically (a rename in the same directory), keeping the
   old one as `<binary>.prev` for a rollback, and prints how to restart.

The command never restarts the server itself (the dashboard's update does, above) and does not touch the database; pending
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

- **Go packages moved under `pkg/`.** The repository root holds no Go code any
  more, and everything a framework-mode `main` or a plugin module imports now
  lives in `pkg/`:

  | Before | After |
  |---|---|
  | `github.com/paymux/paymux` (package `paymux`) | `github.com/paymux/paymux/pkg/hub` (package `hub`) |
  | `github.com/paymux/paymux/framework` | `github.com/paymux/paymux/pkg/framework` |
  | `github.com/paymux/paymux/plugins/jsvm` | `github.com/paymux/paymux/pkg/jsvm` |

  Change the imports and the qualifier; nothing else in the packages changed, so
  a find-and-replace is the whole migration:

  ```go
  // before
  import (
  	"github.com/paymux/paymux"
  	"github.com/paymux/paymux/framework"
  )
  func(app *paymux.App) { ... paymux.PaymentEvent ... }

  // after
  import (
  	"github.com/paymux/paymux/pkg/framework"
  	"github.com/paymux/paymux/pkg/hub"
  )
  func(app *hub.App) { ... hub.PaymentEvent ... }
  ```

  The module path (`github.com/paymux/paymux`, used in `go.mod` and in
  `paymux build --with`) did not change. The build-version flag is now
  `-X github.com/paymux/paymux/pkg/hub.Version=...`. `pkg/hub`, `pkg/framework`
  and `pkg/jsvm` are part of the API snapshot (`scripts/apisnap/pkg.txt`). These
  moves were made before there were framework-mode users.

## PayU webhook event ids

PayU payment webhooks now get their event id from the transaction id and the
status (`payu:<txnid>:<status>`), the fields PayU's reverse hash signs. Before,
it also used `mihpayid` and `unmappedstatus`, which the hash does not cover, so
a captured webhook replayed with another `mihpayid` was treated as new. Event
ids of deliveries stored before the upgrade keep the old form, so a webhook PayU
re-sends across the upgrade may be processed once more; this is harmless, since
the payment is fetched from PayU by `txnid` and applied idempotently.

## Writing migrations that keep the promise

- Forward only, in lockstep for `internal/migrations/sqlite` and `internal/migrations/postgres`;
  never edit a released migration.
- Prefer adding (columns with defaults or nullable, tables, indexes). A change
  that rewrites existing data must be correct for rows of every earlier shape,
  which is what the fixtures exercise.
- If you add a table whose rows an old database should already have in some
  form, extend the fixture data and the test's row-count table.
