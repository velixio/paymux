# Paymux — Design Spec

*Status: draft v2 for review · Date: 2026-10-17 · Name: **Paymux** (payment multiplexer).*

This spec covers two delivery paths for one codebase:

- **Path 1 — internal hub.** Runs payments, subscriptions and GST invoicing for our own three products (voicestudio, supermetry, yupcha).
- **Path 2 — self-hostable product.** Lets other companies download, extend and theme it.

**Path 2 is built on top of Path 1. It is not a rewrite.** Section 14 lists the seams Path 1 must build in so that Path 2 only adds to it.

**v2 changes** (from the user's answers):
- Subscriptions and mandates move into Path 1.
- GST invoicing, credit notes and fee/tax reports are added.
- Apps can have their own custom domains.
- SQLite **and** Postgres are first-class from day one.
- Global default routing rules, with BDT going to Razorpay then Whop.
- Optional auto-refund of duplicate payments.
- A dev-only 2FA skip.
- Toolchain auto-download.
- No import of old payments.

---

## 1. Intent and why

We sell through several small products. Each needs to take one-time and **recurring** payments in local currency through several providers: Razorpay, PayU India, Zoho Payments India and Whop, with more later. Today this hurts in four ways:

1. **Webhook fan-out.** Providers give one account per company. Their webhooks (IPN) go to every site, or the sites have to work out which events belong to them.
2. **Duplicated payment logic.** Each site would need its own code for payments, refunds, disputes, subscriptions and reconciliation. That is the hardest code to get right, and it would be written three times.
3. **GST compliance.** Every sale needs a GST-compliant tax invoice and every refund needs a credit note. Provider fees and the GST on them have to be tracked for input tax credit.
4. **Recurring billing differs per provider.** Razorpay runs subscriptions itself. PayU and Zoho give us mandates and we trigger each debit. Whop runs memberships.

Paymux is one small service between our apps and the providers:

```
voicestudio ─┐                              ┌─ Razorpay
supermetry  ─┼─ HTTPS API ─► Paymux ◄─ IPN ─┼─ PayU India
yupcha      ─┘ ◄─ signed webhooks ─┘        ├─ Zoho Payments
                 customers ─► pay pages,    └─ Whop
                 invoices (hub or app domain)
```

Apps ask Paymux for a checkout or a subscription. Paymux picks a provider, sends the customer to that provider's hosted page, tracks the truth, issues GST invoices, and tells the app what happened.

**Why not Hyperswitch.** We evaluated it on 2026-10-17:

- It does not support Zoho or Whop.
- Its PayU connector is PayU Poland, not PayU India.
- Its Razorpay connector is UPI-only, in sandbox status, and its docs say payments get stuck in `requires_customer_action`.
- It runs about 30 services and is sized for roughly 25M transactions a month. We expect about 3k.
- Connectors are compiled Rust spread across about 17 files, with no plugin system.
- Reconciliation is a paid add-on when self-hosting.
- It does no GST invoicing.

Its domain model is still a useful reference. Its "business profile" concept maps to our "App".

**Scale.** About 100 payments a day across all apps, about 36k a year. We design for 10× that without architectural change. Above about 100× that is explicitly out of scope.

## 2. Goals, non-goals, anti-goals

### Goals

1. **Apps never talk to providers.** They integrate once with Paymux and get provider-neutral objects and events, for one-time payments **and subscriptions**.
2. **Every provider webhook is routed correctly** to the owning app, exactly-once in effect, with retries and replay.
3. **One source of truth** for payments, refunds, disputes, subscriptions and invoices across all apps and providers, reconciled against provider records.
4. **Adding a provider requires no app changes.** Adding a provider is writing one connector.
5. **GST-compliant documents.** Paymux issues tax invoices on every successful sale and credit notes on refunds, with gapless numbering. It also produces monthly reports: sales register, GSTR-1-oriented exports, and provider fees with GST on fees.
6. **Single static binary** (`CGO_ENABLED=0`) with an embedded dashboard. It runs on **SQLite or Postgres** with identical behavior, and fits on a 1 vCPU / 1 GB VPS (SQLite) or scales to several instances (Postgres).
7. **Custom domains are optional.** Pay pages and invoices are served from the hub domain or from each app's own domain.
8. **Path 2 is additive.** Plugins, themes and hooks are extensions of seams that already exist in Path 1.

### Non-goals (out of scope for this product, not just "later")

- **Handling raw card data, a card vault, or our own 3DS.**
  - RBI card-on-file rules (Oct 2022) forbid merchants from storing card numbers.
  - Touching card data puts us in full PCI DSS scope.
  - Hosted checkout keeps us at SAQ-A.
- **Success-rate or auth-rate routing.** It needs thousands of payments per bucket to beat noise.
- **Cross-provider "smart retries".** With hosted checkout these are visible to the customer.
- **ML fraud scoring.** Providers already screen. Whop is merchant of record and carries fraud risk itself.
- **Kafka, ClickHouse, OpenSearch, Redis or Kubernetes.**
- **Filing GST returns.** Paymux produces compliant documents and the report data. Defaults come from researched rules (§10). Every rule-derived value is a setting or a versioned template, so whoever files the returns can adjust it without a code change.
- **Importing payments made before Paymux.** The user decided not to. Provider events about older payments are recorded as `unknown_provider_object` and otherwise ignored.
- **Multi-tenant SaaS hosting for strangers.** One install serves one organization. The `org_id` seam keeps a Path 3 open.

### Later (Path 2 or on demand, designed as seams now)

- GST e-invoicing (IRN). It is mandatory for B2B and export invoices once aggregate turnover passes ₹5 crore (§10). Path 1 ships the interface and a turnover guard.
- Subscription proration and mid-period plan changes.
- Payment links and payouts.
- Tax regimes other than Indian GST.

### Anti-goals (failure even if it "works")

- Money is taken but the app is never told, or is told the wrong amount.
- A refund is issued twice, or an auto-refund hits the wrong attempt.
- A dispute deadline passes silently.
- A webhook that cannot be verified changes state.
- **An invoice number is skipped or duplicated, or an invoice total differs from the amount paid.**
- **A renewal is charged without the required pre-debit notification, or above the mandate's limit.**
- An app has to change code because we added or switched a provider.
- Behavior differs between SQLite and Postgres.
- The dev-mode 2FA skip can be switched on in production.
- Path 2 needs a rewrite of the core, the store or the connectors.

## 3. Constraints

| Area | Constraint |
|---|---|
| Language | **Latest stable Go**, currently **go1.27.2** (2026-10-08). `go.mod` declares `go 1.27` and `toolchain go1.27.2`. With the default `GOTOOLCHAIN=auto`, any installed Go ≥1.21 downloads the right toolchain automatically. Nobody installs Go 1.27 by hand. `make build` is the entry point. Bump the `toolchain` line when a new patch ships. |
| Build | `CGO_ENABLED=0`, one binary, cross-compiles for linux/amd64 and linux/arm64. |
| Database | **Both first-class from M0:** SQLite through `modernc.org/sqlite` (pure Go), and Postgres ≥14 through `github.com/jackc/pgx/v5/stdlib`. One store implementation (§7). The whole test suite runs on both in CI. |
| Dependencies | Stdlib first. Each third-party dependency needs a one-line justification in `docs/dependencies.md`. No provider SDKs (§9). |
| Checkout | Hosted, redirect or provider-widget only. Card data never reaches Paymux. |
| Providers | Razorpay, PayU India, Zoho Payments India, Whop. Built in that order. |
| Hosting (Path 1) | One small Linux VPS behind Caddy (on-demand TLS for app domains). SQLite with a Litestream backup sidecar, or a managed Postgres. |

## 4. Domain model

**IDs.**
- All IDs are generated by the application.
- Public IDs are prefixed: `pay_`, `att_`, `ref_`, `dsp_`, `sub_`, `mdt_` (mandate), `inv_`, `crn_` (credit note), `cus_`, `evt_`, `app_`, `pa_` (provider account), `whd_` (webhook delivery), `te_` (tax entity), `blob_`.
- The body is a UUIDv7 in lowercase Crockford base32 (26 chars). Use stdlib `uuid` if it provides v7; otherwise use `github.com/google/uuid`.

**Provider-facing references are separate and short.** PayU's refund token is ≤23 characters, Razorpay's receipt ≤40 and Zoho's reference ≤50. So every attempt, refund and mandate gets a `provider_ref` of 20 characters, `[0-9A-Z]` Crockford, time-sortable and unique. Connectors send `provider_ref`, never the long ID.

**Every row carries `org_id` and `mode` (`test` | `live`).** Path 1 has exactly one org. The mode comes from the API key and the provider account, and keeps sandbox and live data separate end to end.

### Entities

| Entity | Key fields | Notes |
|---|---|---|
| **Org** | id, name | One row in Path 1. |
| **User** | email, password_hash (argon2id), totp_secret (encrypted), role | Dashboard users. |
| **App** | id, name, slug, default_return_url, branding (name, logo blob, accent color), payment_ttl, routing_rules, tax_profile, settings | One per product. `settings` holds only the per-app overrides from §8a. Anything unset inherits the org value, then the built-in default. |
| **AppDomain** | app_id, host, is_primary, status (`pending` \| `active` \| `failed`), last_check_at, last_error | Optional custom domains for pay, return, invoice and subscription pages. |
| **APIKey** | app_id, mode, prefix, sha256_hash, last_used_at, revoked_at | Format `pmx_{mode}_sk_<32 random bytes base62>`. Shown once. |
| **WebhookEndpoint** | app_id, url, signing_secret (encrypted), event_types[], enabled | Outbound to apps. |
| **ProviderAccount** | connector, name, mode, config (encrypted JSON, validated against the connector's schema), status, statement_descriptor, provider_gstin, accepted_currencies[], enabled_purposes[] (`one_time`, `subscription`) | Several accounts per connector are allowed, e.g. one per legal entity. `accepted_currencies` defaults to the connector's declared list, and the admin can narrow it (§8). `provider_gstin` feeds the fee input-tax-credit register. |
| **AppProviderAccount** | app_id, provider_account_id | Which accounts an app may use. |
| **Customer** | app_id, external_ref, name, email, phone, billing{legal_name, gstin, line1, line2, city, state_code, postal_code, country} | Billing details come from the API or the pay-page form. |
| **Payment** | app_id, kind (`one_time` \| `renewal` \| `mandate_setup`), subscription_id?, amount, currency, amount_received, amount_refunded, status, order_ref, description, customer_id, metadata (≤20 keys), return_url, expires_at, version, flags | The app's intent to collect money. |
| **PaymentAttempt** | payment_id, provider_account_id, provider_ref, provider_order_id, provider_payment_id, status, method, failure_code, failure_message, raw_last, fee, fee_tax, net, mandate_id? | One try at one provider. A payment has 1..n attempts. |
| **Refund** | payment_id, attempt_id, amount, status, reason, provider_ref, provider_refund_id, initiated_by (`api` \| `dashboard` \| `provider` \| `system`), actor | `system` is the duplicate auto-refund. |
| **Dispute** | attempt_id, amount, currency, reason, status, stage, due_by, provider_dispute_id, evidence (blob ids, notes), source (`webhook` \| `poll` \| `manual`) | |
| **Subscription** | app_id, customer_id, status, billing_mode (`provider_managed` \| `merchant_triggered`), provider_account_id, provider_subscription_id?, mandate_id?, amount, currency, interval (`day` \| `week` \| `month` \| `year`), interval_count, anchor_at, current_period_start/end, trial_end?, total_count?, renewals_done, cancel_at_period_end, canceled_at, ended_at, pending_amount?, dunning{attempt, next_retry_at}, metadata, version | §6.8. |
| **Mandate** | customer_id, provider_account_id, provider_ref, provider_mandate_id, method (`upi` \| `card` \| `emandate`), status (`pending` \| `active` \| `revoked` \| `expired` \| `failed`), max_amount, frequency, valid_until | Only for merchant-triggered billing. |
| **TaxEntity** | legal_name, trade_name, gstin, pan, address, state_code, lut{number, valid_from, valid_to}?, invoice_prefix, credit_note_prefix, logo blob, signatory_name, dsc (PKCS#12, encrypted)?, aggregate_turnover_band, footer_note, support_email | One per legal entity / GSTIN. Apps point to one. |
| **TaxProfile** (on App) | tax_entity_id, issue_invoices (bool), sac_code, gst_rate_bp (e.g. 1800 = 18%), prices_include_tax (bool) | Defaults come from §10's researched defaults. Every field is editable. |
| **DocumentSeries** | tax_entity_id, kind (`invoice` \| `credit_note`), fiscal_year (e.g. `2026-27`), next_number | Gapless counter, incremented inside the issuing transaction. |
| **Invoice** | number, tax_entity_id, app_id, payment_id, subscription_id?, issued_at, supplier snapshot, recipient snapshot, place_of_supply (2-digit state code, or `OUTSIDE_INDIA` plus country), template_version, flags (`pos_defaulted`, `fx_pending`, `export_igst`), supply_type (`b2b` \| `b2c` \| `export_lut` \| `export_igst`), currency, fx_rate_inr?, lines[{description, sac, qty, unit_amount, taxable, cgst, sgst, igst}], totals, pdf_blob_id?, access_token_hash | Immutable once issued. Corrections are made only through credit notes. |
| **CreditNote** | number, invoice_id, refund_id? / dispute_id?, issued_at, reason, lines, totals, pdf_blob_id? | Immutable. |
| **FxRate** | currency, date, rate_to_inr, source (`manual` \| `provider`) | Used for export invoices' INR value. The rate rule is to be confirmed with the CA. |
| **LedgerEntry** | kind, amount (signed), currency, payment_id, refund_id, dispute_id, settlement_id, occurred_at | Append-only single-entry journal. `kind` is one of `charge`, `refund`, `dispute_hold`, `dispute_release`, `dispute_loss`, `fee`, `fee_tax`, `payout`. |
| **Settlement** | provider_account_id, provider_settlement_id, amount, fees, fee_tax, currency, utr, settled_at | Filled by reconciliation. |
| **ProviderEvent** | provider_account_id, provider_event_id (or sha256 of body), headers (allow-listed), body, verified, received_at, processed_at, error | Raw inbound webhook. Unique on (provider_account_id, provider_event_id). |
| **Event** | id, type, object_type, object_id, object_version, data (snapshot), created_at | Outbound domain event. Append-only. |
| **WebhookDelivery** | event_id, endpoint_id, attempt_count, next_attempt_at, last_status, last_error, delivered_at | |
| **Job**, **Schedule** | §7. | |
| **IdempotencyKey** | app_id, key, request_hash, response, created_at | Expires after 24h. |
| **AuditLog** | actor (user \| api_key \| system), action, target, before/after (redacted), ip, at | |
| **ReconRun / ReconItem** | §11. | |
| **ConnectorKV** | provider_account_id, key, value (encrypted), expires_at | Connector state, e.g. Zoho OAuth tokens or cached Razorpay plan ids. |
| **Blob** | id, kind, content_type, size, sha256, storage, data? | Exactly four kinds of file:<br>1. **App and tax-entity logos** (a few KB each);<br>2. **dispute evidence** an operator uploads (PDF/JPG/PNG, ≤10 MiB);<br>3. **generated invoice and credit-note PDFs** (about 30–80 KB each, roughly 2–3 GB a year at our volume);<br>4. **custom fonts** for invoice templates.<br>Storage is pluggable through the `BlobStore` interface, chosen by `PAYMUX_BLOB_STORE`:<br>- `db` (default): bytes in the `data` column. Backups stay single-source and multi-instance Postgres is safe.<br>- `fs:<dir>`: local disk, single instance only.<br>- `s3://bucket/prefix`: any S3-compatible store, via a small SigV4 client.<br>`paymux blobs migrate --to <store>` moves existing blobs between stores. |

### Money rules

- **Amounts.** `int64` minor units plus ISO-4217 currency, with a `money` exponent table (INR 2, JPY 0, KWD 3, …).
- **Parsing.** Connectors convert provider formats with `money.ParseDecimal`. Floats are never used.
- **No cross-currency sums.** Amounts in different currencies are never added together. Reports group by currency.

## 5. State machines

### Payment

```
created ──► requires_action ──► processing ──► succeeded
   │              │                 │              ▲
   │              ├──► failed ──────┴──(late success)┘
   ├──► canceled  ├──► expired ─────(late success)──┘
   └──────────────┴──► needs_review   (amount/currency mismatch, success after cancel)
```

- **Forward-only.** The one allowed backward move is to `succeeded` from `failed`, `expired` or `canceled`. Late UPI and bank successes are real and must never be dropped.
  - A late success after an app **cancel** goes to `needs_review` with an ops alert. It is never auto-refunded.
- **`succeeded` is terminal.** Refunds and disputes change `amount_refunded` and set flags (`refunded_partial`, `refunded_full`, `disputed`). They do not change the status.
- **The amount is checked on success.** If the received amount or currency differs from the expected one, the payment goes to `needs_review` with event `payment.needs_review`.
- **Double success** happens when a second attempt on an already-succeeded payment succeeds, for example when a customer pays twice.
  - The payment stays `succeeded` from the first attempt and gets the flag `duplicate_success`. The extra attempt is recorded.
  - What happens next depends on the app's `duplicate_payment_action`:
    - **`review` (default):** `payment.needs_review` fires and an ops alert is raised. An operator refunds the extra attempt from the dashboard.
    - **`auto_refund`:** a full Refund of the **extra attempt only** is created (`initiated_by=system`, `reason=duplicate_payment`, audited) and an info alert is raised. The setting is meant to be enabled after the detection has been observed to be correct in `review` mode.
  - The duplicate attempt is not a supply, so it gets **no invoice**, and its refund gets **no credit note**.
- **One transaction per change.** Every change increments `version`, appends LedgerEntries, appends Events, and issues any Invoice, all in **one DB transaction**.

### Other objects

| Object | States |
|---|---|
| Attempt | `created → pending → authorized → captured` or `failed`. Monotonic, with the same late-success exception. |
| Refund | `pending → succeeded` or `failed`. |
| Dispute | `needs_response → under_review → won` or `lost`. Also `accepted`, and `closed` for non-chargeback stages. |
| Subscription | `incomplete → trialing? → active ⇄ past_due → canceled` or `ended`. From `incomplete` it can also go to `incomplete_expired` (mandate or first payment not done within 24h). |
| Mandate | `pending → active → revoked` or `expired`. `pending → failed`. |

Transition tables live in `internal/core/state`. They are pure functions with exhaustive table tests.

## 6. Truth model and core flows

**Rule: a provider webhook is a hint, never the truth.** Paymux keeps state right through three layers:

1. **Webhook.** Verify, store the raw event, deduplicate, return 2xx fast. A job then identifies the object and calls `connector.Fetch*` for the authoritative state before applying a transition. This handles out-of-order delivery, weak signatures and Zoho's per-attempt `payment.failed`.
2. **Poller.** Every non-terminal attempt, refund and mandate is polled on a backoff schedule (1m, 2m, 5m, 15m, 1h, 6h, 24h) for up to 72h.
3. **Daily reconciliation** (§11).

**Silence detector.** If a provider account created attempts in the last N hours but no verified inbound webhook arrived, raise an ops alert. Providers disable failing webhooks: Razorpay after 24h, Zoho after 2 days, Whop after 72h.

### 6.1 Create payment

1. The app calls `POST /v1/payments` with an `Idempotency-Key`. The body includes `amount`, `currency`, `order_ref`, `description`, `customer{… billing?}`, `metadata{}`, `return_url?`, `allowed_connectors?` and `country?`.
2. Paymux validates, evaluates routing (§8), creates the Payment (`created`), and returns `{ id, status, checkout_url }`.
   - `checkout_url` is `https://<app primary active domain or PAYMUX_BASE_URL>/pay/<id>?t=<client_secret>`.
   - The secret is 32 random bytes, stored hashed, valid until `expires_at`.
3. The provider call is made lazily when the pay page opens. *(Builder may make it eager per connector if a provider requires it.)*

### 6.2 Pay page and domains

1. `GET /pay/{id}` checks whether billing details are needed.
   - If the app's `billing_details` is `optional` or `required` and the customer has none, the page shows a short form first:
     - name, email, phone;
     - "I need a GST invoice": GSTIN (format and checksum validated) and legal name;
     - address and **state** (dropdown) for India, or country.
2. It then creates an Attempt on the first routed account, calls `connector.CreateCheckout`, and renders the `NextAction`:

| NextAction | Used by | Rendering |
|---|---|---|
| `redirect{url}` | Whop, Zoho hosted checkout | 303. |
| `form_post{url, fields}` | PayU India | Auto-submitting form carrying the hash. |
| `sdk{script_url, options}` | Razorpay checkout.js, Zoho widget | Loads the provider script with `options`. |

- **Branding:** the app's name, logo and accent color.
- **Retries:** "Try again" creates a new attempt. "Pay another way" uses the next routed account. Both are app-configurable.
- **Fallback:** automatic only when `CreateCheckout` errors or the circuit breaker is open. It never happens after a customer-side failure.

**Domains (both options are supported):**

- **Hub domain (default).** Everything is served at `PAYMUX_BASE_URL`.
- **Per-app custom domain.** An admin adds e.g. `pay.voicestudio.com` as an AppDomain.
  - **Customer-facing routes only:** custom hosts serve `/pay`, `/return`, `/i` (invoices), `/s` (subscription checkout) and `/assets`. Dashboard, `/v1` API and `/hooks` stay on the base host only. Any other route on a custom host returns 404.
  - **Unknown hosts:** requests with an unknown `Host` get 421.
  - **TLS:** Caddy on-demand TLS calls `GET /internal/tls-allow?domain=<host>` on the **internal listener** (`PAYMUX_INTERNAL_LISTEN`, default `127.0.0.1:9100`, shared with metrics). It answers 200 only for AppDomains in `pending` or `active`.
  - **Activation:** a background check fetches `https://<host>/.well-known/paymux/<token>` through the public internet and marks the domain `active`.
  - **Provider URLs:** return URLs sent to providers use the app's primary active domain.
  - **Provider whitelists:** the dashboard shows a per-provider checklist for registering the domain in each provider's website whitelist. PayU and Razorpay review merchant websites.
  - **Cookies:** dashboard cookies are host-only on the base host. Pay pages use no cookies (token in URL plus a per-page CSRF token for the billing form).

### 6.3 Return

1. The provider redirects or posts to `/return/{provider_account_id}/{attempt_provider_ref}`.
2. `connector.HandleReturn` verifies what the provider signs: the Razorpay `order_id|payment_id` HMAC, or the PayU reverse hash.
3. Paymux fetches the truth, applies the transition, and redirects the customer to the app's `return_url?payment_id=…&status=…`.
4. **Apps must not trust the redirect.** They confirm via webhook or `GET /v1/payments/{id}`.

### 6.4 Inbound webhooks

1. A request arrives at `POST /hooks/{provider_account_id}` on the base host. There is one URL per provider account.
2. Paymux reads the raw body (≤1 MiB) and calls `connector.VerifyWebhook(headers, raw)`.
   - **Failure:** store with `verified=false`, return 401, **never** change state.
   - **Unsignable events** (PayU refund webhooks): the connector returns `connector.ErrUnsigned`. Paymux stores the event with `verified=false`, returns 200, and enqueues a *fetch-only* job. Only the authenticated API result may change state. Where the provider publishes source IPs, the connector also checks the allow-list.
   - **Success:** insert the ProviderEvent with `ON CONFLICT DO NOTHING` and enqueue `process_provider_event` in the same transaction. Return 200 within about 200ms; Whop's limit is 5s and Zoho's is 15s.
3. The job calls `connector.ParseWebhook`, which returns `[]Notification`.
   - Look up objects by provider ids. Metadata ids are a fallback only.
   - Fetch the truth and apply the transition.
   - **Provider-managed renewals** (a new payment on a known `provider_subscription_id`) create a `renewal` Payment linked to the subscription. They are not flagged as unknown.
   - **Unknown objects** become a `ReconItem{unknown_provider_object}`.

### 6.5 Outbound webhooks to apps

- **Format:** [Standard Webhooks](https://www.standardwebhooks.com).
  - Headers: `webhook-id`, `webhook-timestamp`, `webhook-signature: v1,<base64 HMAC-SHA256(id.timestamp.body)>`.
  - Apps verify with off-the-shelf libraries.
- **Body:** `{ "type", "timestamp", "data": { "object": { …snapshot…, "version" } } }`.
- **Ordering is not guaranteed.** Apps drop snapshots with a lower `version`.
- **Event types:**
  - **Payments:** `payment.created`, `payment.requires_action`, `payment.succeeded`, `payment.failed`, `payment.canceled`, `payment.expired`, `payment.needs_review`.
  - **Refunds:** `refund.created`, `refund.succeeded`, `refund.failed`.
  - **Disputes:** `dispute.opened`, `dispute.updated`, `dispute.won`, `dispute.lost`.
  - **Subscriptions:** `subscription.created`, `subscription.activated`, `subscription.renewal_upcoming`, `subscription.renewed` (carries `payment_id` and `invoice_id`), `subscription.payment_failed`, `subscription.past_due`, `subscription.reauthorization_required`, `subscription.canceled`, `subscription.ended`.
  - **Mandates:** `mandate.activated`, `mandate.revoked`.
  - **Documents:** `invoice.issued`, `credit_note.issued`.
- **Delivery:**
  - A `deliver_webhook` job is created in the same transaction as the Event.
  - Retries at 0, 1m, 5m, 30m, 2h, 6h, 12h, 24h and 24h, about 3 days in total.
  - Timeout 10s. 2xx means delivered.
  - Ops alert if an endpoint keeps failing for more than 24h.
  - Replay of one delivery or in bulk.
- **SSRF guard:** private or loopback targets are rejected unless `PAYMUX_ALLOW_PRIVATE_WEBHOOKS=true`.

### 6.6 Refunds

1. `POST /v1/payments/{id}/refunds` with `{ amount?, attempt_id?, reason }` and an `Idempotency-Key`. The default is a full refund of the succeeded attempt.
2. In one transaction, with the payment locked (SQLite single writer; Postgres `SELECT … FOR UPDATE`):
   - check `amount ≤ received − refunded − pending refunds` for that attempt;
   - insert the Refund (`pending`) with a fresh `provider_ref`;
   - enqueue `submit_refund`.
3. `connector.Refund` uses `provider_ref` as the provider idempotency token, so it is safe to retry.
4. Typed errors such as `ErrNotRefundable{reason}` become `refund.failed`.
5. **When the refund succeeds**, a CreditNote is issued in the same transaction if the payment has an Invoice (§10).

### 6.7 Disputes

- **Sources:** webhooks (Razorpay, Whop, PayU if enabled), polling (`ListDisputes`; PayU has a 20-day window), and manual dashboard entry (Zoho, or anything missed).
- **Inbox:** sorted by `due_by`. Actions are accept, upload evidence and submit where the connector supports them. Otherwise it shows a deep link plus "mark as …".
- **Evidence:** stored as Blobs (≤10 MiB, PDF/JPG/PNG).
- **Alerts:** on open (with the deadline), 72h before, 24h before, and on loss.
- **Credit notes for disputes:** by default an operator decides, using "Issue credit note" with a reason code. The `dispute_credit_note=on_loss` setting automates it (§10).

### 6.8 Subscriptions and mandates (Path 1)

The app owns pricing. A subscription carries its amount and interval directly; there is no plan catalog in Path 1. Every renewal produces an ordinary `renewal` **Payment**, so the ledger, invoices, refunds, disputes and recon reuse the one-time machinery unchanged.

**API:**

- `POST /v1/subscriptions` with `{ customer{… billing?}, amount, currency, interval, interval_count, trial_days?, start_at?, total_count?, description, metadata, return_url }`. Returns `{ id, status: "incomplete", checkout_url }`, where `checkout_url` is `/s/{id}?t=…`, on the app domain if set.
- `GET /v1/subscriptions/{id}`, `GET /v1/subscriptions?customer=&status=`.
- `POST /v1/subscriptions/{id}/cancel` with `{ at_period_end: bool }`. Revokes the mandate or cancels at the provider.
- `POST /v1/subscriptions/{id}/update` with `{ amount }`. The new amount applies from the next period, with no proration. If it exceeds the mandate's `max_amount`, the response carries `requires_reauthorization: true` and a new `checkout_url`, and `subscription.reauthorization_required` fires.
- `POST /v1/subscriptions/{id}/payment_method`: returns a `checkout_url` to set up a new mandate. The old one is revoked once the new one is active.

**Two billing modes.** Routing (§8) only considers accounts whose connector supports the needed mode.

| Mode | Connectors (Path 1) | How renewals happen |
|---|---|---|
| `provider_managed` | Razorpay Subscriptions *(my call: Razorpay handles pre-debit notices and retries)*; Whop memberships | The connector creates the subscription at the provider. For Razorpay it creates or reuses a provider plan keyed by (amount, currency, interval), with the id cached in ConnectorKV. The provider charges on schedule, and renewal payments arrive by webhook as `renewal` Payments. Cancel and update call the provider. Paymux mirrors the period. |
| `merchant_triggered` | PayU recurring (card, UPI, net-banking mandates), Zoho UPI mandates | Checkout registers a **Mandate** (`kind=mandate_setup` payment, which may be ₹0 or ₹1 per provider) with `max_amount = min(amount × mandate_headroom (default 1.5), method limit)`, where the method limit is `mandate_afa_limit` (₹15,000) for card and UPI, and ₹1 crore for e-NACH. Validity is `mandate_validity` (default 5 years). Paymux then runs the renewal schedule itself (below). |

**Merchant-triggered renewal schedule:**

| When | Job | What happens |
|---|---|---|
| `period_end − predebit_lead` (default **48h**) | `notify_predebit` | Calls `connector.NotifyPreDebit` where the provider requires it (PayU pre-debit API). The issuer must notify the customer at least 24h before the debit, with merchant name, amount, date, mandate reference and reason (RBI E-mandate Framework 2026). Emits `subscription.renewal_upcoming` and optionally emails the customer. **The charge job refuses to run within 24h of the notice and reschedules itself.** |
| `period_end` | `charge_renewal` | Creates the `renewal` Payment and Attempt, then calls `connector.ChargeMandate(mandate, provider_ref, amount)`. The result comes via webhook or poller. |
| on failure | dunning | Retries on the `dunning_schedule` (built-in default +1d, +3d, +5d; configurable per org and per app, §8a), each preceded by a fresh pre-debit notice if `Info().PreDebitPerAttempt`. After the first failure the subscription is `past_due`. After the last retry, `dunning_final_action` applies (`cancel` by default, or `keep_past_due`). |

**Rules:**

- **Spending limit** (RBI E-mandate Framework 2026; NPCI OC-151A):
  - Card and UPI mandates can be debited without authentication up to `mandate_afa_limit` = **₹15,000** per debit. ₹1 lakh applies only to insurance, mutual funds and credit-card bills, not SaaS.
  - Above that, routing picks, in order: an **e-NACH** method (PayU net-banking mandate, up to ₹1 crore), then a `provider_managed` account. If neither exists, the API returns `422 amount_exceeds_mandate_limit`.
- **UPI AutoPay execution** (NPCI OC-215A):
  - At most **1 attempt + 3 retries per renewal cycle**. The dunning schedule is capped at 3 retries for UPI mandates.
  - Executions run only outside peak hours (10:00–13:00 and 17:00–21:30 IST). `charge_renewal` for UPI snaps `run_at` to the next allowed window (`upi_execution_windows`).
- **No customer fees.** RBI forbids charging the customer for mandate use. Paymux never adds mandate surcharges.
- **Calendar.** Periods are computed from `anchor_at`, with the day of month clamped: an anchor of Jan 31 renews on Feb 28/29, then Mar 31.
- **Revocations.** A mandate revoked by the customer (from their UPI app) arrives as a webhook. The mandate becomes `revoked`, `mandate.revoked` fires, and the next renewal fails straight into dunning with `reauthorization_required`.
- **Trials.** The mandate or provider subscription is set up at signup. No charge happens until `trial_end`.
- **Incomplete subscriptions.** If setup is not completed within 24h, the subscription moves to `incomplete_expired`.

### 6.9 Invoices on success (summary; details in §10)

When a Payment becomes `succeeded` (including renewals), the same transaction issues an Invoice if all of these hold:
- the app's `tax_profile.issue_invoices` is true;
- the connector is not a merchant of record (`Info().MerchantOfRecord`; Whop issues its own receipts);
- the payment is not a duplicate attempt.

`invoice.issued` fires. The PDF is rendered by a job and optionally emailed.

## 7. Architecture inside the binary

```
cmd/paymux/main.go           thin: imports built-in connectors, calls cli.Run → paymux.New(cfg).Start()
paymux.go                    public: App, New, Start, options (framework entry; Path 2 adds hooks here)
pkg/connector/               PUBLIC SDK: interfaces, types, errors, registry
pkg/connector/money/         PUBLIC money helpers
pkg/connector/connectortest/ PUBLIC conformance kit
pkg/tax/                     PUBLIC tax-regime interface (India GST implementation in internal/tax/gstin)
connectors/{razorpay,payu,zoho,whop}/   built-in connectors — import ONLY pkg/... + stdlib
internal/core/               payments, refunds, disputes, subscriptions, mandates, routing; state machines
internal/tax/                GST calc, place of supply, GSTIN validation, document numbering
internal/invoice/            invoice/credit-note issue, PDF render, email, reports
internal/store/              all SQL; one implementation; dialect-isolated (sqlite, postgres)
internal/jobs/               durable queue + worker pool + scheduler
internal/hooks/              internal post-commit event bus
internal/httpapi/ inbound/ outbound/ payui/ dashboard/ domains/
internal/auth, authz, secrets, audit, recon, notify, config, logging, metrics
web/                         templates + static (embed.FS overlaid by data/custom/)
migrations/{sqlite,postgres}/  forward-only SQL, kept in lockstep
```

### Request handling

- Stdlib `net/http` `ServeMux` patterns.
- A host router in front separates the base host from app custom hosts (§6.2).
- Middleware order: request ID, slog, recover, timeout, then auth.

### Store: SQLite and Postgres, no behavior difference

- **One `database/sql` implementation in a portable SQL subset:**
  - `ON CONFLICT`, `RETURNING` and CTEs;
  - no dialect-specific functions in shared queries.
- **Types:**
  - money `BIGINT`;
  - IDs `TEXT`;
  - timestamps `BIGINT` unix ms UTC;
  - JSON `TEXT`;
  - blobs `BLOB` / `BYTEA`, via a dialect type name in migrations.
- **`internal/store/dialect` holds every difference:**
  - placeholders (`?` vs `$n`);
  - the job-claim query (Postgres adds `FOR UPDATE SKIP LOCKED`);
  - row locking (`FOR UPDATE` vs SQLite's single writer);
  - `BEGIN IMMEDIATE` (SQLite);
  - the blob column type;
  - backup and snapshot capability.
- **Selection:** `PAYMUX_DATABASE_URL` is `sqlite:///abs/path.db`, `sqlite://./data/paymux.db` (default under `PAYMUX_DATA_DIR`), or `postgres://…`.
- **SQLite runtime:**
  - WAL, `busy_timeout=5000`, `foreign_keys=on`, `synchronous=NORMAL`, all set in the DSN;
  - one writer connection plus a read pool;
  - a data-dir **flock**, so only one process can run.
- **Postgres runtime:**
  - pool of 10 by default;
  - **multiple instances allowed** (scheduler leases plus `SKIP LOCKED`).
- **Migrations:** `migrations/sqlite` and `migrations/postgres` have identical version numbers and names.
  - **Parity test (CI):** after migrating both, normalized schema descriptions must match (tables, columns, nullability, indexes, uniques). This is the "doesn't miss anything" guard.
  - **Pre-migration snapshot:** SQLite uses `VACUUM INTO`. Postgres relies on transactional DDL (each migration in one transaction) plus operator backups.
- **Backups:**
  - SQLite: Litestream sidecar, and `paymux backup`.
  - Postgres: managed point-in-time recovery or `pg_dump`, documented.
- **Switching databases:** `paymux db copy --from <url> --to <url>` copies all tables (verifying row counts and checksums), so an install can move from SQLite to Postgres later.
- **Gapless numbering:**
  - `UPDATE document_series SET next_number = next_number + 1 WHERE … RETURNING next_number - 1` runs inside the issuing transaction.
  - It is correct on both dialects: SQLite has a single writer, and Postgres takes a row lock.
  - A concurrency test runs on Postgres.

### Jobs (own small queue)

- The transactional outbox needs enqueueing inside the caller's transaction, and River's SQLite driver is experimental.
- **Delivery and retries:**
  - At-least-once delivery, with idempotent handlers.
  - Exponential backoff with jitter.
  - `dedupe_key` uniqueness while a job is pending or running.
- **Schedules:**
  - Interval and daily-at-local-time schedules use DB leases.
  - Subscription jobs (`notify_predebit`, `charge_renewal`) are ordinary jobs with `run_at` and dedupe keys `sub:<id>:<period>:<step>`, so a renewal can never be charged twice.

### Secrets

- **Encrypted:** provider configs, webhook secrets, TOTP secrets and ConnectorKV.
- **Cipher:** AES-256-GCM, with a master key from `PAYMUX_MASTER_KEY` (base64) or `PAYMUX_MASTER_KEY_FILE`.
- **Key ids and AAD:** each ciphertext records a key id, and its AAD binds it to its table, column and row.
- **Rotation and startup:** `paymux secrets rotate` re-encrypts everything. A missing key means the process refuses to start.

### Config and environments

- **Bootstrap env vars:**
  - `PAYMUX_ENV` (`production` default, or `development`);
  - `PAYMUX_BASE_URL`, `PAYMUX_LISTEN` (`:8080`), `PAYMUX_INTERNAL_LISTEN` (`127.0.0.1:9100`);
  - `PAYMUX_DATA_DIR` (`./data`), `PAYMUX_DATABASE_URL`;
  - `PAYMUX_MASTER_KEY` or `PAYMUX_MASTER_KEY_FILE`;
  - `PAYMUX_TZ` (`Asia/Kolkata`);
  - `PAYMUX_SMTP_*`, `PAYMUX_OPS_EMAILS`, `PAYMUX_OPS_WEBHOOK_URL`, `PAYMUX_ALLOW_PRIVATE_WEBHOOKS`.
- **`.env` file:**
  - If `./.env` exists (or the path in `PAYMUX_ENV_FILE`), it is loaded at startup by a small built-in parser: `KEY=VALUE`, `#` comments, optional quotes.
  - Real environment variables win over the file.
  - `.env` is git-ignored. `.env.example` is committed.
- **Dev-only 2FA skip:**
  - `PAYMUX_DEV_SKIP_2FA=true` is honored **only** when `PAYMUX_ENV=development` **and** the base URL host is `localhost`, `127.0.0.1`, `*.localhost` or `*.test`.
  - If it is set in any other case, startup fails with `PAYMUX_DEV_SKIP_2FA is only allowed in development on a local host`.
  - When it is active, the dashboard shows a red banner "2FA disabled (development)", and the log prints a warning every startup.
- **Everything else is data, edited in the dashboard:** apps, domains, providers, rules, tax entities, endpoints and users.

### CLI and build

- **CLI:** `paymux serve | migrate | user create | user reset-2fa | secrets rotate | backup --to <file> | db copy | version`.
- **Makefile targets:**
  - `build`: runs `go build` (the toolchain auto-downloads) with `CGO_ENABLED=0`, `-trimpath` and version ldflags.
  - `test` (SQLite) and `test-pg`, which needs `PAYMUX_TEST_POSTGRES_URL` or starts `postgres:17` through Docker if available.
  - `test-all`, `ci`, `run-dev` (uses `.env`) and `fmt`.

### Observability

- **Logging:** slog JSON, redacting secrets, PAN-like digit runs, emails, phones and GSTINs (GSTIN is shown as first 2 and last 3 characters).
- **Health endpoints:**
  - `GET /healthz` reports liveness.
  - `GET /readyz` checks that the DB is writable, migrations are current, and the queue and scheduler are alive.
- **Internal listener:**
  - `/metrics` (Prometheus text) and `/debug/pprof/*`, including `goroutineleak`;
  - `/internal/tls-allow`.
- **Metrics cover:**
  - payments, subscriptions and invoices by status;
  - queue depth and age;
  - delivery failures and verification failures;
  - provider latency and errors.

## 8. Routing

All routing is **data, edited in the dashboard**. Nothing is hard-coded except the seed values written on first start.

**Rule sets are evaluated in this order. The first matching rule wins:**

1. **App (site) rules:** optional, per app.
2. **Org default rules:** shared by all apps, seeded and editable.
3. **Org catch-all:** a single rule that applies when nothing above matched. It is seeded and editable. It can be set to route somewhere or to reject.

**Rules:**

- **Conditions** (all optional, ANDed):
  - `currency in […]`, `country in […]`, `amount between`;
  - `purpose in [one_time, subscription]`, `metadata.<key> = value`;
  - the request's `allowed_connectors`.
- **Action:** one of:
  - `route`: an ordered list of **targets**. A target is either a connector name (meaning any active account of that connector the app may use, in the current mode) or a specific provider account id.
  - `reject`: with a customer-facing message, e.g. "We don't accept payments in this currency yet". The API returns `422 currency_not_accepted` with that message.
- **Seeded values:**

  | Rule set | Condition | Action |
  |---|---|---|
  | Org defaults | currency = INR | route `razorpay` → `zoho` → `payu` |
  | Org defaults | currency = BDT | route `razorpay` → `whop` |
  | Org catch-all | any | route `razorpay` → `whop` |

- **Capability filtering**, applied after a rule matches, so misconfiguration can't send money somewhere that can't take it:
  - **Currency:** each **provider account** has an `accepted_currencies` list. It defaults to the connector's declared currencies, and the admin can narrow it. For example, if Razorpay turns out not to accept BDT for our account, untick BDT on that account, and BDT falls through to Whop with no code change. A connector that isn't configured (e.g. no Whop account yet) is skipped.
  - **Subscription mode:** must be supported.
  - **Mandate limit:** see §6.8.
- **Fall-through:** if every target of the matched rule is filtered out, evaluation **continues to the next matching rule**, ending at the catch-all. If nothing remains, the API returns `422 no_route` naming the currency and purpose, and an ops alert fires.
- **Test routing:** the dashboard has a "test routing" box. Enter currency, country, amount and purpose, and see which rule matched and the final account order.
- **Implementation:** `routing.Choose(orgRules, appRules, req, catalog, health) []ProviderAccountID` is a pure function with table tests.
- **Circuit breaker:** it opens after 5 consecutive `CreateCheckout` failures within 5 minutes, and stays open for 2 minutes. An open account sorts last.

## 8a. Settings and defaults (everything is configurable)

**How a value is resolved:** **app override → org value → built-in default.**

- The built-in defaults below are seeded as the org values on first start.
- An admin edits the org values on the dashboard Settings page. Each app's page shows the effective value and where it comes from ("inherited from org" / "overridden here"), with a "reset to org" control.
- Each value is validated against the limits in the "Allowed" column. Every change is audited.
- In code, `settings.Resolve(app, key)` is the only way to read a policy value. A seam check forbids reading `app.Settings` fields directly.

| Key | Built-in default | Allowed | Level | Used by |
|---|---|---|---|---|
| **Routing** (rules, catch-all) | INR → razorpay, zoho, payu; BDT → razorpay, whop; catch-all → razorpay, whop | Any rules; catch-all may `reject` | org + app | §8 |
| `duplicate_payment_action` | `review` | `review` \| `auto_refund` | org + app | §5 |
| `billing_details` | `state_required` (India) when invoicing is on, else `off` | `off` \| `state_required` \| `optional` \| `required` (a full address is always required at ≥ ₹50,000, see §10) | org + app | §6.2 |
| `payment_ttl` | 60 min | 10 min – 7 days | org + app | §6.1 |
| `pay_page_retry_buttons` | try again: on; pay another way: on | on/off each | org + app | §6.2 |
| `invoice_delivery` | `app` | `app` \| `paymux` \| `both` | org + app | §10 |
| `issue_invoices` | on (off for merchant-of-record connectors regardless) | on/off | app | §10 |
| `sac_code` | `998315` | 6-digit SAC | app | §10 |
| `gst_rate_bp` | 1800 | 0–2800 | app | §10 |
| `prices_include_tax` | true | bool | app | §10 |
| `invoice_series_prefix` / `credit_note_series_prefix` | `INV` / `CN` | `[A-Z0-9]{1,5}`, final number ≤ 16 chars | tax entity + app (an app prefix creates its own series) | §10 |
| `dispute_credit_note` | `manual` | `manual` \| `on_loss` | org + app | §10 |
| `dunning_schedule` | +1d, +3d, +5d | 0–6 retries, each 1h–30d; UPI mandates capped at 3 retries per cycle | org + app | §6.8 |
| `dunning_final_action` | `cancel` | `cancel` \| `keep_past_due` | org + app | §6.8 |
| `predebit_lead` | 48h | 25h–7d; the charge is never sent within 24h of a notice | org + app | §6.8 |
| `mandate_headroom` | 1.5× | 1.0–5.0 | org + app | §6.8 |
| `mandate_afa_limit` | ₹15,000 | the per-category limit from RBI's list; ₹1,00,000 only for categories RBI allows | org | §6.8 |
| `mandate_validity` | 5 years | 1–30 years; `reauthorization_required` fires 30 days before expiry | org + app | §6.8 |
| `upi_execution_windows` | outside 10:00–13:00 and 17:00–21:30 IST | editable windows | org | §6.8 |
| `webhook_retry_schedule` | 0, 1m, 5m, 30m, 2h, 6h, 12h, 24h, 24h | 1–15 steps | org | §6.5 |
| `recon_time` | 03:00 `PAYMUX_TZ` | HH:MM | org | §11 |
| `provider_event_retention` | 400 days | 30 days – forever | org | §12 |
| `document_retention` | forever (statutory minimum: 72 months after the GSTR-9 due date) | forever, or ≥ 8 years | org | §10 |
| `pdf_signing` | `dsc` if a DSC certificate is uploaded, else `signatory_note` | `signatory_note` \| `dsc` | tax entity | §10 |

Install-level config (env, not dashboard): `PAYMUX_BLOB_STORE` (`db` default \| `fs:<dir>` \| `s3://…`), plus everything in §7 Config.

## 9. Connector SDK (`pkg/connector`) — the central seam

Built-in connectors use exactly the same public API that third parties will use in Path 2.

```go
type Connector interface {
    Info() Info // name, version, capabilities, config JSON schema, currencies, methods, rate limits,
                // MerchantOfRecord bool, SubscriptionModes []BillingMode, PreDebitPerAttempt bool, MandateLimits
    CreateCheckout(ctx context.Context, req CheckoutRequest) (CheckoutResult, error)
    HandleReturn(ctx context.Context, req ReturnRequest) (ReturnResult, error)
    VerifyWebhook(ctx context.Context, req WebhookRequest) error
    ParseWebhook(ctx context.Context, req WebhookRequest) ([]Notification, error)
    FetchPayment(ctx context.Context, ref PaymentRef) (PaymentSnapshot, error)
}

// Optional capabilities: type assertion + declared in Info().Capabilities.
type Refunder           interface { Refund(…) (RefundSnapshot, error); FetchRefund(…) (RefundSnapshot, error) }
type DisputeReader      interface { ListDisputes(…); FetchDispute(…) }
type DisputeResponder   interface { AcceptDispute(…) error; SubmitEvidence(…) error }
type Lister             interface { ListPayments(…); ListRefunds(…) }
type Settler            interface { ListSettlements(…) ([]SettlementSnapshot, error) }
type Canceler           interface { CancelCheckout(…) error }
type SubscriptionManager interface { // provider_managed
    CreateSubscriptionCheckout(…) (CheckoutResult, error); FetchSubscription(…) (SubscriptionSnapshot, error)
    CancelSubscription(…) error; UpdateSubscriptionAmount(…) error }
type MandateManager     interface { // merchant_triggered
    CreateMandateCheckout(…) (CheckoutResult, error); FetchMandate(…) (MandateSnapshot, error)
    NotifyPreDebit(…) error; ChargeMandate(…) (PaymentSnapshot, error); RevokeMandate(…) error }

type Factory func(cfg json.RawMessage, deps Deps) (Connector, error)
func Register(name string, f Factory, info Info)
type Deps struct { HTTP *http.Client; KV KV; Clock func() time.Time; Logger *slog.Logger; URLs URLs }
```

### Rules

1. **Types.** All request and response types are plain, JSON-serializable structs. Raw bodies are `[]byte` and headers are `map[string][]string`. The Path 2 remote protocol is a mechanical mapping.
2. **Stateless.** Connectors keep any state in `Deps.KV` and never touch the DB.
3. **Typed errors:** `ErrNotRefundable`, `ErrNotFound`, `ErrInvalidCredentials`, `ErrRateLimited{RetryAfter}`, `ErrTemporary`, `ErrUnsupported`, `ErrUnsigned` (from `VerifyWebhook` only), and `ErrMandateLimit`.
4. **Status mapping.** Connectors map provider statuses to SDK statuses and keep the raw status for display.
5. **No provider SDKs.** Thin, typed HTTP clients per connector.
6. **Conformance kit.** `connectortest.Run(t, factory, fixtures)` checks:
   - signature accept and reject cases;
   - that status mapping is total;
   - money round-trips;
   - that `Info()` matches the implemented interfaces;
   - a valid NextAction against a fake server;
   - a JSON round-trip of every SDK type;
   - for `MandateManager`, that `ChargeMandate` passes `provider_ref` for idempotency.

### Connector notes (from provider docs, 2026-10-17; re-verify at build time)

| Connector | One-time checkout | Webhook verify | Refunds / disputes | Subscriptions | Notes |
|---|---|---|---|---|---|
| **razorpay** | `POST /v1/orders` (receipt = provider_ref, notes = `{pay, att, app}`); NextAction `sdk`; `callback_url` → `/return`. | `X-Razorpay-Signature` HMAC-SHA256(raw body). Dedupe on `x-razorpay-event-id`. | Refund API (partial, idempotent). Disputes webhooks plus accept/contest with Documents API. | **provider_managed** via Subscriptions API (plans cached in KV). Verify `subscription.*` event names at build. | Ensure capture of authorized payments. Payment entity carries `fee`/`tax` for the fee input-tax-credit register. Up to 30 webhook URLs. Settlements and recon APIs. |
| **payu** (India) | NextAction `form_post` with SHA-512 hash; `txnid` = provider_ref; `udf1` = att id. | Reverse hash (golden-tested). Refund webhooks unsigned (`ErrUnsigned`). Dispute signature V2 if enabled. | `cancel_refund_transaction` (`var2` ≤23). Chargeback API with separate token, 20-day window. | **merchant_triggered**: card, UPI and net-banking mandates; pre-debit API; we trigger each debit. | Four auth schemes. `pending` requires polling. Settlement Range API in ≤3-day windows. |
| **zoho** (India) | Payment sessions (OAuth); NextAction `sdk` (widget) or `redirect`. | `X-Zoho-Webhook-Signature` HMAC-SHA256(`t.body`). | Refunds at 60/min. **No dispute API: manual only.** | **merchant_triggered**: UPI mandates only. | INR only. Max 5 webhook endpoints. Sandbox must be enabled by Zoho support. |
| **whop** | `checkout_configurations` with metadata; NextAction `redirect`. | Standard Webhooks; pin `api_version: v1`. | Refund API, plus Resolution-Center refunds (`initiated_by=provider`). Dispute evidence API. | **provider_managed**: memberships/plans. | **Merchant of record**, so no Paymux invoice. Settles in org currency (record FX). Optional: routing uses it only if a Whop account is configured, and only for the currencies ticked on that account. |

## 10. GST invoicing, credit notes and tax reports

> **Research basis (2026-10-17).** Defaults come from:
> - the CGST Rules 34, 46, 47, 53, 59 and 96A;
> - CGST Act sections 34 and 36;
> - IGST Act sections 2(6), 2(17) and 12;
> - Notifications 74/2018, 26/2022 as substituted by 38/2023, 12/2024 and 10/2023;
> - the Finance Act 2025 change to section 34.
>
> Every rule-derived value is a **setting (§8a) or a versioned template**, never a constant in code, so the law can change without a release. Paymux produces the documents and data. Filing returns is out of scope.

The design is behind the public interface `pkg/tax.Regime`. India GST is the only Path 1 implementation.

### When documents are issued

- **Tax invoice:** issued in the same transaction as a Payment becoming `succeeded`, when §6.9 applies. That is immediate, well inside the Rule 47 limit of 30 days for services.
- **Credit note:** issued in the same transaction as a Refund becoming `succeeded`, when the payment has an Invoice.
  - **The credit note carries the GST portion of the refund.** B2C buyers can't reverse input tax credit, so Paymux refunds the full amount including GST and credits both the taxable value and the tax.
  - **Cutoff:** this applies only up to 30 November after the end of the financial year of the original supply, or the GSTR-9 filing date if earlier (an org setting with that default). After the cutoff, the refund is processed with **no credit note**, flagged `credit_note_cutoff_passed`, and listed in the monthly report.
- **Disputes and chargebacks** (`dispute_credit_note`, default `manual`): no automatic credit note, because a fraud loss on a delivered service is not a reduction in the value of the supply. The dispute page has an "Issue credit note" action that asks for a reason code (`service_deficiency`, `accepted_as_refund` or `other`) and records it in the audit log. Setting `on_loss` issues one automatically.
- **Immutability:** documents are never edited or deleted.
- **Reissue:** "Reissue with corrected billing details" issues a full CreditNote followed by a new Invoice, and is audited.

### Numbering

- **Series key:** (tax entity, kind, prefix, fiscal year).
- **Format:** `{PREFIX}/{FY}/{seq:06}`, for example `INV/2627/000123` (15 chars) and `CN/2627/000045`.
  - Allowed characters are `[A-Z0-9/-]`, upper case only, since the invoice portal treats numbers as case-insensitive.
  - Setup rejects any prefix that could push a number past 16 characters.
- **Per-app prefixes:** an app may set its own prefix (e.g. `VS`), which gives it its own series. Rule 46 allows several series.
- **Gapless:** numbers are consecutive within a series, taken from `DocumentSeries` inside the issuing transaction, and reset at 1 April 00:00 IST.

### Buyer details captured (defaults)

| Case | Required on the invoice | How Paymux gets it |
|---|---|---|
| Registered buyer (B2B) | Legal name, address, GSTIN | Pay-page form or API. GSTIN is format- and checksum-validated. |
| Unregistered buyer in India, < ₹50,000 | Name, and **State (with code) on every invoice**. SaaS is an online information (OIDAR) service under IGST Act s.2(17), and the 38/2023 proviso to Rule 46 makes the declared State the address on record. | `billing_details=state_required`: the pay page asks for the State (dropdown) if it isn't already known. Renewals reuse the stored State. |
| Unregistered buyer in India, ≥ ₹50,000 | Name, full address, PIN, State with code | Pay page requires the full address at this amount, whatever the setting. |
| Buyer outside India | Name, address, country | Pay page asks for the country, plus the address if `billing_details` is `optional` or `required`. |

### Classification and tax

| Recipient | supply_type | Place of supply | Tax |
|---|---|---|---|
| Valid GSTIN | `b2b` | State from GSTIN (first 2 digits) | Same state as the supplier: CGST 9% + SGST 9%. Otherwise IGST 18%. |
| India, no GSTIN | `b2c` | Declared State | Same rule. |
| India, State unknown (only after a bad import or API misuse; the pay page prevents it) | `b2c` | Supplier State (IGST Act s.12(2)(b)(ii)) | CGST + SGST. The invoice is flagged `pos_defaulted` and an ops alert fires. |
| Outside India, paid in convertible foreign currency, and the tax entity has a **valid LUT for the invoice date** | `export_lut` | Outside India | 0%. The endorsement is printed verbatim. |
| Outside India, any LUT or payment condition not met | `export_igst` | Outside India | IGST at rate (refund claim route). The invoice is flagged for review. |

- **Rate:** default 18% (`gst_rate_bp=1800`).
- **SAC:** default `998315`. Other suggested choices shown in the dashboard: `997331` (licence to use software) and `998314`. The code is always printed as 6 digits.
- **Export endorsement text** (statutory, current): **"SUPPLY MEANT FOR EXPORT/SUPPLY TO SEZ UNIT OR SEZ DEVELOPER FOR AUTHORISED OPERATIONS UNDER BOND OR LETTER OF UNDERTAKING WITHOUT PAYMENT OF INTEGRATED TAX"**. It is stored in the versioned template, followed by the LUT reference number and the destination country.
- **FX for export invoices (Rule 34(2)):** the INR value uses the **RBI/FBIL reference rate on the invoice date**. The rate and its source are stored on the invoice. The FxRate table is filled by manual daily entry in the dashboard (default). An optional fetcher job can be added later behind an interface. If no rate exists for that date, invoice issue still succeeds with `fx_pending`, the INR value is filled in when the rate is entered, and an ops alert fires.
- **LUT tracking:** the tax entity stores the LUT number and its valid_from and valid_to dates. The dashboard warns 30 days before 31 March to file the next year's form RFD-11.

### Amount math (integer paise, invoice total always equals `amount_received`)

- **Tax-inclusive** (default):
  - `taxable = round_half_up(total × 10000 / (10000 + rate_bp))`
  - `tax = total − taxable`
- **Tax-exclusive:**
  - `tax = round_half_up(taxable × rate_bp / 10000)`
  - `total = taxable + tax`
- **Split:**
  - Intra-state: `cgst = tax / 2` (floor), `sgst = tax − cgst`.
  - Inter-state: `igst = tax`.
- **Partial refunds:** the credit note credits taxable value and tax in proportion, using the same rounding. The sum of all credit notes never exceeds the invoice total; the last credit note absorbs any rounding remainder.

### Content (Rule 46 and Rule 53(1A) fields; versioned template)

- **Invoices:**
  - supplier legal and trade name, address, GSTIN, and State name and code;
  - "Tax Invoice", the number and date;
  - recipient name and address, GSTIN if registered, and State name and code;
  - place of supply (State name, or "Outside India" plus the country);
  - SAC, description (app-provided, e.g. "Voicestudio Pro — Nov 2026"), and quantity 1 with unit "service";
  - taxable value, then rate and amount for each of CGST, SGST and IGST, then the total in figures and in words;
  - "Whether tax is payable on reverse charge: No";
  - the payment method and provider reference;
  - the signature block (below).
- **Credit notes:** "Credit Note", the number and date, plus the original invoice number and date, the reason, and the taxable value and tax credited.
- **Signature** (`pdf_signing`):
  - **`dsc`:** when a digital signature certificate (DSC, PKCS#12, stored encrypted) is uploaded for the tax entity, the PDF is digitally signed (PAdES). This meets the IT Act route in the 74/2018 proviso.
  - **`signatory_note`:** otherwise, the authorised signatory's name plus "This is a computer-generated document" is printed. Because no CBIC circular endorses that legend on its own, the dashboard shows a standing warning recommending a DSC.
- **Template versioning:** each document stores the template version that rendered it. Re-rendering an old document uses its original version.

### Delivery

1. A job renders the PDF from the versioned layout using a pure-Go PDF library, with an embedded font that has the ₹ glyph (e.g. Noto Sans). If `dsc` is on, it signs the PDF. *(Builder picks the PDF and signing libraries under the dependency policy.)*
2. The PDF is stored as a Blob.
3. **Delivery is configurable per app** (`invoice_delivery`):
   - **`app`** (default): Paymux only produces the document. The app gets `invoice.issued` / `credit_note.issued` with a signed hosted URL and a PDF URL, and sends its own email.
   - **`paymux`:** Paymux also emails the customer over SMTP with an overridable template. Each send is recorded, and resend is available.
   - **`both`:** Paymux emails, and the app still gets the webhook.
4. Hosted view at `/i/{id}?t=…` and PDF at `/i/{id}.pdf?t=…`, on the app domain if set.
5. API: `GET /v1/invoices/{id}`, `/v1/invoices/{id}/pdf`, `GET /v1/invoices?payment_id=`, and the same for `/v1/credit_notes`.

### Monthly reports (dashboard, CSV, per tax entity and month)

1. **Sales register:** every invoice and credit note with all tax fields and flags.
2. **GSTR-1-oriented sheets:**
   - **B2B.**
   - **B2CL:** inter-state B2C invoices **above ₹1,00,000** (Rule 59(4) as amended by 12/2024; the threshold is a setting).
   - **B2CS:** a summary by place of supply and rate.
   - **EXP:** exports with or without payment, with LUT reference.
   - **CDNR / CDNUR:** credit notes to registered and unregistered buyers.
   - **HSN/SAC summary** (6-digit).
   - **Documents issued:** first number, last number, count, and cancelled = 0, per series.
3. **Provider fees and GST on fees (input tax credit register):** per provider account and its GSTIN, by month. It uses per-payment `fee` / `fee_tax` where the connector provides them (Razorpay), reconciled with Settlement totals.
4. **Settlement report:** gross, refunds, fees, fee GST, net and UTR, per provider per day.
5. **Exceptions:** `pos_defaulted`, `fx_pending`, `export_igst`, `credit_note_cutoff_passed`.

### E-invoicing (IRN)

- **Off by default.** It is mandatory for B2B and export invoices once aggregate turnover passed **₹5 crore in any financial year since 2017-18** (Notification 10/2023). B2C is excluded. At ₹10 crore or more, the IRN must be obtained within 30 days.
- **Seam:** `pkg/tax.EInvoicer` interface. Path 1 ships the interface and the turnover setting. If the org sets turnover above ₹5 cr while no e-invoicer is configured, the dashboard shows a blocking warning. The actual integration with the invoice registration portal is a P2.6 plugin, or earlier if needed.

## 11. Reconciliation

- **When:** daily at 03:00 `PAYMUX_TZ` per provider account, for the window D-2 to D. It can also be run on demand.
- **Payments and refunds:**
  - status mismatch: the provider wins, transition applied, item `auto_fixed`;
  - missing on our side: `unknown_provider_object` with an alert (renewals of known provider subscriptions are linked, not flagged);
  - amount or currency mismatch: flag only, with an alert.
- **Subscriptions and mandates:** a mandate or provider-subscription status mismatch gets the same treatment.
- **Settlements:** upsert, attach fees, fee GST and net to attempts, and write `fee`, `fee_tax` and `payout` entries. Settlement fee totals that differ from the per-payment fee sum are flagged.
- **Dashboard:** run summary, and resolving items with a note (audited).

## 12. Dashboard and security

### Technology

- Server-rendered `html/template` plus vendored htmx. No Node build.
- CSS tokens as variables, with light and dark modes.
- An **overlay FS**: `data/custom/` (opened with `os.Root`) sits over the embedded `web/` and covers templates, CSS, email templates and invoice templates.

### Pages (Path 1)

- **Overview:** per app per currency, plus open disputes, past-due subscriptions and alerts.
- **Payments:** filters, and a timeline detail with attempts, events, raw provider events, refunds, disputes, deliveries and invoice links.
- **Refund modal.**
- **Subscriptions:** list and detail (periods, renewals, mandate, dunning state), cancel, and copy a reauthorization link.
- **Disputes inbox.**
- **Invoices and credit notes:** list, view, download, resend email, and reissue with corrected details.
- **GST reports:** month picker and CSV downloads.
- **Reconciliation.**
- **Webhook deliveries:** with replay.
- **Apps:** keys, endpoints, return URL, branding, **domains** (add, status, provider checklist), routing rules, allowed accounts, tax profile and settings (duplicate action, billing-details mode, dunning).
- **Org routing defaults.**
- **Provider accounts:** schema-driven form, webhook URL, test connection, provider GSTIN.
- **Tax entities:** GSTIN, address, LUT, prefixes, logo, signatory.
- **FX rates.**
- **Settings:** org values with the built-in default shown alongside. Each app's settings tab shows the effective value and its source, and has reset-to-org (§8a).
- **Users, Audit log.**
- **Test/live toggle.**

### Auth and authorization

- Email and password (argon2id), plus **TOTP required for every user**. The only exception is the dev skip (§7).
- Sessions: HttpOnly, Secure, SameSite=Lax, host-only on the base host, 12h idle timeout.
- CSRF tokens on every form, and login rate limiting.
- **Roles:** `admin`, `operator` (refunds, disputes, replay, reissue, view) and `viewer`.
- **Authorization:** every action goes through `authz.Can(actor, action, resource)` with a closed action enum.

### API

- `Authorization: Bearer pmx_{mode}_sk_…`. Keys are looked up by prefix and compared in constant time on the hash.
- Rate limit: 20 requests per second per key.
- `docs/openapi.yaml` (OpenAPI 3.1) is kept true by a contract test.

### Data protection, backups, deployment

- **Data protection:** minimal PII. Raw provider events are kept for 400 days, configurable. **Invoices and credit notes are kept indefinitely** (statutory minimum: 72 months after the GSTR-9 due date, CGST Act s.36; default `document_retention` = forever). Logs are redacted.
- **Deployment:** systemd plus Caddy (on-demand TLS with the ask endpoint).
- **Downtime:** if Paymux is down, providers retry webhooks for 24–72h, and pollers and renewal jobs catch up on restart. Renewal jobs whose `run_at` was missed run on start; the pre-debit lead is re-checked and the charge is rescheduled if the notice window was missed.
- **Restore:** under 15 minutes, with a runbook for each database.

### Performance targets

- `POST /v1/payments` p99 under 100ms.
- Pay page under 1.5s excluding the provider.
- Invoice PDF render under 500ms.
- Under 150 MB RSS.
- Sustains 20 payments/s bursts on 1 vCPU.

## 13. Testing strategy

- **Both databases:**
  - The entire suite runs against SQLite and Postgres. CI sets `PAYMUX_REQUIRE_PG=1`, so skipped Postgres tests fail the build.
  - The schema parity test.
  - Gapless numbering under 20 concurrent issuers on Postgres.
- **Unit:**
  - state machines, routing (org plus app layers, capability filters), money;
  - PayU hash golden vectors and Standard Webhooks vectors;
  - **GST:** golden cases for inclusive and exclusive amounts, intra- and inter-state, export LUT and IGST, odd-paise splits, totals always equal to `amount_received`, GSTIN checksum vectors, and fiscal-year rollover on Apr 1 at 00:00 IST;
  - **GST rules:**
    - the State is required for every Indian B2C invoice, and a full address at ≥ ₹50,000;
    - `pos_defaulted` path;
    - LUT validity on the invoice date (`export_lut` vs `export_igst`);
    - `fx_pending`;
    - credit-note cutoff (30 Nov after FY end);
    - proportional partial credit notes summing exactly to the invoice;
    - the 16-char number limit;
    - the B2CL >₹1,00,000 inter-state flag;
  - **Subscriptions:**
    - month-end clamping;
    - the 48h pre-debit lead, and never charging within 24h of a notice;
    - dunning schedule, and the UPI cap of 3 retries;
    - UPI peak-window snapping;
    - mandate-limit routing to e-NACH or provider-managed, otherwise refusal;
    - mandate-expiry reauthorization 30 days before;
    - missed-window rescheduling after downtime;
    - a renewal is never charged twice;
  - **Settings:** resolution order (app → org → default), validation limits, audit on change.
- **Connector:** `connectortest`, `httptest` fakes, and **fuzzing** of every `VerifyWebhook`/`ParseWebhook`.
- **Integration:** in-process full flows per connector on both databases:
  - payment → webhook → app event → invoice PDF → refund → credit note;
  - subscription → mandate → renewal → invoice → dunning.

  Timing uses a fake clock or `testing/synctest`.
- **Sandbox:** tests behind the build tag `sandbox`, run before each connector goes live.
- **Invariants:**
  - no state regress except late success;
  - per payment, Σ `charge`/`refund`/`dispute_*` = received − refunded − net dispute losses (fee entries reconcile to settlements);
  - one Event per version;
  - one Invoice per succeeded non-duplicate, non-MoR payment for apps with invoicing on;
  - credit-note totals for a payment ≤ its invoice total.
- **CI:** gofmt, vet, staticcheck, govulncheck, `go test -race` on both databases, seam checks, amd64 and arm64 builds.

## 14. Path 1 → Path 2 seams (what makes Path 2 additive)

| # | Seam built in Path 1 | Path 2 adds (no rewrite) |
|---|---|---|
| S1 | `pkg/...` public SDK. Built-ins under `connectors/` import only `pkg/...` and stdlib. A CI test enforces this. | SDK v1 under semver, docs, a template repo. Third-party connectors are plain Go modules. |
| S2 | JSON-serializable SDK types, capability interfaces, `Info()` config schema. | `remote` connector over HTTP+JSON (signed both ways), so connectors can be written in any language. Optional Prism sidecar. |
| S3 | `connectortest` used by all built-ins. | Published as the bar for third-party connectors. |
| S4 | Thin `main.go` over public `paymux.New`; connectors register by import. | Framework mode (PocketBase-style), plus `paymux build --with module@ver` (xcaddy-style). |
| S5 | Post-commit event bus, already used by notifications, invoices and emails. | Public Go hooks, plus optional goja JS hooks for side effects and routing suggestions. Hooks can never move money. |
| S6 | Both SQL dialects, the parity test, `db copy`. | Nothing required. More dialects are optional. |
| S7 | Overlay FS, CSS tokens, per-app branding, custom domains; email and invoice templates overridable. | Theme packages (`theme.json` tokens, CSS, templates, assets), per-install and per-app; pay-page and invoice i18n. |
| S8 | `authz.Can` with a closed enum, audit everywhere. | Custom roles, scoped API keys, admin API, OIDC SSO. |
| S9 | `org_id` and `mode` on every row. | Path 2 stays single-org. Path 3 SaaS stays possible. |
| S10 | Versioned `/v1` and OpenAPI with a contract test. | Public docs, generated clients, an API version header. |
| S11 | Forward-only embedded migrations, snapshots, `version`. | Upgrade guarantees (N-1 to N), self-update, signed releases. |
| S12 | Capability interfaces for every feature, including `SubscriptionManager` and `MandateManager`. | Payment links, payouts and proration as new optional interfaces. v1 connectors stay untouched. |
| S13 | `pkg/tax.Regime` interface, with India GST as one implementation; document numbering and rendering are generic. | Other tax regimes (VAT, etc.) and e-invoicing (IRN) as plugins. |

**CI seam checks (from M0):**
- `sql-outside-store`;
- `connector-imports`;
- `thin-main`;
- `templates-via-overlay` (M4);
- `money-actions-via-authz` (M1);
- `settings-via-resolver` (M1): policy values are read only through `settings.Resolve`;
- `schema-parity` (M0).

## 15. Delivery outline (detail in the plans)

### Path 1 (one experienced Go engineer, estimated about 13–16 weeks)

- **M0** — Foundation. Both databases, `.env`, Makefile, CI.
- **M1** — Core domain and `/v1` payments API.
- **M2** — Webhooks in and out.
- **M3** — SDK, conformance kit, **Razorpay** one-time, pay page, **custom domains**. Ends with an internal ₹1 live test payment.
- **M4** — Dashboard with auth (TOTP and dev skip), routing defaults and duplicate settings.
- **M5** — **GST invoicing and credit notes.**
- **M6** — **Subscriptions** core plus Razorpay provider-managed subscriptions.
  - → **The first app can take real customers.** Disputes are temporarily handled in the provider dashboard until M9.
- **M7** — **PayU India**: one-time plus mandates.
- **M8** — **Zoho**: one-time plus UPI mandates.
- **M9** — Disputes inbox, reconciliation and settlements, GST and fee reports, notifications.
- **M10** — **Whop**: one-time plus memberships. BDT and non-INR fallback.
- **M11** — Hardening, `db copy`, runbooks, all apps live.

### Path 2 (estimated about 2–3 months after Path 1)

- **P2.1** — SDK v1 freeze, docs, template repo.
- **P2.2** — Framework mode, builder, remote connectors.
- **P2.3** — Go and JS hooks.
- **P2.4** — Themes and i18n.
- **P2.5** — Productization (setup wizard, auto-TLS, Docker, self-update, signed releases, OIDC, custom roles, docs site).
- **P2.6** — Expansions: payment links, payouts, proration, e-invoicing (IRN), other tax regimes, more connectors.

## 16. Decided, my calls, and open items

### Decided by the user

- **Toolchain and build:** latest Go via automatic toolchain download and `make build`; nobody installs Go by hand.
- **Shape:** single binary, with dashboard, plugins and themes. Path 2 grows from Path 1 without a rewrite.
- **Providers:** Razorpay, PayU, Zoho, Whop, in that order.
- **Subscriptions** are in Path 1.
- **No import** of old payments.
- **Domains:** both the hub domain and per-app custom domains.
- **Routing:** rules with defaults: INR → Razorpay, Zoho, PayU; BDT → Razorpay, Whop. Whop is not needed for INR.
- **Duplicates:** duplicate-payment action is configurable, default `review`, with `auto_refund` available.
- **GST:** invoicing, credit notes, fee and GST tracking.
- **Databases:** SQLite and Postgres both supported, missing nothing.
- **2FA:** required, with a dev-only skip through `.env`.

### My calls (override any)

- **Checkout:** hosted checkout only. Hub-hosted pay, subscription and invoice pages.
- **Infrastructure:** own job queue; blobs stored in the database; unix-ms timestamps.
- **Dashboard and APIs:** server-rendered dashboard with htmx. No provider SDKs. Standard Webhooks for outbound.
- **Routing:** seeded org catch-all = razorpay → whop (editable, can be set to reject). Lazy provider checkout creation.
- **Subscriptions:**
  - Razorpay uses provider-managed subscriptions.
  - Mandate headroom 1.5×; mandate validity 5 years.
  - Dunning at +1, +3 and +5 days.
  - No proration in Path 1.
- **Researched defaults (§8a, §10, §6.8)**, all configurable:
  - SAC 998315; 18%; prices include tax;
  - `INV/2627/000123` numbering;
  - State required for B2C;
  - FBIL/RBI FX on the invoice date;
  - DSC signing when a certificate is uploaded;
  - credit notes for disputes are manual;
  - pre-debit 48h;
  - ₹15k limit for card/UPI mandates, e-NACH above;
  - UPI ≤3 retries outside peak hours.
- **Dev 2FA skip:** also requires a local host.

### Left to the builder

- Table DDL.
- Libraries within the dependency policy (PDF, CLI).
- Page layouts.
- Eager vs lazy checkout per connector.

### Open, non-blocking for M0–M4

- **Optional:** have whoever files your GST returns glance at one sample invoice and credit note PDF from M5. All the values are researched defaults and editable (§10), so this no longer blocks anything.
- **Legal entities and GSTINs:** one TaxEntity per GSTIN is supported either way.
- **Whop eligibility and Razorpay BDT support no longer block anything.** Routing skips unconfigured connectors and unticked currencies (§8), so whichever account actually works takes BDT.
- **Mandate rules** use the researched defaults (RBI E-mandate Framework 2026, NPCI OC-151A and OC-215A, NACH/003). Nothing blocks M7.
- **Which app goes live first.**
- **Path 2 license.**
