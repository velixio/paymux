package store

import (
	"context"
	"time"
)

// Customer is a row of customers.
type Customer struct {
	ID, OrgID, AppID, Mode, ExternalRef, Name, Email, Phone string
	Billing                                                 Billing
	CreatedAt                                               time.Time
}

// Billing holds the customer's billing details.
type Billing struct {
	LegalName, GSTIN, Line1, Line2, City, StateCode, PostalCode, Country string
}

// UpsertCustomer inserts c, or, when c.ExternalRef is set and a customer with
// that reference exists in the app and mode, updates it. It returns the id of
// the stored customer.
func UpsertCustomer(ctx context.Context, tx Tx, c Customer, now time.Time) (string, error) {
	if c.ExternalRef != "" {
		var id string
		err := tx.QueryRowContext(ctx, "SELECT id FROM customers WHERE app_id = ? AND mode = ? AND external_ref = ?",
			c.AppID, c.Mode, c.ExternalRef).Scan(&id)
		switch {
		case err == nil:
			_, err = tx.ExecContext(ctx, `UPDATE customers SET name = ?, email = ?, phone = ?, billing_legal_name = ?,
				billing_gstin = ?, billing_line1 = ?, billing_line2 = ?, billing_city = ?, billing_state_code = ?,
				billing_postal_code = ?, billing_country = ?, updated_at = ? WHERE id = ?`,
				nullIfEmpty(c.Name), nullIfEmpty(c.Email), nullIfEmpty(c.Phone), nullIfEmpty(c.Billing.LegalName),
				nullIfEmpty(c.Billing.GSTIN), nullIfEmpty(c.Billing.Line1), nullIfEmpty(c.Billing.Line2),
				nullIfEmpty(c.Billing.City), nullIfEmpty(c.Billing.StateCode), nullIfEmpty(c.Billing.PostalCode),
				nullIfEmpty(c.Billing.Country), ms(now), id)
			return id, err
		case notFound(err) != ErrNotFound:
			return "", err
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO customers (id, org_id, app_id, mode, external_ref, name, email, phone,
		billing_legal_name, billing_gstin, billing_line1, billing_line2, billing_city, billing_state_code,
		billing_postal_code, billing_country, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.OrgID, c.AppID, c.Mode, nullIfEmpty(c.ExternalRef), nullIfEmpty(c.Name), nullIfEmpty(c.Email),
		nullIfEmpty(c.Phone), nullIfEmpty(c.Billing.LegalName), nullIfEmpty(c.Billing.GSTIN),
		nullIfEmpty(c.Billing.Line1), nullIfEmpty(c.Billing.Line2), nullIfEmpty(c.Billing.City),
		nullIfEmpty(c.Billing.StateCode), nullIfEmpty(c.Billing.PostalCode), nullIfEmpty(c.Billing.Country),
		ms(now), ms(now))
	return c.ID, err
}

// GetCustomer loads a customer by id.
func GetCustomer(ctx context.Context, q Queryer, id string) (Customer, error) {
	var c Customer
	var created int64
	err := q.QueryRowContext(ctx, `SELECT id, org_id, app_id, mode, COALESCE(external_ref,''), COALESCE(name,''),
		COALESCE(email,''), COALESCE(phone,''), COALESCE(billing_legal_name,''), COALESCE(billing_gstin,''),
		COALESCE(billing_line1,''), COALESCE(billing_line2,''), COALESCE(billing_city,''),
		COALESCE(billing_state_code,''), COALESCE(billing_postal_code,''), COALESCE(billing_country,''), created_at
		FROM customers WHERE id = ?`, id).
		Scan(&c.ID, &c.OrgID, &c.AppID, &c.Mode, &c.ExternalRef, &c.Name, &c.Email, &c.Phone, &c.Billing.LegalName,
			&c.Billing.GSTIN, &c.Billing.Line1, &c.Billing.Line2, &c.Billing.City, &c.Billing.StateCode,
			&c.Billing.PostalCode, &c.Billing.Country, &created)
	if err != nil {
		return Customer{}, notFound(err)
	}
	c.CreatedAt = fromMS(created)
	return c, nil
}

// ProviderAccount is a row of provider_accounts (config is opaque here).
type ProviderAccount struct {
	ID, OrgID, Mode, Connector, Name, Status string
	ConfigEnc                                []byte
	CreatedAt                                time.Time
}

// InsertProviderAccount inserts pa with default currency and purpose lists.
func InsertProviderAccount(ctx context.Context, tx Tx, pa ProviderAccount) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO provider_accounts (id, org_id, mode, connector, name, config_enc, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, pa.ID, pa.OrgID, pa.Mode, pa.Connector, pa.Name, pa.ConfigEnc,
		pa.Status, ms(pa.CreatedAt), ms(pa.CreatedAt))
	return err
}

// GetProviderAccount loads a provider account by id.
func GetProviderAccount(ctx context.Context, q Queryer, id string) (ProviderAccount, error) {
	var pa ProviderAccount
	var created int64
	err := q.QueryRowContext(ctx, `SELECT id, org_id, mode, connector, name, config_enc, status, created_at
		FROM provider_accounts WHERE id = ?`, id).
		Scan(&pa.ID, &pa.OrgID, &pa.Mode, &pa.Connector, &pa.Name, &pa.ConfigEnc, &pa.Status, &created)
	if err != nil {
		return ProviderAccount{}, notFound(err)
	}
	pa.CreatedAt = fromMS(created)
	return pa, nil
}
