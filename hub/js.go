package hub

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/paymux/paymux/internal/outbound"
	"github.com/paymux/paymux/plugins/jsvm"
)

// jsFetchTimeout is the longest a single fetch() from a script may take.
const jsFetchTimeout = 5 * time.Second

// startJSHooks loads the JavaScript hooks in cfg.JSHooksDir, if that directory
// exists, and backs the Go hooks with them: every event hook calls the
// scripts' handlers, and OnRoute asks onRoute. Scripts are reloaded when a
// file changes (checked every few seconds) and on SIGHUP, until ctx ends.
//
// A script error is reported like a Go hook error (log plus a hook.failed ops
// alert); a failing onRoute only means "no opinion".
func (a *App) startJSHooks(ctx context.Context) {
	dir := a.cfg.JSHooksDir
	if dir == "" {
		return
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		a.log.Debug("js hooks are off: directory not found", "dir", dir)
		return
	}
	failed := func(hook string, ev hookEvent, err error) {
		if a.hookFailed != nil {
			a.hookFailed(context.Background(), hook, ev, err)
		}
	}
	rt, err := jsvm.New(jsvm.Options{
		Dir:          dir,
		HTTPClient:   outbound.GuardedClient(a.cfg.AllowPrivateWebhooks, jsFetchTimeout),
		AllowedHosts: a.cfg.JSHooksAllowedHosts,
		Logger:       a.log,
		OnError: func(file string, err error) {
			failed("js:"+file, hookEvent{Type: "js.load", ObjectType: "file", ObjectID: file}, err)
		},
	})
	if err != nil {
		a.log.Error("js hooks are off", "dir", dir, "err", err)
		return
	}
	a.log.Info("js hooks enabled", "dir", dir, "hooks", rt.Hooks(), "allowed_hosts", a.cfg.JSHooksAllowedHosts)

	call := func(name string) func(context.Context, any) error {
		return func(ctx context.Context, e any) error {
			if err := rt.Call(ctx, name, e); err != nil {
				return fmt.Errorf("js: %w", err)
			}
			return nil
		}
	}
	a.OnPaymentSucceeded(func(ctx context.Context, e PaymentEvent) error { return call("onPaymentSucceeded")(ctx, e) })
	a.OnRefundSucceeded(func(ctx context.Context, e RefundEvent) error { return call("onRefundSucceeded")(ctx, e) })
	a.OnDisputeOpened(func(ctx context.Context, e DisputeEvent) error { return call("onDisputeOpened")(ctx, e) })
	a.OnSubscriptionRenewed(func(ctx context.Context, e SubscriptionEvent) error { return call("onSubscriptionRenewed")(ctx, e) })
	a.OnInvoiceIssued(func(ctx context.Context, e InvoiceEvent) error { return call("onInvoiceIssued")(ctx, e) })
	a.OnRoute(func(r RouteRequest) []string {
		order, err := rt.Route(context.Background(), r)
		if err != nil {
			a.log.Warn("js onRoute failed; routing keeps its own order", "err", err)
			failed("onRoute", hookEvent{Type: "js.route", ObjectType: "route", ObjectID: r.AppID}, fmt.Errorf("js: %w", err))
			return nil
		}
		return order
	})

	go rt.Watch(ctx)
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		defer signal.Stop(hup)
		defer rt.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				a.log.Info("SIGHUP: reloading js hooks")
				_ = rt.Reload()
			}
		}
	}()
}
