package store

import (
	"context"
	"time"
)

// LedgerEntry is a row of the append-only ledger_entries journal.
type LedgerEntry struct {
	ID, OrgID, Mode, Kind string
	Amount                int64 // signed
	Currency              string
	PaymentID, RefundID   string
	OccurredAt            time.Time
}

// InsertLedgerEntry appends e.
func InsertLedgerEntry(ctx context.Context, tx Tx, e LedgerEntry) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO ledger_entries (id, org_id, mode, kind, amount, currency, payment_id, refund_id, occurred_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, e.ID, e.OrgID, e.Mode, e.Kind, e.Amount, e.Currency,
		nullIfEmpty(e.PaymentID), nullIfEmpty(e.RefundID), ms(e.OccurredAt))
	return err
}

// ListLedgerEntries returns the entries of a payment, oldest first.
func ListLedgerEntries(ctx context.Context, q Queryer, paymentID string) ([]LedgerEntry, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, org_id, mode, kind, amount, currency, COALESCE(payment_id,''),
		COALESCE(refund_id,''), occurred_at FROM ledger_entries WHERE payment_id = ? ORDER BY occurred_at, id`, paymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LedgerEntry
	for rows.Next() {
		var e LedgerEntry
		var at int64
		if err := rows.Scan(&e.ID, &e.OrgID, &e.Mode, &e.Kind, &e.Amount, &e.Currency, &e.PaymentID, &e.RefundID, &at); err != nil {
			return nil, err
		}
		e.OccurredAt = fromMS(at)
		out = append(out, e)
	}
	return out, rows.Err()
}
