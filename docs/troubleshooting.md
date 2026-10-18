# Troubleshooting

Begin with the hub version, selected test/live mode, application, relevant resource ID, request ID, and timestamp. Use redacted logs and avoid sharing API keys, provider secrets, checkout tokens, or customer documents.

## Hub will not start

Check the configuration errors first. A missing base URL, invalid master key, unsupported database URL, or development-only option in production is a configuration problem rather than a routing failure.

For SQLite, another hub or CLI process may hold the database lock. Stop the intended process cleanly before maintenance; do not delete the lock or database to bypass the error. For PostgreSQL, verify network reachability, credentials, TLS configuration, and migrations.

Check disk capacity and data-directory permissions. For the website binary, a 503 saying the frontend is unbuilt means to build the frontend and rebuild `paymux-home`; it does not mean the hub database is unavailable.

## Setup or sign-in fails

Read the setup code from the current process's log; a restart changes the code while setup remains open. Once an administrator exists, setup returns 404 by design. Repeated wrong attempts can trigger a lockout.

For TOTP, check the user's saved authenticator secret and clock synchronization. Use the documented CLI recovery flow in the [runbook](runbook.md), respecting the SQLite lock. For SSO, check issuer/client credentials, exact callback URI, verified email, disabled-user state, allowed provisioning domain, and configured role.

## API returns 401, 403, or 404

- **401:** check the bearer token and whether it was revoked.
- **403:** check the resource's read/write scope. Payouts need explicit scopes.
- **404:** check the resource ID and the key's application and mode. A valid ID from another app or mode is intentionally not visible.

A dashboard session cookie is not an application API key.

## No route or wrong provider

1. Confirm the payment's app, mode, currency, amount, country, and purpose.
2. Confirm a provider account exists, is enabled, and is linked to that app.
3. Check accepted currencies, purposes, connector capabilities, and health state.
4. Inspect app rules before organization rules and catch-all behavior in the route tester.
5. Check `allowed_connectors` stored on the payment; it restricts future retries as well as initial checkout.

For the quickstart, link a **test** simulator account. A simulator cannot process live payments.

## Customer paid, order still unpaid

Inspect the provider attempt and authoritative payment state. A browser return can arrive before provider verification. If the hub is not current, check registered provider webhook URL, signing configuration, provider-side endpoint status, and reconciliation results.

If the payment is succeeded but your order is unchanged, inspect app webhook deliveries and your receiver's durable inbox. A failed receiver is not a reason to create another charge. Fix the receiver and replay the event idempotently.

If the payment is `needs_review`, check amount/currency mismatch, late success after cancellation, or duplicate captures. Do not automatically fulfill or refund without checking the flags and applicable policy.

## Webhook signature or delivery failure

Use the exact raw body and endpoint's signing secret, not the provider account secret. Preserve `webhook-id`, `webhook-timestamp`, and `webhook-signature`. Check clock skew, reverse-proxy behavior, and middleware that rewrites JSON.

For missing requests, inspect event filters, enabled state, DNS, TLS, network reachability, and private-IP blocking. Localhost inside a container is not your laptop. A replay uses the same event ID, so a correctly deduplicated receiver may acknowledge without repeating business work.

## Refund pending or rejected

A refund is queued and confirmed asynchronously. Inspect its detail and provider refund ID, delivery/reconciliation records, and available captured balance. Check connector refund support and dispute constraints. Reusing a key with a changed amount or reason produces an idempotency conflict; use a new key only for a genuinely new operation.

## Subscription did not renew

Inspect authorization state, billing mode, renewal payment, mandate status and amount limits, pre-debit handling, retry schedule, and provider capability. Read the proration outcome if a plan change occurred. A provider-managed subscription and a merchant-triggered mandate have different operational behavior.

## Invoice or PDF is missing

Check the app's tax profile, enabled issuance, supplier/customer details, payment state, and whether the provider is merchant of record. An issued invoice may have a PDF still rendering; honor `Retry-After` and retry the PDF request later.

Inspect tax alerts, certificate validity, FX prerequisites, and e-invoice workflow state. Offline e-invoice export does not submit a document to the IRP automatically.

## After an outage or restore

Use the [runbook](runbook.md) for a controlled restore. Confirm readiness, reconcile affected provider accounts, inspect disputes and deadlines, re-enable provider webhooks if needed, and replay failed application deliveries. Check your application against current payment state rather than assuming every missed event was lost forever.

## Gather a useful report

Include a minimal reproduction, installed version, database type, selected app/mode, redacted request and response, request ID, affected object IDs, and relevant timestamps. Identify whether the failure concerns provider verification, hub state, document generation, or outbound delivery. Never include secrets or full customer PII in a public issue.
