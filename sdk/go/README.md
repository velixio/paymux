# Paymux Go SDK

The merchant-facing Go SDK for a Paymux instance. It is an independent Go
module, so applications do not pull in the hub or connector implementation.

```bash
go get github.com/velixio/paymux/sdk/go
```

```go
client, err := paymux.New(os.Getenv("PAYMUX_API_KEY"),
    paymux.WithBaseURL("https://payments.example.com"),
)
if err != nil {
    log.Fatal(err)
}

payment, err := client.Payments.Create(ctx, paymux.CreatePaymentParams{
    Amount: 49900, Currency: "INR", OrderRef: "order-1048",
}, paymux.WithIdempotencyKey("order-1048-payment-v1"))
if err != nil {
    log.Fatal(err)
}

// Redirect or link from your own application. The SDK itself never redirects.
http.Redirect(w, r, payment.CheckoutURL, http.StatusSeeOther)
```

To open the selected provider from the merchant storefront instead, return
`payment.CheckoutURL` to that order's authenticated browser and pass it to the
[JavaScript checkout SDK](../js/README.md). Never expose the API key.

For no Paymux URL in the browser, set the App's `checkout_mode` to
`provider_action`, then resolve the public provider action on the backend:

```go
action, err := client.Payments.CheckoutAction(ctx, payment.ID,
    paymux.CheckoutActionParams{},
    paymux.WithIdempotencyKey("order-1048-checkout-v1"),
)
```

Return only `action` to the authenticated order owner and execute it with
`executeCheckout` from the JavaScript checkout SDK. Supply billing details
required by the App's tax policy during payment creation or in
`CheckoutActionParams`; the browser never receives a Paymux URL or API key.

Every mutation requires `WithIdempotencyKey`. Reuse a key only when retrying
the same logical operation. The client does not automatically retry mutations.

Available services cover payments, refunds, payment links, subscriptions,
invoices, credit notes, payouts, pagination, PDF downloads, typed API errors,
and Standard Webhooks verification. All methods accept `context.Context`.

## Webhooks

Read a bounded raw body, verify it, persist/deduplicate `event.ID`, and only
then act on it:

```go
verifier, err := paymux.NewWebhookVerifier(os.Getenv("PAYMUX_WEBHOOK_SECRET"))
event, err := verifier.Verify(r.Header, rawBody)
if err != nil {
    http.Error(w, "invalid webhook", http.StatusBadRequest)
    return
}

if event.Type == "payment.succeeded" {
    var payment paymux.Payment
    if err := event.DecodeObject(&payment); err != nil { /* handle */ }
}
```

The verifier uses the Standard Webhooks format and rejects signatures outside
the default five-minute tolerance. Unknown event types should be ignored safely.

## Versioning

This module is released from the main repository using directory-prefixed tags,
for example `sdk/go/v0.2.0`. It does not require a separate repository.
