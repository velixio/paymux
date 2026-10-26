# Configuration reference

Paymux separates **deployment configuration** from **organization and application settings**. Environment variables configure listeners, persistence, encryption, mail, SSO, and hook execution. Dashboard settings control payment and delivery behavior and expose effective values and sources.

## Loading and validation

CLI commands read the process environment and an optional dotenv file, selected by `PAYMUX_ENV_FILE` (default `.env`). Existing process variables take precedence. Do not commit a populated environment file. Startup validates the configuration and reports problems rather than inventing missing secrets.

Configuration examples below use placeholder domains and credentials. Copy `.env.example` for development and replace its master-key placeholder before starting.

## Core deployment variables

| Variable | Default | Meaning |
|---|---|---|
| `PAYMUX_ENV` | `production` | `production` or `development`; independent of payment test/live mode. |
| `PAYMUX_BASE_URL` | Required | Hub public origin. HTTPS is required except localhost addresses. |
| `PAYMUX_CHECKOUT_URL` | Unset | Optional, recommended in production. Origin of the customer-facing pages (`/pay/`, `/l/`, `/s/`, `/i/`, `/return/`, `/sim/`), e.g. `https://checkout.example.com`. Must be an origin (no path or query) on a host name (not an IP address) that differs from `PAYMUX_BASE_URL`'s; HTTPS is required except localhost addresses. When set, the pages are served on that host only (and it serves nothing else), the base host redirects them there with a 308, and every customer URL (`checkout_url`, payment-link and invoice URLs, subscription checkouts, provider return URLs, emails) uses it. Webhooks, the API and the dashboard stay on the base host. A production hub without it logs a warning at startup. See [installation](installation.md#two-host-names-recommended-in-production). |
| `PAYMUX_LISTEN` | `:8080` | Public listener for API, checkout, dashboard, and provider webhooks. |
| `PAYMUX_INTERNAL_LISTEN` | `127.0.0.1:9100` | Private metrics, profiling, and custom-domain TLS authorization listener. |
| `PAYMUX_DATA_DIR` | `./data` | Database default location, custom files, and hooks default directory. |
| `PAYMUX_DATABASE_URL` | SQLite under the data directory | `sqlite://...` or `postgres://...`. For a PostgreSQL server that is not on the same machine use `?sslmode=verify-full` (add `&sslrootcert=` for a private CA); any other mode logs a warning at startup. |
| `PAYMUX_MASTER_KEY` | Exactly one key source required | Base64 encoding of 32 random bytes. |
| `PAYMUX_MASTER_KEY_FILE` | Exactly one key source required | File containing the same base64 key; alternative to the direct variable. |
| `PAYMUX_MASTER_KEY_OLD` | none | Earlier master keys, base64, comma separated. Only for a key rotation: values sealed under them can still be read. See the runbook, *Rotating the master key*. |
| `PAYMUX_MASTER_KEY_OLD_FILE` | none | A file with earlier master keys (separated by commas or new lines); combined with the variable above. |
| `PAYMUX_TZ` | `Asia/Kolkata` | Date boundaries, scheduling, and date filters. Use an IANA time-zone name. |
| `PAYMUX_TRUSTED_PROXIES` | Local network | Comma-separated trusted proxy addresses or CIDRs, or `none`. Unset, any address on the local network (loopback, 10/8, 172.16/12, 192.168/16, link-local, fc00::/7) is believed, so a reverse proxy on the LAN needs no setting; a host on those networks can then set the client address Paymux records. A `/0` network is rejected. |
| `PAYMUX_CUSTOMER_RATE` | `60` | Requests a minute one client address may make to each customer route family (`/pay/`, `/return/`, `/l/`, `/s/`, `/i/`), with a burst of 20; past it the page answers 429 with `Retry-After`. `0` turns the limit off. The address is the connection's, or the `X-Forwarded-For` client when the connection comes from a trusted proxy; an IPv6 client is limited per /64 (every address in one /64 shares a bucket). Behind a reverse proxy, `PAYMUX_TRUSTED_PROXIES` must name it, or all customers share the proxy's bucket. Webhooks and `/v1` have their own limits. |
| `PAYMUX_ENV_FILE` | `.env` | Optional dotenv file loaded by CLI commands. |

Generate a key once, then preserve it separately from the database:

```sh
umask 077
head -c 32 /dev/urandom | base64 > master.key
```

Choose either `PAYMUX_MASTER_KEY_FILE` or `PAYMUX_MASTER_KEY`. Regenerating the key while keeping the database makes previously encrypted provider credentials, webhook secrets, and TOTP secrets unreadable. Setting a new variable is not a key-rotation procedure; [Rotating the master key](runbook.md#rotating-the-master-key) is.

## Development switches

| Variable | Default | Use |
|---|---|---|
| `PAYMUX_DEV_SKIP_2FA` | `false` | Allows development sign-in without the second factor; rejected in production. |
| `PAYMUX_ALLOW_PRIVATE_WEBHOOKS` | `false` | Allows outbound app webhooks to private or loopback destinations; needed for the local quickstart receiver. |

These switches are not substitutes for configuring a public endpoint and dashboard authentication in production. A test API key still uses test data even when `PAYMUX_ENV=production`.

## Email and operational alerts

| Variable | Default | Meaning |
|---|---|---|
| `PAYMUX_SMTP_HOST` | Unset | SMTP server; leaving it unset disables email. |
| `PAYMUX_SMTP_PORT` | `587` | SMTP port using STARTTLS. |
| `PAYMUX_SMTP_USERNAME` | Empty | SMTP username, when needed. |
| `PAYMUX_SMTP_PASSWORD` | Empty | SMTP password or application credential. |
| `PAYMUX_SMTP_FROM` | Required with SMTP host | Sender mailbox, optionally with display name. |
| `PAYMUX_OPS_EMAILS` | Empty | Comma-separated alert recipient addresses. |
| `PAYMUX_OPS_WEBHOOK_URL` | Empty | Slack-compatible destination for operator alerts. |
| `PAYMUX_DB_LOG_LEVEL` | `warn` | Lowest level of application log records stored for the dashboard's Logs page: `warn`, `error`, or (on purpose) `info`, `debug`. |
| `PAYMUX_OTLP_LOGS_ENDPOINT` | Unset | OpenTelemetry collector URL (OTLP/HTTP, JSON); the stored, redacted rows are also posted there. A private address is allowed. |
| `PAYMUX_OTLP_LOGS_HEADERS` | Unset | `Name=value,Name=value` sent with each OTLP request. Treated as a secret. |

The last three are described in the runbook under *Logs*. SMTP settings affect invoice and operational email. The ops webhook is separate from an application's signed payment-event endpoint. Alerts also appear in the dashboard; acknowledge them after investigation.

## Dashboard sessions

These are organisation settings (the dashboard's *Settings* page), not environment variables.

| Setting | Default | Meaning |
|---|---|---|
| `session_idle_hours` | `12` (1 to 720) | A session ends after this many hours without a request. |
| `session_max_days` | `7` (1 to 90) | A session ends this many days after sign-in, however active. |
| `session_remember_days` | `30` (0 to 90) | With *Remember me* ticked, both limits become this many days. `0` removes the checkbox; sessions already remembered fall back to the two limits above. |

## Dashboard single sign-on

| Variable | Default | Meaning |
|---|---|---|
| `PAYMUX_OIDC_ISSUER` | Unset | Identity provider issuer URL; setting it enables OIDC when the client credentials are also present. |
| `PAYMUX_OIDC_CLIENT_ID` | Unset | Registered OIDC client identifier. |
| `PAYMUX_OIDC_CLIENT_SECRET` | Unset | Client secret. |
| `PAYMUX_OIDC_ALLOWED_DOMAINS` | Empty | Email domains allowed for new users at first sign-in. |
| `PAYMUX_OIDC_DEFAULT_ROLE` | `viewer` | Role for auto-created users; must not be admin. |

Issuer, client ID, and secret must be configured together. Follow the SSO instructions in the [security guide](security.md) for callback registration and account behavior; existing users and new-user provisioning are different cases.

## JavaScript hooks

| Variable | Default | Meaning |
|---|---|---|
| `PAYMUX_JS_HOOKS_DIR` | `<data dir>/hooks` | Directory containing optional JavaScript hooks. |
| `PAYMUX_JS_HOOKS_ALLOWED_HOSTS` | Empty | Hosts hook `fetch()` may contact; supports patterns such as `*.example.com`, without scheme, port, or path. |

Read [extension points](extending.md) before enabling hooks. Post-commit hooks cannot make a payment transaction atomic with an external application.

## Organization settings and app overrides

Use **Settings** for organization defaults and the application's **Settings** tab for permitted overrides. The interface shows the effective value and where it comes from. Resetting an override restores inheritance; it does not copy the current organization value into the application.

Review routing, payment expiry, duplicate-payment handling, delivery retry schedules, retention, and tax policies before changing them. Save one change, verify the resulting effective value, and use the audit log for administrative changes.

For deployment examples and operational recovery, see the [runbook](runbook.md). The authoritative bootstrap definitions are in [config.go](../internal/config/config.go).
