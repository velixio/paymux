# Security audit, October 2026

An evidence-based audit of the whole repository at commit `677e9c0`. It covers
money flows, the ledger, idempotency, authentication and tenant isolation,
webhooks, SSRF, secrets and logging, extensions, the web surface, operations
and CI. The earlier [security review](security-review.md) (F1 to F53) was
treated as unverified: its fixes in the audited areas were re-tested, and three
of them turned out to be incomplete.

Every finding marked *Confirmed* has a test that fails against `677e9c0` and
passes after the fix. All of these tests are in files named `audit_*_test.go`
next to the code they cover. Findings without such a test are listed
separately as hypotheses. Nothing here was run against a real provider, a
provider sandbox or production data.

## Executive summary

- **Five high-severity issues were confirmed and fixed.** Four of them let a
  payment, or a subscription's first payment, be booked as paid on the
  strength of something the customer controls. The fifth let a client that
  hangs up run an idempotent request (a payout, a refund) a second time.
- **Fourteen medium- and low-severity issues were confirmed.** Thirteen are
  fixed. One (A-13) was first left open, and is fixed by the remediation plan.
- **No critical issue was confirmed.** A-1 would be critical in a multi-org
  hub, but the store only ever creates one org today.
- **No confirmed critical or high issue remains unresolved.** The hypotheses
  in [Potential risks](#potential-risks-requiring-further-verification) are
  not demonstrated and have not been fixed.
- **Paymux is not declared production-safe by this audit.** See
  [Production readiness](#production-readiness-assessment).

## Threat model

**Assets.** Money flows (charges, captures, refunds, payouts), the ledger and
the payment states it follows, provider credentials, the master key, API keys,
dashboard sessions, webhook signing secrets, and buyers' personal and tax data.

**Trust boundaries.**

| Boundary | Trusted side | Untrusted input |
|----------|--------------|-----------------|
| Customer browser → hosted pages (`/pay`, `/s`, `/l`, `/return/{pa}/{ref}`) | hub | every query, form and path value, including the provider account in the return URL |
| Provider → `/hooks/{pa}` | hub, once the signature verifies | the body is a hint; state comes from a provider fetch |
| Provider API → connector | hub, for what it fetched | ids handed to us from outside (`payment_id` on a return) |
| App backend → `/v1` | an API key with its app, mode and scopes | bodies, ids of other apps' objects |
| Dashboard user → `/dashboard` | a session with its role | a stolen long-lived cookie; custom roles |
| Hub → outbound URLs | — | app webhook URLs, custom domains, remote connectors, JS hook fetches |
| Hub → stderr, log DB, OTLP | operators | anything a provider echoes back in an error |
| Remote connector / JS hooks | partly trusted code | their output is validated by the core |

**Attacker capabilities considered.**

1. A paying customer who edits URLs and forms.
2. Someone holding a working API key of one app.
3. A dashboard user with a custom role.
4. Someone holding a stolen "remember me" admin cookie.
5. Someone who can sign webhooks for one provider account, or who runs a
   remote connector.
6. An attacker on the network between the hub and its database.
7. A local user on the host.

**Principal abuse cases.** Getting goods without paying (a forged or
transplanted success), paying once and settling many orders, duplicate payouts
or refunds, paying a customer twice during a dispute, privilege escalation
through key delegation or roles, credential leaks through logs, SSRF into the
host's network, and spreadsheet code execution through exports.

**Assumptions not verifiable from the repository.**

- Whether each real provider's fetch-by-id response always carries our
  reference or order (A-3 and A-4 rely on it to keep legitimate returns
  working).
- Whether a real Zoho payment can lack both session and reference (the
  precondition of A-4).
- Each provider's sandbox and live key formats (A-6).
- Proxy, TLS and network configuration in deployment.

## Findings

Severity follows the brief: critical (practical unauthorized money movement or
master-secret compromise), high, medium, low, informational. Line numbers refer
to the fixed tree.

### Summary

| ID | Finding | Severity | Status |
|----|---------|----------|--------|
| A-1 | A subscription or mandate return via another provider account's URL decides that mandate's state and books the first payment | High | Fixed |
| A-2 | A renewal or setup success that states no amount was booked as the full expected charge | High | Fixed |
| A-3 | Core ownership check (F3) fails open; one provider payment could settle any number of payments | High | Fixed |
| A-4 | Zoho: a payment fetched by the unsigned return-URL `payment_id` that names no session and no reference was accepted (F6 incomplete) | High | Fixed |
| A-5 | Idempotency-Key released after the handler committed, when storing the response failed (client disconnect) | High | Fixed |
| A-6 | Live provider accounts accepted sandbox credentials (and test accounts live keys) | Medium | Fixed |
| A-7 | A pending refund was still sent to the provider after a dispute opened (F9 gap) | Medium | Fixed |
| A-8 | The first of two open disputes to end cleared the `disputed` flag, re-enabling refunds | Medium | Fixed |
| A-9 | Role changes, user disable/sign-out, key revoke and provider-page app linking skipped the recent sign-in check (F52 incomplete) | Medium | Fixed |
| A-10 | stderr logs carried secrets that the database log redacts | Medium | Fixed |
| A-11 | CSV formula injection in the GST report files | Medium | Fixed |
| A-12 | Key delegation (F29) did not require the cancel power for keys that can cancel | Low | Fixed |
| A-13 | The sum of dispute holds on a payment can exceed what was received | Low | Fixed (capped holds) |
| A-14 | Open redirect through the dashboard's `next` (`/dashboard/../\evil`) | Low | Fixed |
| A-15 | Redactor gaps: non-Bearer `Authorization`, chained labels, `key`/`sig`/`auth`/`pass`/`totp` names | Low | Fixed |
| A-16 | The custom-domain activation check had its own, weaker private-address guard | Low | Fixed |
| A-17 | New SQLite files and GST report files were group/world-readable under umask 022 | Low | Fixed (new files) |
| A-18 | Directory listings under `/assets/` | Low | Fixed |
| A-19 | Whop: a `whsec_` secret with no key material gave an empty HMAC key | Low | Fixed |

### A-1: a subscription or mandate return through another account's URL is applied

- **Severity:** High. It would be Critical with several orgs, but today the
  store creates only `org_default`. **Confidence:** high. **Status:** Fixed.
- **Where:** `internal/subscriptions/notify.go`, `ApplyReturn` (fix at line
  712). The mandate or subscription is looked up by the ref in the URL alone
  (`GetMandateByRef`, `GetSubscriptionByProviderRef` have no account filter).
  The connector came from the account in the URL path, `/return/{pa}/{ref}`.
- **Preconditions:** the attacker knows their own mandate or subscription ref
  (it is in the return URL they are sent to). They also need a second provider
  account on the hub whose connector reports that ref as active. Examples: a
  remote connector they control, an account they administer, or a connector
  that answers for unknown ids.
- **Attack:** the attacker opens `/return/{pa_X}/{ref}`. `pa_X`'s connector
  says "active". The mandate becomes active with a provider mandate id of the
  attacker's choosing, the subscription goes active, and its first payment is
  booked as succeeded with an invoice. No money moved.
- **Root cause:** the object's own provider account was never compared with
  the account in the URL.
- **Fix:** if the mandate's or subscription's `ProviderAccountID` is not the
  URL's account, the return is treated as not a subscription return. It falls
  through to the payment-return path, which is scoped to the account.
- **Regression test:** `TestAuditPayMandateReturnThroughAnotherAccountIsNotApplied`
  (`internal/subscriptions/audit_pay_returnscope_test.go`).
- **Residual risk:** the test environment shares one fake responder between
  accounts. It demonstrates the account confusion, not a full cross-org
  set-up.

### A-2: a success with no amount was booked as the full charge

- **Severity:** High. **Confidence:** high. **Status:** Fixed.
- **Where:** `internal/subscriptions/renewals.go`, `toSnapshot` (line 578).
  It is used for setup payments, renewal charges, renewal polls and
  provider-managed renewals.
- **Attack:** combined with A-1, or with any connector that reports a success
  without an amount. The missing amount and currency were replaced with the
  amounts we expected. That bypassed both core safety nets: "captured needs an
  amount", and "a different amount goes to `needs_review`".
- **Fix:** the provider's amount and currency are passed through unchanged.
  A success without an amount is now refused by the core instead of being
  booked as paid in full.
- **Regression test:** `TestAuditPaySuccessWithoutAmountIsNotTheExpectedCharge`
  (`internal/subscriptions/audit_pay_noamount_test.go`). The A-1 test also
  covers it, through a setup payment with no amount.
- **Residual risk:** if a real provider omits the amount on a genuine
  success, that charge now stays pending and is retried (it fails safe).
  PayU and Zoho build these snapshots from the provider's own amount fields.
  The other providers need sandbox confirmation.

### A-3: the core ownership check failed open

- **Severity:** High. **Confidence:** high. **Status:** Fixed.
- **Where:** `internal/checkout/checkout.go`. `syncAttempt` (line 328) and
  the new `hintedSnapshotIsOurs` (line 363). `snapshotIsOurs` rejected a
  snapshot only when both sides named a reference or order and they
  differed.
- **Attack:** the customer starts a checkout, does not pay, and calls the
  return URL with `payment_id=<P>`. `P` is any paid payment of the same
  amount on the same provider account that names no reference and no order.
  The fetched snapshot "did not contradict" the attempt, so it was applied.
  `provider_payment_id` is not unique per account, so the same `P` could
  settle any number of payments. A later refund of the forged payment refunds
  somebody else's provider payment.
- **Fix:** when an id comes from outside (the attempt did not already hold
  that payment id or order id), the snapshot must positively name the
  attempt's own stored reference or order. Its payment id must also not be
  bound to another attempt of the account.
- **Regression tests:** `TestAuditPayCoreAppliesForeignSnapshotThatNamesNoReferenceOrOrder`
  and `TestAuditPayOneProviderPaymentSettlesTwoPayments`
  (`internal/checkout/audit_pay_ownership_test.go`).
- **Residual risk:**
  - A connector whose fetch-by-id snapshot names neither our reference nor
    our order will now have its returns ignored; the payment settles on the
    next poll by reference. Verify each provider in its sandbox.
  - There is still no unique index on
    `(provider_account_id, provider_payment_id)`, because duplicate successes
    (F31) are legitimately recorded. A partial unique index over
    non-duplicate captured attempts would close the race between two
    concurrent returns.

### A-4: Zoho accepted a payment that names no session and no reference

- **Severity:** High, conditional. **Confidence:** medium, because whether
  real Zoho payments can lack both fields is unverified. **Status:** Fixed.
- **Where:** `connectors/zoho/zoho.go`, `FetchPayment` (line 498). Both
  checks had the form `got != "" && … && got != ref`, so a missing value
  counted as a match. The connector then stamped our session id onto the
  foreign payment, which also defeated A-3's check.
- **Fix:** when the caller names a reference or session, the payment must
  carry a matching one.
- **Regression test:** `TestAuditPayZohoFetchByIDAcceptsPaymentWithNoSessionOrReference`.

### A-5: the idempotency key was released after the request had taken effect

- **Severity:** High: a duplicate payout is possible. **Confidence:** high.
  **Status:** Fixed.
- **Where:** `internal/httpapi/idempotency.go`, `(*API).idempotent` (lines
  65 to 95).
- **Attack:**
  1. The handler commits a payout.
  2. The client hangs up.
  3. `CompleteIdempotencyKey` ran on the request's context, which is now
     cancelled, so it failed. Any database error at that moment had the same
     effect.
  4. The deferred release deleted the reservation.
  5. The client retries with the same key and the payout is created again.
- **Fix:** the response is stored on a context the client cannot cancel. Once
  the handler has returned a status below 500, the key is never released. If
  storing still fails, the reservation stays in progress and a retry gets 409.
- **Regression test:** `TestAuditLedgerIdempotencyClientGoneAfterCommitRunsTwice`.
- **Residual risk (H-1):** an in-progress reservation older than 60 s
  (`idempotencyStale`) is taken over by a retry. The full fix is to store the
  idempotency key on the money object itself, e.g.
  `UNIQUE(app_id, mode, idempotency_key)` on payouts and refunds.

### A-6: live accounts accepted sandbox credentials

- **Severity:** Medium. It needs a mistaken or malicious user with
  `provider_account.manage`. **Status:** Fixed.
- **Where:** `internal/connectors/connectors.go`, the new
  `checkCredentialMode` (line 563), called from `CreateAccount` and
  `UpdateAccount`.
- **Effect:** a live Paymux payment could be "paid" with a public test card
  through a live account that held sandbox credentials. In the other
  direction, a test-mode API key could move real money through a test account
  that held live keys.
- **Fix:** a live account refuses `test_mode` or `sandbox` set to true, and
  `sk_test_`, `rk_test_` and `rzp_test_` keys. A test account refuses
  `sk_live_`, `rk_live_` and `rzp_live_` keys. A sandbox switch left off on a
  test account is ambiguous and stays allowed. Two dashboard test fixtures
  that paired a live account with `test_mode: true` were corrected.
- **Regression test:** `TestAuditPayLiveAccountRefusesSandboxCredentials`.
- **Residual risk:**
  - Existing accounts are not re-checked. Review them by hand.
  - Stripe events' `livemode` field is not checked.

### A-7: a pending refund was sent after a dispute opened

- **Severity:** Medium: the customer is paid twice. **Status:** Fixed.
- **Where:** `internal/checkout/jobs.go`, `SubmitRefund` (line 250).
- **Fix:** the refund job reloads the payment. If the payment is disputed or
  its dispute was lost, the refund fails without being sent.
- **Regression test:** `TestAuditLedgerPendingRefundIsNotSentOnceThePaymentIsDisputed`.
- **Also (R-A7):** when a dispute opens, `disputes.upsert` fails the payment's
  pending refunds whose `refund.submit` job has never been claimed, and cancels
  that job, in the same transaction (`core.FailUnsentRefunds`,
  `store.CancelUnclaimedJob`), so nothing is sent. A refund whose job has been
  tried, is running or is done is left pending: it may be with the provider,
  failing it is final, and the provider's answer settles it. Regression tests:
  `TestDisputeOpeningFailsARefundWhoseSubmitJobNeverRan` and
  `TestDisputeOpeningLeavesARefundThatMayBeWithTheProvider`.
- **Residual risk:** a dispute that arrives once the job has been claimed (the
  provider call itself) is not caught beyond the submit-time check; a refund of
  a payment disputed in that window can still reach the provider.

### A-8: the first dispute to end cleared the flag

- **Severity:** Medium. **Status:** Fixed.
- **Where:** `internal/disputes/disputes.go`, `transition` (line 278). The new
  `store.CountOtherOpenDisputes` is in `internal/store/disputes.go` (line
  111).
- **Fix:** the `disputed` flag is dropped only when no other dispute on the
  payment is still open.
- **Regression test:** `TestAuditLedgerSecondOpenDisputeStillBlocksRefund`.

### A-9: sensitive dashboard actions skipped the recent sign-in check

- **Severity:** Medium. **Status:** Fixed.
- **Where:**
  - `internal/dashboard/dashboard.go`, lines 232 and 236 to 238: user disable
    and roles.
  - `internal/dashboard/account.go`, line 285: user sign-out-everywhere.
  - `internal/dashboard/apps.go`, line 110: key revoke.
  - `internal/dashboard/providers.go`, line 78: provider-page app linking.
- **Attack:** someone holding a stolen "remember me" admin cookie (valid 30
  to 90 days) could do the following without giving a password and code:
  - give a custom role `payout.create` and `api_key.create`;
  - disable and sign out the other admins;
  - revoke the live keys;
  - re-point provider accounts.

  F52 documents these actions as protected.
- **Fix:** these routes are wrapped in `d.recent`, like their siblings.
- **Regression tests:** `internal/dashboard/audit_authz_reauth_test.go` (3
  tests).
- **Residual risk:** `endpointCreate`, `domainAdd` and organisation settings
  still do not need a recent sign-in (see H-3).

### A-10: stderr carried secrets that the database log redacts

- **Severity:** Medium. **Status:** Fixed.
- **Where:**
  - `internal/logging/logging.go`, `replaceAttr` (line 40).
  - `internal/dblog/handler.go`: `Handle`, `WithAttrs` and the new
    `scrubbed`/`scrubAttrs`.
- **Effect:** every deployment keeps stderr (journald, Docker logs). The full
  redactor ran only on the database copy. Messages, error text, `Bearer …`,
  `whsec_…`, keys such as `salt` or `private_key`, structured values and the
  configured provider secrets all reached stderr in the clear.
- **Fix:**
  - stderr now applies the shared redactor's credential names and value
    patterns, after its own PII masking.
  - The database log handler hands stderr a copy of each record with the
    known secret values replaced.
  - Two existing tests now accept the stronger URL user-info redaction
    (`[redacted]@h` instead of `u:xxxxx@h`).
- **Regression tests:** `TestAuditStderrLoggerLeaks` and
  `TestAuditKnownSecretsAreScrubbedOnStderr`.
- **Residual risk:** OTLP export was not re-examined, and neither were
  secrets wrapped in base64 or hex of an unknown format.

### A-11: CSV formula injection in GST reports

- **Severity:** Medium. **Status:** Fixed.
- **Where:** `internal/invoice/reports.go`, `csvTable.file` and the new
  `csvCell` (lines 72 to 100).
- **Source of the data:** the buyer's name is typed on a hosted billing form
  and goes into `sales_register.csv` and the b2b, export and credit-note
  files.
- **Fix:** a text cell that starts with `= + - @`, a tab or a carriage return
  is prefixed with `'`. Plain numbers, including negative amounts, are left
  as they are. The files are now written 0600 in a 0700 directory.
- **Regression test:** `TestAuditWebSalesRegisterNeutralisesFormulas`.
- **Not affected:** `internal/dashboard/revenue.go` writes labels and numbers
  only.

### A-12: key delegation did not include the cancel power

- **Severity:** Low. **Status:** Fixed.
- **Where:** `internal/authz/authz.go`, `ActionsForScopes` (lines 182 and 193
  to 195).
- **Fix:** a full-access key, or a `payments:write` or `subscriptions:write`
  key, now also requires its issuer to hold `payment.cancel`.
  `TestActionsForScopes` was updated.
- **Regression test:** `TestAuditAuthzKeyScopesNeedCancelPower`.

### A-13: dispute holds can add up to more than was received (fixed)

- **Severity:** Low. Only provider-supplied data can cause it. **Status:**
  Fixed. A dispute keeps the provider's amount; its hold (`amount_held`) is
  capped at what the attempt has left after its other open disputes, release
  and loss move the held amount, and the payment gets the `dispute_overlap`
  flag and an ops alert. A dispute that is lost takes what is left of the
  attempt after the disputes still open and those already lost; if that is
  more than it held (an earlier dispute ended in our favour), its hold is
  topped up first, so release and loss still net per dispute and the ledger
  never goes below minus the amount received.
- **Where:** `internal/disputes/disputes.go`, `upsert`. F11 checks each
  dispute against the amount received, not the sum of the open holds.
- **Why it is not fixed:** refusing the second dispute would drop a real
  chargeback. Capping its hold needs the matching release and loss entries to
  be capped too, a change to ledger semantics that needs a design decision.
- **Test:** `TestAuditLedgerDisputeHoldsCannotExceedReceived`.
- **Recommended fix:** record the dispute, cap its hold at what remains, store
  the capped amount for its release, and alert.

### A-14 to A-19: hardening fixes

| ID | Where (fixed tree) | Fix | Test |
|----|--------------------|-----|------|
| A-14 | `internal/dashboard/mode.go` `safeNext` (line 43) | The path is checked after `path.Clean`; backslashes and user-info are refused. | `TestAuditWebSafeNextStaysOnSite` |
| A-15 | `internal/redact/redact.go` (lines 255, 267, 410) and `names.go` (lines 43, 78) | The rest of an `Authorization:` line is redacted whatever the scheme. Credential labels that follow another label are caught. `pass`, `totp`, `mfa` and `sig` are credential words, and bare `key` and `auth` are credential names (`auth_payu_id` and `key_id` stay readable). | `TestAuditRedactLeaks`, `TestAuditRedactChainedLabel` |
| A-16 | `internal/housekeeping/housekeeping.go` `DefaultFetch` (line 169) | Uses `connector.IsPrivateIP`, which also blocks CGNAT, NAT64, 198.18/15 and 240/4, so all three outbound guards agree. | `TestAuditDomainFetchGuardRanges` |
| A-17 | `internal/store/open_sqlite.go` `openSQLiteFile` (line 42) | A new database file is created 0600; SQLite gives its `-wal` and `-shm` files the same mode. An existing file keeps its mode. | `TestAuditSQLiteFileMode` |
| A-18 | `internal/web/web.go` `AssetHandler` (line 176) | Directories answer 404. | `TestAuditWebAssetsNoDirectoryListing` |
| A-19 | `connectors/whop/whop.go` `New` (line 95) | A secret that yields no key material is refused. | `TestAuditPayWhopSecretThatDecodesToEmptyKeyIsRefused` |

## Previously fixed issues revalidated

| Prior ID | Result |
|----------|--------|
| F1, F2 (Whop and Razorpay outside payment ids) | Holds |
| F3 (core defence in depth) | **Did not hold**: it failed open (A-3) |
| F6 (Zoho outside payment id) | **Incomplete** (A-4) |
| F7, F43 (refund ownership) | Holds |
| F9 (no refund on a disputed payment) | Holds at creation; gaps at submission (A-7) and with two disputes (A-8) |
| F10 (payout timeouts are not failures) | Holds |
| F11 (dispute amount and currency) | Holds per dispute; the sum is unchecked (A-13) |
| F12 (framing, nosniff on hosted pages) | Holds |
| F29 (keys cannot exceed the issuer) | **Incomplete** (A-12) |
| F31 (duplicate success flagged) | Holds; see A-3's residual risk |
| F33 (single-use links) | The claim and spend are conditional updates; a second success is flagged |
| F36, F41 (OIDC binding, enrolment codes) | Holds |
| F38 (payouts need an Idempotency-Key) | Holds; see A-5 |
| F51 (actions pinned to SHAs) | Holds |
| F52 (recent sign-in for sensitive actions) | **Incomplete** (A-9) |
| "Two outbound guards that disagreed" | A third guard was missed (A-16) |

The following were checked and found sound:

- **Refund over-refund under concurrency:**
  `TestAuditLedgerConcurrentRefundsNeverOverRefund` passes. It runs 16
  concurrent refunds, uses a payment row lock (`FOR UPDATE` on PostgreSQL)
  and a single SQLite writer.
- **Outbound webhook SSRF guard:** it is checked at dial time, so DNS
  rebinding is closed, and it does not follow redirects or proxies.
  `TestAuditOutboundGuard` passes over 12 address forms.
- **Webhook signature schemes:** all seven connectors use raw bytes and a
  constant-time compare, with timestamp windows where the provider supports
  them.
- **API-key scoping:** object-level scoping by app and mode holds on every
  `/v1` resource family.
- **Encryption at rest:** AES-256-GCM with a random nonce and AAD bound to
  the table, column and row.
- **Templates:** HTML is rendered with `html/template`.
- **SQL:** parameterised SQL throughout.
- **CSRF:** on every dashboard POST.
- **Hardening:** the systemd unit and the distroless non-root image.

## Potential risks requiring further verification

These are not demonstrated and not fixed. Each needs a decision or a sandbox
check.

| ID | Risk | Where | Suggested action |
|----|------|-------|------------------|
| H-1 | A request running longer than 60 s loses its idempotency reservation to a retry. | `internal/httpapi/idempotency.go` `idempotencyStale` | Store the idempotency key on payouts and refunds under a UNIQUE constraint, or heartbeat the reservation. |
| H-2 | A payout whose submit job dies after repeated ambiguous errors stays `pending` with no explicit "unknown" state; a merchant may re-send it under a new key. | `internal/payouts/payouts.go` | Add a `needs_review` state, alert, and poll by reference. |
| H-3 | The grantable `settings.write` sets session lifetimes (up to 90 days) and `einvoice_irp_keys` (the IRP signature trust anchor, F22), with no recent sign-in. | `internal/settings/defs.go` | Make these admin-only behind `d.recent`. |
| H-4 | The default trusted proxies are every private range; behind Docker port publishing, any client's `X-Forwarded-For` is believed. | `internal/config/config.go` `localNetworks` | Default to none, and document the proxy CIDR. |
| H-5 | Provider-managed renewals are booked from the signed webhook body, not fetched. | `internal/subscriptions/notify.go` `recordProviderRenewal` | Fetch the payment and check its subscription id. |
| H-6 | A single-use variable-amount link can get two payable payments when the first is `processing`. | `internal/paylinks/paylinks.go` `reuseOpenPayment` | Do not create a new payment when the cancel fails. |
| H-7 | Hosted payment pages, whose URLs carry tokens, send no script CSP. | `internal/paypage`, `internal/paylinks`, `internal/subscriptions` | A nonce-based CSP with the provider SDK hosts. |
| H-8 | `/debug/pprof` and `/metrics` have no authentication; the runbook suggests binding the internal listener to `0.0.0.0` on a Docker network. | `internal/server/server.go`, `docs/runbook.md` | Warn on, or refuse, a non-loopback internal listener without an explicit flag. |
| H-9 | PostgreSQL TLS is not enforced, and the runbook's `sslmode=require` does not verify the certificate. | `internal/config/config.go` | Warn for a non-loopback host without `verify-full`. |
| H-10 | `PAYMUX_ALLOW_PRIVATE_WEBHOOKS` is accepted in production and also opens JS hook `fetch`. | `pkg/hub/js.go` | Refuse it outside development, or warn. |
| H-11 | `paymux update` checks only `SHA256SUMS` from the same release unless cosign flags are given. | CLI self-update | Make the signature check the default. |
| H-12 | Org checks are dead code for dashboard users (`OrgID` is never set); harmless with one org. | `internal/dashboard/dashboard.go` `actor` | Fix before multi-org. |
| H-13 | Ledger invariants are application-only: no `CHECK (amount > 0)`, no `UNIQUE` on ledger entries per refund or dispute and kind, and no immutability trigger. The ledger is single-entry with signed amounts. | `internal/migrations/*/0002_core.sql` | Add constraints in a migration once the existing data has been checked. |
| H-14 | The dashboard refund form has no idempotency; a double submit can make two partial refunds, each within the balance. | dashboard | Add a form nonce. |
| H-15 | `http.TimeoutHandler` buffers whole responses in memory, including large exports. | `internal/server/middleware.go` | Stream exports outside it. |
| H-16 | Supply chain: Docker base images are not pinned by digest; `actions/checkout` keeps credentials in the release job while `bun install` runs install scripts; no release environment protection; no SLSA provenance; no Dependabot; no gosec or secret scanning; fuzz targets run their seed corpus only. | `deploy/docker/Dockerfile`, `.github/workflows/*` | Pin digests, `persist-credentials: false`, a protected environment, attestations. |

## Code changes

All changes are uncommitted on branch `security-audit-2026-10`, in the
worktree `/home/user/code/payments-audit`.

**Source files changed:**

- `connectors/whop/whop.go`, `connectors/zoho/zoho.go`
- `internal/authz/authz.go`
- `internal/checkout/checkout.go`, `internal/checkout/jobs.go`
- `internal/connectors/connectors.go`
- `internal/dashboard/{account,apps,dashboard,mode,providers}.go`
- `internal/dblog/handler.go`, `internal/logging/logging.go`
- `internal/disputes/disputes.go`, `internal/store/disputes.go`
- `internal/housekeeping/housekeeping.go`
- `internal/httpapi/idempotency.go`
- `internal/invoice/reports.go`
- `internal/redact/{names,redact}.go`
- `internal/store/open_sqlite.go`
- `internal/subscriptions/{notify,renewals}.go`
- `internal/web/web.go`

**Existing tests adjusted:** the expectations moved toward stricter
behaviour; nothing was relaxed.

- `internal/authz/scopes_actions_test.go`
- `internal/dashboard/providers_test.go`
- `internal/logging/logging_test.go`
- `pkg/hub/paymux_test.go`

**New tests:** 21 `audit_*_test.go` files.

**Manual steps:**

1. Review existing live provider accounts for sandbox credentials (A-6).
2. `chmod 600` existing database files if the data directory was shared
   (A-17).
3. Verify each provider in its sandbox: returns with a `payment_id` still
   settle, because the payment names our reference or order (A-3); and
   renewal successes carry an amount (A-2).

## Tests executed and results

Go 1.27.3 on linux/amd64. PostgreSQL 17 ran in a throwaway local container.

**Baseline**, before any change, at `677e9c0` in a clean checkout:

| Command | Result |
|---------|--------|
| `go build ./...` | OK |
| `go vet ./...` | OK |
| `go test -race -timeout 30m ./...` (SQLite) | exit 0: 79 packages pass, 10 have no tests |
| `PAYMUX_REQUIRE_PG=1 go test -race ./...` (PostgreSQL) | exit 0: 79 packages pass |

**Red check.** Every `audit_*_test.go` file was copied onto the baseline and
run there. All confirming tests fail. The exceptions are expected:
`TestAuditLedgerConcurrentRefundsNeverOverRefund` and `TestAuditOutboundGuard`
are regression guards that pass on both trees, and A-13's test now passes on the fixed tree.

**After the fixes**, the Go steps of `scripts/ci.sh`:

| Command | Result |
|---------|--------|
| gofmt (tracked and new files) | clean |
| `go vet ./...` and `go vet -tags loadtest ./...` | OK |
| `go run ./scripts/apisnap -check` | OK: the exported API is unchanged |
| `go run ./scripts/capmatrix -check` | OK |
| staticcheck v0.8.2 | OK |
| govulncheck v1.9.0 | "No vulnerabilities found" |
| `go test -race -timeout 30m ./...` (SQLite) | exit 0 |
| `go test ./internal/redact -run TestHostileInputIsLinear` | OK: the new label pass stays linear |
| `PAYMUX_REQUIRE_PG=1 go test -race ./...` (PostgreSQL) | One failure, `pkg/hub` `TestStartupLogRedactsPassword` (see below) |
| `PAYMUX_SMOKE_SQLITE_ONLY=1 bash scripts/smoke.sh` | "SMOKE OK" |

About the PostgreSQL failure:

- The test expected the old URL redaction (`u:xxxxx@`). The password itself
  was not leaked.
- The assertion was changed to `://[redacted]@`. `pkg/hub` was then re-run
  with `-race` and passed on both databases (PostgreSQL 77 s, SQLite 53 s).
- That run also showed a cosmetic effect of A-10. The test database's
  password is `paymux`, and known secret values are now removed from stderr
  as well, so the word "paymux" in a message is replaced there too. The
  database log copy already behaved this way. Real credentials are not
  common words.

**Not run:**

- the frontend and browser steps of `scripts/ci.sh` (no frontend code
  changed);
- the PostgreSQL leg of the smoke test;
- fuzzing beyond the seed corpora;
- anything against a provider sandbox.

Every open item above (A-13, H-1 to H-16, and the residual risks of the fixes)
has a step-by-step plan in the [remediation plan](security-remediation-plan.md).

## Production readiness assessment

Not established by this audit. The confirmed critical-path issues found here
are fixed. Before live money:

- the provider sandbox checks above;
- decisions on H-1 to H-3 and H-13;
- the supply-chain items in H-16;
- an independent review of the connectors' real responses, which this audit
  could only simulate.

Passing tests show that the specific attacks above no longer work, not that
the system is secure.
