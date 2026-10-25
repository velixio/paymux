package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"
)

// Dispute is a row of disputes.
type Dispute struct {
	ID, OrgID, AppID, Mode, PaymentID, AttemptID, ProviderAccountID, ProviderDisputeID string
	Amount                                                                             int64
	Currency, Reason, Status, Stage                                                    string
	DueBy                                                                              time.Time
	Source                                                                             string
	Evidence                                                                           string // JSON: notes and blob ids
	Version                                                                            int64
	CreatedAt, UpdatedAt                                                               time.Time
}

const disputeCols = `id, org_id, app_id, mode, payment_id, attempt_id, provider_account_id, provider_dispute_id, amount,
	currency, COALESCE(reason,''), status, COALESCE(stage,''), due_by, source, evidence, version, created_at, updated_at`

func scanDispute(row interface{ Scan(...any) error }) (Dispute, error) {
	var d Dispute
	var due sql.NullInt64
	var created, updated int64
	err := row.Scan(&d.ID, &d.OrgID, &d.AppID, &d.Mode, &d.PaymentID, &d.AttemptID, &d.ProviderAccountID, &d.ProviderDisputeID,
		&d.Amount, &d.Currency, &d.Reason, &d.Status, &d.Stage, &due, &d.Source, &d.Evidence, &d.Version, &created, &updated)
	if err != nil {
		return Dispute{}, notFound(err)
	}
	d.DueBy, d.CreatedAt, d.UpdatedAt = fromNullMS(due), fromMS(created), fromMS(updated)
	return d, nil
}

func nullMSTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return ms(t)
}

// InsertDispute inserts d (version 1). A repeat of (account, provider id)
// returns ErrConflict.
func InsertDispute(ctx context.Context, tx Tx, d Dispute) error {
	if d.Evidence == "" {
		d.Evidence = "{}"
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO disputes (id, org_id, app_id, mode, payment_id, attempt_id, provider_account_id,
		provider_dispute_id, amount, currency, reason, status, stage, due_by, source, evidence, version, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.OrgID, d.AppID, d.Mode, d.PaymentID, d.AttemptID, d.ProviderAccountID, d.ProviderDisputeID, d.Amount,
		d.Currency, nullIfEmpty(d.Reason), d.Status, nullIfEmpty(d.Stage), nullMSTime(d.DueBy), d.Source, d.Evidence,
		d.Version, ms(d.CreatedAt), ms(d.UpdatedAt))
	if IsUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

// GetDispute loads a dispute by id.
func GetDispute(ctx context.Context, q Queryer, id string) (Dispute, error) {
	return scanDispute(q.QueryRowContext(ctx, "SELECT "+disputeCols+" FROM disputes WHERE id = ?", id))
}

// FindDispute loads a dispute by the delivering account and the provider's id.
func FindDispute(ctx context.Context, q Queryer, accountID, providerDisputeID string) (Dispute, error) {
	return scanDispute(q.QueryRowContext(ctx, "SELECT "+disputeCols+" FROM disputes WHERE provider_account_id = ? AND provider_dispute_id = ?",
		accountID, providerDisputeID))
}

// UpdateDispute writes the mutable fields of d if its stored version is
// d.Version, then bumps the version.
func UpdateDispute(ctx context.Context, tx Tx, d *Dispute, now time.Time) error {
	res, err := tx.ExecContext(ctx, `UPDATE disputes SET status = ?, stage = ?, reason = ?, due_by = ?, evidence = ?, amount = ?,
		version = version + 1, updated_at = ? WHERE id = ? AND version = ?`,
		d.Status, nullIfEmpty(d.Stage), nullIfEmpty(d.Reason), nullMSTime(d.DueBy), d.Evidence, d.Amount, ms(now), d.ID, d.Version)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrConflict
	}
	d.Version++
	d.UpdatedAt = now
	return nil
}

// ListDisputes returns an org's disputes, soonest deadline first (disputes
// without one last), optionally only the open ones.
func ListDisputes(ctx context.Context, q Queryer, orgID string, openOnly bool, limit int) ([]Dispute, error) {
	query := "SELECT " + disputeCols + " FROM disputes WHERE org_id = ?"
	if openOnly {
		query += " AND status IN ('needs_response','under_review')"
	}
	query += " ORDER BY (due_by IS NULL), due_by, id LIMIT ?"
	return listDisputes(ctx, q, query, orgID, limit)
}

// CountDisputesNeedingResponse is how many of an org's disputes wait for the
// merchant's answer (the nav badge); those under review wait on the provider.
func CountDisputesNeedingResponse(ctx context.Context, q Queryer, orgID string) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM disputes WHERE org_id = ? AND status = 'needs_response'`, orgID).Scan(&n)
	return n, err
}

// CountOtherOpenDisputes is how many of a payment's disputes other than
// exceptID are still open (needs_response or under_review).
func CountOtherOpenDisputes(ctx context.Context, q Queryer, paymentID, exceptID string) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM disputes WHERE payment_id = ? AND id <> ? AND status IN ('needs_response','under_review')`,
		paymentID, exceptID).Scan(&n)
	return n, err
}

// ListOpenDisputes returns every open dispute (for deadline alerts).
func ListOpenDisputes(ctx context.Context, q Queryer, limit int) ([]Dispute, error) {
	return listDisputes(ctx, q, "SELECT "+disputeCols+" FROM disputes WHERE status IN ('needs_response','under_review') ORDER BY id LIMIT ?", limit)
}

func listDisputes(ctx context.Context, q Queryer, query string, args ...any) ([]Dispute, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Dispute
	for rows.Next() {
		d, err := scanDispute(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

var alertColumns = []string{"alert_open_at", "alert_72h_at", "alert_24h_at", "alert_lost_at"}

// ClaimDisputeAlert marks one of a dispute's alerts as sent and reports
// whether this call did it, so each alert fires once. column is one of
// alert_open_at, alert_72h_at, alert_24h_at, alert_lost_at.
func ClaimDisputeAlert(ctx context.Context, tx Tx, id, column string, now time.Time) (bool, error) {
	if !slices.Contains(alertColumns, column) {
		return false, fmt.Errorf("store: unknown dispute alert column %q", column)
	}
	res, err := tx.ExecContext(ctx, "UPDATE disputes SET "+column+" = ? WHERE id = ? AND "+column+" IS NULL", ms(now), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ReleaseDisputeAlert clears a claim so the alert is retried (its delivery failed).
func ReleaseDisputeAlert(ctx context.Context, tx Tx, id, column string) error {
	if !slices.Contains(alertColumns, column) {
		return fmt.Errorf("store: unknown dispute alert column %q", column)
	}
	_, err := tx.ExecContext(ctx, "UPDATE disputes SET "+column+" = NULL WHERE id = ?", id)
	return err
}

// ListActiveProviderAccounts returns every active provider account (for polling,
// reconciliation and the silence detector).
func ListActiveProviderAccounts(ctx context.Context, q Queryer) ([]ProviderAccount, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+providerAccountCols+" FROM provider_accounts WHERE status = 'active' ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProviderAccount
	for rows.Next() {
		pa, err := scanProviderAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, pa)
	}
	return out, rows.Err()
}
