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

## 9. Checklist

* [ ] imports only stdlib and `pkg/...`
* [ ] `Info` complete; capabilities match the interfaces
* [ ] one status mapping function; unknown statuses are in-flight
* [ ] `VerifyWebhook` returns only nil, `ErrInvalidSignature` or `ErrUnsigned`
* [ ] errors typed (`ErrTemporary` for network and 5xx)
* [ ] no `time.Now`, no `http.DefaultClient`, no logging of secrets
* [ ] conformance kit and fake-server tests pass
* [ ] blank import added in `cmd/paymux/connectors.go`
* [ ] `go run ./scripts/apisnap -check` is clean if you touched `pkg/`
