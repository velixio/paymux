package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// TaxEntity is a row of tax_entities: one legal entity / GSTIN.
type TaxEntity struct {
	ID, OrgID, LegalName, TradeName, GSTIN, PAN, Address, StateCode string
	// LUT is the letter of undertaking; zero times mean none.
	LUTNumber                                             string
	LUTValidFrom, LUTValidTo                              time.Time
	InvoicePrefix, CreditNotePrefix, LogoBlobID           string
	SignatoryName, FooterNote, SupportEmail, TurnoverBand string
	CreatedAt, UpdatedAt                                  time.Time
	// DSCSubject and DSCNotAfter describe the digital signature certificate
	// uploaded for the entity, if any; the sealed file is EntityDSC.
	DSCSubject  string
	DSCNotAfter time.Time
}

const taxEntityCols = `id, org_id, legal_name, trade_name, gstin, pan, address, state_code, COALESCE(lut_number,''),
	COALESCE(lut_valid_from,0), COALESCE(lut_valid_to,0), invoice_prefix, credit_note_prefix, COALESCE(logo_blob_id,''),
	signatory_name, footer_note, support_email, turnover_band, created_at, updated_at, dsc_subject, COALESCE(dsc_not_after,0)`

func zeroMS(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return ms(t)
}

func fromMSZero(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return fromMS(v)
}

func scanTaxEntity(row interface{ Scan(...any) error }) (TaxEntity, error) {
	var e TaxEntity
	var from, to, created, updated, dscNotAfter int64
	err := row.Scan(&e.ID, &e.OrgID, &e.LegalName, &e.TradeName, &e.GSTIN, &e.PAN, &e.Address, &e.StateCode, &e.LUTNumber,
		&from, &to, &e.InvoicePrefix, &e.CreditNotePrefix, &e.LogoBlobID, &e.SignatoryName, &e.FooterNote,
		&e.SupportEmail, &e.TurnoverBand, &created, &updated, &e.DSCSubject, &dscNotAfter)
	if err != nil {
		return TaxEntity{}, notFound(err)
	}
	e.DSCNotAfter = fromMSZero(dscNotAfter)
	e.LUTValidFrom, e.LUTValidTo = fromMSZero(from), fromMSZero(to)
	e.CreatedAt, e.UpdatedAt = fromMS(created), fromMS(updated)
	return e, nil
}

// InsertTaxEntity inserts e. A second entity with the same GSTIN in the org
// fails with ErrConflict.
func InsertTaxEntity(ctx context.Context, tx Tx, e TaxEntity) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO tax_entities (id, org_id, legal_name, trade_name, gstin, pan, address,
		state_code, lut_number, lut_valid_from, lut_valid_to, invoice_prefix, credit_note_prefix, logo_blob_id,
		signatory_name, footer_note, support_email, turnover_band, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.OrgID, e.LegalName, e.TradeName, e.GSTIN, e.PAN, e.Address, e.StateCode, nullIfEmpty(e.LUTNumber),
		zeroMS(e.LUTValidFrom), zeroMS(e.LUTValidTo), e.InvoicePrefix, e.CreditNotePrefix, nullIfEmpty(e.LogoBlobID),
		e.SignatoryName, e.FooterNote, e.SupportEmail, e.TurnoverBand, ms(e.CreatedAt), ms(e.UpdatedAt))
	if IsUniqueViolation(err) {
		return fmt.Errorf("%w: a tax entity with this GSTIN already exists", ErrConflict)
	}
	return err
}

// GetTaxEntity loads a tax entity by id.
func GetTaxEntity(ctx context.Context, q Queryer, id string) (TaxEntity, error) {
	return scanTaxEntity(q.QueryRowContext(ctx, "SELECT "+taxEntityCols+" FROM tax_entities WHERE id = ?", id))
}

// FindTaxEntityByGSTIN loads the entity with the given GSTIN in an org.
func FindTaxEntityByGSTIN(ctx context.Context, q Queryer, orgID, gstin string) (TaxEntity, error) {
	return scanTaxEntity(q.QueryRowContext(ctx, "SELECT "+taxEntityCols+" FROM tax_entities WHERE org_id = ? AND gstin = ?", orgID, gstin))
}

// AppTaxProfile is a row of app_tax_profiles.
type AppTaxProfile struct {
	AppID, TaxEntityID              string
	IssueInvoices                   bool
	SACCode                         string
	GSTRateBP                       int
	PricesIncludeTax                bool
	InvoicePrefix, CreditNotePrefix string // optional per-app series prefixes
	CreatedAt, UpdatedAt            time.Time
}

// UpsertAppTaxProfile inserts or replaces the profile of an app.
func UpsertAppTaxProfile(ctx context.Context, tx Tx, p AppTaxProfile, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO app_tax_profiles (app_id, tax_entity_id, issue_invoices, sac_code,
		gst_rate_bp, prices_include_tax, invoice_prefix, credit_note_prefix, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (app_id) DO UPDATE SET tax_entity_id = excluded.tax_entity_id, issue_invoices = excluded.issue_invoices,
		sac_code = excluded.sac_code, gst_rate_bp = excluded.gst_rate_bp, prices_include_tax = excluded.prices_include_tax,
		invoice_prefix = excluded.invoice_prefix, credit_note_prefix = excluded.credit_note_prefix, updated_at = excluded.updated_at`,
		p.AppID, p.TaxEntityID, boolInt(p.IssueInvoices), p.SACCode, p.GSTRateBP, boolInt(p.PricesIncludeTax),
		nullIfEmpty(p.InvoicePrefix), nullIfEmpty(p.CreditNotePrefix), ms(now), ms(now))
	return err
}

// GetAppTaxProfile loads the profile of an app; ErrNotFound when the app has none.
func GetAppTaxProfile(ctx context.Context, q Queryer, appID string) (AppTaxProfile, error) {
	var p AppTaxProfile
	var issue, incl int
	var created, updated int64
	err := q.QueryRowContext(ctx, `SELECT app_id, tax_entity_id, issue_invoices, sac_code, gst_rate_bp, prices_include_tax,
		COALESCE(invoice_prefix,''), COALESCE(credit_note_prefix,''), created_at, updated_at
		FROM app_tax_profiles WHERE app_id = ?`, appID).
		Scan(&p.AppID, &p.TaxEntityID, &issue, &p.SACCode, &p.GSTRateBP, &incl, &p.InvoicePrefix, &p.CreditNotePrefix, &created, &updated)
	if err != nil {
		return AppTaxProfile{}, notFound(err)
	}
	p.IssueInvoices, p.PricesIncludeTax = issue == 1, incl == 1
	p.CreatedAt, p.UpdatedAt = fromMS(created), fromMS(updated)
	return p, nil
}

// NextDocumentNumber takes the next sequence number of a series inside the
// caller's transaction. The UPDATE locks the series row until commit, so
// concurrent issuers queue and the numbers stay gapless: a rolled-back
// transaction hands its number back.
func NextDocumentNumber(ctx context.Context, tx Tx, entityID, kind, prefix, fiscalYear string) (int64, error) {
	if _, err := tx.ExecContext(ctx, `INSERT INTO document_series (tax_entity_id, kind, prefix, fiscal_year, next_number)
		VALUES (?, ?, ?, ?, 1) ON CONFLICT DO NOTHING`, entityID, kind, prefix, fiscalYear); err != nil {
		return 0, err
	}
	var next int64
	err := tx.QueryRowContext(ctx, `UPDATE document_series SET next_number = next_number + 1
		WHERE tax_entity_id = ? AND kind = ? AND prefix = ? AND fiscal_year = ? RETURNING next_number - 1`,
		entityID, kind, prefix, fiscalYear).Scan(&next)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, errors.New("store: document series vanished")
	}
	return next, err
}

// FXRate is a row of fx_rates; Rate is a decimal string (INR per 1 unit).
type FXRate struct {
	OrgID, Currency, Date, Rate, Source string
}

// UpsertFXRate stores the rate for a currency and IST calendar date (YYYY-MM-DD).
func UpsertFXRate(ctx context.Context, tx Tx, r FXRate, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO fx_rates (org_id, currency, rate_date, rate_to_inr, source, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (org_id, currency, rate_date) DO UPDATE SET rate_to_inr = excluded.rate_to_inr, source = excluded.source`,
		r.OrgID, r.Currency, r.Date, r.Rate, r.Source, ms(now))
	return err
}

// GetFXRate loads the rate for a currency and date; ErrNotFound when none.
func GetFXRate(ctx context.Context, q Queryer, orgID, currency, date string) (FXRate, error) {
	r := FXRate{OrgID: orgID, Currency: currency, Date: date}
	err := q.QueryRowContext(ctx, `SELECT rate_to_inr, source FROM fx_rates WHERE org_id = ? AND currency = ? AND rate_date = ?`,
		orgID, currency, date).Scan(&r.Rate, &r.Source)
	if err != nil {
		return FXRate{}, notFound(err)
	}
	return r, nil
}

// EntityDSC is the digital signature certificate of a tax entity. Enc is the
// PKCS#12 file and its password, sealed by the caller; the rest is readable.
type EntityDSC struct {
	Enc                  []byte
	Subject, Fingerprint string
	NotAfter, UploadedAt time.Time
}

// SetTaxEntityDSC stores (or replaces) the entity's DSC; ErrNotFound for an unknown entity.
func SetTaxEntityDSC(ctx context.Context, tx Tx, entityID string, d EntityDSC, now time.Time) error {
	res, err := tx.ExecContext(ctx, `UPDATE tax_entities SET dsc_enc = ?, dsc_subject = ?, dsc_fingerprint = ?, dsc_not_after = ?,
		dsc_uploaded_at = ?, updated_at = ? WHERE id = ?`, d.Enc, d.Subject, d.Fingerprint, ms(d.NotAfter), ms(d.UploadedAt), ms(now), entityID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ClearTaxEntityDSC removes the entity's DSC; ErrNotFound for an unknown entity.
func ClearTaxEntityDSC(ctx context.Context, tx Tx, entityID string, now time.Time) error {
	res, err := tx.ExecContext(ctx, `UPDATE tax_entities SET dsc_enc = NULL, dsc_subject = '', dsc_fingerprint = '', dsc_not_after = NULL,
		dsc_uploaded_at = NULL, updated_at = ? WHERE id = ?`, ms(now), entityID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetTaxEntityDSC loads the entity's DSC; ok is false when it has none.
func GetTaxEntityDSC(ctx context.Context, q Queryer, entityID string) (d EntityDSC, ok bool, err error) {
	var notAfter, uploaded int64
	err = q.QueryRowContext(ctx, `SELECT dsc_enc, dsc_subject, dsc_fingerprint, COALESCE(dsc_not_after,0), COALESCE(dsc_uploaded_at,0)
		FROM tax_entities WHERE id = ?`, entityID).Scan(&d.Enc, &d.Subject, &d.Fingerprint, &notAfter, &uploaded)
	if err != nil {
		return EntityDSC{}, false, notFound(err)
	}
	if len(d.Enc) == 0 {
		return EntityDSC{}, false, nil
	}
	d.NotAfter, d.UploadedAt = fromMSZero(notAfter), fromMSZero(uploaded)
	return d, true, nil
}
