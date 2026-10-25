# Features

The complete feature list of Paymux. The [README](../README.md) has the short version.

## What is built

**Payments**

- **Create and track payments** through a JSON API: [`/v1/payments`](openapi.yaml), refunds, idempotency keys, per-key rate limits, test and live modes.
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
- **Deploy kit** in [`deploy/`](../deploy): Caddyfile with on-demand TLS, an nginx site for a proxy on another device (`deploy/nginx/`), a hardened systemd unit, a Litestream config and a distroless Docker image (`Dockerfile`, non-root, static). `scripts/release.sh` builds linux and darwin archives for amd64 and arm64 with `SHA256SUMS` and optional cosign signatures; GitHub Actions workflows run the CI gate and tagged releases.

**Extending**

- **Connectors.** A connector is a Go package against the small public SDK in `pkg/connector`, with a conformance kit and fuzz harness. Or run it **out of process** in any language: the `remote` connector speaks a signed HTTP protocol (`pkg/connector/wire`).
- **Hooks and framework mode.** Write a small `main` that calls `framework.Main` and register Go hooks (`OnPaymentSucceeded`, `OnRefundSucceeded`, `OnDisputeOpened`, `OnSubscriptionRenewed`, `OnInvoiceIssued`, `OnRoute`) that run after commit and cannot affect money. `paymux build --with` adds modules to the stock binary without writing a `main`.
