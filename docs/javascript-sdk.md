# JavaScript SDK

`@paymux-js/sdk` provides a type-safe backend client, Standard Webhooks verifier,
and a separate browser checkout entry point. The browser flow opens the provider
selected by Paymux without showing the hosted payment page.

Hosted checkout remains available and is the simplest option. Both modes use
the same payment API, routing, provider webhooks, reconciliation, and normalized
app webhooks.

## Choose the checkout experience

| Experience | Merchant implementation | Best when |
|---|---|---|
| Backend-resolved provider checkout | Set the App's `checkout_mode` to `provider_action`. The merchant backend resolves an action and the browser calls `executeCheckout`. No Paymux URL or token enters the browser. | The merchant wants checkout to begin on its own storefront and keep Paymux out of browser requests. |
| Browser-resolved provider checkout | Keep `hosted` mode but pass the server-created `checkout_url` to `checkout`. The SDK opens the routed provider without displaying the Paymux-hosted page. | An existing integration wants SDK-managed billing and retries with the least backend code. |
| Paymux-hosted checkout | Redirect the browser to the server-created `checkout_url`. | The merchant wants the smallest integration or cannot allow provider scripts and frames on its storefront. |

Do not create a separate provider integration in the merchant browser. The
checkout entry point handles redirects, signed form posts, official provider
SDKs, required billing details, and recovery behavior without exposing backend
or provider secrets.

## Install

```sh
npm install @paymux-js/sdk
```

Choose the default on **Apps → your app → Settings → checkout_mode**. The
application backend creates a payment with the JavaScript SDK, [Go SDK](../sdk/go/README.md),
or HTTP API. Never create payments from browser code because that would expose the API key.

```ts
import { Paymux } from "@paymux-js/sdk";

const paymux = new Paymux({ apiKey: process.env.PAYMUX_API_KEY, baseURL: "https://payments.example.com" });
const payment = await paymux.payments.create(
  { amount: 49900, currency: "INR", order_ref: "order-1048", return_url: "https://shop.example.com/orders/1048" },
  { idempotencyKey: "order-1048-payment-v1" },
);
```

The backend entry point covers payments, refunds, links, subscriptions,
invoices, credit notes, payouts, PDF downloads, and webhook verification.

Every mutation requires an `idempotencyKey`; use the same key only to retry the
same logical operation. API errors are typed as `APIError` and include HTTP
status, error type, request ID, replay status, and retry delay.

## Provider action without a Paymux URL in the browser

With `checkout_mode=provider_action`, payment creation returns
`checkout_mode: "provider_action"` and no `checkout_url`. Resolve the provider
action on the authenticated merchant backend:

```ts
const payment = await paymux.payments.create(params, {
  idempotencyKey: `order-${order.id}-payment-v1`,
});

const checkoutAction = await paymux.payments.checkoutAction(
  payment.id,
  {},
  { idempotencyKey: `order-${order.id}-checkout-v1` },
);

// Return only checkoutAction to the authenticated owner of this order.
```

The merchant UI executes that JSON directly:

```ts
import { executeCheckout } from "@paymux-js/sdk/checkout";

const checkoutAction = await fetch(`/api/orders/${orderId}/checkout`, {
  method: "POST",
  credentials: "same-origin",
}).then((response) => response.json());

await executeCheckout(checkoutAction);
```

The merchant UI receives no Paymux checkout URL or token. A provider may still
return the customer through Paymux's verified return endpoint before Paymux
redirects to the merchant `return_url`; configure an App custom domain if that
return hop must use the merchant's domain. Include all customer and billing
information required by the App's tax policy when creating the payment; this
mode does not ask Paymux to collect missing billing data in the browser. Use a
new idempotency key only when the merchant intentionally requests a fresh
provider attempt.

## Checkout URL modes

Verify webhooks against the exact raw request bytes:

```ts
import { WebhookVerifier } from "@paymux-js/sdk";

const verifier = new WebhookVerifier(process.env.PAYMUX_WEBHOOK_SECRET);
const event = await verifier.verify(request.headers, new Uint8Array(await request.arrayBuffer()));
```

Return only the customer checkout URL to that order's authenticated browser:

```json
{"checkout_url":"https://pay.example.com/pay/pay_...?t=..."}
```

The checkout URL is returned only when the payment is created; retrieving the
payment does not reproduce its customer token. Keep the URL in encrypted or
equivalently access-controlled server storage until payment expiry, return it
only to the authenticated order owner, and remove it after expiry or a terminal
outcome. If it is lost, inspect and cancel the old payment when possible before
creating a replacement. Keep every payment linked to the order so a late
success or `needs_review` result is reconciled instead of causing automatic
duplicate fulfilment.

Then open it:

```ts
import { checkout } from "@paymux-js/sdk/checkout";

await checkout({ checkoutUrl });
```

The `return_url` origin must exactly match the storefront origin running the
SDK. Paymux enforces that boundary before returning a provider action. Use HTTPS
except on loopback during local development.

## What happens in browser-resolved mode

The SDK exchanges the customer checkout token for a server-validated action:

- `redirect`: navigate to Stripe, Whop, the simulator, or another hosted checkout;
- `form_post`: create and submit PayU's signed form without exposing the PayU salt;
- `sdk`: load and launch Razorpay, Cashfree, or Zoho's official browser SDK.

If billing details are required, the SDK displays an accessible dialog with
keyboard focus management, inline validation, retry, and recovery states. Supply
the customer and billing details when creating a payment to skip this dialog.

The checkout token is a customer credential. Do not log it, persist it in
analytics, put it in HTML generated for another customer, or send it anywhere
except the matching Paymux origin. Responses are CORS-bound to the payment's
return origin and are never cacheable.

## Content Security Policy

Razorpay, Cashfree, and Zoho run their own scripts and frames on the storefront.
The storefront CSP must include the origin that issued `checkout_url` in
`connect-src`, because the checkout SDK first posts to that Paymux origin. It
must also permit the selected provider's declared checkout hosts. Use hosted
Paymux checkout when the storefront cannot allow those resources. Redirect and
form-post integrations leave the page and do not execute provider scripts in
it.

With a nonce-based policy, pass the fresh nonce generated for the current HTML
response. Paymux applies it to the built-in dialog style and dynamically loaded
provider script:

```ts
await checkout({ checkoutUrl, nonce: window.cspNonce });
```

The CSP must still allow the selected provider's script, frame, image, form, and
connection hosts. Do not reuse a nonce across responses.

## Confirm payment on the backend

Browser events are for interface state only. A customer can close the page,
forge query parameters, or return before a delayed payment settles. Mark an
order paid only after a verified Paymux `payment.succeeded` webhook, or after
the backend retrieves the payment with its Paymux API key.

The return page should show a neutral "Confirming payment" state until its own
backend reports the order paid, failed, or still processing.

Coding agents can start with [`/llms.txt`](../llms.txt) and use the complete
[`/llms-full.txt`](../llms-full.txt) integration contract and acceptance tests.
