// Command framework is an example custom Paymux binary: the standard commands
// and built-in connectors, an extra connector linked in by import, and a Go
// hook. Build and run it like the paymux binary:
//
//	go build -o paymux-custom ./examples/framework
//	./paymux-custom serve
//
// See docs/extending.md.
package main

import (
	"context"
	"log/slog"

	"github.com/paymux/paymux"
	"github.com/paymux/paymux/framework"

	// An extra connector: importing it registers it.
	_ "github.com/paymux/paymux/examples/framework/demopay"
)

func main() {
	framework.Main(func(app *paymux.App) {
		// Runs after the payment has committed. It cannot change the payment;
		// an error is logged and raised as an ops alert.
		app.OnPaymentSucceeded(func(_ context.Context, e paymux.PaymentEvent) error {
			slog.Info("payment succeeded", "payment", e.Payment.ID, "order_ref", e.Payment.OrderRef,
				"amount", e.Payment.Amount, "currency", e.Payment.Currency)
			return nil
		})

		// Prefer one connector for large payments. This only reorders the
		// accounts routing already allows.
		app.OnRoute(func(r paymux.RouteRequest) []string {
			if r.Currency == "INR" && r.Amount >= 5_000_000 { // Rs 50,000 and up
				return []string{"razorpay"}
			}
			return nil
		})
	})
}
