# JavaScript SDK

`@paymux/sdk` provides a type-safe backend client, Standard Webhooks verifier,
and a separate browser checkout entry point. The browser flow opens the provider
selected by Paymux without showing the hosted payment page.

Hosted checkout remains available and is the simplest option. Both modes use
the same payment API, routing, provider webhooks, reconciliation, and normalized
app webhooks.

## Install

```sh
npm install @paymux/sdk
```

The application backend creates a payment with the JavaScript SDK, [Go SDK](../sdk/go/README.md),
or HTTP API. Never create payments from browser code because that would expose the API key.

```ts
import { Paymux } from "@paymux/sdk";

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

Verify webhooks against the exact raw request bytes:

```ts
import { WebhookVerifier } from "@paymux/sdk";

const verifier = new WebhookVerifier(process.env.PAYMUX_WEBHOOK_SECRET);
const event = await verifier.verify(request.headers, new Uint8Array(await request.arrayBuffer()));
```

Return only the customer checkout URL to that order's authenticated browser:

```json
{"checkout_url":"https://pay.example.com/pay/pay_...?t=..."}
```

Then open it:

```ts
import { checkout } from "@paymux/sdk/checkout";

await checkout({ checkoutUrl });
```

The `return_url` origin must exactly match the storefront origin running the
SDK. Paymux enforces that boundary before returning a provider action. Use HTTPS
except on loopback during local development.

## What happens

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
The storefront CSP must permit the selected provider's declared checkout hosts.
Use hosted Paymux checkout when the storefront cannot allow those resources.
Redirect and form-post integrations leave the page and do not execute provider
scripts in it.

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
