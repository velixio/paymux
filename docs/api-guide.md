# Application integration

This guide covers the HTTP integration between your application backend and the Paymux hub. The [OpenAPI specification](openapi.yaml) defines exact fields and responses. Go applications can use the independent [merchant SDK](../sdk/go/README.md); `pkg/connector` is the provider integration SDK, and `framework` is for embedding the hub.

## Authenticate from your backend

Create an application in the dashboard and save its API key when displayed. Send it as a bearer token:

```sh
export PAYMUX_URL=http://localhost:8080
export PAYMUX_API_KEY=pmx_test_sk_REPLACE_ME
curl --fail-with-body "$PAYMUX_URL/v1/payments?limit=20" \
  -H "Authorization: Bearer $PAYMUX_API_KEY"
```

Keep secret keys out of browser code, mobile bundles, public URLs, and source control. The key selects both the application and test/live mode. A key for one app cannot retrieve another app's payment; a live key does not see test records.

## Scopes

| Resource | Read scope | Write scope |
|---|---|---|
| Payments and payment links | `payments:read` | `payments:write` |
| Refunds | `refunds:read` | `refunds:write` |
| Subscriptions | `subscriptions:read` | `subscriptions:write` |
| Invoices and credit notes | `invoices:read` | No general write API |
| Payouts | `payouts:read` | `payouts:write` |

A write scope permits the corresponding reads. Legacy/unscoped keys have the ordinary scopes, but **payout scopes must always be explicitly granted**. Missing permissions return `403 insufficient_scope`. Use separate keys for services with different responsibilities and revoke unused keys in the app's Keys tab.

## Create a payment

```sh
curl --fail-with-body "$PAYMUX_URL/v1/payments" \
  -H "Authorization: Bearer $PAYMUX_API_KEY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: order-1048-payment-1" \
  -d '{"amount":49900,"currency":"INR","order_ref":"order-1048","description":"Pro plan"}'
```

The response is a payment object with a `checkout_url`. Store its `id` against your order and redirect the buyer to that URL. Do not log the checkout token. A `201` means the payment was created, not that money has been collected.

To keep the buyer on the storefront until the selected provider opens, pass the
same `checkout_url` to the [JavaScript checkout SDK](javascript-sdk.md). The SDK
handles redirects, signed forms, provider scripts, and required billing details.
The payment `return_url` must share the storefront's origin. The API key always
stays on the backend.

Confirm fulfillment from a verified `payment.succeeded` event or the authoritative `GET /v1/payments/{id}` response. Match the app's order, expected amount/currency, and payment state before fulfillment. Treat `needs_review` as a reason for investigation.

## Amounts and timestamps

Amounts are integer minor units: `49900` is ₹499.00 for INR. Do not use floating-point arithmetic for stored amounts. Timestamps use RFC 3339 UTC.

If an app issues invoices with tax-exclusive pricing, `amount` is the taxable value and the returned payment amount includes applicable tax. Inspect the returned payment and [billing guide](billing.md); do not assume the submitted taxable value always equals the collected total.

## Retry writes idempotently

Persist one `Idempotency-Key` per logical write, up to 255 characters. Reuse the same key, HTTP method, path, and body when retrying that write. The stored-response window is 24 hours. `POST /v1/payouts` requires the header (400 without it), because a retry that sends money twice cannot be undone.

- A completed matching request returns its stored response with `Idempotent-Replay: true`.
- A changed request using the same key returns `409 idempotency_key_reuse`.
- A concurrent matching request still running returns `409 idempotency_in_progress`; retry later with backoff.
- Responses with status 5xx are not stored. On an uncertain outcome, inspect the known resource and retry the original request rather than creating a fresh logical payment.

Do not use your customer's ID as a permanent idempotency key: that would conflate different purchases.

## Pagination and filtering

Lists return `object`, `data`, `has_more`, and, when present, `next_cursor`. Pass that cursor as `starting_after` on the next request. Payment lists accept `limit` from 1 to 100 (default 20) and a status filter. Check each operation's parameters: not every list has identical filters.

```sh
curl --get --fail-with-body "$PAYMUX_URL/v1/payments" \
  -H "Authorization: Bearer $PAYMUX_API_KEY" \
  --data-urlencode "limit=20" \
  --data-urlencode "starting_after=REPLACE_WITH_NEXT_CURSOR"
```

## Errors and rate limits

Errors have one envelope:

```json
{"error":{"type":"not_found","message":"…","request_id":"…"}}
```

| Response | What to check |
|---|---|
| 400 / 413 | Schema, required fields, amount type, and body size. |
| 401 | Bearer token, revoked key, or missing authentication. |
| 403 | Key scopes and requested operation. |
| 404 | Identifier, owning application, and test/live mode. |
| 409 | Resource state, refundable balance, or idempotency conflict. |
| 422 | Provider capabilities, eligible routing, currency, or mandate constraints. |
| 429 | Per-key rate limit; honor `Retry-After`. |
| 5xx | Record the request ID; retry cautiously with the same logical request. |

The API applies a token bucket of 20 requests per second per key. A rate-limited request is not processed. Use bounded exponential backoff and jitter rather than immediate loops.

## Examples and next steps

The [Go SDK](../sdk/go/README.md) covers all public resources, idempotent mutations, typed errors, pagination, PDF downloads, and webhook verification without depending on hub internals. The [raw HTTP example](../examples/api-client/main.go) remains useful when you do not want an SDK dependency. For webhook delivery rules, see [webhooks](webhooks.md). For checkout, failed attempts, refunds, and routing, see [payments](payments.md).
