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
| `PAYMUX_LISTEN` | `:8080` | Public listener for API, checkout, dashboard, and provider webhooks. |
| `PAYMUX_INTERNAL_LISTEN` | `127.0.0.1:9100` | Private metrics, profiling, and custom-domain TLS authorization listener. |
| `PAYMUX_DATA_DIR` | `./data` | Database default location, custom files, and hooks default directory. |
| `PAYMUX_DATABASE_URL` | SQLite under the data directory | `sqlite://...` or `postgres://...`. |
| `PAYMUX_MASTER_KEY` | Exactly one key source required | Base64 encoding of 32 random bytes. |
| `PAYMUX_MASTER_KEY_FILE` | Exactly one key source required | File containing the same base64 key; alternative to the direct variable. |
| `PAYMUX_TZ` | `Asia/Kolkata` | Date boundaries, scheduling, and date filters. Use an IANA time-zone name. |
| `PAYMUX_TRUSTED_PROXIES` | Local network | Comma-separated trusted proxy addresses or CIDRs, or `none`. Unset, any address on the local network (loopback, 10/8, 172.16/12, 192.168/16, link-local, fc00::/7) is believed, so a reverse proxy on the LAN needs no setting; a host on those networks can then set the client address Paymux records. A `/0` network is rejected. |
| `PAYMUX_ENV_FILE` | `.env` | Optional dotenv file loaded by CLI commands. |

Generate a key once, then preserve it separately from the database:

```sh
umask 077
head -c 32 /dev/urandom | base64 > master.key
```

Choose either `PAYMUX_MASTER_KEY_FILE` or `PAYMUX_MASTER_KEY`. Regenerating the key while keeping the database makes previously encrypted provider credentials, webhook secrets, and TOTP secrets unreadable. Setting a new variable is not a key-rotation procedure.

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
