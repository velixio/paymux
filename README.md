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
                                        └─ Whop
```

> **Status:** under active development. The core API, connectors and webhooks are built; see `docs/superpowers/plans/2026-10-17-paymux-roadmap.md` for what's next.

## Highlights

- **One binary, no other services to run.** Pure Go (`CGO_ENABLED=0`), with SQLite or Postgres. It needs no Redis, Kafka or Kubernetes.
- **Provider-neutral API.** Apps see payments, refunds and events, never provider specifics. Adding a provider means writing one connector against the public SDK in `pkg/connector`.
- **Correct by construction:**
  - Each state change, its ledger entries and its events are written in one transaction, using a transactional outbox.
  - A provider webhook is treated as a hint. Paymux re-fetches the truth from the provider's API before changing anything.
  - Signatures are checked in constant time.
  - Outgoing webhooks use the [Standard Webhooks](https://www.standardwebhooks.com) format.
- **Built for India:**
  - GST tax invoices and credit notes, with gapless numbering.
  - CGST/SGST or IGST, and exports under a Letter of Undertaking (LUT).
  - Recurring mandates that follow the RBI and NPCI rules: pre-debit notices, the ₹15,000 limit, and UPI execution windows.
- **Routing rules.** Rules are evaluated at app level, then org level, then a catch-all. Each provider account has its own list of accepted currencies, and a circuit breaker skips accounts that keep failing.

## Quick start

New here? [`docs/quickstart.md`](docs/quickstart.md) takes you from nothing to a test payment, a signed webhook, an invoice and a refund in ten minutes, with no provider account: it uses the built-in simulator. The short version follows.

You need a Go toolchain of version 1.21 or later. `go.mod` pins `toolchain go1.27.2`, and Go downloads it automatically.

```bash
make build                       # → dist/paymux
cp .env.example .env             # set PAYMUX_MASTER_KEY: head -c32 /dev/urandom | base64
./dist/paymux migrate
./dist/paymux app create --name "Voicestudio" --slug voicestudio --mode test   # prints an API key once
./dist/paymux serve
curl localhost:8080/readyz
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

## Configuration

Settings are read from environment variables, or from a `.env` file. A real environment variable takes priority over the same key in `.env`.

| Variable | Default | |
|---|---|---|
| `PAYMUX_ENV` | `production` | `production` or `development` |
| `PAYMUX_BASE_URL` | required | Public URL of the hub |
| `PAYMUX_LISTEN` | `:8080` | Public listener |
| `PAYMUX_INTERNAL_LISTEN` | `127.0.0.1:9100` | `/metrics`, pprof, TLS-ask endpoint |
| `PAYMUX_DATA_DIR` | `./data` | SQLite file, custom templates and themes |
| `PAYMUX_DATABASE_URL` | `sqlite://<data dir>/paymux.db` | Or `postgres://…` |
| `PAYMUX_MASTER_KEY` / `_FILE` | required | 32 bytes, base64; encrypts secrets at rest |
| `PAYMUX_TZ` | `Asia/Kolkata` | Time zone used for reconciliation and report windows |
| `PAYMUX_SMTP_HOST`, `_PORT`, `_USERNAME`, `_PASSWORD`, `_FROM` | unset | Invoice email and ops alerts |
| `PAYMUX_OPS_EMAILS`, `PAYMUX_OPS_WEBHOOK_URL` | unset | Ops alert targets |
| `PAYMUX_JS_HOOKS_DIR` | `<data dir>/hooks` | Folder of JavaScript hook files; hooks are off if it does not exist (see docs/extending.md) |
| `PAYMUX_JS_HOOKS_ALLOWED_HOSTS` | unset | Hosts `fetch()` in JavaScript hooks may call, comma separated (`hooks.slack.com,*.example.com`) |
| `PAYMUX_ALLOW_PRIVATE_WEBHOOKS` | `false` | Allow webhook URLs that resolve to private IPs |
| `PAYMUX_DEV_SKIP_2FA` | `false` | Honoured only in development on a local host |

Everything else is data that you edit in the dashboard or with the CLI: apps, provider accounts, routing rules, tax entities and policy settings. Policy settings resolve as app value, then org value, then built-in default.

## Development

```bash
make test       # SQLite
make test-pg    # Postgres (uses PAYMUX_TEST_POSTGRES_URL or starts postgres:17 in Docker)
make ci         # gofmt, vet, staticcheck, govulncheck, both databases, seam checks, cross builds
```

**Architecture rules, enforced by `internal/seams`:**

- All SQL lives in `internal/store`.
- Connectors import only `pkg/...`.
- `main.go` stays thin.
- Policy values are read through `internal/settings`.

**Docs:**

- Design spec: `docs/superpowers/specs/2026-10-17-paymux-design.md`
- API: `docs/openapi.yaml`

## License

[Apache License 2.0](LICENSE).
