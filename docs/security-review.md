# Security and correctness review

A fresh-eyes audit of money-moving paths, tenant scoping, idempotency, input
handling and hosted pages. Every real issue below has a failing-test commit
followed by a fix commit (see `git log`); items that are not issues say why.

Severity is the damage if exploited or hit in production: High (money or
another tenant's data wrong), Medium (wrong state that a person must repair,
or hard to exploit), Low (hardening).

## Summary

| ID | Finding | Severity | Disposition |
|----|---------|----------|-------------|
| F1 | Whop: the unsigned return-URL `payment_id` was fetched and applied to our attempt without checking the payment was ours | High | Fixed |
| F2 | Razorpay: a payment fetched by an outside payment id was used and captured without checking its order or receipt; the receipt search trusted the first order of the list | High | Fixed |
| F3 | Core: any snapshot the provider returned for an id handed to us (return URL, notification) was applied to the attempt, whatever reference or order it named | High | Fixed (defence in depth) |
| F4 | Refunds made in a provider dashboard (Razorpay, PayU, Zoho, Whop Resolution Center) never reached amount_refunded, the ledger, the credit note or the app | High | Fixed |
| F5 | Whop: a refund of ours that the provider reports under its real id could be booked a second time as a provider refund | Medium | Fixed (adoption) |
| F6 | Zoho: a payment fetched by an outside payment id was not checked against our reference | Medium | Fixed |
| F7 | A refund the provider returned for our refund was applied even if it named another payment, receipt, amount or currency | Medium | Fixed |
| F8 | PayU: `FetchRefund` took whichever entry the JSON map yielded first when a payment had several refunds | Medium | Fixed |
| F9 | Core: our own refund was accepted on a payment with an open or lost dispute (customer paid twice) | Medium | Fixed |
| F10 | Payouts: a timeout, cancelled context or cut connection failed the payout, which a caller can then re-send while the first is paid | High | Fixed |
| F11 | Disputes: the provider's dispute amount and currency were trusted (a dispute larger than the payment, or in another currency, held that much from the ledger) | Low | Fixed |
| F12 | `/s/{id}` and `/i/{id}` could be framed (clickjacking) and lacked `nosniff` | Low | Fixed |
| F13 | `/sim` POST had no explicit body cap | Low | Hardened (Go capped it at 10 MiB already) |
| F14 | Zoho refunds carry no provider-side idempotency key | Medium | Mitigated, residual risk documented |
| F15 | Whop `FetchRefund` is synthetic (cumulative refunded amount) | Low | Accepted, mitigated |
| F16 | Outbound: a webhook endpoint could be created for an app that does not exist, or for another org's app | Low | Fixed |
| F17 | A remote connector that is merchant of record could not be seen by invoicing, so Paymux issued a second tax invoice | Medium | Fixed |
| F18 | Tax-exclusive pricing: the invoice total could exceed what was received | Medium | Fixed (the core charges taxable + GST) |
| F19 | The buyer's country was never kept, so a foreign buyer paying in rupees was invoiced as an Indian consumer | Medium | Fixed |

## Findings fixed

### F1, F2, F6: outside payment ids are pointers, not facts

The customer's browser return carries a `payment_id` that nothing signs, and
notifications are hints. The checkout flow fetches the payment by that id from
the provider and applies it to the attempt. A genuine, paid payment that
belongs to someone else (or to nobody: Whop's company page, a Razorpay payment
of another order) would be applied to our attempt, and Razorpay's
authorize-then-capture path would have captured it.

Fix (connector level): when `FetchPayment` gets a payment id and the caller
also names an order, checkout or reference, the fetched payment must belong to
it, else `ErrNotFound`. Razorpay checks `order_id` (or the order's receipt),
Whop checks the checkout configuration or `metadata.ref`, Zoho checks the
reference. A bare payment id with nothing to compare (a dispute lookup) still
works. Razorpay's receipt search also verifies the receipt of the order it
picked instead of taking `Items[0]`. This is the same class as Whop's ignored
list filter that prompted the audit.

Tests: `connectors/{razorpay,whop,zoho}` `TestFetchPaymentByID...`, plus
`TestFetchPaymentByReceiptChecksTheReceiptOfTheOrderFound`.

Other Fetch* lookups were read for list/search use: Razorpay by receipt (now
verified), Whop checkout lookup (now verified by `withRef`), PayU by
transaction id (a direct key, not a search), Zoho by session or payment id
(direct keys). No other list-based lookup feeds a Fetch* result.

### F3, F7: generic ownership check in the checkout flow

Even with correct connectors, the flow must not trust them. `syncAttempt` now
ignores any snapshot whose `ProviderRef` or `ProviderOrderID` contradicts the
attempt's (warn log, nothing applied). `syncRefund` calls `refundIsOurs`: the
payment id, receipt, amount and currency must agree where both sides state
them, else `ErrNotOurs` and the refund stays pending for a person to look at.
Tests: `internal/checkout/ownership_test.go`.

### F4: provider-initiated refunds (the closed TODO(spec))

A refund notification for an unknown refund used to be recorded as an unknown
event and nothing else. Now, test-first on both databases:

- The notification is only a hint. `FetchRefund` (authenticated) returns the
  truth, including the provider payment id; our attempt is located by
  `(provider account, provider_payment_id)` of the delivering account.
- If found, `core.ImportProviderRefund` books a refund row with
  `initiated_by=provider`, the fetched amount (never the webhook's), in one
  transaction under the payment lock: negative ledger entry, `amount_refunded`,
  `refund.created`/`refund.succeeded`, credit note. Unique
  `(attempt_id, provider_refund_id)` (migration 0014, both dialects) makes a
  second delivery or a recon pass a no-op.
- If the attempt is not ours, it stays an unknown event.
- Reconciliation (`reconcileRefunds`) imports their-only refunds the same way
  and marks the item auto-fixed.
- Over-refund: if the provider refunded more than the balance, the truth is
  still recorded, the payment gets the `refund_exceeds_balance` flag, a
  `payment.needs_review` event and a `refund.exceeds_balance` alert.
- A provider refund never gets a `refund.submit` job, and `SubmitRefund`
  refuses `initiated_by=provider`.
- The simulator has a "Refund from provider dashboard" button and `smoke.py` a
  step for it.

### F5: refund adoption

A refund we created and sent may come back with the provider's real id (Whop
reports a synthetic id equal to our `provider_ref` until a webhook arrives). To
avoid booking it twice, `FindAdoptableRefund` adopts our own pending or
succeeded, non-provider refund of the same amount whose provider id is NULL or
equals our `provider_ref`, instead of importing a second row.

### F8: PayU `FetchRefund`

`check_action_status` lists every refund of a payment under one key. The entry
was chosen by map iteration order. It now picks the entry whose `request_id`
equals the one asked for; a lone entry answers a lookup by token; among several
with no match the answer is `ErrNotFound`, never a guess.

### F9: refunds and disputes

A card network that has taken money back (open or lost dispute) plus our own
refund pays the customer twice. `core.CreateRefund` now refuses with
`ErrNotRefundable` when the payment carries `disputed` or `dispute_lost` and
the refund is not `initiated_by=provider`. Provider-made refunds are still
booked (F4), because they already happened.

### F10: payouts under ambiguity

`handleSubmit` failed the payout on any error that was not `ErrTemporary`.
A context deadline, cancellation, `io.EOF` or net error says nothing about
whether the provider received the request. These are now retried with the same
`ProviderRef` (the provider's idempotency token), exactly like `ErrTemporary`.
Only an answer from the provider fails the payout.

### F11: dispute sanity

`disputes.upsert` rejects (`ErrInvalid`) a snapshot with an amount larger than
the attempt's `AmountReceived` or a currency different from it. A partial
dispute is fine.

### F12, F13: hosted page headers and bodies

`/s/{id}` (a payment form) and `/i/{id}` (invoices, credit notes, PDFs) now
send `X-Frame-Options: DENY`, `frame-ancestors 'none'` and
`X-Content-Type-Options: nosniff`, on 404s too. `/pay`, `/l`, the dashboard and
the API docs already did. `/sim` POST caps its body at 64 KB; Go's `ParseForm`
already limited it to 10 MiB, so there was no failing test, and this is
hardening only.

### F16: webhook endpoints and their app

`CreateEndpoint` carried a `TODO(spec)` to check that the app exists "once the
apps table lands". It now loads the app, refuses an unknown one, refuses an org
that is not the app's, and takes the org from the app when none is given (it
used to take the first org of the database). The CLI, the dashboard and the
tests all go through it.

### F17: merchant of record is a fact of the account

Invoicing decided "this connector sells as merchant of record, so issue no
invoice" from the connector's registry entry. A `remote` connector learns that
by discovery, so its registry entry cannot say, and a remote MoR connector would
have had a Paymux invoice issued on top of the provider's. `CreateAccount` now
reads the live connector's `Info` (it already builds the connector, which is what
probes `GET /info`) and stores `merchant_of_record` on the account (migration
0018); invoicing reads the account.

### F18, F19: tax correctness

With `prices_include_tax=false` the invoice used to treat the amount received as
the taxable value, so the invoice total could exceed `amount_received`. The core
now charges taxable + GST (`core.chargeAmount`, for payments, payment links and
renewals) and the invoice derives its heads from the total received, so the two
always agree; documented in the OpenAPI `amount` field. The pay page now asks for
the buyer's country and keeps it on the customer; a buyer abroad is classified as
an export (`export_igst` for a payment in rupees, with an alert). Limits: the
amount is computed from what is known when the payment is created, so a buyer
who gives a foreign country only on the pay page after the amount was fixed is
invoiced as an export with IGST at the same total, not zero-rated (a zero-rated
export needs a payment in a foreign currency).

## Accepted or mitigated

### F14: Zoho refund idempotency (Medium, residual)

Razorpay (`X-Refund-Idempotency`), Whop (`Idempotency-Key`) and PayU (the
23-character request token) deduplicate a retried refund. Zoho documents no
idempotency key; we send `reference_number`, which is not documented as
unique. A refund whose response is lost can therefore be retried and made
twice at Zoho. Mitigations: Zoho refuses a refund above the payment's balance;
a duplicate that does succeed is found by the daily reconciliation as a
provider-only refund, imported as `initiated_by=provider` and, if it exceeds the
balance, flagged and alerted (F4). TODO(spec): confirm at sandbox time whether
Zoho honours `reference_number` as a dedup key.

### F15: Whop synthetic refunds (Low)

Whop has no refund object API; `FetchRefund` is synthesized from the payment's
cumulative `refunded_amount`. With two partial refunds in flight, the first
may be reported with the cumulative amount. `refundIsOurs` then refuses the
amount mismatch for refunds we created, so it stays pending rather than being
booked wrong, and the payment-level `amount_refunded` reconciliation catches up
on the next recon pass. Accepted.

## Reviewed, not issues

- **Open redirects / return_url.** `return_url` is set by the app through its
  authenticated API key or by an org admin in the dashboard, validated as an
  absolute http(s) URL with a host (`javascript:`, `ftp:`, relative URLs are
  rejected; tests in core, httpapi, dashboard, paylinks). The hosted pages
  never redirect to a URL taken from the request. A per-app host allow-list
  would be a defence against a stolen API key, which can already create
  payments; not built. Recommend as a later option.
- **CSRF on hosted pages.** `/pay` and `/l` bind the form to an unguessable
  client secret or link token carried in the URL; there is no ambient session
  for a cross-site request to ride on. `/sim` exists only for the test-only
  simulator connector, is addressed by an unguessable reference, and cannot
  touch live mode. The dashboard has its own CSRF protection and headers.
- **Cache headers.** Secret-bearing URLs (`/pay`, `/s`, `/i`, `/l`, `/sim`,
  `/return`) send `Cache-Control: no-store` (or `private, no-store`) and
  `Referrer-Policy: no-referrer`.
- **Tenant, app and mode scoping.** API reads and writes go through
  `core.Scope{Org,App,Mode}` and compare `AppID` and `Mode` on the loaded
  object, answering not-found otherwise; dashboard queries are org-scoped
  (covered by `TestDisputeOperationsAreOrgScoped`, `TestReadAPIScopesToAppAndMode`
  and the dashboard org tests). Provider ids are always looked up scoped to the
  delivering provider account (`FindAttempt`, `FindDispute`, `FindRefundByProviderID`),
  so one account's ids cannot match another's, in either mode.
- **Body and page limits.** API bodies, inbound webhooks, dashboard forms and
  uploads, `/pay`, `/l` use `http.MaxBytesReader`; API list endpoints cap
  `limit` at 100 (`maxPageSize` in core); the store's list queries carry a LIMIT.
- **Idempotency of jobs.** `refund.submit`, `payout.submit`, `outbound`
  delivery and the provider-event processor re-read state first and return
  early when the object is no longer pending; provider calls carry the stable
  `provider_ref` (see F14 for the exception). Webhook events are deduplicated
  by `(account, event id)`.
- **Secrets in logs.** Spot-checked only: the new warn logs contain ids, never
  payloads, and the redaction tests pass. No full log review was done.

## TODO(spec) items that leave money state un-applied

| Item | Effect | Disposition |
|------|--------|-------------|
| Provider-initiated refunds | Never booked | Fixed (F4) |
| Provider dispute won/lost reversals | A dispute opened or resolved in a provider dashboard | Not a gap: dispute webhooks go through `disputes.SyncNotification`, which fetches the dispute and applies won/lost (ledger release or loss, flags, credit note) and the poll/recon pass catches missed webhooks. PayU's Chargeback API (separate token, 20-day window) and Whop's dispute evidence API are not built: PayU and Whop disputes are not read automatically, so they must be entered in the dashboard (manual entry exists). TODO(spec) stays until a sandbox exists. |
| Mandate revocation at the provider/bank | Our mandate stays active | Covered by the daily mandate reconciliation, which fetches the mandate from the provider and applies `revoked`/`failed`; the next renewal also fails with a mandate error and moves the subscription to past due. A real-time webhook mapping is not built. |
| Payout webhooks and reversals | Status only moved when polled | Fixed: a `payout` notification kind in the SDK (`connector.NotifyPayout`); the simulator sends `payout.processed/failed/reversed`; inbound processing finds the payout in the delivering account, fetches it, checks reference, amount and currency, and applies that. Polling remains for providers without webhooks. No built-in provider connector pays out yet. |
| PayU refund webhooks are unsigned | Cannot be trusted | Handled: they are treated as a hint and the refund is fetched (`FetchRefund`), now deterministic (F8). |
| `RefundsVerificationDebit` (PayU, Zoho) | The Rs 1 verification debit is assumed reversed by the provider | Unverified without sandbox; recon reports a difference if wrong. |
| PayU `ListRefunds` unsupported | Recon cannot list PayU refunds | Recon polls known refunds by `FetchRefund`; a PayU dashboard refund is found only through its webhook. Justified: PayU offers no listing API. |

## Second batch: TODO(spec) items that did not need provider keys

Everything below was closed with a failing test first, on both databases.

| Item | Disposition |
|------|-------------|
| Tax-exclusive pricing | Fixed (F18). |
| Buyer country on the payment | Fixed (F19). |
| Ops alerts for tax exceptions | Fixed: `pos_defaulted`, `export_igst`, `fx_pending` and `credit_note_cutoff_passed` raise `tax.*` alerts through a job enqueued in the issuing transaction (so an alert is never sent for a transaction that rolls back), deduplicated by document and by the alert service. |
| Credit-note cutoff and B2CL threshold | Fixed: org settings `credit_note_cutoff` (`MM-DD` or `off`, default `11-30`) and `b2cl_threshold` (paise, default 10000000). |
| Payout webhooks | Fixed (above). |
| Stale TODOs | Fixed: endpoint app check (F16); the remote connector's "validate at account creation" was already true (the factory probes `GET /info`) and its MoR gap is F17; notes that were never gaps (settings table, e-invoice schema optional sections, the Cashfree timestamp decision, an example's remarks) are now plain comments. |
| DSC / PAdES signing | Done in pure Go (`internal/invoice/pades`; `golang.org/x/crypto/pkcs12` is part of a module already required, listed in `docs/dependencies.md`). The PKCS#12 file and password are sealed with the master key, validated on upload (password, key type, validity, the AES encryption it cannot read), audited without the secret, and a certificate that stops working never stops an invoice (note plus alert). Verified against OpenSSL and an independent parser; **not** against Adobe Acrobat or the CCA India trust list (see `docs/provider-verification.md`). |
| E-invoice JSON for credit notes | Fixed: a `CRN` document with `RefDtls.PrecDocDtls`; dashboard download on the credit note page. |
| Stripe `UpdateSubscriptionAmount` | Implemented as an inline price swap on the subscription item with `proration_behavior=none`, idempotent per subscription and amount; the wire shape is from documentation and is on the verification list. |
| Zoho widget launcher | The SDK page had no launcher for Zoho (the widget mode showed a page that did nothing). A launcher is added from the documented call shape; unverified, on the list. |

Security notes on the new surface: the DSC upload is admin-only
(`SettingsWrite`), capped at 64 KB in the service and by the multipart limit in
the dashboard, CSRF-protected like every dashboard form, and the page never
echoes the file or the password; the sealed blob is bound to its tax entity
(AAD), so it cannot be moved to another entity's row.

## Third batch and live findings

| ID | Finding | Severity | Disposition |
|----|---------|----------|-------------|
| F20 | A payment's `allowed_connectors` and `country` were checked when it was created but not kept: the pay page, a retry and a fallback routed again without them, so money could reach a provider the app had excluded | High | Fixed: both stored on the payment (migration 0019) and used for every routing decision |
| F21 | A card payment completed at the provider whose return and webhook never arrived (a PayU response handler timeout) was never looked at again, and could be expired or failed as unpaid | High | Fixed: polling starts with the checkout (2 minutes, backing off, through the checkout's expiry plus an hour); expiring a payment asks the provider first and a success wins; reconciliation matches a started checkout by our reference |
| F22 | The e-invoice portal's `SignedQRCode` signature was never verified, so any well-formed token naming the right IRN was accepted | Medium | Fixed with a strict verifier (`internal/invoice/irp`): alg exactly RS256 (no `none`, no HMAC under the public key), the token's `kid` / `x5t` must name a configured key, no embedded or referenced keys (`jwk`, `jku`, `x5u`), key of 2048 bits or more. Active once `einvoice_irp_keys` is set; **no key is shipped** (see below) |
| F23 | OAuth Connect (Zoho) is a new route that stores a credential from a redirect | Design | The `state` is random, hashed at rest, single use, ten minutes, bound to the session user and the account; the callback needs `provider_account.manage`; the data centre it names is looked up in Zoho's own list, so an `accounts-server` parameter can never make Paymux call another host; tokens and the client secret are sealed and never logged, rendered or audited; the page rendered from a failed callback no longer carries the code in its forms |
| F24 | A proration credit could take a renewal below the provider's minimum charge, and a small prorated extra charge was attempted on its own | Low | Fixed: `Info.MinChargeMinor`, credit carried forward, small charge folded into the next renewal |
| F25 | A verification debit that the provider was *believed* to reverse was never checked | Low | Fixed: a 72-hour check refunds it explicitly if the provider still shows it unrefunded (`verification_debit_fallback`), idempotent and never on top of another refund |

Decisions worth knowing:

- **No IRP key is shipped.** The portal publishes its sandbox and production
  keys, but a key typed from memory cannot be checked from here, and a wrong
  default would either reject genuine tokens or give a false sense of
  verification. The setting is empty by default (the claims are still checked,
  as before) and the runbook says where to get the key.
- **`RefClm` defaults to claiming** the IGST refund on an export that paid IGST
  (`auto`), never under a LUT. It is an org setting; the choice belongs to
  whoever files the returns.
- **The DEMO setup** (Karnataka entity, prices including 18%) is covered end to
  end: a succeeded payment issues exactly one invoice whose total equals
  `amount_received`, for a Karnataka buyer (CGST + SGST), an out-of-state buyer
  (IGST), B2B in and out of state, an export under a LUT or without one (IGST),
  a payment in rupees from abroad, exclusive pricing, and a duplicate success.

## Fourth batch: the error log in the database

| ID | Finding | Severity | Disposition |
|----|---------|----------|-------------|
| F26 | Keeping errors, failed provider calls (with their request and response) and rejected requests in a database table, for later debugging, would put credentials and buyers' details in a second place that is read by people and copied by backups | High if done naively | Designed out, then tested: every row is redacted before it is queued (below); no info or debug rows; no body for a call that worked; readable only with `logs.read` (admin by default); nothing in a column is ever the raw value of a header, cookie, token, key or card |
| F27 | The audit log had no retention setting: it grew forever, and a future purge could have been set to any length | Low | `audit_retention_days`: default 400, 0 keeps it forever, and nothing below 365 days is accepted (PCI DSS 10.5.1); the purge job repeats the check and every purge is itself audited |

**How the log avoids secrets and personal data.** Three layers, strongest first
(`internal/redact`), each applied to the message, every attribute, the URL and
both bodies of a provider call:

1. *Known values.* The redactor is given the actual secrets in use and replaces
   every occurrence, raw, URL-encoded, JSON-escaped or HTML-escaped, wherever it
   is: the secret config fields (`x-secret`) of every provider account (loaded
   at start and refreshed whenever an account is built or changed), the master
   key in each encoding, SMTP and OIDC secrets, the ops webhook URL, the
   database password and the OTLP headers. The Logs page and the export apply it
   once more on the way out, so a secret learned after a row was stored is
   hidden too.
2. *Field names*, matched by whole words (so `payment_id` and `key_id` survive
   but `razorpay_signature`, `refresh_token`, `api_key`, `salt`, `hash`, `otp`,
   `cvv`, `card_number`, `private_key` and `pkcs12` do not) in JSON bodies (also
   JSON inside a string and JSON cut short by the size limit), form bodies,
   query strings and log attributes. Buyer details go the same way: `email`,
   `phone`, `mobile`, `contact`, `name` (and `first_name`, `customer_name`, ...),
   `address`, `vpa`, `upi_id`, `gstin`, `pan`. Our own ids and the provider's ids
   are kept: a row says *which* payment, attempt, refund, subscription, mandate,
   payout, job and provider account it is about, and the entity row holds the
   details.
3. *Value patterns*, for what has no telling name: `sk_`/`rk_`, `rzp_`, `whsec_`,
   `ws_`, `apik_`, `cfsk_`, `pmx_` keys, session tokens, Zoho `1000.x.y` tokens,
   JWTs, `Bearer` and `Basic` values, PEM blocks, credentials in URLs, Luhn-valid
   card numbers (masked to the last four), e-mail addresses, phone numbers,
   GSTIN, PAN and UPI handles.

Headers are an allowlist (content type and length, user agent, date, retry-after
and request-id style headers), applied in the HTTP client before a record leaves
it; the client also removes the query values of credential-named parameters and
the secrets it was configured with. A body is cut at 64 KiB before redaction
(the last bytes dropped, so a secret cut in half cannot survive), and stored
capped at 8 KiB after it.

What is tested: a scan of **every column of every row** after real flows (a
payment through the pay page, a signed and a forged webhook, a refund, a rejected
API key and login, a failing hook job, a refused OAuth exchange) against a
provider that answers with errors echoing every credential and the buyer's
details; zero hits on SQLite and PostgreSQL. Field-name false-positive guards
(ids, `footprint`, `monkey`, `connector_name`) and hostile-input timing are unit
tests.

Residual risks, accepted: a buyer's name in free text with no field name around
it (`refund failed for Asha Rao`) is not recognisable; a secret in a format the
patterns do not know, in a field with no secret-like name and not configured
anywhere, would stay. Both are why the table is admin-only and why bodies are
kept for failed calls only, for 90 days. The OTLP export (off by default) ships
the same redacted rows; its endpoint is operator-configured and may be private,
so it has no address guard.

Availability: rows go through a bounded queue and are inserted in batches by one
goroutine; a full queue drops rows and counts them (`paymux_logs_dropped_total`
and a periodic line on stdout); a failed write is reported on stdout only and is
never itself logged to the table, so logging cannot recurse. On SQLite the table
lives in its own file, `<data>/logs.db`, so a log write never takes the main
database's write lock (tested with a long main write transaction); the file can
be deleted at any time and is left out of `paymux backup`.

## Fifth batch: the external audit of October 2026

An outside review of the hub (dashboard sign-in, API keys, outbound and inbound webhooks, refunds, mandates,
single sign-on, logging, JS hooks, deployment) produced the findings below. Each was fixed test first: a test that
fails on the old behaviour, then the change, on SQLite and PostgreSQL. To add a finding, append a row and a
paragraph under the same headings; the numbers continue from F28.

| # | Finding | Severity | Status |
|---|---|---|---|
| F28 | Dashboard sign-in: the second factor could be brute-forced. The failure counter was per address and cleared by a correct password, the login ticket allowed unlimited guesses, and the address came from `X-Forwarded-For` (attacker-controlled when the peer is on the trusted local network) | High | Fixed: a login ticket carries a nonce and allows 5 guesses; per-account ceilings (20 wrong passwords, 10 wrong codes per 15 minutes) hold whatever address is claimed; a correct password no longer resets the code counter |
| F29 | Creating or rotating an API key handed out payout and refund power to a dashboard user who did not have it | High | Fixed: a key's scopes are mapped to dashboard actions (`authz.ActionsForScopes`) and a user can create or rotate a key only within their own authority, checked on the final (inherited) scopes inside the transaction |
| F30 | A webhook endpoint received the events of both modes, and event bodies did not say which mode they were from | Medium | Fixed: endpoints have a `mode` (migration 0022); delivery is filtered by it; every event body, API object and view carries `livemode` |
| F31 | A duplicate refund of an attempt the provider had already refunded could be booked against a different, real sale | Medium | Fixed: the duplicate is recognised on the attempt, flagged (`duplicate_refunded`), alerted and kept out of the sale's balance |
| F32 | Tax-exclusive subscriptions: renewals through a mandate charged the taxable price only, so the invoice total exceeded what was received | Medium | Fixed: renewals charge taxable + GST like a first payment. The mandate's AFA limit and headroom are still computed on the taxable price (see deferred) |
| F33 | A single-use payment link could be paid twice (two visitors, or one visitor twice) | Medium | Fixed: the link remembers the payment its holder is paying and the payment that spent it (migration 0023); the same visitor gets the same payment back; a second success is flagged (`link_already_used`), the other open payments of the order are cancelled and an alert is raised |
| F34 | `/hooks`: forged events were stored in full without bound (disk), unsigned-provider events shared a key space with signed ones, and a webhook body was applied as if it were the truth (stale or forged snapshots, L1 and L2) | Medium | Fixed: an event that fails verification keeps only its hash (windows and budgets per source and per account, the client address through `internal/clientip`), `unverified:` and `unsigned:` key spaces, an hourly purge job (`provider_events.purge`, `provider_event_retention` was declared but never enforced), and a webhook is a hint: mandate and subscription state is fetched from the provider before it is applied |
| F35 | Password hashing (argon2id, 64 MiB each) was unbounded: a caller who opens many connections can exhaust memory | Medium | Fixed: at most 2 x GOMAXPROCS hashes run at once, the surplus gets 503 instead of queueing, and login attempts per address are limited (30 a minute) before any hashing |
| F36 | Single sign-on matched an account by e-mail address alone, so a verified address at the identity provider took over a local account that had a password and a second factor; allowed domains applied only to new users | Medium | Fixed (migration 0024): accounts are bound to the provider's (issuer, subject); a password account can be reached by single sign-on only after an admin allows it (audited); a different subject with the same address is refused; the allowed domains apply on every sign-in |
| F37 | An idempotency reservation taken over as stale could be completed or released by the request that lost it | Low | Fixed (migration 0025): each reservation carries its owner's token |
| F38 | `POST /v1/payouts` without an `Idempotency-Key` could pay twice on a client retry | Low | Fixed: the header is required (400 without it) |
| F39 | The HTTP servers set only a read-header timeout | Low | Fixed: read, write, idle timeouts and a header size limit on both listeners |
| F40 | A hand-built `Config` with `DevSkip2FA` in production started with the second factor off | Low | Fixed: `hub.New` refuses it (the environment loader already did) |
| F41 | Whoever first signed in with the password of a user who had not set up two-factor authentication was shown the secret and enrolled their own authenticator | Low | Fixed (migration 0026): enrolment needs a one-time code an admin issues (shown once when creating the user or resetting 2FA; stored as a hash, 7 days); first-run setup is authorised by the setup code |
| F42 | `/i/{id}` and `/s/{id}` were served on any custom domain, whatever the app | Low | Fixed: a custom domain serves only its own app's invoices and subscription checkouts (404 otherwise, before anything is started at the provider) |
| F43 | `ImportRefund` applied a refund it already knew without checking it against the refund fetched from the provider | Low | Fixed: the same `refundIsOurs` check as the poll path |
| F44 | Information: database copies were created with the process umask and tightened afterwards; JSON request bodies could repeat a key; the API echoed the Go decoder's error text; the return URL's `status` looks like proof of payment | Info | Fixed: copies are created 0600 (the pre-migration snapshot too); a repeated key is a 400; errors name the field in the API's terms; the OpenAPI description and the quickstart say the return URL is unsigned and must not be trusted |
| F45 | Redaction replaced a known secret only in its plain form, so the same secret hex- or base64-encoded (a rebuilt `Authorization` header, HTTP Basic `base64(id:secret)`) went to the log table | Medium | Fixed: hex (both cases), base64 in all four alphabets (padded or not), the secret at any of the three alignments inside a longer base64 string, and `id:secret` pairs of an account |
| F46 | Redaction missed spaced or dashed Indian phone numbers, Aadhaar numbers, dot- or slash-separated card numbers, `ccvv` / `cvn` / `ccnum` field names and secrets in webhook URL paths (Telegram, Slack, Discord, long random segments) | Low | Fixed, with guards: our ids (`pay_...`, `ref_...`), ULIDs and UUIDs stay readable; plain 12-digit runs are Aadhaar numbers only when the Verhoeff check digit is right |
| F47 | Log flooding: every rejected `/v1` or `/hooks` request became a row, so one source could fill the table and the disk, and the caps were applied only by the hourly job | Medium | Fixed: one row per source, route and status a minute (30 per route and status from all sources), the next row carries a `suppressed` count; caps on rows and size (`log_max_mb`) are applied as rows arrive |
| F48 | JavaScript hooks had a time limit but no memory or output limit: `'x'.repeat(1 << 30)` or a growing array took gigabytes | Medium | Fixed: a per-call memory ceiling (128 MiB of live heap, checked every millisecond), one-step allocators (`repeat`, `padStart`, `padEnd`, `fill`, `join`, `concat`, `Array.from`, `apply`) refuse huge sizes, console and routing output are capped; fails closed |
| F49 | The on-demand TLS "ask" approved every domain the hub knew, including pending ones, so adding names in the dashboard could use up the certificate authority's rate limits | Low | Fixed: verified domains, and pending ones only while their A or AAAA record already points at the hub |
| F50 | There was no way to rotate the master key | Design | Added: `PAYMUX_MASTER_KEY_OLD` / `_FILE`, `paymux secrets reseal` (one transaction, idempotent, refuses if any value cannot be opened) and a runbook procedure; every sealed column is listed in one place and a test fails if the schema gets one that is not |
| F51 | Third-party GitHub Actions were referenced by tag, which their owners can move | Low | Fixed: pinned to commit SHAs with the tag in a comment; a test fails on an unpinned `uses:` |
| F52 | "Remember me" (a 30 day sign-in) makes a stolen session cookie useful for much longer | Design | Added with limits on the window rather than none: off by default and switchable off (`session_remember_days` = 0); 30 days absolute from sign-in and 30 days idle, never extended by use; it never replaces the password and authenticator code at sign-in (the choice is signed into the login ticket and the SSO state, not read at the second step); every session is revoked at once by disabling the user, a password change, a 2FA reset, a new enrolment code or an admin's sign-out everywhere; users see and end their own sessions (created, last seen, kind, network prefix, device); sensitive actions (keys, roles, users, enrolment codes, provider accounts) need a password and code confirmation when the last full sign-in is over 12 hours old; sign-ins and revocations are audited. Residual: a cookie stolen within its window works until it is revoked, and the sensitive actions above are the only ones behind the re-confirmation |

### Notes on the fixes

- **F28.** The per-account ceilings can lock a legitimate user out for up to 15 minutes if someone keeps guessing;
  that is the price of a limit that does not depend on a claimed address. The counters are in memory, per process
  (a second instance behind a load balancer has its own); the dashboard is not meant to be scaled out. The default
  trusted-proxy setting (the local network) was left as the operator chose it; the fix holds when
  `X-Forwarded-For` is attacker-controlled because it does not rely on the address.
- **F29.** A key can be created or rotated with the scopes the user's role allows; an admin can still issue a key
  with payout scopes. What changed is that a user without refund or payout power cannot mint it.
- **F34.** Forged events are not stored; only a hash and counts. Verified events are kept for
  `provider_event_retention` days (default 400); unverified ones for at most 30 days regardless.
- **F36.** Users that single sign-on created before identities were recorded (no password) are bound on their next
  sign-in. After an admin allows a password account, the next sign-in with that address binds it and the permission
  is spent.
- **F41.** An upgrade leaves users who have not enrolled without a code; an admin issues one from the Users page or
  with `paymux user reset-2fa`.
- **F45 to F47.** Redaction is a backstop, not a guarantee: a secret in a format nothing knows, in a field with no
  secret-like name and not configured anywhere, would stay (see the fourth batch).
- **F48.** The memory figure is the process's heap, not the script's, so it is an approximation under heavy load.
  Hooks are code the operator wrote; a container memory limit remains the hard bound.

### Reviewed, not issues (fifth batch)

- An automated review said the dashboard's per-account limiter keys used the raw e-mail. They do not: the login
  form and the second-factor step both normalise the address (trim, lower case) before it is used as a key, the same
  way the store does. Regression tests were added for the spellings `Alice@Example.com`, ` alice@example.com ` and
  the like.

### Deferred

- Mandate AFA limit and headroom for tax-exclusive subscriptions are still computed on the taxable price (F32); the
  charge itself is right.
- The duplicate-refund ledger entry keeps the kind `refund` (a new kind would need a table rebuild for the CHECK
  constraint); the duplicate is tracked by the flag, the event and the refund row.
- Per-account sign-in counters, the single-use link hold and the log throttle are in memory per process.
- On SQLite `paymux secrets reseal` needs the hub stopped (the data directory is locked); on PostgreSQL it can run
  beside the hub and stops without changes if a value changes under it.

## Connector-side findings from the 2026-10 audit

Found by an independent auditor; the numbering of the core and dashboard findings
is kept elsewhere. Each has a failing test that was written first.

- **L3, an unsigned PayU mandate notice made Paymux call PayU while answering the
  webhook** (Low): the inbound handler asked `ParseWebhook` for the event id, and
  for an unsigned e-NACH notice that read the mandate back from PayU, so a flood of
  forged notices used up the account's PayU rate limit. Fixed: new optional
  `connector.WebhookEventIDer`; the PayU connector names a mandate notice from the
  body with no call, and the confirming fetch stays in the background job. The
  unsigned notice is still only a hint. The id is deterministic from the body.
- **L2, webhook event ids from unsigned headers, and no replay window**
  (Low/Medium): Razorpay's `X-Razorpay-Event-Id` and Cashfree's `x-idempotency-key`
  are not covered by the signatures, so a captured signed delivery replayed with a
  different value had a new event id and was applied again. Fixed: both ids are
  hashes of the signed body (retries and resends still dedupe, a replay with another
  header does not apply twice). The signed Cashfree timestamp and the signed
  `created_at` of Razorpay subscription events (which are applied from their
  payload) must be within 48 hours of receipt. Payment, refund and dispute events
  are applied from a fresh fetch and are not time-checked. The id derivation for
  the inbound store (`unsigned:` prefix) is the core's.
- **L11, the remote-connector replay cache evicted early** (Low): it held 10,000
  ids for a 10 minute window, so above about 16 requests a second an id left before
  its window ended and its replay was accepted. Fixed: a full cache never evicts; it
  answers `503` (temporary) and every served request stays refused as a replay for
  the whole window. The default is 100,000 ids (about 166 requests a second);
  `wire.WithReplayCacheSize` sizes it.
- **Two SSRF guards that disagreed** (info): `internal/outbound` missed the rest of
  0/8, the documentation ranges, 198.18/15, 240/4, NAT64 and 6to4. Both now use
  `connector.IsPrivateIP`, which covers every special-purpose range (table test),
  including IPv4-mapped IPv6. `PAYMUX_ALLOW_PRIVATE_WEBHOOKS` still skips the guard
  for deliveries.
- **`allow_insecure` on a live remote account** (info): it switched off the
  private-address guard. Now refused for live accounts on create and update, and
  ignored (both by the host and by the connector) for a live account that already
  stored it.
- **A changed endpoint reused the stored secret** (info): editing the remote
  connector's `base_url` kept the stored secret and sent it to the new host.
  Chosen: the simpler guard that blocks rather than records: a config that changes
  an `x-endpoint` field while a secret field still holds its stored value is
  refused ("enter the secret again"). The dashboard form that keeps a blank secret
  field will therefore show that error when the URL is edited without re-entering
  the secret (the dashboard is the core builder's).

## Provider messages in API errors

A provider's "request invalid or not allowed" answer reaches the API caller as a 422
`provider_rejected` (or `unsupported`) with the provider's own text. That text could echo
a credential or personal data, so it passes through the log redactor (every account's
known secrets, key shapes, personal data) and is cut to 300 characters before it is put in
the response; a test checks that a message echoing a configured secret does not leak. The
full error stays in the (redacted) log. Everything else remains an internal error that
says nothing.

## What remains

Every `TODO(spec)` left in the code needs a provider sandbox, a provider
document, or a portal; each is a checklist item in
[provider-verification.md](provider-verification.md). The one remaining design
question that is not verification: the IRP key to ship by default (see above).
