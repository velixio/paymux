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

## Done when

- Every item above is merged, or recorded as "accepted risk" with a reason
  and an owner in `docs/security-audit.md`.
- `scripts/ci.sh` passes, including the PostgreSQL leg.
- A-13's test is no longer skipped.
- V-1 is recorded for every connector.
- An independent review covers the connectors against real provider
  responses and the money paths changed here.
