# Writing a Paymux connector

A connector is a Go package that teaches Paymux to talk to one payment
provider. It imports only the standard library and `pkg/...`. It never touches
the database, the job queue or the HTTP server: everything it needs arrives
through `connector.Deps`, and everything it returns is a plain value.

The SDK lives in `pkg/connector` (godoc has the full reference) and its test
kit in `pkg/connector/connectortest`. Read `connectors/whop` for a small
complete example and `connectors/payu` for a mandate-based one.

## 1. The shape of a connector

```go
package acme

func init() { connector.Register("acme", New, info) }

var info = connector.Info{ /* see section 2 */ }

func New(cfg json.RawMessage, deps connector.Deps) (connector.Connector, error) {
	// decode and validate cfg, keep deps; no network calls here
}
```

`New` is called once per provider account with that account's decrypted JSON
config and a `Deps`:

| field    | use |
|----------|-----|
| `HTTP`   | the only way to reach the provider. Built by Paymux with timeouts, GET retries, log redaction and a per-account rate limit. Never use `http.DefaultClient`. |
| `KV`     | a small per-account key/value store with optional TTL (cached provider plans, tokens). |
| `Clock`  | use `deps.Now()`, never `time.Now()`, so tests control time. |
| `Logger` | structured logging; secrets are never logged by the HTTP client, do not log them yourself. |
| `URLs`   | `ReturnURL(providerRef)` and `WebhookURL()`: the URLs you hand to the provider. |

### Required interface

```go
type Connector interface {
	Info() Info
	CreateCheckout(ctx, CheckoutRequest) (CheckoutResult, error)
	HandleReturn(ctx, ReturnRequest) (ReturnResult, error)
	VerifyWebhook(ctx, WebhookRequest) error
	ParseWebhook(ctx, WebhookRequest) ([]Notification, error)
	FetchPayment(ctx, PaymentRef) (PaymentSnapshot, error)
}
```

* `CreateCheckout` starts a payment attempt and returns a `NextAction`
  (`redirect`, `form_post` or `sdk`). `CheckoutRequest.ProviderRef` is the
  attempt's unique reference: send it to the provider as its merchant
  reference or idempotency key so every later webhook and fetch can be tied
  back to the attempt.
* `HandleReturn` is called when the customer comes back. It verifies whatever
  the provider signs on the return, and says what it learned. It never decides
  the final status on its own: Paymux confirms with `FetchPayment`.
* `VerifyWebhook` authenticates a delivery; `ParseWebhook` turns an already
  verified one into `Notification`s. Parsing never calls the network.
* `FetchPayment` is the source of truth. A payment id that reaches Paymux from
  outside (a return URL, a notification) is only a pointer: when the caller also
  names an order, checkout or reference, `FetchPayment` must return
  `ErrNotFound` for a payment that is not of that order, checkout or reference,
  and must never capture or otherwise act on a payment it has not matched.
  Never take the first result of a list or search: check the field you searched by. Paymux fetches after every webhook and
  return, so a forged or replayed webhook cannot change state by itself.

Notification kinds are `payment`, `refund`, `dispute`, `mandate`,
`subscription` and `payout` (a payout processed, failed or reversed; it carries
a `PayoutSnapshot` with your reference or the provider's payout id). Like every
notification a payout webhook is only a hint: Paymux finds its own payout in the
account, calls your `FetchPayout`, checks reference, amount and currency, and
applies that. A payout is only booked as succeeded when the snapshot states the
amount and currency that were sent (case-insensitively for the currency); a
success without them, or with others, is left unapplied and raises an alert, so
set both on every `PayoutSnapshot` you return.

### Optional interfaces

Implement an interface and declare its capability in `Info.Capabilities`. The
conformance kit fails when the two disagree.

| interface             | capability             | purpose |
|-----------------------|------------------------|---------|
| `Refunder`            | `refund`               | `Refund`, `FetchRefund` |
| `DisputeReader`       | `dispute_read`         | `ListDisputes`, `FetchDispute` |
| `DisputeResponder`    | `dispute_respond`      | `AcceptDispute`, `SubmitEvidence` |
| `Lister`              | `list`                 | `ListPayments`, `ListRefunds` (reconciliation) |
| `Settler`             | `settle`               | `ListSettlements` |
| `Canceler`            | `cancel`               | `CancelCheckout` |
| `SubscriptionManager` | `subscription_manager` | provider-managed recurring billing |
| `MandateManager`      | `mandate_manager`      | merchant-triggered debits on a mandate |
| `Payouter`            | `payout`               | `CreatePayout`, `FetchPayout`: money out to a bank account or UPI id |
| `OAuthConnector`      | `oauth`                | `OAuthDefaultScopes`, `OAuthAuthorizeURL`, `OAuthCallbackConfig`, `OAuthExchange`: the dashboard's **Connect** button |
| `ConnectionTester`    | (none)                 | `TestConnection`: the dashboard's **Test** button; see below |

### Testing the credentials (`ConnectionTester`)

The dashboard's **Test** button asks the connector whether the account's
credentials work. A connector that does not implement `ConnectionTester` is asked
`FetchPayment` for a payment that cannot exist, and `ErrNotFound` counts as
"accepted". That only works if the connector can look a payment up by Paymux's
reference alone and makes a request to do it. Stripe, Zoho and Whop cannot
(they need their own ids), so they would fail before any request and the button
would say "Could not tell". Implement

```go
TestConnection(ctx context.Context) error
```

with one authenticated, read-only call that needs only the permissions the
connector already uses (list one payment). Return nil when the provider accepted
the credentials, an error wrapping `ErrInvalidCredentials` when it rejected them,
and anything else when the result is inconclusive (network, rate limit, outage).
The text after `ErrInvalidCredentials` is shown to the operator, so make it say
what is wrong and what to do: "not connected yet", "this token belongs to the
sandbox server, the account is set to live". The conformance kit requires
`Fixtures.ConnectionOK` and `ConnectionRejected` of a connector that implements it.

### OAuth connectors (`OAuthConnector`)

A provider whose credentials come from an OAuth authorization (Zoho Payments)
can let the operator authorise the account from the dashboard instead of
pasting grant codes. Implement `OAuthConnector` and declare `oauth`; the
provider account page then shows a Connect panel. The contract:

* An account that is **not connected yet** must be constructible (no refresh
  token): the operator creates it, then connects it. Its API calls fail with
  `ErrInvalidCredentials` saying it is not connected, without calling the
  provider.
* Mark the config properties in the schema so the dashboard knows what they
  are: `"x-oauth": "client_id"`, `"client_secret"`, `"refresh_token"` or
  `"scopes"`. A property can be kept out of the form with `"x-hidden": true`
  (the dashboard keeps its stored value when the form is saved).
* `OAuthAuthorizeURL(cfg, redirectURI, state, scopes)` builds the provider's
  authorization URL and must refuse an empty state (the state is what makes the
  callback safe: the dashboard makes it random, single use, ten minutes, bound
  to the user and the account).
* `OAuthCallbackConfig(cfg, params)` lets the provider say, on the callback,
  which data centre or accounts host the account is in. **Never follow a host
  that came from outside**: look it up in a list of the provider's own hosts and
  refuse anything else (the Zoho connector does this; the tests try IP
  addresses, look-alike hosts, other schemes and ports).
* `OAuthExchange(ctx, cfg, redirectURI, code)` trades the code for tokens (an
  empty `redirectURI` is a self client's code). It returns an `OAuthResult`
  whose `ConfigPatch` is merged into the stored config and sealed: put the
  refresh token there, never the access token. Error messages must not contain
  the client secret, the code or a token.
* The conformance kit runs these checks when `Fixtures.OAuth` is set.

## 2. Declaring capabilities (`Info`)

`Info` is static: `Register` stores it, and the dashboard and router read it
without building a connector. It states what the provider can do and the rules
Paymux must respect:

* `Currencies`, `Methods`: what the account may be routed.
* `CheckoutHosts`: the https origins (`https://host`, or `https://*.host`) that
  an `sdk` or `form_post` checkout needs on the hosted payment page, by CSP
  directive (`Script`, `Frame`, `Form`, `Connect`, `Img`), for both the test
  and the live endpoints. The page sends a nonce-based,
  **enforced** `Content-Security-Policy` built from the hosts of the connector
  account (`connector.HostsOf`) and from nothing else: the script and form URLs
  of the `NextAction` are allowed because they are declared here, so a host that
  is missing from the list is blocked by the browser. Leave it nil for a
  connector that only redirects. Entries that are not plain https
  origins are dropped.
  It is also the allow-list of where the connector may send the customer: the
  hub refuses a checkout (payment, subscription or mandate) whose `sdk` script
  URL is not in `Script`, whose `form_post` URL is not in `Form`, or whose
  `redirect` URL is not in `Redirect` (add the provider's hosted checkout host
  there; a connector that only redirects sets just `Redirect`). The URL must be
  https and match an entry exactly (host and port); `https://*.host` matches
  one or more labels in front of `host`, never `host` itself. A refused
  checkout is an ordinary checkout error, so routing falls back to the next
  account; it is logged with the URL and raises a `provider.checkout_host_refused`
  ops alert naming the account. A connector whose hosts depend on its account
  (Zoho's operator-set hosted checkout URL) implements
  `connector.CheckoutHoster` and returns them per account; the hub uses that
  instead of `Info.CheckoutHosts`. The simulator is the one connector that
  redirects to the hub itself: it reports the hub's own origin (`Self`, http
  allowed, redirect only) through `CheckoutHoster`; `Self` is not part of the
  JSON form, so a remote connector cannot claim it. `connectortest.Run` and
  `connectortest.CheckAllowed` check a connector's checkout against its hosts.
* `MerchantOfRecord`: true when the provider sells to the customer (Whop). No
  GST invoice is issued for its payments.
* `SubscriptionModes`: `provider_managed` (the provider bills and retries) and/or
  `merchant_triggered` (Paymux schedules each debit on a mandate).
* `MandateLimits`: per-method caps on a single debit.
* `PreDebitPerAttempt`: the provider needs a notice per debit, not once per mandate.
* `SubscriptionCurrencies`: the currencies the connector can bill a subscription in, when fewer than `Currencies` (Razorpay: INR only). Routing never sends a subscription in another currency to it, so a customer is not left at a checkout that fails.
* `PreDebitMinHours`: the least time the provider wants between a notice and its debit, when that is longer than the RBI's 24 hours (PayU: 48). Paymux never debits sooner.
* `MandateSetupAmount` and `RefundsVerificationDebit`: the verification debit a
  mandate registration makes when nothing is due (default 100 minor units).
  When the provider does not reverse it itself, Paymux refunds it
  (`initiated_by=system`, `reason=mandate_verification`).
* `RateLimit`: requests per second and burst for this provider's API.
* `ConfigSchema`: a JSON Schema for the account config, used by the dashboard form.
* `TestOnly`: the connector only makes sense in test mode. Paymux refuses to
  create a live-mode account for it, and refuses to build one that is live.
  The simulator sets it.

### The simulator, a built-in connector for test mode

`connectors/simulator` is a provider that exists only on your machine. It lets
you exercise the whole flow with no provider account and no keys: add a
"Simulator" provider account in **test mode** (no configuration needed), link it
to an app, and send test payments to it (see `docs/quickstart.md`). It cannot be
linked to a live-mode account. No routing rule is needed: in test mode, a
payment that no rule routes to any account goes to the app's linked accounts of
a test-only connector (a rule that matches, or one that rejects, is never
overridden).

How it works, which also makes it a reference for a connector that needs a
hosted page or a webhook source:

* `CreateCheckout` returns a redirect to Paymux's own page `/sim/{ref}`. The
  page offers Succeed, Fail, Authorize only (then Capture), Pending then succeed
  after N seconds, and Late success after expiry. The choice is stored in the
  account's `KV` (`Controller.Decide`), then the browser goes back through the
  normal `/return/{account}/{ref}` path, which fetches the truth like for any
  provider.
* It posts **signed webhooks** to Paymux's own `/hooks/{account}` (HMAC-SHA256
  over `timestamp.body`, with a secret it makes up and keeps in `KV`), so the
  real inbound pipeline, with signature check, dedupe and fetch-the-truth,
  processes every event. A delayed success is delivered by a timer; if the
  process restarts first, the poller and `FetchPayment` apply it.
* Refunds succeed at once, or after `refund_delay_seconds` (config) as
  pending then succeeded. Disputes are opened from the payment page of the
  dashboard ("Simulate a dispute"), can be accepted or answered with evidence
  (under review, then won ten seconds later). Settlements list one entry per
  day with a flat 2% fee plus 18% GST on it.
* Subscriptions: provider-managed (authorise on the page, then "Simulate a
  renewal" on the subscription page, or a failed one to turn it past due) and
  merchant-triggered through a UPI-style mandate (authorise on the page; later
  debits are immediate).
* It declares every capability, `MerchantOfRecord=false`, and the currencies
  INR, USD and BDT.

## 3. Types: plain and JSON-serializable

Every request and response type is a struct of strings, numbers, bools, slices,
maps and other such structs. No funcs, channels, interfaces or `time.Time`
fields. This is what lets a connector later run out of process (Path 2) with
the same types on the wire. The conformance kit round-trips `SampleValues()`
through JSON and fails on any exported type that breaks this rule.

Conventions:

* Money is `int64` minor units plus an ISO currency code. Use
  `pkg/connector/money` (`ParseDecimal`, `FormatDecimal`) for provider decimals;
  never use floats for amounts you send.
* Times are Unix milliseconds (`int64`).
* Raw bodies are `[]byte`, headers `map[string][]string`.
  `WebhookRequest.Header(name)` is case-insensitive.
* Always keep the provider's own status in `RawStatus`.

## 4. Error semantics

Return the typed errors from `pkg/connector/errors.go` (wrap with `%w`):

| error | meaning and effect |
|-------|--------------------|
| `ErrNotFound` | the provider has no such object. |
| `ErrInvalidCredentials` | 401/403 from the provider; the account is flagged. |
| `ErrTemporary` | network error, 5xx, anything worth retrying. Jobs back off and retry. |
| `ErrRateLimited{RetryAfter}` | retry after the delay. |
| `ErrUnsupported` | this provider cannot do it (for example amount updates). The API answers 422 `unsupported` with the message. |
| `ErrRejected` | the provider says the request is invalid or not allowed: its 400, 409 and 422-style answers ("currency not supported", "only offers can be updated"). Classify them after the specific errors above (not found, invalid credentials, rate limited, not refundable, unsupported) and before the 5xx case. The API answers 422 `provider_rejected` with the provider's message, scrubbed by the log redactor (known secrets, key shapes, personal data) and cut to 300 characters, so keep the provider's text in the error. Not retried. |
| `ErrNotRefundable{Reason}` | the provider refuses this refund; not retried. |
| `ErrMandateLimit` | the debit exceeds the mandate limit. |
| `ErrInvalidReturn` | `HandleReturn` signature check failed. |
| `ErrInvalidSignature` | see below. |
| `ErrUnsigned` | see below. |

Anything else is treated as a permanent failure of that call and shown to an API
caller as an internal error (500, no message), so classify the provider's 4xx
answers as `ErrRejected` (or a more specific error) and network and 5xx problems
as `ErrTemporary` explicitly.

### `ErrInvalidSignature` versus `ErrUnsigned`

`VerifyWebhook` has exactly three outcomes:

* `nil`: the delivery is authentic.
* `ErrInvalidSignature`: the signature is missing, wrong, or stale (timestamp
  outside your tolerance). The delivery is answered with 401, stored as
  unverified, and never causes a fetch. Compare signatures with
  `hmac.Equal` or `subtle.ConstantTimeCompare`.
* `ErrUnsigned`: only for event types the provider genuinely does not sign
  (today PayU refund webhooks). The delivery is stored unverified and Paymux
  enqueues a fetch-only job: the fetched API result, not the webhook body, may
  change state. Never return it just because verification is inconvenient.

## 5. Status mapping rules

`PaymentStatus` is one of `created`, `requires_action`, `processing`,
`authorized`, `succeeded`, `failed`, `canceled`, `expired`.

1. Map every status the provider documents explicitly, in one function used by
   both `FetchPayment` and `ParseWebhook`.
2. A status you do not recognise maps to an in-flight status (`processing`,
   or `requires_action` when the customer must still act), never to
   `failed`. A new provider status must not fail a payment that later
   succeeds; the next fetch corrects it.
3. Terminal states are final: only map to `succeeded`, `failed`, `canceled`
   or `expired` when the provider says the outcome is decided.
4. Fill `RawStatus` with the provider's string, `Amount` and `Currency` from
   the provider's own response (not from the request), and the ids you have
   (`ProviderPaymentID`, `ProviderOrderID`, `ProviderRef`).
5. A provider with an authorize-then-capture flow captures inside
   `FetchPayment`/`HandleReturn` where the spec says so, and reports the
   resulting state (the kit's `WantRaw` covers this).
6. Subscription and mandate snapshots follow the same rule: unknown means the
   provider keeps trying (`active`/`pending`), not cancelled.

## 6. Using `KV`

`deps.KV` is for small derived state: an OAuth token with a TTL, a provider
plan id created for a given price (`plan:month:49900:INR`). Rules: keys are
per account already (do not add the account id); values are not secrets
storage; a missing key must always be recoverable (re-create the plan, fetch a
new token); never rely on a write being visible to another process.

## 7. Registration

Add a blank import of your package to `cmd/paymux/connectors.go`:

```go
import _ "github.com/paymux/paymux/connectors/acme"
```

That is the only line outside your package. `init` registers the factory and
`Info`; registering a name twice panics. A new third-party dependency needs a
line in `docs/dependencies.md`; prefer the standard library.

## 8. Testing

### The conformance kit

```go
func TestConformance(t *testing.T) {
	connectortest.Run(t, New, connectortest.Fixtures{
		Config:   testConfig,
		Ref:      connector.PaymentRef{ProviderRef: "pay_ref_01"},
		Statuses: statusCases,    // one StatusCase per provider status
		Webhooks: webhookFixtures, // valid, wrong secret, signature headers, stale
		Return:   &returnFixtures,
		Checkout: checkoutRequest, CheckoutHandler: provider,
	})
}
```

`Run` checks: webhook verification (tampered body, wrong secret, each missing
header and a stale timestamp all give `ErrInvalidSignature`), parsing, the
return flow, that every `StatusCase` maps to a known status, money round trips
for every currency, that `Info` and the implemented interfaces agree, that
`CreateCheckout` returns a valid `NextAction`, and that the SDK types
round-trip through JSON.

### Fuzzing

Every connector has `FuzzVerifyWebhook` and `FuzzParseWebhook` (see
`connectors/whop/fuzz_test.go` for the shortest example), seeded with the golden
fixtures. Their invariants are the contract of the two methods:

* nothing panics, whatever the bytes;
* `VerifyWebhook` returns only nil, `ErrInvalidSignature` or `ErrUnsigned`, and
  never nil for a body that differs from the signed one (a provider that signs
  only some fields, like PayU, can only be held to those fields: say so in the test);
* the event id of a notification comes from what the signature covers (the body,
  or a signed header), never from an unsigned header: Razorpay's
  `X-Razorpay-Event-Id` and Cashfree's `x-idempotency-key` are not signed, so a
  captured delivery replayed with another value would have been applied again;
  both connectors hash the body instead. A provider that signs a timestamp has it
  checked against the time of receipt (Cashfree, 48 hours; Razorpay checks the
  signed `created_at` of subscription events, which are applied from their
  payload, over the same window);
* a connector whose `ParseWebhook` can call the provider (PayU reads an unsigned
  mandate notice back from its status command) also implements
  `connector.WebhookEventIDer`, so the inbound handler can name the event from the
  request alone: a delivery never costs a provider call, and the confirming fetch
  runs in the background job;
* `ParseWebhook` results pass `connectortest.CheckNotifications` (the same check
  as `Notification.Validate`): event id and type present, the snapshot of the
  notification's kind present, and a provider id in it.

Call `Validate` on what you are about to return from `ParseWebhook`: fuzzing
found empty entities (`{"dispute":{}}`) being turned into notifications that
could not be matched to anything. In CI the targets run as ordinary tests over
the seeds and over every crashing input a fuzz run saved under
`testdata/fuzz/`; to fuzz for real run `bash scripts/fuzz.sh [fuzztime]`.

### Fake-server patterns

Never call a real provider in tests.

* Put an `httptest.Server` in front of the connector:
  `deps := connectortest.NewDeps(srv, now)`. `NewDeps` wires a client that
  redirects every request to the fake whatever host the connector uses, a
  `MemKV`, a fixed clock and a `URLs` with example.com addresses.
* Write the fake as one `http.Handler` switching on `r.Method + " " + r.URL.Path`.
  Record request bodies so tests can assert what was sent (amounts in the
  provider's units, metadata, idempotency keys).
* Keep real-looking provider payloads in `testdata/*.json` and sign them in the
  test with the same algorithm as the provider. A fixture should come from the
  provider's documentation or a sandbox capture.
* Cover each failure class: 401 to `ErrInvalidCredentials`, 429 to
  `ErrRateLimited`, 5xx to `ErrTemporary`, 404 to `ErrNotFound`.
* Use `connector.NewMemKV(clock)` with a controllable clock to test caching.
* Anything you could not check against the live API gets a
  `// TODO(spec): ...` comment saying what to verify at sandbox time, and a line
  in [provider-verification.md](provider-verification.md).

For engine-level behaviour (linking, renewals, invoices) the test in
`internal/subscriptions/whop_test.go` shows how to feed the real connector's
`ParseWebhook` output to the engine.

## 9. The public API is guarded

`pkg/...` is the contract connector authors build against. CI snapshots its
exported declarations (no comments, no bodies, no unexported details) into
`scripts/apisnap/pkg.txt` and fails when the package no longer matches the file. After an
intended change, run `go run ./scripts/apisnap -w` and commit the updated file
in the same change; reviewers read the diff of that file as the API change.
Prefer adding to the SDK over changing or removing: removals and signature
changes break existing connectors.

## 10. Remote connectors (Path 2)

A connector does not have to be compiled into Paymux. `connectors/remote`
forwards every method to an HTTP service that serves the same interfaces, so a
connector can be written in any language, deployed and upgraded on its own.

**Paymux side.** Create a provider account of connector `remote` with

```json
{"base_url": "https://connector.example.com/v1", "secret": "whsec_<base64>",
 "checkout_hosts": {"redirect": ["https://pay.connector.example.com"]}}
```

**Checkout hosts.** `checkout_hosts` is required (the account is refused on
create and on update without it): the https origins the service may send the
customer to, with the same lists as `connector.CheckoutHosts` (`script` for
`sdk` checkouts, `form` for `form_post`, `redirect`; `frame`, `connect` and `img`
are accepted too but do not widen the hosted page's policy for a remote
account yet). Every entry must be `https://host[:port]`
or `https://*.host[:port]`, and at least one origin must be listed. The service
speaks for the account, so the hub does not trust the URLs it returns: a next
action (from `create_checkout`, `create_subscription_checkout` or
`create_mandate_checkout`) outside these hosts is refused and alerted, and the
payment falls back to another account. The service's own `Info.checkout_hosts`
is ignored for this check.

**Discovery.** `GET {base_url}/info` returns the connector's `Info` (JSON of
`connector.Info`). The remote decides the optional capabilities: the remote
connector implements exactly the optional interfaces in `Info.Capabilities`,
so the conformance kit's "declared equals implemented" rule still holds. Info is
read when the account's connector is built (on config change or restart).

**Calls.** Each method is `POST {base_url}/{method}` with the JSON encoding of
the request type as the body, answered by `200` and the JSON result:

| method | body | result |
|--------|------|--------|
| `create_checkout` | `CheckoutRequest` | `CheckoutResult` |
| `handle_return` | `ReturnRequest` | `ReturnResult` |
| `verify_webhook` | `WebhookRequest` | `{}` |
| `parse_webhook` | `WebhookRequest` | `{"items": [Notification]}` |
| `fetch_payment` | `PaymentRef` | `PaymentSnapshot` |
| `refund`, `fetch_refund` | `RefundRequest`, `RefundRef` | `RefundSnapshot` |
| `list_disputes` | `{"range", "cursor"}` | `Page[DisputeSnapshot]` |
| `fetch_dispute` | `DisputeRef` | `DisputeSnapshot` |
| `accept_dispute` | `DisputeRef` | `{}` |
| `submit_evidence` | `{"dispute", "evidence"}` | `{}` |
| `list_payments`, `list_refunds` | `{"range", "cursor"}` | `Page[...]` |
| `list_settlements` | `{"range"}` | `{"items": [SettlementSnapshot]}` |
| `cancel_checkout` | `PaymentRef` | `{}` |
| `create_subscription_checkout` | `SubscriptionRequest` | `CheckoutResult` |
| `fetch_subscription` | `SubscriptionRef` | `SubscriptionSnapshot` |
| `cancel_subscription` | `{"subscription", "at_period_end"}` | `{}` |
| `update_subscription_amount` | `{"subscription", "amount"}` | `{}` |
| `create_mandate_checkout` | `MandateRequest` | `CheckoutResult` |
| `fetch_mandate` | `MandateRef` | `MandateSnapshot` |
| `notify_pre_debit` | `PreDebitRequest` | `{}` |
| `charge_mandate` | `ChargeRequest` | `PaymentSnapshot` |
| `revoke_mandate` | `MandateRef` | `{}` |
| `create_payout` | `PayoutRequest` | `PayoutSnapshot` |
| `fetch_payout` | `PayoutRef` | `PayoutSnapshot` |

The Go definitions are in `pkg/connector/wire` (method names, the multi-argument
envelopes, error codes); raw bodies and headers travel as base64 and header
maps exactly as `encoding/json` encodes the SDK types.

**Errors.** A failed call answers with a non-200 status and
`{"error": {"code", "message", "reason", "retry_after_ms"}}`. The codes are
`not_found`, `invalid_credentials`, `temporary`, `unsupported`,
`invalid_signature`, `unsigned`, `invalid_return`, `mandate_limit`,
`not_refundable` (with `reason`), `rate_limited` (with `retry_after_ms`),
`bad_request` and `internal`; the client turns them back into the SDK's typed
errors, so `errors.Is(err, connector.ErrInvalidSignature)` works across the
hop. A 5xx without a valid signature (a proxy, a crash) and any network error
are `ErrTemporary`; a response whose signature does not verify is
`ErrInvalidCredentials` (almost always a wrong secret).

**Signing.** Both directions use the Standard Webhooks headers `webhook-id`,
`webhook-timestamp` and `webhook-signature: v1,<base64 HMAC-SHA256(...)>` under
the shared `whsec_` secret, with a five minute tolerance. What the HMAC covers
is bound to the message's purpose, so a captured message cannot be reused:

* a request signs `id.timestamp.VERB.method.body`, for example
  `msg_x.1760000000.POST.refund.{...}`. The server verifies against the method
  taken from the URL path, so a request posted to another path (a signed
  `fetch_payment` sent to `/refund`) fails with `401`. `GET /info` signs
  `GET.info` with an empty body;
* the server accepts each `webhook-id` once: a replay of a signed request within
  the tolerance window is refused with `401`. The ids are kept in an in-memory
  cache bounded by size (`wire.DefaultReplayCacheSize`, 100,000 ids, about 166
  signed requests a second; `wire.WithReplayCacheSize`) and time (twice the
  tolerance). An id is never forgotten inside its window: when the cache is full
  the server answers `503` (code `temporary`, so the client retries with a new
  id) instead of evicting the oldest, so a replay inside the window is always
  refused. The cache is per
  process: a service running several instances needs a shared store, or sticky
  routing, to reject a replay sent to another instance;
* a response signs `id.timestamp.response.<request webhook-id>.body`, so the
  client refuses a genuine response that was recorded for a different request.
  A service in another language must implement exactly these strings.

`connectors/remote` also refuses a plain-http `base_url` unless its host is
loopback (`localhost`, `127.0.0.1`, `::1`) or the account config sets
`"allow_insecure": true` (tests and local development only; a **live** account
is refused it, on create and on update, and an account that already stored it
ignores it): the signature authenticates messages but does not encrypt them, and
customer data travels in the bodies.

**Network guard (SSRF).** `base_url` is supplied by whoever configures the
account, so it must not reach internal services. The remote connector declares
`Info.DenyPrivateNetwork`; the host then builds its HTTP client with
`connector.HTTPOptions{DenyPrivate: true}`, which

* resolves the host name and refuses it if *any* resolved address is in a
  special-purpose range (`connector.IsPrivateIP`, the one guard the webhook and
  hook deliveries use too): loopback, RFC 1918 and ULA private, carrier-grade NAT
  (100.64/10), link-local (169.254/16 including the cloud metadata address,
  fe80::/10), the rest of 0/8, 192.0.0/24, the documentation ranges, 198.18/15,
  multicast, reserved 240/4, NAT64, 6to4, Teredo and the unspecified address; an
  IPv4-mapped IPv6 address is judged as the IPv4 address it carries;
* checks the address again at connect time in the dialer's `Control` function,
  which defeats DNS rebinding (a second answer that differs from the first);
* ignores proxy environment variables and never follows a redirect (the remote
  connector refuses redirects even if the host's client would follow them);
* is lifted only by `"allow_insecure": true` in the config of a test-mode
  account, for tests and local development (`http://127.0.0.1:8090`).

A config property annotated `"x-endpoint": true` in the schema (the remote
connector's `base_url`) is the address the account's secrets are sent to.
Changing it while a secret field still holds its stored value is refused
("enter the secret again"): the dashboard keeps a stored secret when its field is
left blank, so otherwise a new address would silently receive the old secret.

Literal private addresses in `base_url` are also refused when the account is
built, so a bad config fails early with `ErrPrivateAddress`. Any other connector
that calls an operator-supplied URL should set the same `Info` flag.

**Serving a connector.** In Go, `wire.NewHandler(connector, secret, nil)` is the
whole server. `examples/remote-connector` is a runnable reference that wraps the
Razorpay connector (see its header comment for the environment it reads), and
`connectors/razorpay/remote_test.go` runs the unmodified Razorpay conformance
fixtures through `wire.NewHandler` and `connectors/remote`: the same
`connectortest.Run` that the in-process connector passes. A service in another
language implements the table above and the signing rule; run the conformance
kit against it with a Go test that builds the remote connector from the service
URL and passes provider-side fixtures through it.

Limits to know: the return and webhook URLs belong to Paymux, so the remote is
configured with them separately (the example reads `PAYMUX_BASE_URL` and
`PAYMUX_ACCOUNT_ID`); KV and the HTTP client live in the remote service, not in
Paymux; and each call is one HTTP round trip with the platform's usual timeout, and POSTs are not
retried by the client (every mutating method is idempotent by `ProviderRef`, so the
engine retries at its own level).

## 11. Checklist

* [ ] imports only stdlib and `pkg/...`
* [ ] `Info` complete; capabilities match the interfaces
* [ ] one status mapping function; unknown statuses are in-flight
* [ ] `VerifyWebhook` returns only nil, `ErrInvalidSignature` or `ErrUnsigned`
* [ ] errors typed (`ErrTemporary` for network and 5xx)
* [ ] no `time.Now`, no `http.DefaultClient`, no logging of secrets
* [ ] conformance kit and fake-server tests pass
* [ ] blank import added in `cmd/paymux/connectors.go`
* [ ] `go run ./scripts/apisnap -check` is clean if you touched `pkg/`
