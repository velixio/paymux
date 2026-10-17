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

## What remains

Every `TODO(spec)` left in the code needs a provider sandbox, a provider
document, or a portal; each is a checklist item in
[provider-verification.md](provider-verification.md). Open design decisions that
are not verification: the e-invoice `RefClm` setting for exporters who claim an
IGST refund, and a per-provider minimum debit for proration credits.
