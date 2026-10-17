# Paymux runbook

How to install, run, back up and recover a Paymux hub. Commands assume Linux with systemd; a Docker variant is given where it differs. Everything is one static binary, `paymux`, plus a database (SQLite by default, Postgres optional) and a reverse proxy for TLS.

Contents: [Layout](#layout) · [Install](#install) · [First admin and 2FA](#first-admin-and-2fa) · [Apps and provider accounts](#apps-and-provider-accounts) · [Themes and languages](#themes-and-languages) · [Webhook URLs](#webhook-urls-to-register-at-each-provider) · [Configuration reference](#configuration-reference) · [Backups and restore](#backups-and-restore) · [Upgrades](#upgrades) · [Monitoring](#monitoring) · [If the hub is down](#if-the-hub-is-down)

## Layout

| Piece | Where | Notes |
|---|---|---|
| Hub | `paymux serve` | Listens on `PAYMUX_LISTEN` (default `:8080`) for everything public, and on `PAYMUX_INTERNAL_LISTEN` (default `127.0.0.1:9100`) for `/metrics`, `/debug/pprof` and Caddy's `/internal/tls-allow`. Never expose the internal listener to the internet. |
| Data directory | `PAYMUX_DATA_DIR` (default `./data`; `/var/lib/paymux` in the unit) | Holds `paymux.db` (SQLite), its lock file, the optional `themes/<name>/` themes and the optional `custom/` overrides. |
| Reverse proxy | Caddy (`deploy/Caddyfile`) | TLS for the hub's host and, on demand, for app custom domains. |
| Config | `/etc/paymux/paymux.env` | `KEY=value` lines, read by systemd. A `.env` file in the working directory is also read for CLI use. |
| Master key | `PAYMUX_MASTER_KEY` or `PAYMUX_MASTER_KEY_FILE` | 32 random bytes, base64. Encrypts provider credentials, webhook secrets and TOTP secrets. **Back it up separately from the database; without it the encrypted data cannot be read.** |

## Install

### Binary and systemd

```sh
# 1. The binary (build with `make build`, or download a release) and a service user.
install -m 0755 dist/paymux /usr/local/bin/paymux
useradd --system --home /var/lib/paymux --shell /usr/sbin/nologin paymux
install -d -o paymux -g paymux -m 0700 /var/lib/paymux
install -d -m 0750 -g paymux /etc/paymux

# 2. A master key, kept in a file only root and the service can read.
umask 077
head -c 32 /dev/urandom | base64 > /etc/paymux/master.key
chown root:paymux /etc/paymux/master.key && chmod 0640 /etc/paymux/master.key
#    Store a copy in your password manager / vault NOW.

# 3. The environment file.
cat > /etc/paymux/paymux.env <<'EOF'
PAYMUX_ENV=production
PAYMUX_BASE_URL=https://pay.example.com
PAYMUX_DATA_DIR=/var/lib/paymux
PAYMUX_MASTER_KEY_FILE=/etc/paymux/master.key
PAYMUX_TZ=Asia/Kolkata
# Behind Caddy on the same machine:
PAYMUX_TRUSTED_PROXIES=127.0.0.1/32
# Where alerts go (optional but strongly recommended):
PAYMUX_OPS_EMAILS=ops@example.com
PAYMUX_OPS_WEBHOOK_URL=https://hooks.slack.com/services/XXX/YYY/ZZZ
# Outgoing mail (customer invoices, alerts). Leave PAYMUX_SMTP_HOST unset to disable email.
PAYMUX_SMTP_HOST=smtp.example.com
PAYMUX_SMTP_PORT=587
PAYMUX_SMTP_USERNAME=paymux
PAYMUX_SMTP_PASSWORD=change-me
PAYMUX_SMTP_FROM=Paymux <billing@example.com>
EOF
chown root:paymux /etc/paymux/paymux.env && chmod 0640 /etc/paymux/paymux.env

# 4. Start the service. The schema is created on first start; the first admin is
#    created in the browser (see "First admin and 2FA").
cp deploy/paymux.service /etc/systemd/system/ && systemctl daemon-reload
systemctl enable --now paymux
journalctl -u paymux -f            # structured JSON logs; look for "first-run setup is open"
curl -fsS http://127.0.0.1:8080/readyz
```

`PAYMUX_BASE_URL` must be the public https URL (plain `http://` is accepted only for localhost). In `production` the hub refuses to start without a valid master key and refuses `PAYMUX_DEV_SKIP_2FA`.

### Caddy

Install Caddy, copy `deploy/Caddyfile` to `/etc/caddy/Caddyfile`, replace `pay.example.com` and the e-mail address, and `systemctl reload caddy`. Point DNS for the hub's host at this server. App custom domains (added on the app's *Domains* tab) get certificates on demand, but only for hosts the hub knows: Caddy asks `http://127.0.0.1:9100/internal/tls-allow?domain=...` before it requests one.

### Docker

```sh
docker build -t paymux:latest --build-arg VERSION=$(git describe --always) .
docker run -d --name paymux --restart unless-stopped \
  -p 127.0.0.1:8080:8080 -v paymux-data:/data \
  --env-file /etc/paymux/paymux.env -e PAYMUX_DATA_DIR=/data \
  -e PAYMUX_MASTER_KEY="$(cat /etc/paymux/master.key)" \
  paymux:latest
```

The image is distroless, runs as user 65532 and contains only the binary. There is no shell: run CLI commands with `docker run --rm -v paymux-data:/data --env-file ... paymux:latest user reset-2fa ...` (stop the container first when using SQLite, see below). The first admin is created in the browser, so the CLI is not needed to get started; the setup code is in `docker logs paymux`. The internal listener stays on the container's loopback, so Caddy must run in the same network namespace (`--network container:paymux`, or on the host with `--network host`), or set `PAYMUX_INTERNAL_LISTEN=0.0.0.0:9100` on a private Docker network that only Caddy joins.

### Postgres instead of SQLite

Set `PAYMUX_DATABASE_URL=postgres://user:password@host:5432/paymux?sslmode=require`. Migrations run at start. Use Postgres when you want more than one hub process, managed backups or point-in-time recovery. The hub logs the URL with the password masked.

## First admin and 2FA

There is no default account. A hub that has no users shows a one-time setup page, so nothing has to be run on the server and the SQLite file never has to be unlocked:

1. Start the hub and read its log. It prints the address and a one-time **setup code** (24 letters in four groups):

   ```
   journalctl -u paymux | grep "first-run setup"      # or: docker logs paymux | grep "first-run setup"
   ```

2. Open `https://pay.example.com/setup` (the login page links to it while the hub has no users), enter the code, and fill in the administrator's email and password (at least 12 characters), the organisation name and, optionally, a first app.
3. The next page shows, **once**, a test and a live API key for the app (copy them now), and the key for two-factor sign-in: add it to an authenticator app (Google Authenticator, Authy, 1Password and similar) by hand or through the `otpauth://` link, then enter the 6-digit code. That signs you in.

How it is protected:

- The code exists only in the hub's log and memory, and is new at every start while the hub has no users. Five wrong codes from one address lock the page for 15 minutes.
- The moment the administrator exists, `/setup` answers 404 for good, and the code is gone. Anyone who can reach the hub before you cannot use the page without the code, but they could race you if the log is public: do the setup straight after the first start, and keep the log private.
- The setup is audited (`setup.complete`); the password, code, API keys and 2FA key are never logged or audited.
- If the page is closed before the 2FA code is confirmed, sign in at `/dashboard/login` with the password: the same key is shown again until the first code is accepted. Lost the API keys: create new ones on the app's page.

The command line still works for scripted installs and for recovery. **With SQLite the CLI cannot run while `paymux serve` is running**: the database file is locked by one process, so stop the service for the moment it takes. (With Postgres there is no such limit.)

```sh
paymux user create --email you@example.com --role admin                      # prompts for the password twice
printf '%s\n' "$PASSWORD" | paymux user create --email you@example.com --role admin --password-stdin
```

Codes cannot be reused, and five wrong sign-in attempts per email and address lock sign-in for 15 minutes.

- Lost authenticator: `paymux user reset-2fa --email you@example.com` (signs the user out everywhere; they enrol again at next sign-in).
- More users: *Users* in the dashboard (admins create users with an initial password; roles are viewer, operator, admin).
- `PAYMUX_DEV_SKIP_2FA=true` exists for local development only and is refused in production or on a non-local base URL.

## Apps and provider accounts

Everything below is in the dashboard (admin role); the same can be scripted with `paymux app create`, `paymux provider add`, `paymux tax-entity add`, `paymux app tax`, `paymux domain add` and `paymux fx set`.

1. **Tax entity** (*Tax entities*): the legal entity and GSTIN that issues invoices, with its invoice prefix, signatory, logo and LUT. Skip this if no app issues invoices.
2. **App** (*Apps → Create an app*): name, slug and a default return URL. On the app page:
   - *Keys*: create a **test** and a **live** API key. The token is shown once; store it in the app's secret store. Revoke keys here too.
   - *Webhooks*: add the app's endpoint URL; copy the signing secret (shown once, `whsec_...`, Standard Webhooks). Test with *Deliveries* and *Replay*.
   - *Branding*: display name, accent colour, logo (PNG/JPEG up to 512 KB).
   - *Tax*: which tax entity, SAC, GST rate, prices inclusive or not, issue invoices on or off.
   - *Settings*: per-app overrides of the org defaults.
3. **Provider accounts** (*Providers → Add an account*): pick the connector, fill the credentials the form asks for (secrets are write-only afterwards), tick the currencies and purposes, and link the apps that may use it. Use *Test connection* to check the credentials. Create separate **test** and **live** accounts.
4. **Provider access** (app → *Providers* tab) lists which accounts the app may use; **routing** (*Settings → Routing*) decides which of them takes a payment, and *Test routing* shows the outcome for a sample payment.
5. **Custom domain** (app → *Domains*), optional: add `pay.yourapp.com`, point its DNS (CNAME/A) at this server, and register the domain with each provider (see the checklist on that tab; Razorpay and PayU review merchant websites, so do this before going live).
6. **Settings** (*Settings*): payment expiry, invoice delivery, dunning, retry schedule, reconciliation time and the rest, each with its default and units.

Before going live, run a payment end to end in **test** mode (the header toggle switches the dashboard between test and live data), then check *Reconciliation* after the first day.

## Themes and languages

**Themes** change how the dashboard and the customer pages (pay page, subscription checkout, invoices) look. Two ship with the hub: `default` and `minimal-dark`. Choose them in the dashboard under *Appearance* (admins): one theme for the dashboard (also the default for customer pages) and, per app, an optional theme and language for that app's customer pages. *Preview* shows a sample page in any theme before you switch.

A theme is a directory `<data dir>/themes/<name>/`:

```
themes/acme/
  theme.json        {"name":"acme","version":"1.0.0","description":"...",
                     "tokens":{"--color-accent":"#0a7d5a","--radius":"4px"},
                     "font_stack":"Inter, system-ui, sans-serif"}
  css/*.css         optional extra stylesheets, applied in name order after the tokens
  templates/*.html  optional template overrides (same file names as web/templates)
  assets/...        images and fonts, served at /theme/acme/assets/<file>
```

- `name` must equal the directory name (lower-case letters, digits, hyphens). Tokens are CSS custom properties (`--color-bg`, `--color-fg`, `--color-muted`, `--color-accent`, `--color-danger`, `--color-line`, `--color-nav`, `--radius`, ...); a value may not contain `;`, braces, angle brackets, `url(` or comments. Look at `web/themes/` in the source for the two bundled examples.
- Overlay order is **embedded files, then the active theme, then `custom/`**: a file in `custom/templates/` beats the same file in the theme.
- A new or edited theme is live within a second: no restart. A theme that is invalid (bad `theme.json`, a template that does not parse, missing directory) is skipped, its pages use the standard look, and ops get a `theme.unusable` alert (logged, e-mailed and posted like other alerts, once per problem). *Appearance* lists such themes with the reason.
- A theme in the data directory with the same name as a bundled one replaces it.

**Languages.** Customer pages and the pre-debit e-mail are available in English (`en`) and Hindi (`hi`). Set the language per app under *Appearance* (the `customer_language` setting; the org value is the default). A link may also carry `?lang=hi` to choose an installed language for that visit; anything else is ignored. A message that is missing in a language shows in English.

To change wording or add a language, put `i18n/<code>.json` in the `custom/` directory (flat `"key": "text"` pairs; copy `web/i18n/en.json` for the keys, and keep `%s` placeholders). Entries are merged over the shipped catalog of the same language, so a file with one key rewords just that message. The code is two or three lower-case letters. Catalogs are read at start: restart after editing them. The pre-debit e-mail text itself is `templates/email_predebit.txt` (override it in `custom/templates/`).

## Webhook URLs to register at each provider

Each provider account has its own URL: `https://pay.example.com/hooks/<provider_account_id>`. It is shown, with a copy button, on the account's page and in the *Providers* list. Register it in the provider's dashboard and subscribe to payment, refund and dispute events. Paymux treats a webhook only as a hint and re-fetches the object from the provider's API, so a missing event delays things rather than corrupting them; reconciliation catches the rest.

| Provider | What to do | Notes |
|---|---|---|
| **Razorpay** | Dashboard → Account & Settings → Webhooks → add the URL, set a secret, tick payment, refund and dispute events (subscription events when you use subscriptions). Put the same secret in the account's *webhook secret* field. | Signature header `X-Razorpay-Signature`. Razorpay disables a webhook that keeps failing for about 24 hours: re-enable it after any outage. Up to 30 URLs per account. |
| **PayU** | Configure the success/failure/refund/dispute notification URLs in the PayU merchant dashboard to the account's URL. | Verified by PayU's reverse hash. **Refund webhooks are unsigned**: Paymux never trusts them and only fetches the refund from PayU's API. |
| **Zoho Payments** | Add the URL and signing key in the Zoho Payments developer settings. | At most 5 webhook endpoints per account. Sandbox must be enabled by Zoho support. Zoho has no dispute API: enter disputes by hand (*Disputes → Record a dispute by hand*). Zoho disables failing webhooks after about 2 days. |
| **Whop** | Add the URL in the Whop developer dashboard for your app. | Standard Webhooks signatures; the connector pins `api_version: v1`. Whop disables failing webhooks after about 72 hours. Whop is merchant of record, so Paymux issues no invoice for it. |

Provider menus change; the descriptions above name the settings to look for, not exact clicks. A *silence detector* raises an alert when an account created payment attempts recently but no verified webhook has arrived.

## Configuration reference

| Variable | Default | Meaning |
|---|---|---|
| `PAYMUX_ENV` | `production` | `production` or `development`. |
| `PAYMUX_BASE_URL` | required | Public origin, `https://...`. |
| `PAYMUX_LISTEN` | `:8080` | Public listener (behind Caddy). |
| `PAYMUX_INTERNAL_LISTEN` | `127.0.0.1:9100` | Metrics, pprof, TLS-ask. Keep it private. |
| `PAYMUX_DATA_DIR` | `./data` | SQLite file, lock, `custom/` theme. |
| `PAYMUX_DATABASE_URL` | `sqlite://<data dir>/paymux.db` | `sqlite://path` or `postgres://...`. |
| `PAYMUX_MASTER_KEY` / `PAYMUX_MASTER_KEY_FILE` | exactly one required | Base64, 32 bytes. |
| `PAYMUX_TZ` | `Asia/Kolkata` | Time zone of "today", reconciliation time and date filters. |
| `PAYMUX_TRUSTED_PROXIES` | none | Comma-separated CIDRs/addresses of reverse proxies whose `X-Forwarded-For` is believed (for the sign-in limit, sessions and audit). Without it the proxy's address would be recorded for everyone. A `/0` block is refused. |
| `PAYMUX_ALLOW_PRIVATE_WEBHOOKS` | `false` | Allow outgoing webhooks to private/loopback addresses (development). |
| `PAYMUX_SMTP_HOST`, `_PORT` (587), `_USERNAME`, `_PASSWORD`, `_FROM` | unset | Outgoing mail over STARTTLS. Mail and credentials are never sent in clear text. |
| `PAYMUX_OPS_EMAILS`, `PAYMUX_OPS_WEBHOOK_URL` | unset | Where ops alerts go (email and a Slack-compatible webhook). Identical alerts are sent at most every 10 minutes. |
| `PAYMUX_ENV_FILE` | `.env` | Extra env file for CLI use. |

## Backups and restore

Back up **three things**: the database, the master key, and `paymux.env`. Test a restore before you need one.

### SQLite with Litestream (default)

Litestream streams every change to S3-compatible storage, typically within a second.

```sh
# install litestream, then:
cp deploy/litestream.yml /etc/litestream.yml       # edit bucket, endpoint, path
install -m 0600 /dev/null /etc/litestream.env      # LITESTREAM_ACCESS_KEY_ID / LITESTREAM_SECRET_ACCESS_KEY
systemctl enable --now litestream
litestream snapshots -config /etc/litestream.yml /var/lib/paymux/paymux.db   # should list snapshots
```

**Restore** (new machine or damaged file), with the hub stopped:

```sh
systemctl stop paymux litestream
mv /var/lib/paymux/paymux.db /var/lib/paymux/paymux.db.broken 2>/dev/null
litestream restore -config /etc/litestream.yml -o /var/lib/paymux/paymux.db /var/lib/paymux/paymux.db
# a point in time:  litestream restore -timestamp 2026-11-03T09:00:00Z ...
chown paymux:paymux /var/lib/paymux/paymux.db
systemctl start litestream paymux
curl -fsS http://127.0.0.1:8080/readyz
```

Also keep an occasional offline copy: `sqlite3 /var/lib/paymux/paymux.db ".backup '/backups/paymux-$(date +%F).db'"` is safe while the hub runs.

### Postgres

- **Logical backups**: `pg_dump -Fc -d paymux -f paymux-$(date +%F).dump` nightly, copied off the machine.
- **Point-in-time recovery**: enable WAL archiving (`archive_mode=on`, an `archive_command` to object storage, or use pgBackRest / WAL-G / your cloud's managed PITR) and take a base backup daily.
- **Restore a dump**: stop the hub, `createdb paymux_restore && pg_restore -d paymux_restore paymux.dump`, point `PAYMUX_DATABASE_URL` at it, start the hub.
- **Restore to a point in time**: restore the base backup, set `recovery_target_time`, start Postgres, then start the hub.

### After any restore

1. Start the hub and check `/readyz`.
2. The restore point is behind real life. In *Reconciliation* press **Run now** for every provider account: it re-reads payments and refunds from the providers and repairs or lists the differences.
3. Check *Disputes* for new ones and re-enable any provider webhook that was switched off while the hub was away.
4. In each app's *Webhooks* tab, replay failed deliveries (*Deliveries → Replay*). Apps drop snapshots older than the one they have, so replays are safe.

## Upgrades

1. Read the release notes for migrations or setting changes.
2. Take a backup (an extra `litestream` snapshot, `sqlite3 .backup`, or `pg_dump`).
3. Get the new binary and check it. A release has one archive per platform (`paymux_<version>_<os>_<arch>.tar.gz`, linux and darwin, amd64 and arm64), a `SHA256SUMS` file and, when it was signed, `SHA256SUMS.sig` and `SHA256SUMS.pem`:

   ```sh
   sha256sum -c --ignore-missing SHA256SUMS
   cosign verify-blob --certificate SHA256SUMS.pem --signature SHA256SUMS.sig \
     --certificate-identity-regexp '^https://github.com/OWNER/REPO/' \
     --certificate-oidc-issuer https://token.actions.githubusercontent.com SHA256SUMS
   tar -xzf paymux_v1.2.3_linux_amd64.tar.gz paymux && ./paymux version
   ```

   (Building from source: `scripts/release.sh v1.2.3` makes the same archives locally.)
4. Replace the binary (or `docker pull`/rebuild) and restart: `systemctl restart paymux`. The hub applies new migrations when it starts and reports `/readyz` as not ready while any are pending. You can run `paymux migrate` first to apply them without serving.
5. Check `/readyz`, `journalctl -u paymux -n 50` and a test-mode payment.
6. **Rollback** means restoring the backup of step 2 together with the old binary; migrations only move forward.

In-flight jobs finish on shutdown (up to 30 seconds); queued jobs survive a restart.

## Monitoring

- `GET /healthz` is liveness; `GET /readyz` checks the database, pending migrations, the job queue and the scheduler. Probe `/readyz` from outside too.
- Prometheus metrics are on the internal listener: `curl http://127.0.0.1:9100/metrics` (request counts, job queue depth by state, oldest pending job age, last heartbeat).
- Alerts (`PAYMUX_OPS_EMAILS`, `PAYMUX_OPS_WEBHOOK_URL`): failed reconciliation, dispute deadlines (on open, 72 h and 24 h before, on loss), webhook silence, endpoints failing for a day.
- Audit log: *Audit* in the dashboard (admins) lists every operator action with the actor and address.

## If the hub is down

**What stops:** customers cannot open pay pages or complete new payments through the hub; apps' API calls fail. **What does not:** money already paid is safe at the provider, and providers keep retrying their webhooks for a while (Razorpay about 24 h before it disables the endpoint, Zoho about 2 days, Whop 72 h). Apps keep what they have; they should retry API calls with the same `Idempotency-Key`.

1. **Is it the process?** `systemctl status paymux`, `journalctl -u paymux -n 100`. Common causes: a missing or wrong master key (it refuses to start rather than guess), a full disk, another process holding the SQLite lock (`database is locked by another paymux process`: a CLI command or a second instance is running), Postgres unreachable, an unapplied migration after an upgrade.
2. **Restart it**: `systemctl restart paymux`; confirm `/readyz`. Caddy returns 502 until the hub answers.
3. **Is the data bad?** Restore from backup (above) onto the same or a new machine; DNS or Caddy must point at it.
4. **Hub is up but behind a bad deploy?** Roll back the binary and, if migrations ran, the backup taken before the upgrade.
5. **After recovery, catch up:**
   - *Reconciliation → Run now* for each provider account (this finds payments that were paid at the provider while the hub could not hear about it).
   - Look at *Disputes* for deadlines that passed.
   - Re-enable any provider webhook that was disabled for failures; send a test event.
   - Replay failed outbound webhooks for each app (*App → Webhooks → Deliveries*), and ask apps to reconcile open orders with `GET /v1/payments/{id}` if they missed `payment.succeeded`.
6. **Tell customers** with open orders that the payment status will update shortly; their provider receipt is proof of payment in the meantime.

If the master key is lost, the database cannot be decrypted: provider credentials, webhook secrets and TOTP secrets must be entered again (new provider accounts, new endpoint secrets, `reset-2fa` for every user). This is why the key is backed up separately.
