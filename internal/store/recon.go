package store

import (
	"context"
	"database/sql"
	"time"
)

// ReconRun is a row of recon_runs.
type ReconRun struct {
	ID, OrgID, ProviderAccountID, Status, Error string
	WindowFrom, WindowTo, StartedAt, FinishedAt time.Time
	Checked, Items, Fixed                       int
}

// InsertReconRun inserts a running run.
func InsertReconRun(ctx context.Context, tx Tx, r ReconRun) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO recon_runs (id, org_id, provider_account_id, window_from, window_to, status, started_at)
		VALUES (?, ?, ?, ?, ?, 'running', ?)`, r.ID, r.OrgID, r.ProviderAccountID, ms(r.WindowFrom), ms(r.WindowTo), ms(r.StartedAt))
	return err
}

// FinishReconRun records the outcome of a run (status ok or failed).
func FinishReconRun(ctx context.Context, tx Tx, r ReconRun) error {
	_, err := tx.ExecContext(ctx, `UPDATE recon_runs SET status = ?, checked = ?, items = ?, fixed = ?, error = ?, finished_at = ? WHERE id = ?`,
		r.Status, r.Checked, r.Items, r.Fixed, nullIfEmpty(r.Error), ms(r.FinishedAt), r.ID)
	return err
}

// LastReconStart returns when the latest run of the account started.
func LastReconStart(ctx context.Context, q Queryer, accountID string) (time.Time, bool, error) {
	var v sql.NullInt64
	err := q.QueryRowContext(ctx, "SELECT MAX(started_at) FROM recon_runs WHERE provider_account_id = ?", accountID).Scan(&v)
	if err != nil || !v.Valid {
		return time.Time{}, false, err
	}
	return fromMS(v.Int64), true, nil
}

// GetReconRun loads a run.
func GetReconRun(ctx context.Context, q Queryer, id string) (ReconRun, error) {
	var r ReconRun
	var from, to, started int64
	var finished sql.NullInt64
	var errText sql.NullString
	err := q.QueryRowContext(ctx, `SELECT id, org_id, provider_account_id, window_from, window_to, status, checked, items, fixed,
		error, started_at, finished_at FROM recon_runs WHERE id = ?`, id).
		Scan(&r.ID, &r.OrgID, &r.ProviderAccountID, &from, &to, &r.Status, &r.Checked, &r.Items, &r.Fixed, &errText, &started, &finished)
	if err != nil {
		return ReconRun{}, notFound(err)
	}
	r.WindowFrom, r.WindowTo, r.StartedAt, r.FinishedAt, r.Error = fromMS(from), fromMS(to), fromMS(started), fromNullMS(finished), errText.String
	return r, nil
}

// ListReconRuns returns an org's runs for a mode, newest first. An empty mode
// includes both modes.
func ListReconRuns(ctx context.Context, q Queryer, orgID, mode string, limit int) ([]ReconRun, error) {
	query := `SELECT r.id FROM recon_runs r JOIN provider_accounts p ON p.id = r.provider_account_id WHERE r.org_id = ?`
	args := []any{orgID}
	if mode != "" {
		query += ` AND p.mode = ?`
		args = append(args, mode)
	}
	query += ` ORDER BY r.started_at DESC, r.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var idsOut []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		idsOut = append(idsOut, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]ReconRun, 0, len(idsOut))
	for _, id := range idsOut {
		r, err := GetReconRun(ctx, q, id)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// ReconItem is a row of recon_items.
type ReconItem struct {
	ID, RunID, OrgID, ProviderAccountID, Kind, ProviderID string
	Ours, Theirs, State, Note                             string
	CreatedAt, ResolvedAt                                 time.Time
}

// FindOpenReconItem returns the open item of the same object and kind, if any.
func FindOpenReconItem(ctx context.Context, q Queryer, accountID, kind, providerID string) (ReconItem, bool, error) {
	items, err := listReconItems(ctx, q, `WHERE provider_account_id = ? AND kind = ? AND provider_id = ? AND state IN ('open','to_link')`, accountID, kind, providerID)
	if err != nil || len(items) == 0 {
		return ReconItem{}, false, err
	}
	return items[0], true, nil
}

// InsertReconItem inserts i.
func InsertReconItem(ctx context.Context, tx Tx, i ReconItem) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO recon_items (id, run_id, org_id, provider_account_id, kind, provider_id, ours, theirs, state, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, i.ID, i.RunID, i.OrgID, i.ProviderAccountID, i.Kind, i.ProviderID,
		nullIfEmpty(i.Ours), i.Theirs, i.State, ms(i.CreatedAt))
	return err
}

// CloseReconItems marks the open items of one object and kind as fixed.
func CloseReconItems(ctx context.Context, tx Tx, accountID, kind, providerID string) error {
	_, err := tx.ExecContext(ctx, "UPDATE recon_items SET state = 'fixed' WHERE provider_account_id = ? AND kind = ? AND provider_id = ? AND state IN ('open','to_link')",
		accountID, kind, providerID)
	return err
}

// TouchReconItem points an existing open item at the latest run that saw it.
func TouchReconItem(ctx context.Context, tx Tx, id, runID string) error {
	_, err := tx.ExecContext(ctx, "UPDATE recon_items SET run_id = ? WHERE id = ?", runID, id)
	return err
}

// ListReconItemsOfRun returns the items a run recorded.
func ListReconItemsOfRun(ctx context.Context, q Queryer, runID string) ([]ReconItem, error) {
	return listReconItems(ctx, q, "WHERE run_id = ?", runID)
}

// ListReconItems returns an org's items, optionally only unresolved ones.
func ListReconItems(ctx context.Context, q Queryer, orgID string, unresolvedOnly bool) ([]ReconItem, error) {
	if unresolvedOnly {
		return listReconItems(ctx, q, "WHERE org_id = ? AND state IN ('open','to_link')", orgID)
	}
	return listReconItems(ctx, q, "WHERE org_id = ?", orgID)
}

// GetReconItem loads one item.
func GetReconItem(ctx context.Context, q Queryer, id string) (ReconItem, error) {
	items, err := listReconItems(ctx, q, "WHERE id = ?", id)
	if err != nil {
		return ReconItem{}, err
	}
	if len(items) == 0 {
		return ReconItem{}, ErrNotFound
	}
	return items[0], nil
}

// ResolveReconItem marks an item resolved with a note.
func ResolveReconItem(ctx context.Context, tx Tx, id, note string, now time.Time) error {
	res, err := tx.ExecContext(ctx, "UPDATE recon_items SET state = 'resolved', note = ?, resolved_at = ? WHERE id = ?", note, ms(now), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	return nil
}

func listReconItems(ctx context.Context, q Queryer, where string, args ...any) ([]ReconItem, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, run_id, org_id, provider_account_id, kind, provider_id, COALESCE(ours,''), theirs,
		state, COALESCE(note,''), created_at, resolved_at FROM recon_items `+where+" ORDER BY created_at, id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReconItem
	for rows.Next() {
		var i ReconItem
		var created int64
		var resolved sql.NullInt64
		if err := rows.Scan(&i.ID, &i.RunID, &i.OrgID, &i.ProviderAccountID, &i.Kind, &i.ProviderID, &i.Ours, &i.Theirs, &i.State, &i.Note,
			&created, &resolved); err != nil {
			return nil, err
		}
		i.CreatedAt, i.ResolvedAt = fromMS(created), fromNullMS(resolved)
		out = append(out, i)
	}
	return out, rows.Err()
}

// ReconAttempt is our side of a payment record for reconciliation.
type ReconAttempt struct {
	AttemptID, ProviderPaymentID, ProviderOrderID, ProviderRef, Status string
	CanonicalStatus                                                    string
	AmountReceived                                                     int64
	CurrencyReceived                                                   string
	PaymentAmount                                                      int64
	PaymentCurrency                                                    string
	Historical                                                         bool
}

// ListReconAttempts returns the account's attempts created in [from, to] that
// reached a provider: they have an order or payment id, or a started checkout
// (a next action), which a provider that keys on our reference can match.
func ListReconAttempts(ctx context.Context, q Queryer, accountID string, from, to time.Time) ([]ReconAttempt, error) {
	rows, err := q.QueryContext(ctx, `SELECT a.id, COALESCE(a.provider_payment_id,''), COALESCE(a.provider_order_id,''), a.provider_ref, a.status,
		a.amount_received, COALESCE(a.currency_received,''), p.amount, p.currency
		FROM payment_attempts a JOIN payments p ON p.id = a.payment_id
		WHERE a.provider_account_id = ? AND a.created_at >= ? AND a.created_at <= ?
		AND (a.provider_payment_id IS NOT NULL OR a.provider_order_id IS NOT NULL OR a.next_action IS NOT NULL) ORDER BY a.created_at, a.id`,
		accountID, ms(from), ms(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReconAttempt
	for rows.Next() {
		var r ReconAttempt
		if err := rows.Scan(&r.AttemptID, &r.ProviderPaymentID, &r.ProviderOrderID, &r.ProviderRef, &r.Status, &r.AmountReceived, &r.CurrencyReceived,
			&r.PaymentAmount, &r.PaymentCurrency); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	// A single checkout/order can have a failed provider payment followed by
	// a successful retry. All of those are ours and all appear in the
	// provider's account-wide listing, not only the winning id copied onto the
	// canonical payment_attempts row.
	rows, err = q.QueryContext(ctx, `SELECT p.attempt_id, p.provider_payment_id, COALESCE(p.provider_order_id,''),
		a.provider_ref, p.status, p.amount, COALESCE(p.currency,''), p.amount, COALESCE(p.currency,''), a.status
		FROM provider_payment_attempts p JOIN payment_attempts a ON a.id = p.attempt_id
		WHERE p.provider_account_id = ? AND p.created_at >= ? AND p.created_at <= ?
		AND (a.provider_payment_id IS NULL OR a.provider_payment_id <> p.provider_payment_id)
		ORDER BY p.created_at, p.provider_payment_id`, accountID, ms(from), ms(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r ReconAttempt
		if err := rows.Scan(&r.AttemptID, &r.ProviderPaymentID, &r.ProviderOrderID, &r.ProviderRef, &r.Status,
			&r.AmountReceived, &r.CurrencyReceived, &r.PaymentAmount, &r.PaymentCurrency, &r.CanonicalStatus); err != nil {
			return nil, err
		}
		r.Historical = true
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReconRefund is our side of a refund record for reconciliation.
type ReconRefund struct {
	RefundID, ProviderRef, ProviderRefundID, Status, Currency string
	Amount                                                    int64
}

// ListReconRefunds returns refunds of the account's attempts created in [from, to].
func ListReconRefunds(ctx context.Context, q Queryer, accountID string, from, to time.Time) ([]ReconRefund, error) {
	rows, err := q.QueryContext(ctx, `SELECT r.id, r.provider_ref, COALESCE(r.provider_refund_id,''), r.status, r.currency, r.amount
		FROM refunds r JOIN payment_attempts a ON a.id = r.attempt_id
		WHERE a.provider_account_id = ? AND r.created_at >= ? AND r.created_at <= ? ORDER BY r.created_at, r.id`,
		accountID, ms(from), ms(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReconRefund
	for rows.Next() {
		var r ReconRefund
		if err := rows.Scan(&r.RefundID, &r.ProviderRef, &r.ProviderRefundID, &r.Status, &r.Currency, &r.Amount); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
