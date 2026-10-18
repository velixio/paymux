package store

import (
	"context"
	"encoding/json"
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
	StatementDescriptor, ProviderGSTIN       string
	AcceptedCurrencies, EnabledPurposes      []string
	// MerchantOfRecord says the provider sells as merchant of record for this
	// account, as its live connector reported when the account was created.
	MerchantOfRecord bool
	CreatedAt        time.Time
}

// InsertProviderAccount inserts pa. Nil currency or purpose lists store [] and
// ["one_time"].
func InsertProviderAccount(ctx context.Context, tx Tx, pa ProviderAccount) error {
	cur, _ := json.Marshal(nonNilSlice(pa.AcceptedCurrencies))
	purposes := pa.EnabledPurposes
	if purposes == nil {
		purposes = []string{"one_time"}
	}
	pur, _ := json.Marshal(purposes)
	_, err := tx.ExecContext(ctx, `INSERT INTO provider_accounts (id, org_id, mode, connector, name, config_enc, status,
		statement_descriptor, provider_gstin, accepted_currencies, enabled_purposes, merchant_of_record, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, pa.ID, pa.OrgID, pa.Mode, pa.Connector, pa.Name, pa.ConfigEnc,
		pa.Status, nullIfEmpty(pa.StatementDescriptor), nullIfEmpty(pa.ProviderGSTIN), string(cur), string(pur),
		boolInt(pa.MerchantOfRecord), ms(pa.CreatedAt), ms(pa.CreatedAt))
	return err
}

const providerAccountCols = `id, org_id, mode, connector, name, config_enc, status, COALESCE(statement_descriptor,''),
	COALESCE(provider_gstin,''), accepted_currencies, enabled_purposes, merchant_of_record, created_at`

func scanProviderAccount(row interface{ Scan(...any) error }) (ProviderAccount, error) {
	var pa ProviderAccount
	var cur, pur string
	var created int64
	var mor int
	err := row.Scan(&pa.ID, &pa.OrgID, &pa.Mode, &pa.Connector, &pa.Name, &pa.ConfigEnc, &pa.Status,
		&pa.StatementDescriptor, &pa.ProviderGSTIN, &cur, &pur, &mor, &created)
	if err != nil {
		return ProviderAccount{}, notFound(err)
	}
	pa.MerchantOfRecord = mor == 1
	if err := json.Unmarshal([]byte(cur), &pa.AcceptedCurrencies); err != nil {
		return ProviderAccount{}, err
	}
	if err := json.Unmarshal([]byte(pur), &pa.EnabledPurposes); err != nil {
		return ProviderAccount{}, err
	}
	pa.CreatedAt = fromMS(created)
	return pa, nil
}

// GetProviderAccount loads a provider account by id.
func GetProviderAccount(ctx context.Context, q Queryer, id string) (ProviderAccount, error) {
	return scanProviderAccount(q.QueryRowContext(ctx, "SELECT "+providerAccountCols+" FROM provider_accounts WHERE id = ?", id))
}

// UpdateProviderAccount writes the mutable fields of pa (config included).
func UpdateProviderAccount(ctx context.Context, tx Tx, pa ProviderAccount, now time.Time) error {
	cur, _ := json.Marshal(nonNilSlice(pa.AcceptedCurrencies))
	pur, _ := json.Marshal(nonNilSlice(pa.EnabledPurposes))
	res, err := tx.ExecContext(ctx, `UPDATE provider_accounts SET name = ?, config_enc = ?, status = ?,
		statement_descriptor = ?, provider_gstin = ?, accepted_currencies = ?, enabled_purposes = ?, updated_at = ?
		WHERE id = ?`, pa.Name, pa.ConfigEnc, pa.Status, nullIfEmpty(pa.StatementDescriptor), nullIfEmpty(pa.ProviderGSTIN),
		string(cur), string(pur), ms(now), pa.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	return nil
}

// LinkAppProviderAccount lets an app use a provider account. Repeats are no-ops.
func LinkAppProviderAccount(ctx context.Context, tx Tx, appID, providerAccountID string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO app_provider_accounts (app_id, provider_account_id) VALUES (?, ?)
		ON CONFLICT (app_id, provider_account_id) DO NOTHING`, appID, providerAccountID)
	return err
}

// ListAppProviderAccounts returns the accounts an app may use in one mode.
func ListAppProviderAccounts(ctx context.Context, q Queryer, appID, mode string) ([]ProviderAccount, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+providerAccountCols+` FROM provider_accounts
		WHERE mode = ? AND id IN (SELECT provider_account_id FROM app_provider_accounts WHERE app_id = ?) ORDER BY id`, mode, appID)
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

// ListEveryProviderAccount returns all provider accounts of all orgs, config
// included (encrypted). It exists for start-up work that needs every secret.
func ListEveryProviderAccount(ctx context.Context, q Queryer) ([]ProviderAccount, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+providerAccountCols+` FROM provider_accounts ORDER BY id`)
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

// UpdateCustomer writes all mutable fields of c.
func UpdateCustomer(ctx context.Context, tx Tx, c Customer, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE customers SET name = ?, email = ?, phone = ?, billing_legal_name = ?,
		billing_gstin = ?, billing_line1 = ?, billing_line2 = ?, billing_city = ?, billing_state_code = ?,
		billing_postal_code = ?, billing_country = ?, updated_at = ? WHERE id = ?`,
		nullIfEmpty(c.Name), nullIfEmpty(c.Email), nullIfEmpty(c.Phone), nullIfEmpty(c.Billing.LegalName),
		nullIfEmpty(c.Billing.GSTIN), nullIfEmpty(c.Billing.Line1), nullIfEmpty(c.Billing.Line2),
		nullIfEmpty(c.Billing.City), nullIfEmpty(c.Billing.StateCode), nullIfEmpty(c.Billing.PostalCode),
		nullIfEmpty(c.Billing.Country), ms(now), c.ID)
	return err
}
