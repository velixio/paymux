# Paymux JavaScript SDK

Type-safe server API, Standard Webhooks verification, and provider-neutral browser checkout for Paymux.

```bash
npm install @paymux/sdk
```

## Backend API

Keep the API key on the server. Every mutation requires an idempotency key; reuse it only when retrying the same logical operation.

```ts
import { Paymux } from "@paymux/sdk";

const paymux = new Paymux({
  apiKey: process.env.PAYMUX_API_KEY!,
  baseURL: "https://payments.example.com",
});

const payment = await paymux.payments.create(
  { amount: 49900, currency: "INR", order_ref: "order-1048" },
  { idempotencyKey: "order-1048-payment-v1" },
);
```

Services cover payments, refunds, payment links, subscriptions, invoices, credit notes, payouts, pagination, PDF downloads, abort signals, and typed API errors. `APIError` exposes the status, error type, request ID, replay marker, retry delay, and safe `temporary`, `isNotFound`, and `isConflict` checks.

## Webhooks

Verify the exact raw request bytes before decoding or acting on an event:

```ts
import { WebhookVerifier } from "@paymux/sdk";

const verifier = new WebhookVerifier(process.env.PAYMUX_WEBHOOK_SECRET!);
const event = await verifier.verify(request.headers, new Uint8Array(await request.arrayBuffer()));
```

Persist and deduplicate `event.id`. Unknown event types should be ignored safely.

## Browser checkout

The backend returns `payment.checkout_url` to that order's authenticated browser. The browser entry point contains no backend client and never receives the API key:

```ts
import { checkout } from "@paymux/sdk/checkout";

await checkout({ checkoutUrl: paymentCheckoutURL });
```

It collects required billing details, follows hosted redirects, submits signed PayU forms, and launches Razorpay, Cashfree, or Zoho's official SDK. The payment `return_url` origin must exactly match the storefront origin. Use HTTPS outside loopback development.

For a script tag, self-host `dist/paymux-checkout.global.js` and call `Paymux.checkout({ checkoutUrl })`.

Browser events are interface signals, not proof of payment. Fulfill an order only after a verified webhook or authenticated backend lookup.

## Content Security Policy

Embedded provider SDKs require their declared script, frame, image, form, and connection hosts. For a nonce policy, pass the fresh nonce for the current HTML response:

```ts
await checkout({ checkoutUrl, nonce: window.cspNonce });
```

Use hosted Paymux checkout when the storefront cannot allow provider resources. Never reuse a CSP nonce between responses.
