# Paymux Roadmap — Path 1 (internal hub) and Path 2 (self-hostable product)

> **For agentic workers:** This is the milestone roadmap. Each milestone gets its own bite-sized implementation plan (`docs/superpowers/plans/YYYY-MM-DD-paymux-<milestone>.md`), written just before it starts with superpowers:writing-plans. M0's plan is already written: `2026-10-17-paymux-m0-foundation.md`. **No implementation starts until the user says so.**

**Spec:** `docs/superpowers/specs/2026-10-17-paymux-design.md` (v2). § numbers refer to it.

**Rule for every milestone:**
- It ends with a working binary.
- CI is green on **both SQLite and Postgres**, including the seam checks (§14).
- Its exit criteria pass.

Estimates assume one experienced Go engineer. They are for planning, not commitments.

---

## Path 1 — internal hub (≈ 13–16 weeks)

| # | Milestone | Delivers | Exit criteria (all must pass) | Est. |
|---|---|---|---|---|
| **M0** | Foundation | Module (`go 1.27`, `toolchain go1.27.2`, auto-download); Makefile; CLI (`serve`, `migrate`, `version`); env + `.env` config with dev-flag guard; slog redaction; **store on SQLite and Postgres** (dialects, migrations in lockstep, schema parity test, SQLite snapshots + flock); IDs/`provider_ref`; `pkg/connector/money`; secrets box; job queue + scheduler; health/ready/internal listener; CI script; seam checks | `make build` works on a machine with only Go 1.26 installed. `paymux serve` boots on both DBs and `/readyz` returns 200. A rolled-back enqueue never runs (both DBs). Two Postgres instances run each scheduled job exactly once. A second SQLite instance refuses to start. `PAYMUX_DEV_SKIP_2FA=true` with `PAYMUX_ENV=production` refuses to start. `make ci` is green. | 1.5 wk |
| **M1** | Core domain + `/v1` payments | Org bootstrap; Apps; API keys; Customers (with billing fields); Payments/Attempts; state machines incl. double-success handling with `duplicate_payment_action`; LedgerEntry; Events (outbox); idempotency; Blobs; `POST/GET/list/cancel /v1/payments`; OpenAPI + contract test; `authz.Can` + audit; **settings resolver** (app → org → built-in default, §8a) with validation and audit; seeded org settings and routing | State tables exhaustively tested: late success; success after cancel → `needs_review`; amount mismatch → `needs_review`; duplicate with `review` → alert only; duplicate with `auto_refund` → exactly one system refund on the **extra attempt**. Idempotency gives the same response for the same body and 409 for a different body. `settings.Resolve` returns the app override, then the org value, then the default, and rejects out-of-range values. The `settings-via-resolver` seam check passes. | 2 wk |
| **M2** | Webhooks in/out | `/hooks/{pa}` ingress (verify, `ErrUnsigned` fetch-only path, dedupe); `process_provider_event`; Standard Webhooks signer; endpoints; delivery retries (§6.5); replay; SSRF guard; post-commit bus | Standard Webhooks vectors pass. A duplicate provider event is processed once. Unverified events never change state. An unsigned event changes state only through a fetch. The retry schedule is exact (fake clock). | 1 wk |
| **M3** | SDK + Razorpay + pay page + domains | `pkg/connector` (core + optional interfaces declared, implemented as needed), `connectortest`, `connectors/razorpay` one-time, `/pay` (incl. billing-details form + GSTIN validation), `/return`, poller, routing (org defaults seeded: INR→razorpay,zoho,payu; BDT/other→razorpay,whop) + circuit breaker, **AppDomains** (host router, `/internal/tls-allow`, verification check, custom-host route allow-list) | Razorpay passes `connectortest` and the sandbox test. A full flow on both DBs works through a fake. A custom domain serves `/pay` but 404s `/v1` and the dashboard. An unknown host gets 421. **An internal ₹1 live payment, a webhook to a test app, and a refund all succeed.** | 2 wk |
| **M4** | Dashboard | Users, argon2id + TOTP, **dev skip banner**, sessions, CSRF, roles; pages per §12 except invoices/subs/disputes/recon/reports; overlay FS + tokens; **org Settings page + per-app settings tab** (effective value, source, reset-to-org) for every §8a key; org and app routing editors with catch-all and **test-routing box**; per-account accepted currencies; domain management + provider checklist | An operator refunds partially and the audit entry exists. A viewer gets 403. A `data/custom/` template override works. With the dev skip on, login works without TOTP and the banner shows. Without it, TOTP is enforced. Unticking BDT on the Razorpay account makes the test-routing box show Whop for BDT. | 1.5 wk |
| **M5** | GST invoicing | `pkg/tax.Regime`, `internal/tax` (GSTIN validation, place of supply, inclusive/exclusive math, splits), TaxEntity / TaxProfile / DocumentSeries / FxRate, Invoice + CreditNote issue in the success/refund tx, PDF render job (₹ font, optional DSC/PAdES signing, versioned templates), `invoice_delivery` (app / paymux / both), hosted `/i/…`, `/v1/invoices` + `/v1/credit_notes`, `invoice.issued`/`credit_note.issued`, dashboard invoices pages + reissue; State capture on the pay page; LUT and FX rate tables; e-invoice turnover guard | Golden GST cases pass. The invoice total always equals `amount_received`. 20 concurrent issuers on Postgres produce no gaps or duplicates. Numbering rolls over on Apr 1 00:00 IST. Whop-type (MoR) payments and duplicate attempts get no invoice. All §10 rule cases pass: State is required for B2C, full address at ≥ ₹50k, LUT validity, `fx_pending`, credit-note cutoff, B2CL flag. `invoice_delivery=app` sends no email, and `paymux` sends one. | 2 wk |
| **M6** | Subscriptions core + Razorpay subs | Subscription + Mandate models, state machines, `/v1/subscriptions` (create/get/list/cancel/update/payment_method), `/s/{id}` checkout, renewal payments → invoices, dunning engine, period math, Razorpay `SubscriptionManager` (plans cached in KV), dashboard subscriptions pages | Month-end clamping, trial, cancel-at-period-end and update-with-reauthorization tests pass. A Razorpay sandbox subscription renews (sandbox or simulated webhook) and produces a `renewal` payment and an invoice. **→ First app can go live with real customers.** | 2 wk |
| **M7** | PayU India | `connectors/payu`: form_post, hashes (golden), `verify_payment` polling, refunds (`var2`), unsigned refund webhooks, optional chargeback API; **`MandateManager`** (mandate checkout, pre-debit, charge, revoke) | Passes `connectortest` + sandbox. The pre-debit notice is sent 48h before the charge (default), and the charge never runs within 24h of a notice. UPI mandates retry at most 3 times per cycle, outside 10:00–13:00 and 17:00–21:30 IST. Amounts above ₹15k route to the e-NACH (net-banking) mandate. A missed window after downtime reschedules instead of charging. A renewal is never charged twice (dedupe key). With no e-NACH or provider-managed option, the API refuses with `422 amount_exceeds_mandate_limit`. | 1.5 wk |
| **M8** | Zoho Payments | `connectors/zoho`: OAuth/KV, sessions, widget/hosted, `t.body` HMAC, refunds (60/min limiter), manual disputes; UPI `MandateManager` | Passes `connectortest` + sandbox (after Zoho enables it). A per-attempt `payment.failed` does not fail a payment that later succeeds. A UPI mandate renewal works end to end. | 1 wk |
| **M9** | Disputes, recon, reports, alerts | Disputes inbox (accept/evidence/submit/manual), deadline alerts, credit note on lost dispute; daily recon incl. subscriptions/mandates + settlements + fee/fee_tax/payout; GST reports (sales register, GSTR-1 sheets, SAC summary, document summary, fee input-tax-credit register, settlement report); SMTP + ops webhook; silence detector | A recon run against fakes auto-fixes, flags and links renewals. A dispute due in 24h alerts once. The report CSVs match golden files. The fee input-tax-credit register totals equal settlement fee totals in the fixture. | 1.5 wk |
| **M10** | Whop | `connectors/whop` (optional at runtime: routing skips it unless an account is configured): one-time, memberships (`SubscriptionManager`), Standard Webhooks inbound, refunds incl. Resolution Center, disputes, MoR (no invoice), FX | Passes `connectortest` + sandbox. A BDT payment routes Razorpay → Whop on Razorpay failure (fake). A Resolution-Center refund appears as `initiated_by=provider`. | 1 wk |
| **M11** | Hardening + rollout | Litestream config + restore runbook (rehearsed), Postgres backup runbook, `paymux db copy` (row counts + checksums), metrics, fuzzers, perf check vs §12, ops runbook, all three apps live | Restore drills are timed for both DBs. `db copy` SQLite → Postgres on a production-size fixture verifies. Fuzzers run 10 min each with no crash. All apps are live. Old direct provider webhooks are removed from the apps. | 1 wk |

**Order rationale.**
- **Connector order** is the user's: Razorpay → PayU → Zoho → Whop.
- **Invoicing (M5) and subscriptions (M6) come before real go-live,** because every sale needs a GST invoice and recurring is the main revenue. M3's live test is internal ₹1 only.
- **PayU before Zoho:** PayU is the hardest connector, both for one-time and for mandates, so `MandateManager` gets designed against it first.
- **SDK freeze checkpoint at the end of M8:** review `pkg/connector` against three very different connectors. Breaking changes are cheap until then.

---

## Path 2 — self-hostable product (≈ 2–3 months after M11)

Each milestone states **what it adds** and **what existing code it touches**. Touching more than listed means a seam failed: stop and fix the seam.

| # | Milestone | Adds | Touches | Exit criteria | Est. |
|---|---|---|---|---|---|
| **P2.1** | SDK v1 | Connector author docs, template repo, published `connectortest`, `pkg/...` v1.0.0 + API-compat check (`apidiff`) in CI | Doc comments only | An external template-repo connector passes `connectortest` with **zero `internal/` imports**. | 1 wk |
| **P2.2** | Framework mode, builder, remote | `paymux.New` public docs; `paymux build --with module@ver`; `connectors/remote` (HTTP+JSON, signed both ways, `/info` discovery); reference Node remote connector | `cmd/paymux` (adds `build`) | An external Stripe connector is built in with one command. The Node remote connector passes `connectortest` via `remote`. | 1.5 wk |
| **P2.3** | Hooks | Public Go hooks on `App`; `plugins/jsvm` (goja pool, time/CPU limits, fetch allow-list) | `internal/hooks` (exported subscribe API) | A JS hook posts to Slack on `payment.succeeded`. A runaway `onRoute` hook is killed and rules apply. Hooks cannot refund. | 1.5 wk |
| **P2.4** | Themes + i18n | Theme packages (tokens, CSS, templates incl. invoice/email, assets), theme picker per install and per app, 2 bundled themes, i18n catalogs (en, hi) for pay, subscription and invoice pages | Overlay FS gains a theme layer | Switching theme needs no restart. A missing theme file falls back to embedded. A Hindi pay page renders. | 1 wk |
| **P2.5** | Productization | Setup wizard, optional built-in auto-TLS (certmagic) for base + app domains, Docker image, `paymux update` (signed releases), OIDC, custom roles + scoped keys, SSRF default-on, SECURITY.md, upgrade policy, docs site | `authz` policy (data-driven), `auth` (OIDC), config defaults | An outsider goes from download to a test payment in under 15 minutes (timed). An upgrade from the previous release preserves data on both DBs. | 2–3 wk |
| **P2.6** | Expansions (demand-driven) | Optional SDK interfaces + services: payment links, payouts, proration; `tax.Regime` plugins (VAT); e-invoicing (IRN) plugin; more connectors (Cashfree, Stripe, PayPal) | Core gains services. **v1 connectors untouched.** | A v1.0 connector still compiles and passes `connectortest` unchanged. | ongoing |

---

## Rewrite-risk register

| Risk | Would break | Guard (Path 1) | Checked by |
|---|---|---|---|
| Built-in connectors import `internal/` | P2.1/P2.2 | Import rule S1 | Seam test (M0, live from M3) |
| Dialect-specific SQL in shared code, or schemas drifting | "both DBs miss nothing" | Dialect file + parity test + full suite on both | M0 onward, every PR |
| SDK types not JSON-serializable | P2.2 remote | JSON round-trip check in `connectortest` | M3 |
| Templates/CSS hard-coded in handlers | P2.4 themes | Overlay FS only | Seam test (M4) |
| Inline role checks | P2.5 custom roles | `authz.Can` + closed enum | Seam test (M1) |
| Side effects called directly from core | P2.3 hooks | Post-commit bus (invoices, emails, notifications subscribe) | Review checklist + bus tests (M2) |
| GST logic baked into invoice rendering/numbering | P2.6 other regimes | `pkg/tax.Regime` interface; generic document series and renderer | M5 review |
| Subscription logic per connector in core | new connectors | `SubscriptionManager` / `MandateManager` interfaces; core is mode-driven | M6/M7 review |
| `main.go` grows logic | P2.2 builder | ≤25 lines | Seam test (M0) |
| Single-org assumptions | Path 3 | `org_id` scoping on every query | Seam schema test (M1) |

---

## Inputs needed before specific milestones

- **Before M3 live test:** Razorpay live keys, VPS, base domain. Optional: first app custom domain + DNS.
- **Before M5 go-live:** TaxEntity details (GSTIN, address, LUT number if exporting, optional DSC certificate). All GST defaults are researched and editable (spec §10). It's optional to have whoever files your returns glance at a sample PDF.
- **Before M6 go-live:** which app goes first; Razorpay Subscriptions enabled on the account.
- **Before M7/M8:** PayU India and Zoho sandbox credentials (Zoho sandbox via support). Mandate rules use the researched defaults.
- **Before M10:** a Whop account if you want it live. Nothing blocks the build: routing uses only the accounts and currencies you enable.
- **Before P2.1:** license (Apache-2.0 vs AGPL-3.0); public GitHub org.
