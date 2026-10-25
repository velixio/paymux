# Webhooks and event delivery

There are two directions of webhook traffic. A **provider webhook** arrives at Paymux from a payment provider. An **app webhook** leaves Paymux and informs your application's backend. They have different endpoints, credentials, and verification rules.

## Register the endpoints

For a provider account, copy the webhook URL from that account's detail page: `https://your-hub.example/hooks/{provider_account_id}`. Register it with the provider and set the provider-specific signing secret where required. The account list intentionally does not display webhook URLs.

For your application, open **Apps → your app → Webhooks**, add your receiver URL and event filters, and save the signing secret when it is shown. The secret starts with `whsec_`. Keep it on your backend. Your receiver must be reachable from the hub; localhost refers to the hub's own host or container network.

Private and loopback destinations are blocked by default. Use `PAYMUX_ALLOW_PRIVATE_WEBHOOKS=true` only for the local receiver walkthrough or an intentionally configured private deployment.

## Event envelope

An app webhook is JSON with an event type, event timestamp, and object snapshot:

```json
{
  "type": "payment.succeeded",
  "timestamp": "2026-10-18T09:00:00Z",
  "livemode": true,
  "data": {
    "object": {
      "id": "pay_EXAMPLE",
      "object": "payment",
      "status": "succeeded",
      "amount": 49900,
      "currency": "INR",
      "order_ref": "order-1048",
      "version": 3
    }
  }
}
```

`livemode` is `false` for events of test-mode payments and `true` for live ones; the same flag is on every object (`livemode` next to `object`). **An endpoint belongs to one mode**: a test endpoint receives test events only, a live endpoint live events only, so a test payment never reaches a production receiver. Choose the mode when you add the endpoint (the dashboard form, or `paymux endpoint add --mode test|live`; the default is live). Endpoints that existed before modes were introduced became test endpoints if their app has never had a live API key or a live payment, and live endpoints otherwise; add a second endpoint if you want both.

This is an abbreviated illustrative payload, not a complete schema. The event ID is in the **`webhook-id` header**, not a guaranteed top-level JSON `id`. Payment snapshots expose a version; use object versions or retrieve current state to avoid applying an older snapshot after a newer one.

## Verify the exact bytes

Paymux uses the Standard Webhooks signing format:

| Header | Value |
|---|---|
| `webhook-id` | Stable event identifier, reused for retries and replays. |
| `webhook-timestamp` | Unix seconds for the delivery signature. |
| `webhook-signature` | One or more space-separated `v1,<base64 signature>` values. |

1. Read the raw request body before JSON parsing or middleware reserialization.
2. Base64-decode the signing secret after removing `whsec_`.
3. Compute HMAC-SHA256 over the exact bytes of `id.timestamp.` followed by the raw body.
4. Compare to at least one supported `v1` signature using a constant-time comparison.
5. Check the timestamp against a bounded clock tolerance; Paymux's own verifier uses five minutes.
6. Only then parse and accept the event.

Use a Standard Webhooks library for your application stack or adapt one of the verification and durable inbox examples: [Go](../examples/webhook-receiver-go/main.go) (standard library only), [JavaScript](../examples/webhook-receiver-js/receiver.mjs) (Node 18+, built-in `crypto`) or [Python](../examples/webhook-receiver/receiver.py) (3.9+). Each binds to loopback, limits request size, rejects stale/tampered signatures, and persists the event before returning 2xx (the Python one in SQLite, the Go and JavaScript ones as one file per event id). They are learning receivers, not public production HTTP servers, and each has a test you can run.

## Acknowledge durably, process idempotently

A 2xx response marks delivery as successful. Return it after recording the event in a durable inbox or completing the necessary durable transaction. A process that acknowledges and then loses an in-memory queue has lost the event from the hub's point of view.

Deduplicate by `(endpoint/application context, webhook-id)` in durable storage. If you run one inbox per application endpoint, the event ID can be its unique key. Multiple configured endpoints may receive the same event; do not let a global dedupe key accidentally suppress work that belongs to a different consumer.

Within your own database transaction, apply the order change and record processing completion together. Expect repeated delivery and out-of-order events. Do not send goods twice because `payment.succeeded` was retried. Ignore unknown event types safely and keep an audit trail.

## Retries, failure, and replay

The default retry delays before successive attempts are: immediately, 1 minute, 5 minutes, 30 minutes, 2 hours, 6 hours, 12 hours, 24 hours, and 24 hours. The effective `webhook_retry_schedule` setting can change this. Network failures and non-2xx responses are recorded; permanent transport/configuration failures may stop delivery earlier.

The dashboard shows attempts and final failure. After fixing the receiver, replay a failed delivery from the app's Webhooks tab. Replay keeps the same event ID and restarts delivery attempts, so your deduplication logic still applies. A pending delivery cannot be replayed as if it had already finished.

Payment success and delivery success are independent. Fixing delivery does not require charging the customer again.

## Event families

Expect events for payments, refunds, disputes, subscriptions, mandates, invoices, credit notes, payment links, and payouts where those features apply. Important examples include `payment.created`, `payment.succeeded`, `payment.failed`, `refund.succeeded`, `invoice.issued`, and `credit_note.issued`. Subscribe only to the events your consumer handles; do not infer a provider's capabilities from the existence of an event family.

## Diagnose a receiver failure

- Signature mismatch: check the exact raw bytes, endpoint secret, and three signed headers.
- Timestamp rejection: synchronize clocks and check that a proxy preserved the timestamp header.
- No incoming request: check DNS, TLS, receiver reachability, SSRF restrictions, endpoint enabled state, and filters.
- Delivery is 2xx but the order is unchanged: inspect your durable inbox and business-processing logs.
- Earlier event arrives late: compare the object's version or query the API before overwriting current state.

See the [local quickstart](quickstart.md) to generate real simulator events and [troubleshooting](troubleshooting.md) for recovery procedures.
