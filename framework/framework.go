// Package framework runs Paymux as a library, for a custom binary: the same
// commands as the paymux binary (serve, migrate, app, provider, ...), the
// built-in connectors, and your own code — hooks, extra connectors — linked
// in. See docs/extending.md.
//
//	func main() {
//	    framework.Main(func(app *paymux.App) {
//	        app.OnPaymentSucceeded(func(ctx context.Context, e paymux.PaymentEvent) error {
//	            return nil
//	        })
//	    })
//	}
//
// A module that only extends Paymux (a connector, a set of hooks) is imported
// for its side effects; it registers a connector in init() with
// connector.Register, and hooks with OnSetup. "paymux build --with" produces a
// main package that does exactly this.
package framework

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
	_ "time/tzdata"

	// The built-in connectors, the same set as the paymux binary.
	_ "github.com/paymux/paymux/connectors/payu"
	_ "github.com/paymux/paymux/connectors/razorpay"
	_ "github.com/paymux/paymux/connectors/simulator"
	_ "github.com/paymux/paymux/connectors/whop"
	_ "github.com/paymux/paymux/connectors/zoho"

	"io"

	"github.com/paymux/paymux"
	"github.com/paymux/paymux/internal/cli"
)

var (
	mu     sync.Mutex
	setups []func(*paymux.App)
)

// OnSetup registers f to run on the App, before it starts, in every run of
// "serve" in this binary. Modules linked in by "paymux build --with" call it
// from init() to register their hooks. Functions run in registration order,
// ahead of those passed to Run or Main.
func OnSetup(f func(*paymux.App)) {
	if f == nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	setups = append(setups, f)
}

// Run executes the command line (args without the program name) and returns
// the exit code. serve calls the OnSetup functions and then setup on the App
// before starting it.
func Run(ctx context.Context, args []string, lookupEnv func(string) (string, bool), stdout, stderr io.Writer, setup ...func(*paymux.App)) int {
	mu.Lock()
	all := append(append([]func(*paymux.App){}, setups...), setup...)
	mu.Unlock()
	return cli.RunWith(ctx, args, lookupEnv, stdout, stderr, all)
}

// Main is the whole of a custom binary's main function: it runs the command
// line from os.Args until SIGINT or SIGTERM and exits the process.
func Main(setup ...func(*paymux.App)) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := Run(ctx, os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr, setup...)
	stop()
	os.Exit(code)
}
