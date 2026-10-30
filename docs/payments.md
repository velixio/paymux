# Payments and routing

A **payment** is your application's request to collect an amount. An **attempt** is an individual interaction with a provider. One payment may have several attempts, so a failed attempt is not always a failed payment.

## End-to-end lifecycle

1. Your backend creates a payment with `POST /v1/payments` and saves its ID.
2. Your frontend sends the customer to the returned `checkout_url`, or passes it to the [JavaScript checkout SDK](javascript-sdk.md).
3. Paymux or the JavaScript SDK collects required billing details and starts an eligible provider attempt.
4. The provider notifies Paymux; Paymux verifies the notification and re-fetches authoritative provider state.
5. The hub commits the state change, ledger entries, and outbound event together.
6. Your application verifies the app webhook and updates its order idempotently.

A browser redirect can happen before confirmation or never arrive. It is navigation, not authority to fulfill an order. An app webhook failing to deliver does not reverse an already captured payment.

## Payment state versus attempt state

| Payment state | Interpretation |
|---|---|
| `created` | The request exists, but collection has not completed. |
| `requires_action` | Customer action is required. |
| `processing` | Confirmation is pending. |
| `succeeded` | Verified successful collection matching the payment. |
| `failed` | The payment has reached a failed outcome; late confirmations still require attention. |
| `expired` | Its payment window expired. |
| `canceled` | The payment was canceled before completion. |
| `needs_review` | The result requires operator review, for example an amount mismatch or success after cancellation. |

Attempt states include `created`, `pending`, `authorized`, `captured`, and `failed`. The public payment-success event is **`payment.succeeded`**, not `payment.captured`. A simulator's failed attempt may leave the payment payable for another attempt.

**Paying twice.** A second successful attempt on a succeeded payment is flagged `duplicate_success` (and refunded automatically when `duplicate_payment_action` is `auto_refund`). Refunding that *extra* attempt does not touch the sale: `amount_refunded` and the `refunded_*` flags stay as they were, no credit note is issued, and the app gets `payment.duplicate_refunded` (the payment carries the `duplicate_refunded` flag) rather than `refund.succeeded`. A refund of the sale itself still works for the full amount.

A late matching success can move a failed or expired payment to success with a late flag. Success after cancellation or a currency/amount mismatch goes to review. Another successful attempt on an already succeeded payment triggers duplicate-payment handling. Review the effective `duplicate_payment_action` policy rather than assuming every duplicate is automatically refunded.

## Routing hierarchy

Rules are evaluated at application level, then organization level, then the organization catch-all. Conditions include currency, country, amount, purpose, and metadata. A rule can route to ordered connector/account targets or reject a request with a message.

The target must still be eligible: linked to the app, in the payment's mode, enabled for its currency and purpose, and available under account health rules. A named connector does not magically add an account or grant it a capability. Use the route tester in the dashboard before switching production traffic.

The create-payment `allowed_connectors` field narrows eligible connectors for the payment's lifetime, including retries and fallbacks. It cannot broaden permissions. No eligible route produces `422 no_route`; a rejecting currency rule can produce `currency_not_accepted`.

For local integration, link a test-mode simulator account. Its test fallback avoids the need for a real bank account; the simulator cannot be a live provider account.

## Hosted checkout and links

The create response contains a checkout token shown once and valid until expiry. Treat the URL as a customer capability: do not place it in public logs or analytics.

Hosted checkout executes on Paymux's checkout origin. The JavaScript SDK instead
exchanges the token for a validated provider action from the merchant storefront;
Paymux only answers the exact origin of the payment's `return_url`. It does not
expose the merchant API key or provider signing secrets. Provider SDK checkouts
still load the provider's official browser script and require the matching CSP
allowances on the storefront.

Payment links are reusable or single-use entry pages under `/l/{slug}` that create a payment for a visitor. They can use a fixed amount or bounded customer-chosen amount and may expire or be deactivated. Create and manage them using `/v1/payment_links` or the dashboard. The API reference defines the exact shape and limits.

## Cancel an unpaid payment

Use `POST /v1/payments/{id}/cancel` for a payment in a cancellable state. Canceling does not refund captured money, and it cannot prevent a provider from later reporting an in-flight success. Inspect the returned state and handle review alerts.

## Refund captured funds

```sh
curl --fail-with-body "$PAYMUX_URL/v1/payments/$PAYMENT_ID/refunds" \
  -H "Authorization: Bearer $PAYMUX_API_KEY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: order-1048-refund-1" \
  -d '{"amount":10000,"reason":"customer request"}'
```

A refund starts as pending and is submitted to the provider. Track `refund.succeeded` / `refund.failed` or retrieve `/v1/refunds/{id}`. Creation alone is not final provider confirmation. Refunds require provider support and sufficient unrefunded captured balance; dispute and attempt rules may restrict them.

Partial refunds do not rename the payment to a refund state: the payment keeps its lifecycle status and exposes refunded amounts and flags. If invoicing applies, the corresponding document correction is a credit note.

## Reconcile uncertainty

When a customer has a provider receipt but your order is unresolved, inspect payment attempts, verified provider state, ledger records, and app deliveries. Reconciliation and replay solve different problems: reconciliation compares provider records, while replay delivers an existing event to your application again. See [troubleshooting](troubleshooting.md) and the [runbook](runbook.md).
