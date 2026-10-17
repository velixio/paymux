# Provider verification

Most of Paymux is tested against fakes that are written from each provider's
documentation. Real APIs differ from their documentation (Whop's list filter,
which the real API ignores silently, is the example that taught this), so
everything below was **written from documentation or knowledge and has not been
run against a live sandbox or account**. Each line says where it is in the code
and what to look at. Do them when the credentials exist, ticking the box and
fixing the code (with a test built from the real response) as you go.

The matching `// TODO(spec)` comment is at the place named. Every other
`TODO(spec)` in the code is one of these. A good first pass for any provider:
run its `sandbox_test.go` (skipped without credentials; see
[Writing connectors](connectors.md)) and read what it logs.

## Every connector

- [ ] **Ownership of what a payment id points at.** `FetchPayment` with a
  payment id taken from a return URL or a notification must be `ErrNotFound` for
  a payment of another order, checkout or reference, and a lookup by reference
  must verify the reference of what the provider returned (`internal/checkout`
  also checks the answer, but the connector is the first line). Try it with the
  id of a payment made outside Paymux in the same account.
- [ ] **Idempotency of refunds and payouts.** Send the same refund twice with the
  same reference and confirm one refund is made (Razorpay `X-Refund-Idempotency`,
  Whop `Idempotency-Key`, PayU request token, Stripe `Idempotency-Key`; Zoho has
  none documented, see below).
- [ ] **Provider-made refunds.** Make a refund in the provider's own dashboard and
  confirm the webhook (or the daily reconciliation) books it as
  `initiated_by=provider` once, with the credit note.
- [ ] **Amounts and currency.** The smallest unit and currency of every amount
  returned (payments, refunds, disputes, settlements) match what Paymux stored.

## Razorpay

- [ ] **Payment by id** (`connectors/razorpay/payments.go`): a payment's `order_id`
  and the order's `receipt` are the fields used to prove a payment is ours.
  Confirm both are always present on authorized and captured payments.
- [ ] **Order lookup by receipt** (`GET /v1/orders?receipt=`): confirm the filter
  is honoured. Paymux checks each order's `receipt` anyway.
- [ ] **Dispute evidence** (`disputes_respond.go`): the field list, the document
  reference shape (`{"id": ...}`) and the `others` shape.
- [ ] **Settlements** (`settlements.go`): `settled_at` taken from the
  settlement's `created_at`; refund lines read from `amount`, falling back to
  minus `debit`; line types other than payment and refund reported as adjustments.
- [ ] **Subscriptions** (`subscriptions.go`): the `total_count` limits per period
  and the `schedule_change_at` semantics of `PATCH /v1/subscriptions`.

## Stripe

The whole connector has been written from documentation (`stripe.go`).

- [ ] **Currencies** (`amounts.go`): the zero-decimal and three-decimal lists and
  the ISK, UGX, HUF and TWD special cases, against https://docs.stripe.com/currencies.
- [ ] **Checkout methods** (`stripe.go`): `Info.Methods` is a guess; the real
  methods depend on the account's Checkout settings.
- [ ] **Subscription checkout** (`subscriptions.go`): Checkout requires `trial_end`
  at least 48 hours ahead and `cancel_at` in the future.
- [ ] **Changing a subscription's amount** (`subscriptions.go`,
  `UpdateSubscriptionAmount`): replacing the item's inline price with
  `price_data` (existing product, currency and interval) and
  `proration_behavior=none` is accepted, the next invoice carries the new
  amount, and nothing is prorated.
- [ ] **Disputes** (`disputes.go`): the evidence field names, and that
  `submit=true` may be sent as a second update.
- [ ] **Settlements** (`settlements.go`): the payout list filter `arrival_date`,
  the `balance_transactions` payout filter, `fee_details` entries of type `tax`,
  `payout.trace_id.value` as the bank reference, and that expanding `data.source`
  gives the PaymentIntent of a charge.

## PayU

- [ ] **Refund status** (`api.go`, `FetchRefund`): the shape of
  `transaction_details` in `check_action_status`. A payment with several refunds
  lists them all; Paymux picks the entry whose `request_id` is asked for and
  refuses to guess. Confirm the nesting and that `request_id` is always present.
- [ ] **Mandates** (`mandate.go`): the SI hash layout (`si_details` between the
  `udf` block and the salt), the `pg` code per method, the command names
  (`pre_debit_SI`, `si_transaction`, `check_mandate_status`, `mandate_revoke`)
  and the JSON of their `var1` and responses.
- [ ] **Verification debit** (`payu.go`, `internal/subscriptions/verification.go`):
  does PayU reverse the Rs 1 debit of a mandate registration itself
  (`RefundsVerificationDebit: true` assumes it does)?
- [ ] **Chargebacks.** The Chargeback API (a separate token, a 20-day window) is
  not built, so PayU disputes are entered by hand. Build `DisputeReader` once
  there is access.
- [ ] **Refund listing.** PayU has no listing API Paymux uses (`ListRefunds` is
  unsupported); a refund made in PayU's dashboard reaches Paymux through its
  (unsigned) webhook only, which Paymux treats as a hint and fetches. Check what
  PayU sends for such a refund.

## Zoho Payments

The sandbox has to be enabled by Zoho support (`zoho.go`).

- [ ] **Constants:** the widget script URL, the hosted-checkout URL scheme and the
  webhook event names.
- [ ] **Widget launcher** (`web/templates/pay_sdk.html`): the `ZPayments`
  construction and `requestPaymentMethod` call, what the promise resolves to
  (a `payment_id`?) and the `widget_closed` error code. The page returns to
  Paymux's return URL, which fetches the payment, so a wrong guess about the
  resolved value costs only the `payment_id` hint.
- [ ] **Payment by id** (`FetchPayment`): the `reference` field on a payment is
  our reference and `payments_session_id` is our session; both are used to
  refuse a payment that is not ours.
- [ ] **Refund idempotency:** does Zoho treat `reference_number` as a
  deduplication key? If not, a refund whose response is lost can be made twice.
  Paymux detects the duplicate in reconciliation and flags it, but cannot
  prevent it.
- [ ] **Mandates** (`mandate.go`): the endpoints (`/api/v1/mandates`, `.../charges`,
  `.../revoke`), their request fields and the mandate statuses.
- [ ] **Verification debit:** does Zoho reverse the Rs 1 debit itself?

## Whop

- [ ] **Payments** (`whop.go`): the status and substatus values, the filter
  used to find a checkout configuration's payments (the live API ignored one
  filter silently), the refund response shape, and which fields tie a payment to
  our checkout: `checkout_configuration_id` and `metadata.ref` are used to refuse
  a payment that is not ours.
- [ ] **Refunds.** Whop has no refund object: `FetchRefund` is synthesized from
  the payment's cumulative `refunded_amount`. With two partial refunds in flight
  the amounts can be reported together; Paymux then refuses the mismatch and the
  refund stays pending. Check what the Resolution Center sends.
- [ ] **Subscriptions** (`subscriptions.go`): the inline renewal plan of
  `POST /checkout_configurations`, `billing_period` in days, `trial_period_days`,
  membership statuses and fields, `POST /memberships/{id}/cancel` and its
  `cancellation_mode`, and the `membership.*` event names.
- [ ] **Disputes.** The dispute evidence API is not built.

## Cashfree

Written entirely from documentation (`cashfree.go`).

- [ ] **Rate limits:** the per-endpoint limits (the connector stays at 10 a second).
- [ ] **Webhook secret:** does Cashfree sign webhooks with the same secret key as
  the API (`WebhookSecret` defaults to the client secret)?
- [ ] **Customer phone:** the order API requires `customer_phone`; a placeholder
  is sent when there is none. How does the live API treat it?
- [ ] **Webhook timestamp tolerance:** how old may a retried delivery be? (No
  tolerance is applied today, by design; see `webhook.go`.)
- [ ] **Settlements** (`settlements.go`): request filters (`start_date`,
  `end_date`, `cf_settlement_ids`), cursor pagination, field names
  (`amount_settled`, `service_charge`, `event_type`, `event_service_charge`,
  `event_service_tax`) and status strings.

## Subscriptions engine

- [ ] **Minimum debit per provider** (`internal/subscriptions/proration.go`): a
  proration credit never takes a renewal below 1 minor unit; a provider's own
  minimum debit is not known to Paymux.
- [ ] **Verification debit reversal** (`verification.go`): see PayU and Zoho above.

## GST e-invoice

- [ ] **Schema** (`internal/invoice/einvoice/schema/einvoice-1.1.json`): the
  schema was transcribed from the portal's published field list; diff it against
  the portal's own schema file, and upload one invoice and one credit note
  (`CRN`, with `RefDtls.PrecDocDtls`) to the sandbox portal.
- [ ] **Signed QR code** (`internal/invoice/einvoice.go`): Paymux checks that the
  token names the right IRN, document and seller but does not verify its RS256
  signature, because it does not ship the IRP's certificate.
- [ ] **`RefClm`** (`einvoice/einvoice.go`): always `N`. An exporter paying IGST
  who claims the refund needs `Y`; there is no setting for it.

## Digital signature (PAdES)

- [ ] **Open a signed invoice in Adobe Acrobat** (and one other reader) with a
  real certificate from an Indian certifying authority: the signature should be
  valid, the document unmodified since signing, and the identity trusted once the
  CCA India roots are in the reader's trust store.
- [ ] **PKCS#12 encryption** of the files your authority issues (3DES/RC2 are
  read; AES needs a re-export, see the [runbook](runbook.md)).
- [ ] **Timestamp and revocation** (PAdES-T and -LT) are not added; decide
  whether an auditor needs them.

## Payouts

No built-in connector pays out (only the simulator, the test fake and `remote`
connectors do). When one is built, check its payout idempotency token, its
payout webhook mapping (`NotifyPayout`) and how a reversed payout is reported.
