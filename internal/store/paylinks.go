package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// PaymentLink is a row of payment_links. Amount is a fixed price in minor
// units; when it is 0 the customer chooses, between MinAmount and MaxAmount
// (0: no upper bound).
type PaymentLink struct {
	ID, OrgID, Mode, AppID, Slug string
	Amount, MinAmount, MaxAmount int64
	Currency, Description        string
	ReturnURL                    string
	SingleUse, Active            bool
	ExpiresAt                    time.Time // zero: never
	Metadata                     map[string]string
	DeactivatedAt                time.Time
	DeactivatedReason            string // manual | used
	Version                      int64
	CreatedAt, UpdatedAt         time.Time
}

// InsertPaymentLink stores l; a taken slug is ErrConflict.
func InsertPaymentLink(ctx context.Context, tx Tx, l PaymentLink) error {
	meta, err := json.Marshal(nonNilMap(l.Metadata))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO payment_links (id, org_id, mode, app_id, slug, amount, min_amount, max_amount, currency,
		description, return_url, single_use, expires_at, metadata, active, version, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.ID, l.OrgID, l.Mode, l.AppID, l.Slug, l.Amount, l.MinAmount, l.MaxAmount, l.Currency, l.Description, l.ReturnURL,
		boolInt(l.SingleUse), zeroMS(l.ExpiresAt), string(meta), boolInt(l.Active), max(l.Version, 1), ms(l.CreatedAt), ms(l.UpdatedAt))
	if IsUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

const paymentLinkCols = `id, org_id, mode, app_id, slug, amount, min_amount, max_amount, currency, description, return_url, single_use,
	COALESCE(expires_at, 0), metadata, active, COALESCE(deactivated_at, 0), deactivated_reason, version, created_at, updated_at`

func scanPaymentLink(row interface{ Scan(...any) error }) (PaymentLink, error) {
	var l PaymentLink
	var single, active int
	var expires, deactivated, created, updated int64
	var meta string
	if err := row.Scan(&l.ID, &l.OrgID, &l.Mode, &l.AppID, &l.Slug, &l.Amount, &l.MinAmount, &l.MaxAmount, &l.Currency, &l.Description,
		&l.ReturnURL, &single, &expires, &meta, &active, &deactivated, &l.DeactivatedReason, &l.Version, &created, &updated); err != nil {
		return PaymentLink{}, notFound(err)
	}
	l.SingleUse, l.Active = single != 0, active != 0
	l.ExpiresAt, l.DeactivatedAt = fromMSZero(expires), fromMSZero(deactivated)
	l.CreatedAt, l.UpdatedAt = fromMS(created), fromMS(updated)
	if err := json.Unmarshal([]byte(meta), &l.Metadata); err != nil {
		return PaymentLink{}, fmt.Errorf("payment link %s metadata: %w", l.ID, err)
	}
	return l, nil
}

// GetPaymentLink loads a link by id.
func GetPaymentLink(ctx context.Context, q Queryer, id string) (PaymentLink, error) {
	return scanPaymentLink(q.QueryRowContext(ctx, `SELECT `+paymentLinkCols+` FROM payment_links WHERE id = ?`, id))
}

// GetPaymentLinkBySlug loads a link by the slug in its public URL.
func GetPaymentLinkBySlug(ctx context.Context, q Queryer, slug string) (PaymentLink, error) {
	return scanPaymentLink(q.QueryRowContext(ctx, `SELECT `+paymentLinkCols+` FROM payment_links WHERE slug = ?`, slug))
}

// PaymentLinkFilter selects links of one org, newest first. Empty AppID or
// Mode match all; StartingAfter is an exclusive cursor (a link id).
type PaymentLinkFilter struct {
	OrgID, AppID, Mode, StartingAfter string
	Active                            *bool
	Limit                             int
}

// ListPaymentLinks returns the links matching f.
func ListPaymentLinks(ctx context.Context, q Queryer, f PaymentLinkFilter) ([]PaymentLink, error) {
	where, args := "org_id = ?", []any{f.OrgID}
	if f.AppID != "" {
		where += " AND app_id = ?"
		args = append(args, f.AppID)
	}
	if f.Mode != "" {
		where += " AND mode = ?"
		args = append(args, f.Mode)
	}
	if f.Active != nil {
		where += " AND active = ?"
		args = append(args, boolInt(*f.Active))
	}
	if f.StartingAfter != "" {
		where += " AND id < ?"
		args = append(args, f.StartingAfter)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 25
	}
	rows, err := q.QueryContext(ctx, `SELECT `+paymentLinkCols+` FROM payment_links WHERE `+where+` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PaymentLink
	for rows.Next() {
		l, err := scanPaymentLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// DeactivatePaymentLink switches a link off for good and returns it. It is
// ErrNotFound for an unknown link and ErrConflict when the link was already
// inactive, so exactly one caller observes the change.
func DeactivatePaymentLink(ctx context.Context, tx Tx, id, reason string, now time.Time) (PaymentLink, error) {
	res, err := tx.ExecContext(ctx, `UPDATE payment_links SET active = 0, deactivated_at = ?, deactivated_reason = ?, claim_hash = '',
		claim_until = 0, version = version + 1, updated_at = ? WHERE id = ? AND active = 1`, ms(now), reason, ms(now), id)
	if err != nil {
		return PaymentLink{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		if _, err := GetPaymentLink(ctx, tx, id); err != nil {
			return PaymentLink{}, err
		}
		return PaymentLink{}, ErrConflict
	}
	return GetPaymentLink(ctx, tx, id)
}

func claimHash(holder string) string {
	sum := sha256.Sum256([]byte("paymux-link-claim|" + holder))
	return hex.EncodeToString(sum[:])
}

// ClaimPaymentLink holds an active link for holder until `until`. It reports
// false while somebody else holds it; the holder may claim again, and a lapsed
// claim is free.
func ClaimPaymentLink(ctx context.Context, tx Tx, id, holder string, until, now time.Time) (bool, error) {
	h := claimHash(holder)
	res, err := tx.ExecContext(ctx, `UPDATE payment_links SET claim_hash = ?, claim_until = ?, updated_at = ?
		WHERE id = ? AND active = 1 AND (claim_until <= ? OR claim_hash = ?)`, h, ms(until), ms(now), id, ms(now), h)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ReleasePaymentLink drops holder's claim on a link, for example when the
// payment it was taken for could not be created. Another holder's claim stays.
func ReleasePaymentLink(ctx context.Context, tx Tx, id, holder string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE payment_links SET claim_hash = '', claim_until = 0, updated_at = ?
		WHERE id = ? AND claim_hash = ?`, ms(now), id, claimHash(holder))
	return err
}
