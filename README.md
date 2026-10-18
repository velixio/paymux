# Paymux

Paymux is a self-hosted payments hub that ships as a single binary. It sits between your apps and payment providers. Each app integrates with Paymux once. Paymux then:

- routes each payment to a provider;
- receives the provider's webhooks (IPN) and checks them;
- keeps one ledger of payments, refunds, disputes and subscriptions;
- issues GST invoices;
- sends your apps signed webhooks.

```
app A ─┐                               ┌─ Razorpay
app B ─┼─ /v1 API ─► Paymux ◄─ webhooks ┼─ PayU India
app C ─┘ ◄─ signed webhooks ─┘          ├─ Zoho Payments
                                        ├─ Whop
                                        └─ your own (Go package or HTTP service)
```

> **Status:** under active development. Everything listed under "What is built" below works and is tested on both databases; see `docs/superpowers/plans/2026-10-17-paymux-roadmap.md` for what is next. The public SDK (`pkg/...`) and the framework hooks API are guarded by a snapshot in CI (`api/pkg.txt`), so they do not change by accident.

## What is built

**Payments**

- **Create and track payments** through a JSON API: [`/v1/payments`](docs/openapi.yaml), refunds, idempotency keys, per-key rate limits, test and live modes.
- **Hosted pay page.** The customer sees a themed, translated page that asks for billing details only where the rules require them, then goes to the provider.
- **Payment links.** A shareable page `/l/{slug}` creates a payment for whoever opens it: fixed price or "customer chooses" between a minimum and maximum, reusable or single-use, optional expiry. Created through the API or the dashboard, and served on custom domains too.
- **Routing.** Rules at app level, then org level, then a catch-all; per-account accepted currencies and purposes; a circuit breaker skips accounts that keep failing.
- **Custom domains** with on-demand TLS (Caddy asks the hub which hosts to certify) and a domain checklist.

**Money you can reconcile**

- **One ledger.** A state change, its ledger entries and its events commit in one transaction (transactional outbox). A provider webhook is only a hint: Paymux re-fetches the truth before changing anything.
- **Disputes.** Inbox with due dates, evidence upload and accept/submit for providers that support it, manual entry for those that do not, credit notes for lost disputes.
- **Daily reconciliation** against each provider's own records, with an items list to resolve, a "run now" button and settlements.
- **Subscriptions.** Provider-managed billing (Razorpay, Whop) and merchant-triggered billing on UPI/card/e-mandate mandates (PayU, Zoho) with dunning, pre-debit notices by provider and by e-mail, and the RBI/NPCI limits.

**India**

- **GST invoicing.** Tax invoices and credit notes with gapless numbering per fiscal year, CGST/SGST or IGST, LUT exports, FX rates, hosted copies, e-mail delivery and GST reports as CSV. Prices can include tax or exclude it (the amount you send is then the taxable value and Paymux adds the GST). PDFs can be digitally signed (PAdES) with a PKCS#12 certificate uploaded on the tax entity. Tax exceptions raise ops alerts, and the credit-note cutoff and the B2CL threshold are settings.
- **State, country and address capture** on the pay page per the GST rules; a buyer abroad makes the sale an export.

**Operations**

- **Dashboard.** Payments, subscriptions, disputes, reconciliation, invoices, reports, apps, providers, tax entities, FX rates, settings, audit log and users. Roles (viewer, operator, admin), password plus TOTP, sessions, CSRF protection and a login lockout.
- **First-run setup wizard.** A new hub shows a one-time setup page, protected by a code printed in the log, that creates the admin (with 2FA), names the org and optionally creates a first app with its API keys.
- **Webhooks to your apps** in the [Standard Webhooks](https://www.standardwebhooks.com) format, with retries, an SSRF guard, delivery history and replay. Events include `payment.*`, `refund.*`, `dispute.*`, `subscription.*`, `invoice.issued`, `credit_note.issued` and `payment_link.*`.
- **Themes and languages.** Two bundled themes (`default`, `minimal-dark`) and your own in `<data>/themes/`, applied without a restart; customer pages and the pre-debit e-mail in English and Hindi, with your own catalogs in `custom/i18n/`.
- **Health and metrics.** `/healthz`, `/readyz`, Prometheus `/metrics`, structured logs with secret and PII redaction, ops alerts by e-mail and webhook.
- **GST e-invoice export (offline).** B2B and export invoices, and their credit notes (`CRN`), as the government's INV-01 JSON, per document or per month, checked against a committed schema; record the IRN, acknowledgement and signed QR code the portal returns and they are printed (with a QR code) on the PDF and the hosted page.
- **Alerts center.** Every ops alert is kept and listed in the dashboard with a count in the nav and an acknowledge button, as well as being sent by email and webhook.
- **Phone-friendly pages.** The dashboard and the customer pages are checked at 360px wide by tests: scrolling tables, stacked navigation, full-width forms.
- **API reference** rendered by the hub itself at `/docs` (the raw spec at `/docs/openapi.yaml`), no JavaScript.

**Databases and deployment**

- **SQLite or Postgres**, the same code and the same tests on both (`PAYMUX_DATABASE_URL`). SQLite backs up continuously with Litestream; Postgres with `pg_dump` or WAL archiving.
- **Deploy kit** in [`deploy/`](deploy): Caddyfile with on-demand TLS, an nginx site for a proxy on another device (`deploy/nginx/`), a hardened systemd unit, a Litestream config and a distroless Docker image (`Dockerfile`, non-root, static). `scripts/release.sh` builds linux and darwin archives for amd64 and arm64 with `SHA256SUMS` and optional cosign signatures; GitHub Actions workflows run the CI gate and tagged releases.

**Extending**

- **Connectors.** A connector is a Go package against the small public SDK in `pkg/connector`, with a conformance kit and fuzz harness. Or run it **out of process** in any language: the `remote` connector speaks a signed HTTP protocol (`pkg/connector/wire`).
- **Hooks and framework mode.** Write a small `main` that calls `framework.Main` and register Go hooks (`OnPaymentSucceeded`, `OnRefundSucceeded`, `OnDisputeOpened`, `OnSubscriptionRenewed`, `OnInvoiceIssued`, `OnRoute`) that run after commit and cannot affect money. `paymux build --with` adds modules to the stock binary without writing a `main`.

## Connectors

Which provider can do what, generated from each connector's declared `Info` (the full table, with currencies, methods and mandate caps, is [`docs/capabilities.md`](docs/capabilities.md)):

| Connector | Refunds | Disputes (read / respond) | Reconciliation | Settlements | Provider-managed subscriptions | Mandates |
|---|---|---|---|---|---|---|
| Razorpay | yes | yes / yes | yes | yes | yes | - |
| PayU India | yes | - | yes | - | - | yes |
| Zoho Payments | yes | - | - | - | - | yes |
| Whop (merchant of record) | yes | - | - | - | yes | - |
| Stripe | yes | yes / yes | yes | yes | yes | - |
| Cashfree | yes | - | - | yes | - | - |
| Remote (your service) | per service | per service | per service | per service | per service | per service |

Whop is a merchant of record, so no GST invoice is issued for its payments. The matrix is regenerated with `go run ./scripts/capmatrix -w`; CI fails when it is stale.

## Documentation

Start with the [complete documentation index](docs/README.md). Guides cover installation, application integration, payments and routing, signed webhooks, billing, dashboard operations, configuration, security, recovery, and troubleshooting. The [glossary](docs/glossary.md) defines the product language. The separate public site renders the same Markdown at `/docs` and `/docs/<guide>`; the payment hub’s `/docs` remains its API reference.

## Quick start

New here? [`docs/quickstart.md`](docs/quickstart.md) takes you from nothing to a test payment, a signed webhook, an invoice and a refund in ten minutes, with no provider account: it uses the built-in simulator. The short version follows.

Building from source also requires Bun 1.4.3 or newer to compile the React dashboard. Distributed binaries do not need a JavaScript runtime. You need a Go toolchain of version 1.21 or later. `go.mod` pins `toolchain go1.27.2`, and Go downloads it automatically.

```bash
make build                       # → bin/paymux, bin/paymux-home and the example binaries
make dev                         # build everything, run the hub and the website together (Ctrl-C stops both)
cp .env.example .env             # set PAYMUX_MASTER_KEY: head -c32 /dev/urandom | base64
./bin/paymux                     # the log prints a one-time setup code
```

Open `http://localhost:8080/setup`, enter the code and create the administrator (the first-run wizard also offers a first app and its API keys). Prefer the command line? This works too (with SQLite, while `serve` is stopped):

```bash
./bin/paymux app create --name "Voicestudio" --slug voicestudio --mode test   # prints an API key once
```

Create a payment:

```bash
curl -s localhost:8080/v1/payments \
  -H "Authorization: Bearer pmx_test_sk_..." \
  -H "Idempotency-Key: order-123" \
  -d '{"amount":49900,"currency":"INR","order_ref":"order-123","description":"Pro plan"}'
# → { "id": "pay_…", "status": "created", "checkout_url": "https://…/pay/pay_…?t=…" }
```

Send the customer to `checkout_url`. Your app then learns the outcome from a signed webhook (`paymux endpoint add --app … --url …`) or from `GET /v1/payments/{id}`. Never trust the browser redirect on its own.

Or create a link to share, with no code on your side:

```bash
curl -s localhost:8080/v1/payment_links \
  -H "Authorization: Bearer pmx_test_sk_..." \
  -d '{"amount":49900,"currency":"INR","description":"Pro plan, annual","single_use":true}'
# → { "id": "plink_…", "url": "https://…/l/k3n9x0q7vb2m", … }
```

The full reference is served by your own hub at `/docs`.

## Configuration

Settings are read from environment variables, or from a `.env` file. A real environment variable takes priority over the same key in `.env`.

| Variable | Default | |
|---|---|---|
| `PAYMUX_ENV` | `production` | `production` or `development` |
| `PAYMUX_BASE_URL` | required | Public URL of the hub |
| `PAYMUX_LISTEN` | `:8080` | Public listener |
| `PAYMUX_INTERNAL_LISTEN` | `127.0.0.1:9100` | `/metrics`, pprof, TLS-ask endpoint |
| `PAYMUX_DATA_DIR` | `./data` | SQLite file, `themes/`, `custom/` templates and catalogs |
| `PAYMUX_DATABASE_URL` | `sqlite://<data dir>/paymux.db` | Or `postgres://…` |
| `PAYMUX_MASTER_KEY` / `_FILE` | required | 32 bytes, base64; encrypts secrets at rest |
| `PAYMUX_TZ` | `Asia/Kolkata` | Time zone used for reconciliation and report windows |
| `PAYMUX_TRUSTED_PROXIES` | local network | Reverse proxies whose `X-Forwarded-For` is believed (login limit, sessions, audit); `none` to believe no proxy |
| `PAYMUX_SMTP_HOST`, `_PORT`, `_USERNAME`, `_PASSWORD`, `_FROM` | unset | Invoice and pre-debit e-mail, ops alerts |
| `PAYMUX_OPS_EMAILS`, `PAYMUX_OPS_WEBHOOK_URL` | unset | Ops alert targets |
| `PAYMUX_JS_HOOKS_DIR` | `<data dir>/hooks` | Folder of JavaScript hook files; hooks are off if it does not exist (see docs/extending.md) |
| `PAYMUX_JS_HOOKS_ALLOWED_HOSTS` | unset | Hosts `fetch()` in JavaScript hooks may call, comma separated (`hooks.slack.com,*.example.com`) |
| `PAYMUX_ALLOW_PRIVATE_WEBHOOKS` | `false` | Allow webhook URLs that resolve to private IPs |
| `PAYMUX_DEV_SKIP_2FA` | `false` | Honoured only in development on a local host |
| `PAYMUX_OIDC_ISSUER`, `_CLIENT_ID`, `_CLIENT_SECRET` | unset | Enable OpenID Connect single sign-on (all three together) |
| `PAYMUX_OIDC_ALLOWED_DOMAINS` | unset | Comma-separated e-mail domains whose users may be created at first sign-in |
| `PAYMUX_OIDC_DEFAULT_ROLE` | `viewer` | Role of those users (never `admin`) |

Everything else is data that you edit in the dashboard or with the CLI: apps, provider accounts, routing rules, tax entities, themes, languages and policy settings. Policy settings resolve as app value, then org value, then built-in default.

## Documentation

| | |
|---|---|
| [Runbook](docs/runbook.md) | Install (binary, systemd, Docker), first admin and 2FA, providers and webhook URLs, themes and languages, backups and restore, monitoring, what to do when the hub is down |
| [Upgrades](docs/runbook.md#upgrades) | Verifying a release, backing up, applying migrations, rolling back |
| [Extending](docs/extending.md) | Framework mode, hooks and their guarantees, `paymux build --with`, the public API |
| [Writing connectors](docs/connectors.md) | The SDK, capabilities, conformance kit, fuzzing, remote connectors |
| [Connector capabilities](docs/capabilities.md) | The matrix of connectors and what each supports |
| [API reference](docs/openapi.yaml) | OpenAPI 3.1; rendered at `/docs` on a running hub |
| [Performance](docs/performance.md) | Targets and the load test |
| [Dependencies](docs/dependencies.md) | Third-party code and why |
| [Provider verification](docs/provider-verification.md) | What was written from provider documentation and still has to be checked against a live sandbox, as a checklist per provider |
| [Security review](docs/security-review.md) | Findings of the correctness and security audit, with severities and dispositions |
| [Design spec](docs/superpowers/specs/2026-10-17-paymux-design.md) | The source of truth for behaviour |

## Access control

- **Roles.** `admin`, `operator` and `viewer` are built in. Admins create custom roles on the dashboard's Roles page by ticking actions, and assign them on the Users page; the user and role management actions stay with admins. Every change is audited.
- **API key scopes.** A key may be limited to `payments:read`, `payments:write`, `refunds:read`, `refunds:write`, `subscriptions:read`, `subscriptions:write` and `invoices:read`, `payouts:read` and `payouts:write` (`x:write` includes `x:read`). A missing scope answers 403 `insufficient_scope`; keys without scopes have all of them except the payouts ones, which must be listed: sending money out is always a deliberate grant. Create scoped keys on the app's Keys tab or with `paymux app key --app SLUG --scopes payments:read,refunds:write`; `docs/openapi.yaml` lists the scope of each operation.
- **Payouts.** `POST /v1/payouts` sends money to a bank account or UPI id through a provider account that has the `payout` purpose and a connector that can pay out. It needs the `payout.create` action (admin only unless a custom role is given it) and the explicit `payouts:write` key scope. The beneficiary is stored encrypted and shown masked; the provider gets the payout's own reference as an idempotency token.
- **Logs.** The dashboard's Logs page (errors, failed provider calls, rejected requests, failed jobs, with secrets and buyers' details removed) needs the `logs.read` action: admin only unless a custom role is given it. See the runbook's *Logs* section.
- **Key rotation.** `paymux app key --app SLUG --mode live --rotate [--grace-hours 24]` (or Rotate on the Keys tab) creates a new key and lets the current keys of that mode keep working for the grace period, then stops them.
- **Single sign-on.** With the `PAYMUX_OIDC_*` settings the login page offers an OpenID Connect sign-in (authorization code flow with PKCE, state and nonce). Users are matched by verified e-mail; someone new is created only for an allowed domain, with the default role. The identity provider's sign-in replaces TOTP; password logins still need TOTP. The redirect URI to register is `<PAYMUX_BASE_URL>/dashboard/login/oidc/callback`.

## Development

```bash
make test       # SQLite
make test-pg    # Postgres (uses PAYMUX_TEST_POSTGRES_URL or starts postgres:17 in Docker)
make ci         # gofmt, vet, API snapshot, capability matrix, staticcheck, govulncheck, both databases, cross builds, docker build
scripts/release.sh v1.2.3   # release archives with checksums (and a cosign signature if cosign is installed)
```

**Architecture rules, enforced by `internal/seams`:**

- All SQL lives in `internal/store`.
- Connectors import only `pkg/...`.
- `main.go` stays thin.
- Policy values are read through `internal/settings`.

## License

[Apache License 2.0](LICENSE).

## Public website

The React marketing and developer-entry website lives in [`web-home/`](web-home) and runs independently of the payment hub. Both frontends use TanStack Router and shadcn/ui; the dashboard also uses TanStack Query. They use Vite client builds, without TanStack Start, SSR or new APIs. The home site can deploy directly to a static host or use its separate Go serving binary.

```bash
bun install --frozen-lockfile
make build-home
./bin/paymux-home -listen 127.0.0.1:8051
```

Routes: `/`, `/integrations`, `/docs`, `/self-hosting`; readiness: `/healthz`. Bind behind your TLS reverse proxy for public hosting. The payment hub continues to serve its own operational dashboard and API reference separately.

Frontend development: `bun run dev:home` serves the public site; `bun run dev` serves the dashboard with a proxy to the existing hub. See [`web-dashboard/README.md`](web-dashboard/README.md) for the existing page/form adapter contract and development setup.
