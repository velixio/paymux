# Subscriptions and invoicing

Recurring collection and document issuance depend on the selected connector and the application's configuration. Check both the [capability matrix](capabilities.md) and [provider verification status](provider-verification.md). The existence of an API endpoint does not imply every provider supports it.

## Recurring billing models

| Model | Who collects renewals? | What Paymux tracks |
|---|---|---|
| Provider-managed | The provider manages its billing schedule. | Subscription state, verified renewal outcomes, associated payments, and events. |
| Merchant-triggered | Paymux requests charges against an authorized mandate. | Mandate eligibility, scheduled collection, pre-debit handling, retries, and resulting records. |

A subscription is not the same as a mandate. The customer must complete authorization before a merchant-triggered agreement can charge successfully.

## Create and activate

Use `POST /v1/subscriptions` with customer, amount, currency, and interval. The response starts incomplete and includes a checkout URL for the customer. Authorization must finish within the setup window, currently 24 hours.

```json
{
  "customer": {"external_ref":"customer-1048","email":"buyer@example.com"},
  "amount": 49900,
  "currency": "INR",
  "interval": "month",
  "interval_count": 1,
  "description": "Pro plan"
}
```

Send this to the hub from your backend using a subscriptions write key, JSON content type, and a stable idempotency key. This is an example body, not a full create request. Consult [OpenAPI](openapi.yaml) for optional start dates, trial periods, return URL, mandate method, and allowed fields.

**Choosing the provider.** Routing picks the account, as for a payment, and the same two fields steer it: `allowed_connectors` restricts routing to the listed connectors and `country` is matched by routing rules. Both are kept on the subscription. A connector that cannot bill the subscription is never picked: a provider that bills subscriptions in fewer currencies than it takes payments in (Razorpay: INR only) is skipped for other currencies, and if no listed provider can bill it the call is refused with 422 `no_route` at creation, never at the customer's checkout.

Track `incomplete`, `incomplete_expired`, `trialing`, `active`, `past_due`, `canceled`, and `ended` as separate outcomes. Do not grant indefinite access merely because authorization began. Tie entitlements to your own verified billing policy and event processing.

## Billing details of the subscriber

The hosted checkout (`/s/{id}`) asks the buyer for the billing details the pay page asks for (State for India, the country for everyone paying in a currency other than rupees, an address from Rs 50,000, and an optional GSTIN with its checksum checked), following the org or app setting `billing_details`, and only when the customer does not already have them. Send `customer.billing` (`state_code`, `country`, and `gstin` and `legal_name` for a business) with the subscription to skip the page. The details are kept on the customer: the first invoice, every renewal invoice and every prorated charge are taxed from them (a State in India: CGST and SGST in the supplier's State, IGST elsewhere; a GSTIN: a B2B invoice; a country outside India: an export, zero-rated under a valid LUT when paid in a foreign currency, with IGST otherwise). Without them the invoice defaults to a sale in the supplier's State and is flagged `pos_defaulted`.

## Renewal failures and changes

A failed renewal can move a subscription past due. Dunning and mandate-specific limits affect what can be retried. Inspect the configured schedule and provider-specific constraints rather than issuing unlimited manual charges.

`POST /v1/subscriptions/{id}/update` changes the future amount and can request proration for supported merchant-triggered billing. A requested immediate charge may be deferred to the next renewal when the mandate or authorization cannot support it; read the returned `proration` outcome and reason. Provider-managed subscriptions reject unsupported proration rather than emulating it invisibly.

Some providers keep the plan of a subscription paid by a given method: Razorpay cannot change the plan of a card or UPI subscription. The update then answers 422 `unsupported` with the way out (cancel it and start a new one), before the provider is asked; `connector.Info.CanChangeSubscriptionAmount(method)` says which methods can change. A request the provider itself refuses (an amount or plan it does not allow) answers 422 `provider_rejected` with the provider's message.

`POST /v1/subscriptions/{id}/payment_method` starts payment-method replacement where supported. Cancellation has its own operation and state rules. Read each response instead of assuming every connector can update an active agreement in place.

The mandate amount limits in the current implementation are described in OpenAPI and connector declarations. This guide describes product behavior; verify current provider and regulatory requirements before configuring a live billing program.

## GST invoice setup

To issue invoices, create a tax entity with supplier details, assign the application's tax profile, and enable invoice issuance. Configure the supply classification, relevant customer billing information, tax rate, pricing mode, and document series before taking payments.

- With tax-inclusive prices, the collected total contains the tax calculation.
- With tax-exclusive prices, the submitted amount is the taxable value and applicable tax is added to the payment total.
- Invoice amounts reflect the amount received. Check the returned payment total when comparing it to your order.
- Merchant-of-record payments such as Whop are handled differently; Paymux does not issue its own GST invoice for those payments.

Invoices are immutable once issued. Corrections use credit notes, including eligible refund-related corrections. Invoice creation and PDF generation are separate: a document can exist before its PDF is ready. The PDF endpoint currently returns a not-ready response with `Retry-After: 5` while rendering is pending.

## Retrieve documents

Use `/v1/invoices` and `/v1/credit_notes` to list documents, retrieve a document by ID, or request its `/pdf` endpoint using an invoice-read key. Invoice events can contain signed hosted and PDF URLs; treat these as customer-access capabilities rather than public identifiers.

The dashboard supports operational document inspection, reports, and applicable reissue operations. Reissuing a delivery is not permission to mutate an issued tax record.

## E-invoice and PDF signing limits

Paymux's e-invoice workflow is **offline export**, not automatic IRP registration. Export the supported INV-01 JSON, submit it through the relevant portal process, then record the IRN, acknowledgement, and signed QR data. Review the runbook for validation and IRP key settings.

An uploaded tax-entity PKCS#12 certificate can digitally sign newly rendered PDFs. Existing PDFs are not rewritten. Trusted timestamping and long-term revocation data are not provided by this workflow. See the [runbook](runbook.md) for supported certificate formats, fallback behavior, and verification limits.

## Payouts are separate

Payouts send money out; they are not invoice settlement records or customer refunds. `/v1/payouts` requires explicit payout scopes and an eligible payout-capable account linked to the app. Beneficiary details are encrypted and displayed masked. The endpoint records a pending request and submits it asynchronously; follow its status and events before considering it paid.

For detailed document operations, backups, and billing recovery, see the [runbook](runbook.md). For permissions and secrets, see [security](security.md).
