package store

import (
	"context"
	"database/sql"
	"time"
)

// MarketRate is a row of market_rates: an indicative rate from a public
// provider, for display only. Rate is a decimal string (INR per 1 unit). These
// rows are deliberately not FXRates: invoices and GST must never read them.
type MarketRate struct {
	Currency, Date, Rate, Source string
}

// UpsertMarketRate stores the rate for a currency and the provider's date.
func UpsertMarketRate(ctx context.Context, tx Tx, r MarketRate, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO market_rates (currency, rate_date, rate_to_inr, source, fetched_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (currency, rate_date) DO UPDATE SET rate_to_inr = excluded.rate_to_inr, source = excluded.source,
		fetched_at = excluded.fetched_at`, r.Currency, r.Date, r.Rate, r.Source, ms(now))
	return err
}

func listMarketRates(ctx context.Context, q Queryer, query string, args ...any) ([]MarketRate, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MarketRate
	for rows.Next() {
		var r MarketRate
		if err := rows.Scan(&r.Currency, &r.Date, &r.Rate, &r.Source); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListMarketRatesFor returns every market rate of one currency, oldest date first.
func ListMarketRatesFor(ctx context.Context, q Queryer, currency string) ([]MarketRate, error) {
	return listMarketRates(ctx, q, `SELECT currency, rate_date, rate_to_inr, source FROM market_rates
		WHERE currency = ? ORDER BY rate_date`, currency)
}

// LatestMarketRates returns the newest market rate of each currency, by currency.
func LatestMarketRates(ctx context.Context, q Queryer) ([]MarketRate, error) {
	return listMarketRates(ctx, q, `SELECT m.currency, m.rate_date, m.rate_to_inr, m.source FROM market_rates m
		JOIN (SELECT currency, MAX(rate_date) AS latest FROM market_rates GROUP BY currency) l
		ON l.currency = m.currency AND l.latest = m.rate_date ORDER BY m.currency`)
}

// LastMarketFetch is when market rates were last stored; ok is false when never.
func LastMarketFetch(ctx context.Context, q Queryer) (at time.Time, ok bool, err error) {
	var v sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT MAX(fetched_at) FROM market_rates`).Scan(&v); err != nil {
		return time.Time{}, false, err
	}
	if !v.Valid {
		return time.Time{}, false, nil
	}
	return time.UnixMilli(v.Int64), true, nil
}
