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
| `expired` | Its payment window (`payment_ttl`) ended with no attempt still open. |
| `canceled` | The payment was canceled before completion. |
| `needs_review` | The result requires operator review, for example an amount mismatch, a success after cancellation, or a success after several declined cards. |

**Finding out why a payment failed.** `GET /v1/payments/{id}` returns each attempt with its `tries`: every go at paying that the provider reported, oldest first, kept even if a later try succeeds. A try can include its status, method, card details, `three_d_secure.status` (`passed`, `failed` or `not_verified`), `failure_code` and `failure_message`. Providers differ, so any value the provider did not report is left out, and a missing value does not mean a failure. An attempt's own `failure_code` and `failure_message` describe its current state and clear if it succeeds.

Attempt states include `created`, `pending`, `authorized`, `captured`, and `failed`. The public payment-success event is **`payment.succeeded`**, not `payment.captured`. A simulator's failed attempt may leave the payment payable for another attempt.

**Paying twice.** A second successful attempt on a succeeded payment is flagged `duplicate_success` (and refunded automatically when `duplicate_payment_action` is `auto_refund`). Refunding that *extra* attempt does not touch the sale: `amount_refunded` and the `refunded_*` flags stay as they were, no credit note is issued, and the app gets `payment.duplicate_refunded` (the payment carries the `duplicate_refunded` flag) rather than `refund.succeeded`. A refund of the sale itself still works for the full amount.

**How long a payment stays open.** `payment_ttl` (30 minutes by default) is how long a customer can start or retry a payment. `attempt_ttl` (15 minutes by default, from 5 minutes to 24 hours) is how long they have to finish an attempt they started, so an attempt begun just before the end is not cut off: a payment past its expiry waits for that attempt's limit, then expires. A later change to `attempt_ttl` applies to new attempts only. Once the provider reports processing, neither limit applies, and the payment waits for the provider's result. Paymux keeps polling the provider for up to 72 hours (this window is not configurable), so a customer who paid late is still recorded. A failed payment keeps the status `failed` past its expiry, but can no longer be retried; the dashboard then shows it as **Failed** rather than **Ready to retry**, and an unfinished attempt of a closed payment as **expired**.

**Card testing.** Someone holding stolen cards may try one after another on the same checkout, and some providers keep a checkout open after a decline. Paymux counts the different cards declined (by network and last four digits), using three settings under **Settings → Fraud protection**, each with 0 to turn it off:

- `card_decline_limit` (5 by default): at this many declined cards, the payment is flagged `card_testing` and takes no new attempt.
- `review_after_declined_cards` (3 by default): a success after this many other declined cards goes to `needs_review` with the flag `declined_cards`, instead of `succeeded`. A success on a `card_testing` payment always goes to review.
- `customer_decline_limit` (10 by default): at this many declined cards across one customer's payments, the customer is blocked. A blocked customer cannot start an attempt, and a success of theirs goes to review with the flag `customer_blocked`.

Your app gets `payment.needs_review`, not `payment.succeeded`, so do not grant access yet. In the dashboard, check the card tries and the risk panel, then either **Approve payment** (the payment becomes `succeeded` and your app gets `payment.succeeded`) or refund it. **Block customer** and **Unblock customer** are on the payment page. These rules do not apply to subscription renewals, which are charged with no customer present. The provider may still accept cards on a checkout Paymux has stopped; such a success goes to review.

**Buyer IP checks.** Paymux records the buyer's IP at checkout: each browser that opens a payment's pay page or starts its browser checkout, and the IP your app sends in the `Paymux-Buyer-IP` header when it creates the payment (the Go SDK's `WithBuyerIP`). In `provider_action` mode the buyer never opens a Paymux page, so send the header there. Paymux compares these IPs with the IP the provider saw pay. Two settings under **Settings → Fraud protection**, both on by default:

- `review_vpn`: a success paid from, or started from, a VPN, proxy or Tor IP goes to `needs_review` with the flag `vpn`. A privacy relay such as iCloud Private Relay does not count. This needs `PAYMUX_VPNAPI_KEY` (see [Configuration](configuration.md)).
- `review_ip_mismatch`: a success paid from an IP that never opened the pay page goes to `needs_review` with the flag `ip_mismatch`. With an IP lookup (`PAYMUX_VPNAPI_KEY` or `PAYMUX_IP_LOOKUP_URL`), only an IP in another country than every checkout IP counts, so a phone moving between Wi-Fi and mobile data is not held. Without it, any other IP of the same family (IPv4 or IPv6) counts.

- `review_address_mismatch`: a success whose card billing address, as the buyer entered it at the provider's checkout, is in another country than the billing address your app sent with the payment, or has another ZIP or postal code in the same country, goes to `needs_review` with the flag `address_mismatch`. A side the app or provider did not send is not compared. Only Whop reports the card billing address today.

### Refusing a checkout before any charge

A review comes after the card is charged, and the provider keeps its processing fee even when the payment is refunded. These settings, also under **Settings → Fraud protection**, refuse to start a checkout instead, so no card is charged. They apply to live payments only, never to merchant-initiated renewals, and use the payment's checkout IPs (the pay page's, and the `Paymux-Buyer-IP` your app sent):

- `block_vpn_checkout` (on): the IP is a VPN, proxy or Tor exit. Needs `PAYMUX_VPNAPI_KEY`.
- `block_country_mismatch` (on): the IP is in another country than the billing address your app sent. Needs an IP lookup.
- `block_fraud_linked` (on): the IP or the customer's email address was used on a payment tagged as fraud.
- `ip_payment_limit` (2): payments one IP may start a checkout for in 24 hours; the next is refused. A payment that never started a checkout does not count.
- `ip_decline_limit` (2): different cards declined from one IP in 24 hours, after which that IP cannot start a checkout.
- `email_paid_limit` (0, off): paid payments one email address may make in 24 hours.

A refused payment is tagged `risk_blocked`, with a flag naming the rule (such as `blocked_vpn`), audited and alerted once, and stays refused. Your app's checkout action gets `403 payment_blocked`, and the pay page says the payment cannot be completed, without the reason. If the buyer is genuine, **Allow checkout** on the payment page lets it be paid regardless of these rules. The payments list filters on **Blocked before charge**. An IP that cannot be looked up is skipped, never refused.

A payment with no checkout IP (no browser opened the pay page and your app sent none) is not compared. An IP that cannot be looked up is skipped, never held. The payment page's **Checkout IPs** panel lists each checkout IP with its country, network owner (a hosting company rather than an internet provider often means a VPN) and any VPN tag, and the IP that paid, marked **Matches checkout** or **Not a checkout IP**. Approve or refund these reviews as above. When cards are declined on a checkout that already succeeded, the `payment.card_testing` alert says so: the card that paid may be stolen too, and a refund before the cardholder disputes it costs less than the dispute.

A late matching success can move a failed or expired payment to success with a late flag. A success on a checkout the provider kept open after a decline, before the payment's expiry, is not late. Success after cancellation or a currency/amount mismatch goes to review. Another successful attempt on an already succeeded payment triggers duplicate-payment handling. Review the effective `duplicate_payment_action` policy rather than assuming every duplicate is automatically refunded.

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
