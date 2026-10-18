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

Verified against Stripe test mode on 2026-10-18 (`connectors/stripe/sandbox_test.go`,
`sandbox_flows_test.go`, `sandbox_more_test.go`): Checkout Sessions in INR, USD,
JPY, EUR and HUF, retry with the same Idempotency-Key, a browser-paid session
(INR, card 4242), strict matching by reference, unknown ids and bad keys,
refunds (create, retry, fetch by id and by reference, over-refund), disputes
(`pm_card_createDispute`: list, fetch, evidence, accept), a subscription
(fetch, `UpdateSubscriptionAmount`, cancel at period end and now), balance
transactions with fees, and the verification and parsing of every kind of real
event from `/v1/events`. Fixed on the way: evidence files are sent as their real
type (the Files API takes only PDF, JPEG and PNG), Checkout rejects
`subscription_data[cancel_at]` (a plan with a total count is now refused), and
invoices of newer API versions keep the subscription under
`parent.subscription_details`.

- [x] **Currencies** (`amounts.go`): INR, USD, JPY, EUR, HUF accepted.
- [ ] **Currencies, the rest** (`amounts.go`): KWD and the other three-decimal
  currencies, ISK, UGX, TWD: the test account cannot charge them. Check the
  lists against https://docs.stripe.com/currencies.
- [ ] **Checkout methods** (`stripe.go`): `Info.Methods` is a guess; the real
  methods depend on the account's Checkout settings.
- [x] **Subscription checkout** (`subscriptions.go`): `trial_end` more than 48
  hours ahead is accepted; a nearer start is ignored; `cancel_at` is not a
  Checkout parameter.
- [ ] **Subscriptions through a paid Checkout.** The subscription lifecycle was
  verified on a subscription made through the API; a subscription-mode Checkout
  Session was created but not paid.
- [x] **Changing a subscription's amount** (`UpdateSubscriptionAmount`).
- [x] **Disputes** (`disputes.go`): evidence fields and the second `submit=true`
  update.
- [ ] **Payouts** (`settlements.go`): the test account has no payout bank account
  (the API cannot add one in test mode), so no payout existed. The `arrival_date`
  filter and the balance-transaction `payout` filter are accepted; the
  `trace_id` UTR and `fee_details` of type `tax` were not seen.
- [ ] **Webhook delivery.** Events were fetched from `/v1/events` and signed
  locally; no endpoint received a live delivery. The payload follows the
  endpoint's API version: create the endpoint with 2024-06-20, because newer
  versions leave the payment intent off `invoice.paid`, so a renewal cannot be
  reported as a payment from that event.
- [ ] **3-D Secure and async methods** (`checkout.session.async_payment_*`): not
  exercised.

## PayU

Verified against the PayU test environment (test.payu.in, UAT) on 2026-10-18
(`connectors/payu/sandbox_test.go`, `sandbox_browser_test.go`): the hosted
checkout form post (PayU accepted the request hash and showed the payment
page with the right amount), a card payment (3-D Secure simulator, OTP 123456)
and a net-banking payment (the Test Bank simulator), `verify_payment` for a
captured, a pending and an unknown txnid, bad key and bad salt, refunds
(`cancel_refund_transaction`: queued, a reused token, an over-refund, two refunds
of one amount within five minutes), `check_action_status`, the
`get_Transaction_Details` listing, and that the standing-instruction commands
exist. Fixed on the way:

- `check_action_status` nests its answer as `<request_id>.<request_id>`, takes a
  request id (not a token), and with `var2=payuid` lists every action of a
  payment, the capture included; `FetchRefund` follows that, by request id or by
  token among a payment's actions, and never takes the capture or a lone
  unrelated entry.
- A refund token sent twice fails with error 227; that is now the refund already
  made, returned, not a refusal.
- The transaction listing names the PayU id `id`, reports the unmapped status
  (`captured`, `auth`), has `mer_service_fee`/`mer_service_tax`, and repeats the
  payment as an extra row for each refund (told apart by `request_id`).
- PayU's modes (CC, NB, UPI...) are reported as card, netbanking, upi...; a
  successful payment no longer carries PayU's `E000`/`NO ERROR` as an error.
- `check_mandate_status` and `mandate_revoke` need a `requestId` and answer in
  `message`, not `msg`.

An unknown txnid is answered with HTTP 200, `status: 0` and the txnid echoed back
with `status: "Not Found"`. PayU cannot tell it from a checkout the customer has
not submitted, and Paymux only asks about txnids it issued, so the connector
reports it as a payment still waiting for the customer (`requires_action`), not
`ErrNotFound`.

- [x] **Hosted checkout form post, request hash, test URL** (`payu.go`).
- [x] **Command hash and `verify_payment`**, bad key and salt (`Invalid Hash.`,
  200) as `ErrInvalidCredentials`.
- [x] **Refund status** (`api.go`, `FetchRefund`).
- [ ] **Return and webhook reverse hash.** The test merchant's response page
  (`test.payu.in/.../CommonPgResponseHandler`, `testtxncdn.payubiz.in/newResponse`)
  answered 504 Gateway Time-out after every payment (card and net banking, three
  attempts, a local and a public `surl`), so the browser was never sent back with
  a PayU-signed response, and nothing could reach a webhook. The payments
  themselves were captured (`verify_payment`). Re-check the reverse hash
  (`additionalCharges` first when present) with a real return once PayU's test
  environment completes the redirect; `TestSandboxVerifyCapturedReturn` verifies a
  captured body.
- [ ] **Webhooks.** What PayU sends for a refund made in the dashboard, and its
  field names. Set up in the PayU Dashboard (Webhooks, under Settings): the URL
  is the account's Paymux URL, `<public base>/hooks/<provider account id>`
  (shown on the account page), for the events payment success, payment failure,
  and refund (initiated and completed); dispute events are not used. Paymux
  verifies payment events by the reverse hash and treats refund events as an
  unsigned hint that it confirms with `check_action_status`. Not exercised: the
  dashboard screens and a live delivery.
- [ ] **Mandates** (`mandate.go`): the SI hash layout, the `pg` code per method,
  the var1 of `si_transaction` (the current layout is refused with "Mandatory
  column missing"; the field names are unknown) and the JSON of the successful
  responses. The commands `pre_debit_SI`, `check_mandate_status` and
  `mandate_revoke` exist, with the `requestId` fix above.
- [ ] **Verification debit** (`payu.go`): does PayU reverse the Rs 1 debit of a
  mandate registration itself (`RefundsVerificationDebit: true` assumes it does)?
- [ ] **Chargebacks.** The Chargeback API (a separate token, a 20-day window) is
  not built, so PayU disputes are entered by hand. The OAuth client credentials
  PayU issued (scope `create_payment_links` only) do not cover it.
- [ ] **Settlements.** `get_settlement_details` timed out on the test environment;
  PayU is not a `Settler`.
- [x] **Refund listing.** PayU has no listing API Paymux uses (`ListRefunds` is
  unsupported); a refund made in the dashboard is found among the payment's
  actions (`check_action_status`, `var2=payuid`) when its webhook arrives.

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
