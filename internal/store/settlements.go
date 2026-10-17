package store

import (
	"context"
	"database/sql"
	"time"
)

// Settlement is a row of settlements.
type Settlement struct {
	ID, OrgID, Mode, ProviderAccountID, ProviderSettlementID string
	Amount, Fees, FeeTax                                     int64
	Currency, Status, UTR                                    string
	SettledAt                                                time.Time
	ItemFees, ItemFeeTax                                     int64 // sums of the provider's lines
	FeeMismatch, PayoutRecorded                              bool
	CreatedAt, UpdatedAt                                     time.Time
}

const settlementCols = `id, org_id, mode, provider_account_id, provider_settlement_id, amount, fees, fee_tax, currency, status,
	COALESCE(utr,''), settled_at, item_fees, item_fee_tax, fee_mismatch, payout_recorded, created_at, updated_at`

func scanSettlement(row interface{ Scan(...any) error }) (Settlement, error) {
	var s Settlement
	var settled sql.NullInt64
	var mismatch, payout int
	var created, updated int64
	err := row.Scan(&s.ID, &s.OrgID, &s.Mode, &s.ProviderAccountID, &s.ProviderSettlementID, &s.Amount, &s.Fees, &s.FeeTax,
		&s.Currency, &s.Status, &s.UTR, &settled, &s.ItemFees, &s.ItemFeeTax, &mismatch, &payout, &created, &updated)
	if err != nil {
		return Settlement{}, notFound(err)
	}
	s.SettledAt, s.FeeMismatch, s.PayoutRecorded = fromNullMS(settled), mismatch == 1, payout == 1
	s.CreatedAt, s.UpdatedAt = fromMS(created), fromMS(updated)
	return s, nil
}

// GetSettlementByProviderID loads a settlement of one provider account.
func GetSettlementByProviderID(ctx context.Context, q Queryer, accountID, providerSettlementID string) (Settlement, error) {
	return scanSettlement(q.QueryRowContext(ctx, "SELECT "+settlementCols+" FROM settlements WHERE provider_account_id = ? AND provider_settlement_id = ?",
		accountID, providerSettlementID))
}

// UpsertSettlement inserts s or updates the provider-reported fields of the
// existing settlement of the same account and provider id. It returns the
// stored row.
func UpsertSettlement(ctx context.Context, tx Tx, s Settlement, now time.Time) (Settlement, error) {
	_, err := tx.ExecContext(ctx, `INSERT INTO settlements (id, org_id, mode, provider_account_id, provider_settlement_id, amount, fees,
		fee_tax, currency, status, utr, settled_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (provider_account_id, provider_settlement_id) DO UPDATE SET amount = excluded.amount, fees = excluded.fees,
		fee_tax = excluded.fee_tax, currency = excluded.currency, status = excluded.status, utr = excluded.utr,
		settled_at = excluded.settled_at, updated_at = excluded.updated_at`,
		s.ID, s.OrgID, s.Mode, s.ProviderAccountID, s.ProviderSettlementID, s.Amount, s.Fees, s.FeeTax, s.Currency, s.Status,
		nullIfEmpty(s.UTR), nullMSTime(s.SettledAt), ms(now), ms(now))
	if err != nil {
		return Settlement{}, err
	}
	return GetSettlementByProviderID(ctx, tx, s.ProviderAccountID, s.ProviderSettlementID)
}

// AttachSettlementFees records the fee, the GST on it and the net amount of an
// attempt from a settlement, once: it reports false when the attempt already
// belongs to a settlement.
func AttachSettlementFees(ctx context.Context, tx Tx, attemptID, settlementID string, fee, feeTax, net int64) (bool, error) {
	res, err := tx.ExecContext(ctx, `UPDATE payment_attempts SET fee = ?, fee_tax = ?, net = ?, settlement_id = ?
		WHERE id = ? AND settlement_id IS NULL`, fee, feeTax, net, settlementID, attemptID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// SumSettlementAttemptFees totals the fee and fee GST of the attempts attached
// to a settlement.
func SumSettlementAttemptFees(ctx context.Context, q Queryer, settlementID string) (fees, feeTax int64, err error) {
	err = q.QueryRowContext(ctx, "SELECT COALESCE(SUM(fee), 0), COALESCE(SUM(fee_tax), 0) FROM payment_attempts WHERE settlement_id = ?",
		settlementID).Scan(&fees, &feeTax)
	return
}

// SetSettlementFeeCheck records the provider's line totals and whether the
// fee check found a mismatch.
func SetSettlementFeeCheck(ctx context.Context, tx Tx, id string, itemFees, itemFeeTax int64, mismatch bool) error {
	_, err := tx.ExecContext(ctx, "UPDATE settlements SET item_fees = ?, item_fee_tax = ?, fee_mismatch = ? WHERE id = ?",
		itemFees, itemFeeTax, boolInt(mismatch), id)
	return err
}

// MarkPayoutRecorded claims the right to write the payout ledger entry of a
// settlement; it reports true for the first caller only.
func MarkPayoutRecorded(ctx context.Context, tx Tx, id string) (bool, error) {
	res, err := tx.ExecContext(ctx, "UPDATE settlements SET payout_recorded = 1 WHERE id = ? AND payout_recorded = 0", id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// SettlementFees is one settlement's fees as far as a tax entity's attempts
// are concerned (for the input-tax-credit register).
type SettlementFees struct {
	Settlement     Settlement
	ProviderGSTIN  string
	Connector      string
	AccountName    string
	Attempts       int
	Fee, FeeTax    int64
	TaxEntityState string
}

// SettlementFeesForEntity returns, per settlement settled in [from, to), the
// fees of the attempts whose app belongs to the tax entity.
func SettlementFeesForEntity(ctx context.Context, q Queryer, entityID string, from, to time.Time) ([]SettlementFees, error) {
	rows, err := q.QueryContext(ctx, `SELECT s.id, COUNT(a.id), COALESCE(SUM(a.fee), 0), COALESCE(SUM(a.fee_tax), 0),
		COALESCE(pa.provider_gstin, ''), pa.connector, pa.name
		FROM settlements s
		JOIN payment_attempts a ON a.settlement_id = s.id
		JOIN payments p ON p.id = a.payment_id
		JOIN app_tax_profiles t ON t.app_id = p.app_id
		JOIN provider_accounts pa ON pa.id = s.provider_account_id
		WHERE t.tax_entity_id = ? AND s.settled_at >= ? AND s.settled_at < ?
		GROUP BY s.id, pa.provider_gstin, pa.connector, pa.name ORDER BY MIN(s.settled_at), s.id`, entityID, ms(from), ms(to))
	if err != nil {
		return nil, err
	}
	type agg struct {
		id                 string
		n                  int
		fee, tax           int64
		gstin, conn_, name string
	}
	var aggs []agg
	for rows.Next() {
		var g agg
		if err := rows.Scan(&g.id, &g.n, &g.fee, &g.tax, &g.gstin, &g.conn_, &g.name); err != nil {
			rows.Close()
			return nil, err
		}
		aggs = append(aggs, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]SettlementFees, 0, len(aggs))
	for _, g := range aggs {
		s, err := scanSettlement(q.QueryRowContext(ctx, "SELECT "+settlementCols+" FROM settlements WHERE id = ?", g.id))
		if err != nil {
			return nil, err
		}
		out = append(out, SettlementFees{Settlement: s, ProviderGSTIN: g.gstin, Connector: g.conn_, AccountName: g.name, Attempts: g.n, Fee: g.fee, FeeTax: g.tax})
	}
	return out, nil
}

// SettlementsInRange returns an org's settlements settled in [from, to), oldest first.
func SettlementsInRange(ctx context.Context, q Queryer, orgID string, from, to time.Time) ([]Settlement, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+settlementCols+" FROM settlements WHERE org_id = ? AND settled_at >= ? AND settled_at < ? ORDER BY settled_at, id",
		orgID, ms(from), ms(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Settlement
	for rows.Next() {
		s, err := scanSettlement(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
