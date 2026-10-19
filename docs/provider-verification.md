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
- [ ] **Webhook replay window** (`webhook.go`): subscription events are refused
  when their signed `created_at` is more than 48 hours from receipt (Razorpay
  retries for about a day; the exact schedule was read, not observed), and the
  event id is the body hash, not the unsigned `X-Razorpay-Event-Id`. Confirm a
  late retry still arrives within the window.
- [x] **Dispute evidence** (`disputes_respond.go`), from Razorpay's Contest a
  Dispute documentation (no secret in the last session, no test dispute): the
  named fields take lists of document id **strings** (the connector sent
  `{"id": ...}` objects; fixed), `others` is a list of `{type, document_ids}`,
  and a contest needs at least one document (a summary alone is now refused
  before anything is sent). Still unverified against a live dispute.
- [ ] **Settlements** (`settlements.go`): `settled_at` taken from the
  settlement's `created_at`; refund lines read from `amount`, falling back to
  minus `debit`; line types other than payment and refund reported as adjustments.
- [x] **Subscriptions** (`subscriptions.go`), from the documentation only:
  `total_count` times the interval may not run past 100 years (1200 monthly, 100
  yearly, 5200 weekly, 36500 daily; now refused before the call), `schedule_change_at`
  is `now` or `cycle_end` (cycle_end is used), and a UPI subscription cannot be
  updated (mapped to `ErrUnsupported`). Not run against the test API.

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
- [ ] **Mandates (SI), against PayU's recurring-payments guide** (read on
  2026-10-18 with every page it links: the three consent transactions,
  `pre_debit_notification_api`, `recurring_payment_api`, the status, modify and
  cancel pages, the mandate webhooks, the SI parameter details, the FAQ).
  Where the implementation differed from the guide, and what was done:
  - net-banking e-mandate registration used `pg=NB`; the guide says `ENACH`
    (fixed); UPI now sends `bankcode=UPI`;
  - a registration whose transaction succeeded but whose `payment_source` is
    `payu`, not `sist`, is PayU's "mandate failed, transaction successful": the
    customer was charged and no mandate exists. It was reported active; it is now
    a failed mandate, with the captured payment still reported so that it is
    refunded;
  - the mode of the registration (`mode`: CC, DC, UPI, ENACH) selects the status
    command (`check_mandate_status`, `upi_mandate_status`, `NB_mandate_status`),
    the cancel command (`mandate_revoke` for cards and net banking,
    `upi_mandate_revoke` for UPI; net-banking cancellation is offline and only
    acknowledged) and the var1 of notices and debits; it is remembered per
    account, or found by asking the status commands;
  - `pre_debit_SI`: cards send `invoiceDisplayNumber`; UPI AutoPay adds
    `mandateSeqNo` (execution 1 is the registration, notices start at 2, one number
    per debit); net banking takes no notice at all ("applies only to Cards and
    UPI"). The test environment refuses a notice without an invoice number before
    it looks at the mode, so UPI sends one as well. Answers: `E9254`/`E9256`
    (notice or execution already exists) are success, `E9260` (more than 30 days
    ahead) and `E9263` (wrong period) are `ErrTemporary`;
  - the notice must come at least 48 hours ahead (the guide; its FAQ says 24):
    new `Info.PreDebitMinHours`, and the subscription engine now keeps that gap
    between notice and debit, including for retries and a late notice;
  - `si_transaction` details: `pending` and `in progress` are not final, `failed`
    and a blank status are failures (blank was "processing"); the method is
    reported per mandate (it was always "card");
  - the mandate status vocabulary now covers the guide's lists: cards (`active`,
    `cancelled`, `discarded`, `deleted`, `failed`, `expired`, `paused`), UPI
    (`active`, `revoked`, `pause`), net banking (`SUCCESS`, `INITIATED`, `FAILED`,
    `CANCEL_SUCCESS`, `CANCEL_INITIATED`, `CANCEL_PENDING`, `CANCEL_FAILED`,
    `CANCEL_INITIATION_FAILED`); a failed cancellation leaves the mandate active;
  - UPI AutoPay is capped at Rs 15,000 per debit (`Info.MandateLimits`); the
    NPCI peak windows (10:00-13:00 and 17:00-21:30 IST) and the four-attempt cap
    the guide states are the engine's existing defaults (`upi_execution_windows`;
    three retries after the first attempt);
  - not built, by design: **modify** (cards need a new `_payment` with `si=3` and
    the customer's additional authentication; UPI `upi_mandate_modify` needs the
    customer's approval in the UPI app and works for collect registrations only;
    the guide's alternative is to revoke and register again, which is what Paymux
    does) and pause or unpause (the customer does these in the UPI app; the
    webhook is parsed but changes nothing in the engine).

  Run on the PayU test environment, with the test merchant, on 2026-10-18: the SI form (the
  hosted page shows the plan, and offers UPI and cards only, not net banking);
  `pg=ENACH` is answered "Selected payment option is not available at this
  moment" (the merchant has no e-NACH); UPI collect with `anything@payu` fails
  at the bank ("Bank was unable to authenticate"), and UPI intent needs a UPI
  app; cards (Mastercard 5123456789012346, Visa 4012001037141112, tokenised
  Mastercard 5506900480000008, with and without `user_credentials`, ADHOC and
  MONTHLY) are captured with 3-D Secure, but `payment_source` is `payu` and the
  mandate stays `pending`, so **no mandate could be registered**, and
  `pre_debit_SI` answers "Consent is Not Enrolled Or Not IN Active State" and
  `si_transaction` "Invalid/missing card token". Request shapes accepted by PayU:
  the registration form, `check_mandate_status`, `upi_mandate_status`,
  `NB_mandate_status` (unknown id answers), `mandate_revoke` (a pending mandate:
  "Mandate Revoked Successfully"), `upi_mandate_revoke`, `pre_debit_SI` with
  `mandateSeqNo`, `si_transaction` with `mandateSeqNo`. Ask the PayU account
  manager to enable Subscriptions/SI for the test merchant (the guide's
  prerequisite) and e-NACH and UPI AutoPay; then run the browser registration
  (`PAYU_BROWSER_MANDATE=card|upi|emandate`), `TestSandboxMandateCommands` with
  `PAYU_TEST_MANDATE_ID`, a pre-debit, and a debit after the notice window (the
  test environment's shorter window, if there is one, is not documented).
  Still unverified: the pre-debit lead on the test environment (48 hours), a
  captured debit, `payment_source=sist` on a real registration, and which
  `status` a paused UPI mandate answers.
- [ ] **Mandate webhooks** (`webhook_mandate.go`): cards post JSON
  (`MANDATE_MODIFICATION`, `MANDATE_CANCELLATION`, `MANDATE_DELETION`,
  `MANDATE_CANCELLATION_TOKEN_DELETION`) signed with
  `sha512(status|authpayuId|notificationType|billingAmount|paymentStartDate|
  paymentEndDate|message|eventDate|key|udf1..udf5|salt)`; UPI posts a form
  (`MANDATE_PAUSE`, `MANDATE_UNPAUSE`, `MANDATE_REVOKE`) signed with
  `status|action|authpayuid|dateTime|amount|endDate|salt`; net-banking e-NACH
  cancellations come as JSON with no hash and are only a hint, so the connector
  reads the mandate back with `NB_mandate_status` and reports that (in the background job: the inbound request names the event from the body alone, with no PayU call) PayU's
  integration team switches these on (UPI form, cards JSON, e-NACH through the
  merchant parameter `notifyMandateRevoke`); the UPI webhook has no retries. A
  debit's result is an ordinary payment notice keyed by the txnid Paymux sent.
  Posting notices signed with the test salt to the Paymux URL
  (`/hooks/pa_01m565tk8xfevv09tzde145ve6`) showed that the build deployed at
  paymux.shimul.me predates this change: the card notice (bad hash too) was
  accepted unverified, the UPI form was refused as "invalid signature". After a
  redeploy a signed notice must answer 200 and a bad hash 401. The hash formulas
  are from the documentation only: no real notice has arrived (the test
  merchant cannot register a mandate), and the guide does not say whether the
  UPI hash is SHA-512.
- [ ] **Verification debit** (`payu.go`): does PayU reverse the Rs 1 debit of a
  mandate registration itself (`RefundsVerificationDebit: true` assumes it does)?
- [ ] **Chargebacks** (`chargebacks.go`): built on the Chargeback API (read,
  accept, contest) behind its own token (`chargeback_token`,
  `chargeback_merchant_id`); the OAuth client credentials PayU issued (scope
  `create_payment_links` only) do not cover it. From the documentation and
  fakes; no chargeback token was available, so nothing ran live.
- [ ] **Settlements.** `get_settlement_details` timed out on the test environment;
  PayU is not a `Settler`.
- [x] **Refund listing.** PayU has no listing API Paymux uses (`ListRefunds` is
  unsupported); a refund made in the dashboard is found among the payment's
  actions (`check_action_status`, `var2=payuid`) when its webhook arrives.

## Zoho Payments

Verified on 2026-10-18 against **payments.zoho.in** (Zoho's live server) with
`ZohoPay.*` scopes. The user's own Zoho portal is the **sandbox**
(`paymentssandbox.zoho.in`, which needs `ZohoPaySandbox.*` scopes and
`soid=zohopaysandbox.<account id>` on the authorization); those scopes were not
used, so the sandbox server answered 401 "Not An Authorized User" and a payment
through the widget could not be completed. The payments and refunds the live
server returned looked like real ones (real UPI ids, receipt e-mails, gateway
reference numbers), so nothing was paid or refunded: sessions were created (they
expire in 15 minutes) and existing payments and refunds were read
(`sandbox_test.go`). Verified: the OAuth refresh against `accounts.zoho.in`,
creating payment sessions (`amount` as a number, answered as a string
"100.50"), fetching a session, a payment and a refund, unknown ids, a bad
refresh token, the shape of payments and refunds, the refund request's
validation, and the widget script and its call shape.
Fixed on the way:

- A payment session rejects `return_url` ("Invalid data provided."), so every
  checkout failed; it is a widget option only. `description` is required by the
  widget's `requestPaymentMethod`.
- Unknown ids are HTTP **400** with codes `resource_does_not_exist` /
  `invalid_payment_session`, not 404; they are now `ErrNotFound`.
- `payment_method` is an object (`{"type":"upi",...}`), which made every payment
  fetch fail to decode. Statuses `refunded` / `partially_refunded` are paid
  payments (with `amount_refunded`); a refund can be `canceled`.
- A session lists its payments as ids and statuses only; the payment is read, and
  both the session's and the payment's `reference_number` / `payments_session_id`
  must match.
- A refund needs `type` ("initiated_by_merchant") and a `reason` from a fixed
  list ("Please ensure that the type / reason is valid."); every refund was
  refused. The Paymux reference is stored in `meta_data`, and a retried refund is
  found in the payment's `refunds` before another is made (Zoho documents no
  idempotency).
- The webhook envelope is `event_object` with a numeric `event_id` and a
  millisecond `event_time`, and the signature timestamp is in milliseconds; the
  connector saw no event and rejected every signature.

- [x] **Payment session create / fetch, token refresh, error codes.**
- [x] **Payment, refund, session shapes; strict matching by session and reference.**
- [x] **Widget script URL** (`https://static.zohocdn.com/zpay/zpay-js/v1/zpayments.js`) and the call shape (`new ZPayments({account_id, domain, otherOptions:{api_key}})`,
  `requestPaymentMethod({amount, currency_code, payments_session_id, description})`),
  read from the script and Zoho's docs.
- [ ] **The OAuth Connect flow** (dashboard, *Connect with Zoho Payments*; written from Zoho's OAuth documentation and tested against a fake accounts
  server, not against Zoho): the authorize URL parameters (`response_type=code`, `access_type=offline`, `prompt=consent`, comma-separated `scope`), that
  a Server-based client accepts `<PAYMUX_BASE_URL>/dashboard/oauth/callback` as registered, the callback's `location` / `accounts-server` parameters and
  their values (`us`, `in`, `eu`, `au`, `jp`, `ca`, `sa`, `cn` are assumed), the token response (`refresh_token`, `scope` as comma- or space-separated),
  that the Self Client grant code exchanges without `redirect_uri`, that the default scopes (including `ZohoPay.payouts.READ` and the
  `ZohoPaySandbox.*` set) are all accepted, and the `accounts.zohocloud.ca` host of the Canadian data centre.
- [ ] **A completed payment through the widget.** Not completed. The widget loads
  in headless Chromium and then rejects with `processor_error`: its handshake
  request (`payments.zoho.in/zwaf/encryption/server/key`) answers 401. Zoho's
  documentation does not mention a domain allowlist; it lists the API key
  (Settings, Developer Space, Authentication Keys) as what lets the widget embed,
  says a regenerated key breaks authentication, and says sandbox and live have
  their own API keys, base URLs and session ids. What the widget does with
  `is_test_mode` was observed: it asks the **sandbox** host (it said
  `invalid_payment_session` for a session made on the live host). So the likely
  cause of the failure on a sandbox account is the environment mix: sandbox
  needs all of `sandbox: true` on the Paymux account (sandbox host for the
  sessions, `is_test_mode` for the widget), the **sandbox** API key from the
  sandbox portal's Developer Space, and a token with `ZohoPaySandbox.*` scopes.
  Re-check on the sandbox account once it is connected. Zoho's test values: UPI
  amounts up to 500 succeed (222, 333, 444 pick the instrument), 501 to 1,000
  fail; cards 4111 1111 1111 1111 (Visa), 5299 9202 1000 0277 (Mastercard),
  6071 4898 7654 3212 (RuPay), any future expiry, any CVV, cardholder name
  "Failure" fails authentication; net-banking "Success Test Bank" / "Failure Test
  Bank" / "Refund Failure Test Bank"; refunds up to 300 succeed, 301 to 500 fail.
- [ ] **Refunds that succeed.** The account had no payment to refund (only
  refunded ones; refunding a customer's real payment was out of the question).
  Verified: validation, an already-refunded payment (400
  `payment_already_refunded`), an unknown payment. Unverified: that `meta_data`
  is stored and returned on a refund (the retry lookup depends on it), the
  success response, and the over-refund code.
- [ ] **Webhooks.** Zoho documents them (envelope above, header
  `X-Zoho-Webhook-Signature: t=<ms>,v=<hex>`, HMAC-SHA256 of `t + "." + body`)
  and manages them through `/webhooks` (needs the `ZohoPay.settings.*` scopes;
  `GET /api/v1/webhooks` answers 401 "Not An Authorized User" with the current
  token). Each webhook has its own signing key, returned when it is created and
  shown in the web app. No live delivery was received, so which of the keys the
  user holds signs real deliveries is not proven. Zoho's go-live checklist says:
  register the endpoint under Settings, Developer Space, Webhooks and store the
  signing key shown there (that one signs `X-Zoho-Webhook-Signature`); the
  "signing keys" generated in Developer Space verify the **widget response** and
  the **payment link return URLs**. Sandbox webhooks are registered in the sandbox
  portal (Settings, Webhooks) and signed with the sandbox credentials.
- [ ] **Mandates** (`mandate.go`), rewritten to Zoho's real flow: a customer
  (Customers API, cached), an enrollment payment session (`type:
  mandate_enrollment`), `GET /mandates/{id}`, `POST /mandates/notify` (T+1 to T+3
  days, IST), an execution session (`type: mandate_execution`) plus `POST
  /mandates/execute`. The old endpoints never existed. Built from the
  documentation and unit tests with fakes; the sandbox tests
  (`sandbox_mandate_test.go`, `-tags sandbox`, `ZOHO_TEST_*`, `sandbox: true`)
  **have not run**: the sandbox refresh token is not connected yet. UPI
  unattended debits are capped at Rs 15,000 and refused in NPCI's closed windows
  (`ErrTemporary`); Zoho has no revoke API (`RevokeMandate` is `ErrUnsupported`,
  and the engine treats that as done). Unverified until the sandbox runs: the
  enrollment/execution session shapes, the mandate id in the payment, the notify
  answer.
- [x] **Verification debit** (help pages, read 2026-10-18): the Rs 1 charged to
  authorise a future-dated mandate "will be settled in your next payout", so Zoho
  does **not** reverse it. `RefundsVerificationDebit` is now false: Paymux
  refunds it through the refund API (to be confirmed on the sandbox that a Rs 1
  mandate payment can be refunded). A same-day mandate debits its first amount,
  minimum Rs 6; enrollment accepts exactly Rs 1 or at
  least Rs 6.
- [x] **What Zoho's mandate help pages settle**
  (`/help/mandates/`, `/help/mandates/view/`, and the API reference): UPI
  AutoPay only (no eNACH or card mandates); frequency `as_presented` (what
  Paymux uses), `weekly` ... `yearly`; `amount_rule` variable or fixed, a
  variable mandate needs `max_amount`; end date defaults to 30 years; unattended
  debits above Rs 15,000 need the customer's UPI PIN (Info cap stays Rs 15,000);
  executions are **not automated** (Paymux triggers each one) and only run
  outside NPCI's closed windows (10:00-13:00, 17:00-21:30 IST); the customer
  pauses or cancels in their UPI app, and there is no cancel API, so
  `RevokeMandate` = `ErrUnsupported` is safe: Paymux simply never executes a
  revoked mandate, and a debit on an inactive one is refused client-side.
  Mandate statuses in the dashboard: Active, Inactive (revoked, paused or
  expired), Processing, Paused. Pre-debit notice: the customer must be told at
  least 24h before a debit; Zoho either sends the notice when `notify` is called
  (execution date T+1 to T+3) or itself when the mandate is executed. `notify`
  is therefore optional, but keep `predebit_lead` at most 72h (default 48h) so
  the notice is accepted. Failed debits: an execution session lives 3 days and
  takes up to 3 attempts at least 1h apart; a notice is bound to its session,
  so Paymux (one session per attempt, `max_retry_count` 1) sends a new notice
  per retry (`PreDebitPerAttempt`). Not stated anywhere: per-debit minimum for a
  later debit, what `inactive` hides (paused vs expired is only visible in the
  dashboard's separate Paused tab; the API status for a paused mandate is
  unverified, it maps to pending), whether an automatic notice at execution
  satisfies the 24h rule (then the debit might be refused).
- [ ] **Hosted checkout** (`hosted_checkout_url`): the session returns an
  `access_key` and takes `configurations.hosted_checkout_parameters`
  (`success_url`, `failure_url`); the URL the customer is sent to is not
  documented, so the setting stays a guess.
- [ ] **Scopes** not yet granted: `ZohoPay.settings.*` (webhooks),
  `ZohoPay.payouts.READ` (payouts), `ZohoPay.customers.CREATE` / `.READ`
  (mandates), `ZohoPay.payments.UPDATE`.

- [x] **The dashboard's Test button** (`ConnectionTester`): refreshes the token and
  lists one payment; a refused call is retried on the other server with the same
  token, and a success there is reported as "wrong server (sandbox vs live)".
  Covered by unit tests; against the real API the refresh was refused
  (`invalid_client`) when it was last run, so the message for a refused refresh
  token (which names the accounts host and the usual causes) is what was seen.

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
- [x] **Subscriptions** (`subscriptions.go`), run on the sandbox (2026-10-18):
  the inline renewal plan works, but **`initial_price` on a non-trial renewal
  plan doubled the first charge** (fixed: it is omitted; a $9.99 first payment
  was verified); a membership carries `current_period_start/end` and a
  `renewal_price` object; status "canceling" means active until the period ends.
  Cancellation and the `membership.*` event names are from the documentation.
- [x] **Payments and refunds**, re-run on the sandbox: refunds above the
  remainder are clamped by Whop with a 200, so the connector reads the payment
  first and refuses with `ErrNotRefundable`; refund objects are `rf_...` ids
  with the currency inside `amount`.
- [ ] **Disputes** (`disputes.go`): list, fetch, `update_evidence` (files are
  uploaded to `POST /files`, then PUT to the returned `upload_url`) and
  `submit_evidence`; Whop has no accept call (`ErrUnsupported`). Built from the
  documentation with fakes. The sandbox key lacks the `payment:dispute:read`
  scope, so no live dispute call succeeded: add that scope (and the write scope
  for evidence) to the Whop key to verify.

## Cashfree

Written entirely from documentation (`cashfree.go`).

- [ ] **Rate limits:** the per-endpoint limits (the connector stays at 10 a second).
- [ ] **Webhook secret:** does Cashfree sign webhooks with the same secret key as
  the API (`WebhookSecret` defaults to the client secret)?
- [ ] **Customer phone:** the order API requires `customer_phone`; a placeholder
  is sent when there is none. How does the live API treat it?
- [ ] **Webhook timestamp tolerance:** how old may a retried delivery be? The
  signed `x-webhook-timestamp` must be within 48 hours of receipt (milliseconds
  or seconds; `webhook.go`) because the signature covers it and nothing else
  bounds a replay; the retry schedule is not documented, so the window is a
  guess. The event id is the body hash: `x-idempotency-key` is not signed and is
  ignored.
- [ ] **Settlements** (`settlements.go`): request filters (`start_date`,
  `end_date`, `cf_settlement_ids`), cursor pagination, field names
  (`amount_settled`, `service_charge`, `event_type`, `event_service_charge`,
  `event_service_tax`) and status strings.

## Subscriptions engine

- [ ] **Minimum charge per provider** (`Info.MinChargeMinor`; a proration credit
  never takes a renewal below it, and an extra proration charge under it waits
  for the next renewal). Values in the code: Razorpay, PayU, Cashfree and Zoho
  INR 100 (Rs 1; Zoho publishes no minimum, only Rs 1 for a future-dated registration and Rs 6 for a same-day one),
  Stripe per currency as in its "minimum and maximum charge amounts" table
  (USD 50, EUR 50, GBP 30, INR 50, AUD 50, CAD 50, SGD 50, HKD 400, JPY 50,
  CHF 50, SEK 300, NOK 300, DKK 250, NZD 50, AED 200, BRL 50, MXN 1000,
  PLN 200, CZK 1500, MYR 200, THB 1000; KRW has none stated). Check each against
  the provider's current documentation and, for a recurring debit, against a real
  mandate debit: a mandate may have its own floor. Whop has none stated.
- [ ] **Verification debit reversal** (`verification.go`): see PayU and Zoho above.
  Until it is known, the org setting `verification_debit_fallback` (default
  `refund_after_72h`) refunds the debit explicitly 72 hours later if the provider
  still shows it unrefunded; confirm which of the two a real registration does,
  then set `never` if the provider always reverses it.

## GST e-invoice

- [ ] **Schema** (`internal/invoice/einvoice/schema/einvoice-1.1.json`): the
  schema was transcribed from the portal's published field list; diff it against
  the portal's own schema file, and upload one invoice and one credit note
  (`CRN`, with `RefDtls.PrecDocDtls`) to the sandbox portal.
- [ ] **Signed QR code** (`internal/invoice/irp`, `einvoice.go`): with the portal's
  public key(s) in the org setting `einvoice_irp_keys`, Paymux verifies the
  RS256 signature strictly (alg exactly RS256; the token's `kid` / `x5t` /
  `x5t#S256` must name a configured key; no embedded keys). **No key is shipped:**
  the NIC sandbox and production keys are published by the portal, and a key
  typed from memory would be worse than none. Download the certificate(s) from
  the portal's documentation, put the PEM in the setting, and record a real
  `SignedQRCode`: check which header fields it carries (`x5t`? `kid`?) and that
  the identifiers match the way `ParseKeys` computes them (x5t is the base64url
  SHA-1 of the certificate; kid is the hex of its subject key id or of the SHA-1
  of its public key). Without keys only the claims are checked.
- [ ] **`RefClm`** (`einvoice/einvoice.go`): sent as `N` under a LUT; for an export
  that paid IGST it follows the org setting `einvoice_igst_refund_claim`
  (default `auto` = `Y`, the usual claim). Confirm with whoever files your
  returns; the portal validates it against the export details.

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
