package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Payout is a row of payouts: money sent out to a beneficiary. The beneficiary
// (bank account or UPI id) is personal data and is kept sealed; only the masked
// label is readable.
type Payout struct {
	ID, OrgID, Mode, AppID, ProviderAccountID string
	ProviderRef, ProviderPayoutID             string
	Amount                                    int64
	Currency, Status, RawStatus               string
	BeneficiaryEnc                            []byte
	BeneficiaryLabel                          string
	Narration                                 string
	Metadata                                  map[string]string
	FailureCode, FailureMessage               string
	SubmitAttempts                            int
	Version                                   int64
	CreatedAt, UpdatedAt, CompletedAt         time.Time
}

const payoutCols = `id, org_id, mode, app_id, provider_account_id, provider_ref, COALESCE(provider_payout_id,''), amount, currency,
	status, raw_status, beneficiary_enc, beneficiary_label, narration, metadata, failure_code, failure_message, submit_attempts,
	version, created_at, updated_at, COALESCE(completed_at,0)`

func scanPayout(row interface{ Scan(...any) error }) (Payout, error) {
	var p Payout
	var meta string
	var created, updated, completed int64
	err := row.Scan(&p.ID, &p.OrgID, &p.Mode, &p.AppID, &p.ProviderAccountID, &p.ProviderRef, &p.ProviderPayoutID, &p.Amount,
		&p.Currency, &p.Status, &p.RawStatus, &p.BeneficiaryEnc, &p.BeneficiaryLabel, &p.Narration, &meta, &p.FailureCode,
		&p.FailureMessage, &p.SubmitAttempts, &p.Version, &created, &updated, &completed)
	if err != nil {
		return Payout{}, notFound(err)
	}
	if err := json.Unmarshal([]byte(meta), &p.Metadata); err != nil {
		return Payout{}, fmt.Errorf("payout %s metadata: %w", p.ID, err)
	}
	p.CreatedAt, p.UpdatedAt, p.CompletedAt = fromMS(created), fromMS(updated), fromMSZero(completed)
	return p, nil
}

// InsertPayout inserts p. A repeated provider_ref is ErrConflict.
func InsertPayout(ctx context.Context, tx Tx, p Payout) error {
	meta, _ := json.Marshal(nonNilMap(p.Metadata))
	_, err := tx.ExecContext(ctx, `INSERT INTO payouts (id, org_id, mode, app_id, provider_account_id, provider_ref, provider_payout_id,
		amount, currency, status, raw_status, beneficiary_enc, beneficiary_label, narration, metadata, failure_code, failure_message,
		submit_attempts, version, created_at, updated_at, completed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.OrgID, p.Mode, p.AppID, p.ProviderAccountID, p.ProviderRef, nullIfEmpty(p.ProviderPayoutID), p.Amount, p.Currency,
		p.Status, p.RawStatus, p.BeneficiaryEnc, p.BeneficiaryLabel, p.Narration, string(meta), p.FailureCode, p.FailureMessage,
		p.SubmitAttempts, p.Version, ms(p.CreatedAt), ms(p.UpdatedAt), zeroMS(p.CompletedAt))
	if IsUniqueViolation(err) {
		return fmt.Errorf("%w: payout exists", ErrConflict)
	}
	return err
}

// GetPayout loads a payout by id.
func GetPayout(ctx context.Context, q Queryer, id string) (Payout, error) {
	return scanPayout(q.QueryRowContext(ctx, "SELECT "+payoutCols+" FROM payouts WHERE id = ?", id))
}

// FindPayout finds the payout of a provider account by our reference or by the
// provider's own payout id (whichever is given); ErrNotFound if neither
// matches. It never matches across accounts.
func FindPayout(ctx context.Context, q Queryer, providerAccountID, providerRef, providerPayoutID string) (Payout, error) {
	if providerRef != "" {
		p, err := scanPayout(q.QueryRowContext(ctx, "SELECT "+payoutCols+" FROM payouts WHERE provider_account_id = ? AND provider_ref = ?",
			providerAccountID, providerRef))
		if err == nil || !errors.Is(err, ErrNotFound) || providerPayoutID == "" {
			return p, err
		}
	}
	if providerPayoutID == "" {
		return Payout{}, ErrNotFound
	}
	return scanPayout(q.QueryRowContext(ctx, "SELECT "+payoutCols+" FROM payouts WHERE provider_account_id = ? AND provider_payout_id = ?",
		providerAccountID, providerPayoutID))
}

// GetPayoutForUpdate loads a payout and locks it on Postgres.
func GetPayoutForUpdate(ctx context.Context, tx Tx, id string) (Payout, error) {
	return scanPayout(tx.QueryRowContext(ctx, "SELECT "+payoutCols+" FROM payouts WHERE id = ?"+tx.Dialect().ForUpdate(), id))
}

// UpdatePayout writes the mutable fields of p if its version is unchanged
// (otherwise ErrConflict) and bumps the version.
func UpdatePayout(ctx context.Context, tx Tx, p *Payout, now time.Time) error {
	res, err := tx.ExecContext(ctx, `UPDATE payouts SET provider_payout_id = ?, status = ?, raw_status = ?, failure_code = ?,
		failure_message = ?, submit_attempts = ?, completed_at = ?, version = version + 1, updated_at = ? WHERE id = ? AND version = ?`,
		nullIfEmpty(p.ProviderPayoutID), p.Status, p.RawStatus, p.FailureCode, p.FailureMessage, p.SubmitAttempts,
		zeroMS(p.CompletedAt), ms(now), p.ID, p.Version)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrConflict
	}
	p.Version++
	p.UpdatedAt = now
	return nil
}

// ListPayouts lists an app's payouts in one mode, newest first, after the
// cursor id, optionally of one status.
func ListPayouts(ctx context.Context, q Queryer, appID, mode, status, after string, limit int) ([]Payout, error) {
	query := "SELECT " + payoutCols + " FROM payouts WHERE app_id = ? AND mode = ?"
	args := []any{appID, mode}
	if status != "" {
		query += " AND status = ?"
		args = append(args, status)
	}
	if after != "" {
		query += " AND id < ?"
		args = append(args, after)
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Payout
	for rows.Next() {
		p, err := scanPayout(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
