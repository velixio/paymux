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
* `FetchPayment` is the source of truth. Paymux fetches after every webhook and
  return, so a forged or replayed webhook cannot change state by itself.

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

## 2. Declaring capabilities (`Info`)

`Info` is static: `Register` stores it, and the dashboard and router read it
without building a connector. It states what the provider can do and the rules
Paymux must respect:

* `Currencies`, `Methods`: what the account may be routed.
* `MerchantOfRecord`: true when the provider sells to the customer (Whop). No
  GST invoice is issued for its payments.
* `SubscriptionModes`: `provider_managed` (the provider bills and retries) and/or
  `merchant_triggered` (Paymux schedules each debit on a mandate).
* `MandateLimits`: per-method caps on a single debit.
* `PreDebitPerAttempt`: the provider needs a notice per debit, not once per mandate.
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
linked to a live-mode account.

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
| `ErrUnsupported` | this provider cannot do it (for example amount updates). |
| `ErrNotRefundable{Reason}` | the provider refuses this refund; not retried. |
| `ErrMandateLimit` | the debit exceeds the mandate limit. |
| `ErrInvalidReturn` | `HandleReturn` signature check failed. |
| `ErrInvalidSignature` | see below. |
| `ErrUnsigned` | see below. |

Anything else is treated as a permanent failure of that call, so classify
network and 5xx problems as `ErrTemporary` explicitly.

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
  `// TODO(spec): ...` comment saying what to verify at sandbox time.

For engine-level behaviour (linking, renewals, invoices) the test in
`internal/subscriptions/whop_test.go` shows how to feed the real connector's
`ParseWebhook` output to the engine.

## 9. The public API is guarded

`pkg/...` is the contract connector authors build against. CI snapshots its
exported declarations (no comments, no bodies, no unexported details) into
`api/pkg.txt` and fails when the package no longer matches the file. After an
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
{"base_url": "https://connector.example.com/v1", "secret": "whsec_<base64>"}
```

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

**Signing.** Both directions use Standard Webhooks: headers `webhook-id`,
`webhook-timestamp` and `webhook-signature: v1,<base64 HMAC-SHA256(id.timestamp.body)>`
under the shared `whsec_` secret, with a five minute tolerance. Requests
without a valid signature get `401`. `GET /info` is signed too (empty body).

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
