# Extending Paymux

Paymux runs as a single binary. When you need more than configuration, there
are two ways to add your own Go code, and both give you a binary that works
exactly like `paymux` (same commands, same config, same database).

| You want | Use |
|---|---|
| React to payments, refunds, disputes, renewals or invoices; steer which provider account a payment uses | **Hooks**, in a small `main` package you own (framework mode) |
| A provider Paymux does not ship with | A **connector** module (see `docs/connectors.md`), linked in with an import |
| Add modules to the stock binary without writing a `main` | **`paymux build --with`** |

## Framework mode

A custom binary is a `main` package that calls `framework.Main`. The
`framework` package links the built-in connectors and runs the same command
line as `paymux`; the function you pass receives the `*paymux.App` before
`serve` starts it.

```go
package main

import (
	"context"

	"github.com/paymux/paymux"
	"github.com/paymux/paymux/framework"

	_ "example.com/acmepay" // an extra connector: importing it registers it
)

func main() {
	framework.Main(func(app *paymux.App) {
		app.OnPaymentSucceeded(func(ctx context.Context, e paymux.PaymentEvent) error {
			return notifyCRM(ctx, e.Payment.OrderRef, e.Payment.Amount)
		})
	})
}
```

A complete example, with an extra connector, is in `examples/framework`:

```sh
go build -o paymux-custom ./examples/framework
./paymux-custom migrate
./paymux-custom serve
```

`migrate`, `app`, `provider`, `user` and the other commands work in the custom
binary too, which matters on SQLite where only one process can hold the
database.

A module that exists only to extend Paymux (a bundle of hooks, say) cannot be
handed the `App` by a `main` it does not control, so it registers a setup
function in `init()` instead:

```go
func init() {
	framework.OnSetup(func(app *paymux.App) {
		app.OnInvoiceIssued(archiveInvoice)
	})
}
```

## Hooks

Hooks are methods on `*paymux.App`, registered before `serve` starts. Each can
be registered more than once; hooks for an event run in registration order.

| Method | Event | Argument |
|---|---|---|
| `OnPaymentSucceeded` | `payment.succeeded` | `PaymentEvent` |
| `OnPaymentFailed` | `payment.failed` | `PaymentEvent` |
| `OnRefundSucceeded` | `refund.succeeded` | `RefundEvent` |
| `OnDisputeOpened` | `dispute.opened` | `DisputeEvent` |
| `OnSubscriptionRenewed` | `subscription.renewed` | `SubscriptionEvent` |
| `OnInvoiceIssued` | `invoice.issued` | `InvoiceEvent` |
| `OnRoute` | (inline, see below) | `RouteRequest` |

Every event hook has the signature `func(ctx context.Context, e XEvent) error`.
An event carries `Event` (type, org, app, mode, object id and version), a typed
snapshot (`Payment`, `Refund`, `Dispute`, `Subscription`, `Invoice`) and `Raw`,
the same JSON object your app receives in the webhook's `data.object`, for
fields the snapshot does not model.

### Guarantees

- **After commit.** The event is recorded and a job is queued in the same
  database transaction that changed the payment. The hook runs from the job
  queue once that transaction has committed, so it only ever sees committed
  state, and a transaction that rolls back never runs a hook.
- **Money cannot be affected.** A hook returning an error, panicking or hanging
  changes nothing: the payment, refund or invoice is already committed. The
  failure is logged and raised as an ops alert (`hook.failed`, sent to
  `PAYMUX_OPS_EMAILS` and `PAYMUX_OPS_WEBHOOK_URL`, rate limited like other
  alerts). The event is not retried and the other hooks still run.
- **Read only.** Hooks get plain values, copied for each hook; they hold no
  reference to Paymux. There is no API in a hook to create a refund or move
  money. A hook that needs to call the Paymux API does it over HTTP with an API
  key, like any other app.
- **At least once.** If the process stops while a hook runs, the job runs
  again after a restart. Make hooks idempotent, keyed on `e.Event.ObjectID`
  and `e.Event.ObjectVersion`.
- **Bounded.** Each call gets a context that is cancelled after 30 seconds.
  Respect it. Hooks run on the job workers, so slow hooks delay other jobs.
- **Only listened-for events cost anything.** An event no hook listens to is
  not queued.

### OnRoute

`OnRoute(func(RouteRequest) []string)` is the one hook that runs inline. When a
customer opens the pay page, routing decides which provider accounts can take
the payment; `OnRoute` may reorder them. It receives the payment (app, mode,
currency, country, amount, purpose, metadata) and the eligible accounts
(`Accounts`, each with `ID`, `Connector`, `Name`) in the order routing would
try them. It returns provider account ids, or connector names meaning every
account of that connector, best first.

```go
app.OnRoute(func(r paymux.RouteRequest) []string {
	if r.Currency == "INR" && r.Amount >= 5_000_000 {
		return []string{"razorpay"}
	}
	return nil // no opinion
})
```

It can only reorder: accounts it does not name keep their relative order after
the ones it names, an unknown id is ignored, it cannot add an account that the
routing rules or the currency list exclude, and an account whose circuit
breaker is open is still tried last. Returning `nil`, or panicking, leaves the
order alone. With several `OnRoute` hooks, each sees the order the previous
one produced. It is asked only when two or more accounts are eligible, and not
when a payment is created, only when the checkout starts. Keep it fast and
free of I/O: the customer is waiting.

## Building a custom binary without writing a main

`paymux build` produces a custom binary the way `xcaddy` does: it writes a
temporary `main` package that imports `framework` and each extra module, lets
the `go` tool resolve the dependencies, and builds a static binary
(`CGO_ENABLED=0`). It needs the Go toolchain on `PATH`.

```sh
paymux build \
  --with github.com/acme/paymux-acmepay@v1.2.0 \
  --with github.com/acme/paymux-crm-hooks \
  --out ./paymux-custom
./paymux-custom serve
```

- `--with module[@version][=local/dir]` links a module in (version defaults to
  `latest`). The module is imported by its module path, so the package at the
  module root is what must register things: connectors in `init()` with
  `connector.Register`, hooks with `framework.OnSetup`.
- `=local/dir` uses a directory instead of downloading the module, which is
  also how to build offline or against unpublished code. `--replace
  module=local/dir` does the same for any other module in the build.
- `--paymux-version` picks the Paymux version to build on. The default is the
  running binary's own version when it is a release, else `latest`.
- `--out` is the file to write (default `./paymux-custom`). The command prints
  its absolute path.

Treat a `--with` module like any code you run in production: it executes
inside the Paymux process, which can read the database and every connector
secret. Pin versions and read the code.

## Public API

The root package (`github.com/paymux/paymux`) and `pkg/...` are the public
API. `api/pkg.txt` is a snapshot of their exported declarations, and CI fails
when it is out of date (`go run ./scripts/apisnap -w` regenerates it after an
intended change). Everything under `internal/` can change at any time.
