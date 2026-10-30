# Paymux JavaScript SDK

Type-safe server API, Standard Webhooks verification, and provider-neutral browser checkout for Paymux.

```bash
npm install @paymux-js/sdk
```

## Backend API

Keep the API key on the server. Every mutation requires an idempotency key; reuse it only when retrying the same logical operation.

```ts
import { Paymux } from "@paymux-js/sdk";

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
import { WebhookVerifier } from "@paymux-js/sdk";

const verifier = new WebhookVerifier(process.env.PAYMUX_WEBHOOK_SECRET!);
const event = await verifier.verify(request.headers, new Uint8Array(await request.arrayBuffer()));
```

Persist and deduplicate `event.id`. Unknown event types should be ignored safely.

## Browser checkout

For no Paymux URL in the browser, set the App's `checkout_mode` to
`provider_action`. The same create call returns the action:

```ts
const payment = await paymux.payments.create(
  {
    amount: 49900,
    currency: "INR",
    customer: { name: "Asha Rao", billing: { country: "IN", state_code: "27" } },
  },
  { idempotencyKey: "order-1048-payment-v1" },
);

const action = payment.checkout;
```

Return only `action` to the authenticated order owner and execute it in the UI:

```ts
import { executeCheckout } from "@paymux-js/sdk/checkout";

await executeCheckout(actionFromMerchantBackend);
```

The merchant UI receives no Paymux checkout URL or token. A provider may still
return the customer through Paymux's verified return endpoint before the
merchant `return_url`; use an App custom domain when that hop should carry the
merchant's domain. Provider-action mode requires the backend to supply billing
details required by the App's tax policy during payment creation. Use
`checkoutAction` only for recovery or an intentionally fresh attempt.

For browser-resolved checkout, keep the App in `hosted` mode. The backend
returns `payment.checkout_url` to that order's authenticated browser:

The browser entry point contains no backend client and never receives the API key:

```ts
import { checkout } from "@paymux-js/sdk/checkout";

await checkout({ checkoutUrl: paymentCheckoutURL });
```

It collects required billing details, follows hosted redirects, submits signed PayU forms, and launches Razorpay, Cashfree, or Zoho's official SDK. The payment `return_url` origin must exactly match the storefront origin. Use HTTPS outside loopback development.

For a script tag, self-host `dist/paymux-checkout.global.js` and call
`Paymux.checkout({ checkoutUrl })` or `Paymux.executeCheckout(action)`.

Browser events are interface signals, not proof of payment. Fulfill an order only after a verified webhook or authenticated backend lookup.

## Content Security Policy

Embedded provider SDKs require their declared script, frame, image, form, and connection hosts. For a nonce policy, pass the fresh nonce for the current HTML response:

```ts
await checkout({ checkoutUrl, nonce: window.cspNonce });
```

Use hosted Paymux checkout when the storefront cannot allow provider resources. Never reuse a CSP nonce between responses.
