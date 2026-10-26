# Security remediation plan

This plan covers what the [October 2026 audit](security-audit.md) left open:

- the one confirmed finding that is not fixed (A-13);
- every hypothesis (H-1 to H-16);
- the residual risks left by the fixes that were made.

Each item lists its goal, the change, the tests that prove it, and its
compatibility impact.

Each item becomes its own pull request, with a failing test first and the fix
after it, like the audit's own fixes. Effort is S (under a day), M (one to
three days) or L (more than three days).

## Order

| Wave | Items | Why first |
|------|-------|-----------|
| 1: money and duplicates | R-A5 / H-1, R-A3, H-2, A-13, H-14 | Duplicate payouts or refunds, and money held or booked wrongly |
| 2: privilege and trust | H-3, R-A9, H-4, R-A6, H-5, H-6 | Escalation by a custom role, spoofed client addresses, unverified bookings |
| 3: exposure | H-7, H-8, H-9, H-10, R-A10, R-A17 | Hardening of surfaces that are exposed only under some configurations |
| 4: platform | H-13, H-11, H-16, H-15, H-12, R-A7 | Database invariants, supply chain, operations |
| 5: provider sandboxes | V-1 | Confirms that A-2 and A-3 do not stall genuine payments |
| 6: re-audit of 26 October | B-7a, B-7b, R-B1, B-7c, B-8, B-9 | What the [re-audit](security-audit.md#re-audit-26-october-2026-commit-aa573bd) left open |

## Wave 1: money and duplicates

### R-A5 / H-1: the idempotency key lives on the money object (M)

- **Goal:** one Idempotency-Key gives at most one payout or refund, however
  long the first request runs and whatever happens to its response.
- **Change:**
  1. Migration (SQLite and PostgreSQL): add a nullable `idempotency_key`
     column to `payouts` and `refunds`, with
     `UNIQUE (app_id, mode, idempotency_key)`.
  2. `internal/httpapi`: pass the key into `payouts.Create` and
     `core.CreateRefund` through their input structs.
  3. Insert the money object with the key, in the same transaction as the
     rest of its write.
  4. On a unique violation, load the existing object and answer with it, as
     a replay would.
  5. Keep the middleware reservation as a fast path. A takeover after
     `idempotencyStale` can then no longer create a second object.
- **Tests:**
  - A handler that blocks past `idempotencyStale`, then a retry: exactly one
    payout.
  - The same with a refund.
  - Two concurrent first requests on PostgreSQL: one object.
- **Compatibility:** additive. Old rows have a NULL key, and NULLs do not
  collide under the unique constraint.

### R-A3: one provider payment settles one attempt (S)

- **Goal:** two concurrent returns cannot bind the same provider payment to
  two attempts, the race that A-3's read check leaves.
- **Change:** a partial unique index on
  `payment_attempts (provider_account_id, provider_payment_id)` over
  non-duplicate captured attempts:
  - PostgreSQL: `WHERE provider_payment_id IS NOT NULL AND duplicate = false`.
  - SQLite: the same condition as a partial index.

  `ApplyAttemptSnapshot` maps the violation to "not ours" and logs it.
  Duplicate successes (F31) are written with `duplicate = true`, so they
  stay allowed.
- **Pre-check:** a query in the migration's notes that lists existing
  violations. The migration must fail loudly, with that list, rather than
  drop rows.
- **Tests:** two goroutines apply the same hinted snapshot to two attempts;
  one of them wins.
- **Compatibility:** a data check is needed before upgrading. Document it in
  `docs/upgrades.md`.

### H-2: a payout with an unknown outcome is visible and resolvable (M)

- **Goal:** a payout whose submit job dies after repeated ambiguous errors
  never sits silently in `pending`.
- **Change:**
  1. Add the state `needs_review` to the payout state machine
     (`internal/core/state`).
  2. When the job reaches its attempt limit after ambiguous errors, move the
     payout there and raise an ops alert.
  3. `handlePoll` also polls `needs_review` payouts by `ProviderRef`, and a
     provider answer moves them on.
  4. Re-sending is refused while a payout to the same beneficiary and amount
     is in `needs_review`. Allow it with an explicit `force` that is audited.
  5. A payout we failed from a synthesised provider error is not terminal
     until the provider confirms it. Today a later "processed" is ignored
     (`payouts.go:509`).
- **Tests:**
  - The job dies, the payout is in `needs_review` with an alert, a poll
    answers "processed", and the payout succeeds once with one ledger entry.
  - A synthesised failure followed by a provider "processed" is applied.
- **Compatibility:** a new status value in the API and on the dashboard;
  update `docs/openapi.yaml`.

### A-13: dispute holds never exceed what was received (M)

- **Goal:** the sum of open holds on a payment is at most `AmountReceived`,
  and no provider dispute is ever dropped.
- **Change:**
  1. In `disputes.upsert`, compute `open = SUM(amount_held)` of the
     payment's other open disputes, under the payment lock that is already
     held.
  2. Store the dispute with its provider amount, and with
     `amount_held = min(snap.Amount, AmountReceived - open)` in a new column.
  3. Book `dispute_hold` for `-amount_held`.
  4. `dispute_release` and `dispute_loss` use `amount_held`, so the ledger
     always nets to zero.
  5. When the hold was capped, add the flag `dispute_overlap` and an ops
     alert.
- **Tests:**
  - Un-skip `TestAuditLedgerDisputeHoldsCannotExceedReceived`.
  - Add a test that two disputes, one won and one lost, leave the ledger at
    `-AmountReceived` at most.
- **Compatibility:** a migration adds `amount_held`, backfilled with
  `amount`.

### H-14: the dashboard refund form is idempotent (S)

- **Change:** the refund form carries a single-use nonce: an HMAC of the
  session, the payment and a random value. It is used as the Idempotency-Key
  of the refund that R-A5 adds, so a double submit replays the first refund.
- **Tests:** two POSTs with the same nonce give one refund.

## Wave 2: privilege and trust

### H-3: sign-in policy and trust anchors are admin settings (S)

- **Change:**
  1. Add `adminOnly: true` in `internal/settings/defs.go` for
     `session_idle_hours`, `session_max_days`, `session_remember_days` and
     `einvoice_irp_keys`.
  2. `orgSettingSet` checks `authz.SettingsAdmin`, a new action that custom
     roles cannot be granted, like `user.manage`. It also wraps those keys in
     `d.recent`.
- **Tests:** a custom role holding `settings.write` gets 403 on those keys
  and can still change ordinary settings.

### R-A9: the remaining sensitive dashboard routes need a recent sign-in (S)

- **Change:** wrap `endpointCreate`, `endpointUpdate`, `domainAdd` and the
  organisation settings writes in `d.recent`.
- **Tests:** extend `audit_authz_reauth_test.go` with these routes.

### H-4: trusted proxies default to none (S)

- **Change:**
  1. Change the default of `PAYMUX_TRUSTED_PROXIES` from "the local
     networks" to empty.
  2. When the hub runs behind a proxy (`PAYMUX_BASE_URL` is https and
     requests arrive over plain http) with an empty list, log a startup
     warning that names the variable.
  3. Update `docs/configuration.md`, and add a release-note entry in
     `docs/upgrades.md`, because per-IP limits change for deployments that
     relied on the default.
- **Tests:** with an empty list, `X-Forwarded-For` from 172.17.0.1 is
  ignored; with the proxy listed, it is believed.

### R-A6: provider account mode, part two (S)

- **Change:**
  1. Add a startup and `paymux doctor` check that runs `checkCredentialMode`
     over the existing accounts and lists any that do not match. It warns and
     does not refuse, so a running hub is not broken.
  2. The Stripe connector refuses an event whose `livemode` does not match
     the account's mode.
- **Tests:** a mismatched existing account is reported; a live account
  refuses a `livemode: false` event.

### H-5: provider-managed renewals are fetched before they are booked (M)

- **Change:** `recordProviderRenewal` fetches the payment by its id from the
  provider, checks that its subscription id and amount match the
  subscription, and books what was fetched. Webhook body fields become hints,
  per F34.
- **Tests:** a webhook whose body amount differs from the fetched payment
  books the fetched amount; one whose subscription id belongs to another
  subscription books nothing.

### H-6: a single-use variable-amount link has one payable payment (S)

- **Change:** in `reuseOpenPayment`, when cancelling the previous open
  payment fails (it is `processing`), return that payment instead of creating
  a new one.
- **Tests:** a first payment in `processing` plus a request for another
  amount creates no second payment.

## Wave 3: exposure

### H-7: script CSP on the hosted payment pages (M)

- **Change:**
  1. Generate a nonce per response in `internal/paypage`,
     `internal/paylinks` and `internal/subscriptions` (hosted pages).
  2. Send `Content-Security-Policy: default-src 'self'; script-src 'self'
     'nonce-…' <provider SDK hosts>; frame-src <provider hosts>;
     form-action 'self' <provider hosts>; base-uri 'none';
     frame-ancestors 'none'`.
  3. Each connector declares its SDK and form hosts in `connector.Info`.
  4. The inline scripts in `pay_sdk.html` and `pay_form_post.html` take the
     nonce.
- **Tests:**
  - A header test per page.
  - Per connector, a browser test (Playwright, existing harness) that
    checkout still loads under the policy.
- **Risk:** a missing provider host breaks checkout, so roll out in
  report-only mode (`Content-Security-Policy-Report-Only`) for a release
  first.

### H-8: the internal listener stays private (S)

- **Change:**
  1. When `PAYMUX_INTERNAL_LISTEN` is not loopback and
     `PAYMUX_INTERNAL_PUBLIC=true` is not set, refuse to start in production
     and warn in development.
  2. Correct `docs/runbook.md:82` to suggest a shared network namespace or
     `127.0.0.1` port publishing.
- **Tests:** a config test for each case.

### H-9: PostgreSQL TLS (S)

- **Change:** at startup, warn when the database host is not loopback and
  `sslmode` is missing or one of `disable`, `allow`, `prefer` or `require`.
  `docs/runbook.md` and `docs/configuration.md` recommend `verify-full`.
- **Tests:** a config test of the warning.

### H-10: private webhook destinations are a development switch (S)

- **Change:** refuse `PAYMUX_ALLOW_PRIVATE_WEBHOOKS` unless
  `PAYMUX_ENV=development`, as is done for `PAYMUX_DEV_SKIP_2FA`.
- **Tests:** `hub.New` refuses it in production.
- **Compatibility:** release-note entry.

### R-A10: OTLP export and encoded secrets (S)

- **Change:** run the OTLP shipper's records through the same redaction as
  the database row (`dblog` `row()`), and add tests that a known secret does
  not reach the OTLP payload. Decide whether to scan base64 and hex blobs
  longer than 32 characters for known values. That is costly, so measure it
  with `TestHostileInputIsLinear`.
- **Done:** the shipper takes `OTLPOptions.Redact`, wired to the redactor's
  `Scrub`, and applies it to every text field as the batch is built, so a secret
  that becomes known while a row waits in the queue is still removed.
  `TestAuditOTLPPayloadNeverCarriesAKnownSecret` covers the plain, base64 and hex
  forms of a known secret. **Decision:** no blind scan of long base64 or hex
  blobs. The redactor already matches the base64, hex and URL-escaped forms of
  every known secret, which is the case that matters; a scan for unknown values
  would redact ids, hashes and signatures that operators need in the log.

### R-A17: existing database files and the Docker image (S)

- **Change:**
  1. At startup, warn when the database file, `-wal` or `logs.db` is
     readable by group or others, naming `chmod 600`. It warns rather than
     changing the file, because a backup agent may rely on the mode.
  2. The Dockerfile creates `/data` with mode 0700, owned by 65532.
- **Tests:** a warning test under umask 022.

## Wave 4: platform

### H-13: ledger invariants in the database (L)

- **Change:**
  1. Migrations add `CHECK (amount > 0)` on `payments`, `refunds` and
     `payouts`.
  2. They add `CHECK (amount_refunded >= 0 AND amount_refunded <=
     amount_received)` on `payment_attempts`.
  3. They add `UNIQUE (refund_id, kind)` and `UNIQUE (dispute_id, kind)` on
     `ledger_entries`, where those columns are not null.
  4. They add an `attempt_id` column to `ledger_entries`, so that a
     duplicate charge can be told apart from the sale's.
  5. On PostgreSQL, a trigger refuses UPDATE and DELETE on `ledger_entries`.
     On SQLite, the same refusal as `BEFORE UPDATE` and `BEFORE DELETE`
     triggers.
  6. `LedgerTotals` excludes duplicate charges and their refunds, or reports
     them in their own line (F31 residual).
  7. Settlement fees are booked in the attempt's currency, or refused when
     the currencies differ (`reconcile/settlements.go:90`).
- **Pre-check:** every constraint ships with a query that lists the rows
  that violate it. The migration fails, rather than rewrites, when any
  exist.
- **Tests:** each constraint refuses the bad write on both databases. Run the
  whole suite under the triggers (no code path updates ledger rows today, so
  it should pass unchanged).
- **Compatibility:** a data check before upgrading.

### H-11: self-update verifies the signature by default (S)

- **Change:** `paymux update` verifies the cosign signature of `SHA256SUMS`
  against the release workflow's identity, built into the binary, unless
  `--insecure-skip-signature` is given.
- **Tests:** a signed fixture passes; an unsigned or wrongly signed one is
  refused.

### H-16: supply chain (M)

1. Pin the Dockerfile base images and the CI `postgres` service by
   `@sha256:` digest.
2. Add `persist-credentials: false` to every `actions/checkout`. In
   `release.yml`, run `bun install --ignore-scripts`, or install in a job
   without the write token.
3. Put the release job in a protected `release` environment that needs a
   reviewer.
4. Add `actions/attest-build-provenance` for the binaries and sign the image.
5. Add a Dependabot configuration for Go modules, npm and GitHub Actions,
   with the SHA-pin test kept.
6. Add a gitleaks step, and gosec with its existing false positives noted.
7. Run a nightly `go test -fuzz` job, 10 minutes per target, uploading any
   new corpus entry as an artifact.
8. Remove the stale `TODO(review)` comment in `ci.yml`.

Extend `docs/workflows_test.go` to check items 2 and 3.

**Status.** Done: items 1, 2 (`bun install --ignore-scripts` in
`scripts/release.sh`), 3 (the job declares `environment: release`; the
reviewers are a repository setting), 4 for the archives (provenance
attestation; no image is published yet, so there is none to sign), 5
(`.github/dependabot.yml`: weekly, one grouped pull request per ecosystem, and
the Docker base images too), 6 for gitleaks (the `secrets` job of the gate,
`.gitleaksignore` with a reason for each of the 15 fixture findings), 7
(`.github/workflows/fuzz.yml`, schedule only) and 8. gosec is not added. The
CI `postgres` service image is pinned by digest by hand, as Dependabot does not
read workflow service images.

### H-15: large exports outside the timeout buffer (S)

- **Change:** serve `/dashboard/logs.jsonl` and `/reports/zip` from routes
  that `http.TimeoutHandler` does not wrap. Give them their own write
  deadline (`http.ResponseController`) and stream the output.
- **Tests:** a 50 MB export streams within the memory limit of the test.

### H-12: org checks before multi-org (S, required before multi-org)

- **Change:**
  1. `session.actor()` sets `OrgID` from the user.
  2. Every dashboard handler that loads by id checks `d.inOrg`; add a test
     that walks the route table and fails on a handler without it.
- **Tests:** a second org's user gets 404 on every object of the first.

### R-A7: refund and dispute race (S)

- **Change:** when a dispute opens, `disputes.upsert` also fails the
  payment's pending refunds that have not been sent yet, in the same
  transaction. The remaining window is the provider call itself, and it is
  documented.
- **Tests:** a dispute applied while a refund is pending fails the refund;
  the refund job then sends nothing.

## Wave 5: provider sandboxes

### V-1: confirm the audit's fail-safe changes against real providers (M)

For each of Stripe, Razorpay, PayU, Cashfree, Zoho and Whop, in the
provider's sandbox:

1. Complete a checkout and return with the provider's `payment_id`. The
   payment settles on the return itself, not only on the next poll. This
   shows that the provider's fetch-by-id response names our reference or
   order (A-3).
2. Run a subscription setup and one renewal. The fetched success carries an
   amount and currency (A-2).
3. Record the result in `docs/provider-verification.md`. Any provider that
   fails step 1 or step 2 gets a connector change, filling the reference from
   the provider's own field, not from the caller.

A test-mode account with live keys, and a live account with test keys, are
both refused (A-6). Check that the key prefixes are right for each provider.

## Wave 6: what the 26 October re-audit left open

B-1 to B-6 were fixed in the re-audit itself. This wave covers what it left
open. Order: B-7a and B-7b first (each one closes B-7 for every built-in
connector and for remote accounts), then R-B1, then B-7c before launch. B-8
and B-9 follow, and B-8 must be done before multi-org.

### B-7a: next-action URLs are checked against declared hosts (M)

- **Goal:** a connector can send the customer only to hosts it declared:
  scripts, form posts and redirects. For a remote account, the declaration is
  in the account's config, which is visible and audited.
- **Change:**
  1. `pkg/connector`: add `(NextAction).Allowed(h CheckoutHosts) error`.
     - It requires `https`.
     - It requires the URL's origin to match `h.Script` for `ActionSDK`,
       `h.Form` for `ActionFormPost`, and `h.Redirect` for `ActionRedirect`.
       `CheckoutHosts` gains a `Redirect` list.
     - It uses the origin rule of `originRE` in `internal/web/csp.go`, moved
       to `pkg/connector` so that both packages share one.
  2. Fill `Redirect` (and `Form` where it is missing) in the `CheckoutHosts`
     of every built-in connector, from the URLs their tests and sandbox runs
     use.
  3. `connectors/remote`: add `checkout_hosts` to `config` (the same shape as
     `CheckoutHosts`).
     - It is required when the account is created or updated, and every entry
       must pass the origin rule.
     - `Info` cannot carry per-account data, so the live connector exposes it
       through a new optional interface: `connector.CheckoutHoster`, with
       `CheckoutHosts() CheckoutHosts`.
  4. `internal/checkout`, `createCheckout`: after `NextAction.Valid()`, call
     `Allowed` with the account's hosts (from `CheckoutHoster`, else
     `Info.CheckoutHosts`). A refusal is a checkout error, so routing falls
     back to the next account, logs the URL, and raises an ops alert naming
     the account.
  5. Apply the same check to `CreateSubscriptionCheckout` and
     `CreateMandateCheckout` in `internal/subscriptions/hosted.go`
     `OpenCheckout`.
  6. Changing `checkout_hosts` is part of `providerUpdate`. It already needs
     `provider_account.manage` and a recent sign-in, and the audit log keeps
     the before and after values.
- **Tests:**
  - A remote that answers with a script, form or redirect outside its hosts
    gets no checkout, and the payment falls back to the next account.
  - The same for subscription and mandate checkouts.
  - Every built-in connector's checkout passes `Allowed` (one table test over
    the connectortest fixtures).
  - A remote account without `checkout_hosts` is refused at creation.
- **Compatibility:** breaking for remote accounts, which must declare their
  hosts. A built-in connector whose host list is incomplete will fail
  checkout. That is caught by the table test, and by running V-1 again in the
  sandboxes. Release note in `docs/upgrades.md`, plus the `checkout_hosts`
  field documented in `docs/connectors.md` and in the remote protocol docs.

### B-7b: the hosted-page CSP is enforced (S)

- **Goal:** finish H-7. A script that is not ours or the provider's does not
  run, even if B-7a misses a case.
- **Change:**
  1. `internal/web/csp.go`: `HostedCSPHeader` becomes
     `Content-Security-Policy`.
  2. `HostedCSP` takes the account's `CheckoutHosts` (B-7a) instead of
     looking them up by connector name. It stops adding `originOf(scriptURL)`
     and `originOf(formURL)` on top: the action's own URL is allowed only
     because it is in the declared list.
  3. Add a `report-uri` to a new `POST /csp-report` handler. It counts reports
     in a metric and logs them redacted (no query strings), with a per-IP
     limit and a 4 KB body cap.
- **Tests:**
  - The header is enforcing on `/pay`, `/l`, `/s` and `/return` pages.
  - A script origin outside the declared hosts is absent from the policy.
  - The Playwright checkout tests pass per connector under the enforced
    policy.
- **Order:** after B-7a, and after one release with the report endpoint
  showing no violations from built-in connectors.

### R-B1: per-IP limits on the customer routes (S)

- **Goal:** B-1's per-attempt limits plus a per-address ceiling, so one
  client cannot spread its load over many payments or links.
- **Change:**
  1. A small token-bucket middleware in `internal/server`, the same shape as
     `inbound`'s `window`.
  2. Apply it to `/pay/`, `/return/`, `/l/`, `/s/` and `/i/` on both the base
     host and custom domains. The client address comes from `clientip.From`
     with the configured trusted proxies.
  3. Default to 60 requests a minute per IP, with a burst of 20. It is set
     with `PAYMUX_CUSTOMER_RATE`, and `0` turns it off for operators who
     limit at their proxy.
  4. Answer 429 with the themed result page and `Retry-After`.
- **Tests:**
  - The limit applies per address and route family.
  - A trusted proxy's `X-Forwarded-For` is honoured, an untrusted one is not.
  - `0` disables the limit.
- **Compatibility:** additive. Document the setting in
  `docs/configuration.md`.

### B-7c: payment pages on their own origin (L, before launch)

- **Goal:** even an allowed provider script, or a future bug in a hosted
  page, shares nothing with the dashboard: no cookie, no same-origin reads.
- **Change:**
  1. A new setting, `PAYMUX_CHECKOUT_URL` (for example
     `https://pay.example.com`). When it is set, the customer routes (`/pay`,
     `/l`, `/s`, `/i`, `/return`, `/sim`, `/assets`, `/theme`) are served only
     on that host, and the base host answers them with a redirect there.
     Routing already works per host, through `domains.Router`.
  2. Every URL that is built for a customer uses it: `checkout_url` in the
     API, payment-link and invoice URLs, connector return URLs
     (`connector.CheckoutRequest` return base), and emails.
  3. The dashboard host stops serving the customer routes, so a stale link
     redirects.
  4. In production, `paymux doctor` warns when the setting is unset. A
     startup warning says the same.
- **Tests:**
  - With the setting, the dashboard cookie is never sent to the checkout
    host (host-only cookies, a different host).
  - Customer routes on the base host redirect.
  - Every generated URL uses the checkout host.
  - The smoke test runs with two hosts.
- **Compatibility:**
  - Opt-in at first.
  - Return URLs registered with providers (webhook and return settings in
    their dashboards) change when it is turned on. This is documented step by
    step in `docs/upgrades.md`.
  - It becomes the recommended production layout in `docs/installation.md`.

### B-8: custom domains are proven by DNS (M, before multi-org)

- **Goal:** only the owner of a domain's DNS can claim it, even when the
  domain already points at the hub.
- **Change:**
  1. `AddDomain` also returns a TXT record:
     `_paymux-challenge.<host>` with the value `paymux-verify=<token>`.
  2. Activation (`internal/housekeeping`, the domain check) needs both:
     - the TXT record, read through the system resolver, with a 10 s timeout
       and the answer size capped;
     - the existing HTTPS fetch, which proves that traffic reaches this hub.
  3. `TLSAllow` stops allowing certificates for pending domains. Only an
     active domain, which has passed the TXT check, gets one. The first
     certificate is then issued after activation.
  4. A domain already active is kept and re-checked once. If it fails, it
     goes back to pending with an ops alert; it is not removed.
- **Tests:**
  - A host that points at the hub but has no TXT record stays pending.
  - The right TXT record activates it.
  - A wrong token does not.
  - A pending domain gets no certificate.
- **Compatibility:**
  - Operators add one DNS record per domain.
  - Existing active domains are re-checked once, with a grace period of 7
    days before an alert.
  - Update `docs/` for custom domains.

### B-9: PayU event ids come from signed fields (S)

- **Goal:** a replayed signed webhook dedupes, whatever unsigned fields are
  changed.
- **Change:**
  1. `connectors/payu/webhook.go`, `ParseWebhook`: build the event id from
     the hashed fields only (`txnid`, `status`, `amount`, `key`), not
     `mihpayid` or `unmappedstatus`.
  2. Take the hinted `ProviderPaymentID` (`mihpayid`) as a hint only. It
     already is: PayU fetches by `txnid`, so check that path and keep it so.
- **Tests:**
  - The same signed body with another `mihpayid` gets the same event id and
    is stored once.
  - A real status change (another `status`) still gets a new id.
- **Compatibility:** event ids change for new deliveries only. Rows already
  stored keep theirs, so one duplicate fetch is possible around the upgrade.

### Also from the re-audit

- **B-1 residual:**
  - The return limit is per process. Moving it into the database (a
    `last_return_fetch_at` column on `payment_attempts`) is worth doing only
    if several instances are run. Revisit with multi-instance support.
  - R-B1 covers the rest.
- **B-2 residual:** document in `docs/extending.md` that hooks should not run
  backtracking regular expressions on event data, and that each match is
  capped at 1 s.

## Done when

- Every item above is merged, or recorded as "accepted risk" with a reason
  and an owner in `docs/security-audit.md`.
- `scripts/ci.sh` passes, including the PostgreSQL leg.
- A-13's test is no longer skipped.
- V-1 is recorded for every connector.
- The hosted-page CSP is enforcing (B-7b) and has run one release without
  violations from built-in connectors.
- An independent review covers the connectors against real provider
  responses and the money paths changed here.
