package store

import "context"

// PaymentCount is the number of payments in one status, per connector and mode.
type PaymentCount struct {
	Status, Connector, Mode string
	N                       int64
}

// BusinessCounts are the aggregates behind the business metrics. Every label
// value comes from a closed set (state machine statuses, registered connector
// names, test/live), so the series count stays small.
type BusinessCounts struct {
	Payments []PaymentCount
	// Subscriptions counts subscriptions per status.
	Subscriptions map[string]int64
	// InvoicesIssued counts tax invoices (credit notes are separate documents).
	InvoicesIssued int64
	// WebhooksFailed counts outbound deliveries that exhausted their retries.
	WebhooksFailed int64
	// InboundUnverified counts provider webhooks that failed signature
	// verification, per connector. Housekeeping prunes old events, so it can
	// drop; Prometheus treats that as a counter reset.
	InboundUnverified map[string]int64
}

// BusinessCounts reads the aggregates in one read transaction. A payment's
// connector is the one of its latest attempt ("none" before the first attempt).
func (db *DB) BusinessCounts(ctx context.Context) (BusinessCounts, error) {
	out := BusinessCounts{Subscriptions: map[string]int64{}, InboundUnverified: map[string]int64{}}
	err := db.Read(ctx, func(q Querier) error {
		rows, err := q.QueryContext(ctx, `SELECT p.status, COALESCE(pa.connector, 'none'), p.mode, COUNT(*)
			FROM payments p
			LEFT JOIN provider_accounts pa ON pa.id = (SELECT a.provider_account_id FROM payment_attempts a
				WHERE a.payment_id = p.id ORDER BY a.created_at DESC, a.id DESC LIMIT 1)
			GROUP BY p.status, COALESCE(pa.connector, 'none'), p.mode`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var c PaymentCount
			if err := rows.Scan(&c.Status, &c.Connector, &c.Mode, &c.N); err != nil {
				rows.Close()
				return err
			}
			out.Payments = append(out.Payments, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if err := countBy(ctx, q, `SELECT status, COUNT(*) FROM subscriptions GROUP BY status`, out.Subscriptions); err != nil {
			return err
		}
		if err := countBy(ctx, q, `SELECT COALESCE(pa.connector, 'unknown'), COUNT(*) FROM provider_events e
			LEFT JOIN provider_accounts pa ON pa.id = e.provider_account_id WHERE e.verified = 0
			GROUP BY COALESCE(pa.connector, 'unknown')`, out.InboundUnverified); err != nil {
			return err
		}
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM invoices`).Scan(&out.InvoicesIssued); err != nil {
			return err
		}
		return q.QueryRowContext(ctx, `SELECT COUNT(*) FROM webhook_deliveries WHERE status = 'failed'`).Scan(&out.WebhooksFailed)
	})
	return out, err
}

func countBy(ctx context.Context, q Querier, query string, into map[string]int64) error {
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			return err
		}
		into[k] = n
	}
	return rows.Err()
}
