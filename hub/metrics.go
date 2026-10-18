package hub

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/paymux/paymux/internal/metrics"
	"github.com/paymux/paymux/internal/store"
)

// businessRefresh bounds how often a scrape runs the aggregate queries.
const businessRefresh = 15 * time.Second

// registerBusinessMetrics exposes counts that live in the database. They are
// refreshed on scrape (at most every businessRefresh), so a burst of scrapes
// costs one set of queries. Label values are closed sets: payment and
// subscription statuses, registered connector names, test/live.
func registerBusinessMetrics(reg *metrics.Registry, db *store.DB, log *slog.Logger, now func() time.Time) {
	payments := reg.Counter("paymux_payments_total", "Payments by current status, connector of the latest attempt and mode.", "status", "connector", "mode")
	subs := reg.Gauge("paymux_subscriptions", "Subscriptions by status.", "status")
	invoices := reg.Counter("paymux_invoices_issued_total", "GST invoices issued.")
	failedHooks := reg.Counter("paymux_webhook_deliveries_failed_total", "Outbound webhook deliveries that exhausted their retries.")
	unverified := reg.Counter("paymux_inbound_verification_failures_total", "Inbound provider webhooks that failed signature verification.", "connector")

	var mu sync.Mutex
	var last time.Time
	reg.OnScrape(func() {
		mu.Lock()
		defer mu.Unlock()
		if t := now(); !last.IsZero() && t.Sub(last) < businessRefresh {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		c, err := db.BusinessCounts(ctx)
		if err != nil {
			log.Warn("metrics: business counts", "err", err)
			return
		}
		last = now()
		payments.Reset()
		for _, p := range c.Payments {
			payments.Set(float64(p.N), p.Status, p.Connector, p.Mode)
		}
		subs.Reset()
		for status, n := range c.Subscriptions {
			subs.Set(float64(n), status)
		}
		invoices.Set(float64(c.InvoicesIssued))
		failedHooks.Set(float64(c.WebhooksFailed))
		unverified.Reset()
		for name, n := range c.InboundUnverified {
			unverified.Set(float64(n), name)
		}
	})
}
