package store

import (
	"context"
	"database/sql"
	"time"
)

// ListTaxEntities lists the org's tax entities, oldest first.
func ListTaxEntities(ctx context.Context, q Queryer, orgID string) ([]TaxEntity, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+taxEntityCols+" FROM tax_entities WHERE org_id = ? ORDER BY created_at, id", orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaxEntity
	for rows.Next() {
		e, err := scanTaxEntity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// UpdateTaxEntity replaces every editable field of the entity. The id, org
// and creation time are kept. A GSTIN that belongs to another entity returns
// ErrConflict, an unknown id ErrNotFound.
func UpdateTaxEntity(ctx context.Context, tx Tx, e TaxEntity, now time.Time) error {
	res, err := tx.ExecContext(ctx, `UPDATE tax_entities SET legal_name = ?, trade_name = ?, gstin = ?, pan = ?, address = ?,
		state_code = ?, lut_number = ?, lut_valid_from = ?, lut_valid_to = ?, invoice_prefix = ?, credit_note_prefix = ?,
		logo_blob_id = ?, signatory_name = ?, footer_note = ?, support_email = ?, turnover_band = ?, updated_at = ?
		WHERE id = ?`,
		e.LegalName, e.TradeName, e.GSTIN, e.PAN, e.Address, e.StateCode, nullIfEmpty(e.LUTNumber), zeroMS(e.LUTValidFrom),
		zeroMS(e.LUTValidTo), e.InvoicePrefix, e.CreditNotePrefix, nullIfEmpty(e.LogoBlobID), e.SignatoryName, e.FooterNote,
		e.SupportEmail, e.TurnoverBand, ms(now), e.ID)
	if IsUniqueViolation(err) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DocFilter selects invoices or credit notes. Empty fields match anything;
// From is inclusive and To exclusive (on the issue time). Flag matches a
// document carrying exactly that flag.
type DocFilter struct {
	EntityID, AppID, Mode, Flag string
	From, To                    time.Time
}

func (f DocFilter) where(afterID string) (string, []any) {
	cond, args := "1 = 1", []any{}
	add := func(c string, a ...any) {
		cond += " AND " + c
		args = append(args, a...)
	}
	if f.EntityID != "" {
		add("tax_entity_id = ?", f.EntityID)
	}
	if f.AppID != "" {
		add("app_id = ?", f.AppID)
	}
	if f.Mode != "" {
		add("mode = ?", f.Mode)
	}
	if !f.From.IsZero() {
		add("issued_at >= ?", ms(f.From))
	}
	if !f.To.IsZero() {
		add("issued_at < ?", ms(f.To))
	}
	if f.Flag != "" {
		add(`flags LIKE ? ESCAPE '\'`, `%"`+likeEscape(f.Flag)+`"%`)
	}
	if afterID != "" {
		add("id < ?", afterID)
	}
	return cond, args
}

// SearchInvoices lists matching invoices, newest id first. afterID is an
// exclusive cursor.
func SearchInvoices(ctx context.Context, q Queryer, f DocFilter, afterID string, limit int) ([]Invoice, error) {
	cond, args := f.where(afterID)
	return listInvoices(ctx, q, cond+" ORDER BY id DESC LIMIT ?", append(args, limit)...)
}

// SearchCreditNotes lists matching credit notes, newest id first.
func SearchCreditNotes(ctx context.Context, q Queryer, f DocFilter, afterID string, limit int) ([]CreditNote, error) {
	cond, args := f.where(afterID)
	return listCreditNotes(ctx, q, cond+" ORDER BY id DESC LIMIT ?", append(args, limit)...)
}

// ListFXRates returns the most recent rates, newest date first.
func ListFXRates(ctx context.Context, q Queryer, orgID string, limit int) ([]FXRate, error) {
	rows, err := q.QueryContext(ctx, `SELECT currency, rate_date, rate_to_inr, source FROM fx_rates WHERE org_id = ?
		ORDER BY rate_date DESC, currency LIMIT ?`, orgID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FXRate
	for rows.Next() {
		r := FXRate{OrgID: orgID}
		if err := rows.Scan(&r.Currency, &r.Date, &r.Rate, &r.Source); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListDomains lists an app's custom domains, oldest first.
func ListDomains(ctx context.Context, q Queryer, appID string) ([]Domain, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, org_id, app_id, host, is_primary, status, token, last_check_at,
		COALESCE(last_error,''), created_at FROM app_domains WHERE app_id = ? ORDER BY created_at, id`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Domain
	for rows.Next() {
		var d Domain
		var primary int
		var checked sql.NullInt64
		var created int64
		if err := rows.Scan(&d.ID, &d.OrgID, &d.AppID, &d.Host, &primary, &d.Status, &d.Token, &checked, &d.LastError, &created); err != nil {
			return nil, err
		}
		d.IsPrimary, d.LastCheckAt, d.CreatedAt = primary == 1, fromNullMS(checked), fromMS(created)
		out = append(out, d)
	}
	return out, rows.Err()
}

// UpdateCustomerBilling replaces a customer's billing details.
func UpdateCustomerBilling(ctx context.Context, tx Tx, customerID string, b Billing, now time.Time) error {
	res, err := tx.ExecContext(ctx, `UPDATE customers SET billing_legal_name = ?, billing_gstin = ?, billing_line1 = ?,
		billing_line2 = ?, billing_city = ?, billing_state_code = ?, billing_postal_code = ?, billing_country = ?, updated_at = ?
		WHERE id = ?`, nullIfEmpty(b.LegalName), nullIfEmpty(b.GSTIN), nullIfEmpty(b.Line1), nullIfEmpty(b.Line2),
		nullIfEmpty(b.City), nullIfEmpty(b.StateCode), nullIfEmpty(b.PostalCode), nullIfEmpty(b.Country), ms(now), customerID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
