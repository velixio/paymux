package store

import (
	"context"
	"time"
)

// AnalyticsFilter selects one payment cohort for dashboard analytics.
type AnalyticsFilter struct {
	OrgID, Mode, AppID, Currency, Country string
	From, To                              time.Time
	Hourly                                bool
	OffsetSeconds                         int
}

// PaymentOutcomePoint is one time bucket of payment-level outcomes. Created
// is the cohort size; the other values describe each payment's current state.
type PaymentOutcomePoint struct {
	Slot                                       int64
	Created, Paid, Failed, Waiting, Incomplete int64
}

// CountryOutcome is one customer-country cohort. Unknown contains payments
// for which neither an explicit payment country nor billing country exists.
type CountryOutcome struct {
	Country                                    string
	Created, Paid, Failed, Waiting, Incomplete int64
	PaidAmount, RefundedAmount                 int64
}

// DimensionOutcome is attempt-level provider or payment-method performance.
type DimensionOutcome struct {
	Key, Label             string
	Attempts, Paid, Failed int64
}

// FailureOutcome is one provider failure code and its frequency.
type FailureOutcome struct {
	Code, Message string
	Count         int64
}

func analyticsWhere(f AnalyticsFilter, alias string) (string, []any) {
	country := `COALESCE(NULLIF(` + alias + `.country,''), NULLIF(c.billing_country,''), 'Unknown')`
	where := alias + `.org_id = ? AND ` + alias + `.mode = ? AND ` + alias + `.created_at >= ? AND ` + alias + `.created_at < ?`
	args := []any{f.OrgID, f.Mode, ms(f.From), ms(f.To)}
	add := func(clause string, arg any) { where += " AND " + clause; args = append(args, arg) }
	if f.AppID != "" {
		add(alias+`.app_id = ?`, f.AppID)
	}
	if f.Currency != "" {
		add(alias+`.currency = ?`, f.Currency)
	}
	if f.Country != "" {
		add(country+` = ?`, f.Country)
	}
	return where, args
}

// PaymentOutcomeSeries groups payment cohorts by local hour or day.
func PaymentOutcomeSeries(ctx context.Context, q Queryer, f AnalyticsFilter) ([]PaymentOutcomePoint, error) {
	unit := int64(86_400_000)
	if f.Hourly {
		unit = 3_600_000
	}
	where, args := analyticsWhere(f, "p")
	args = append([]any{int64(f.OffsetSeconds) * 1000, unit}, args...)
	rows, err := q.QueryContext(ctx, `SELECT (p.created_at + ?) / ? AS slot, COUNT(*),
		COALESCE(SUM(CASE WHEN p.status = 'succeeded' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN p.status = 'failed' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN p.status IN ('created','requires_action','processing','needs_review') THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN p.status IN ('expired','canceled') THEN 1 ELSE 0 END),0)
		FROM payments p LEFT JOIN customers c ON c.id = p.customer_id WHERE `+where+`
		GROUP BY slot ORDER BY slot`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PaymentOutcomePoint
	for rows.Next() {
		var p PaymentOutcomePoint
		if err := rows.Scan(&p.Slot, &p.Created, &p.Paid, &p.Failed, &p.Waiting, &p.Incomplete); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CountryOutcomes ranks payment cohorts by paid count, then created count.
func CountryOutcomes(ctx context.Context, q Queryer, f AnalyticsFilter, limit int) ([]CountryOutcome, error) {
	where, args := analyticsWhere(f, "p")
	country := `COALESCE(NULLIF(p.country,''), NULLIF(c.billing_country,''), 'Unknown')`
	args = append(args, limit)
	rows, err := q.QueryContext(ctx, `SELECT `+country+`, COUNT(*),
		COALESCE(SUM(CASE WHEN p.status = 'succeeded' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN p.status = 'failed' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN p.status IN ('created','requires_action','processing','needs_review') THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN p.status IN ('expired','canceled') THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN p.status = 'succeeded' THEN p.amount_received ELSE 0 END),0),
		COALESCE(SUM(p.amount_refunded),0)
		FROM payments p LEFT JOIN customers c ON c.id = p.customer_id WHERE `+where+`
		GROUP BY `+country+` ORDER BY 3 DESC, 2 DESC, 1 LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CountryOutcome
	for rows.Next() {
		var c CountryOutcome
		if err := rows.Scan(&c.Country, &c.Created, &c.Paid, &c.Failed, &c.Waiting, &c.Incomplete, &c.PaidAmount, &c.RefundedAmount); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func dimensionOutcomes(ctx context.Context, q Queryer, f AnalyticsFilter, dimension string, limit int) ([]DimensionOutcome, error) {
	where, args := analyticsWhere(f, "p")
	key, label, joins := `COALESCE(NULLIF(o.method,''),'unknown')`, `COALESCE(NULLIF(o.method,''),'Unknown')`, ""
	if dimension == "provider" {
		key, label = `COALESCE(NULLIF(pa.connector,''),'unknown')`, `COALESCE(NULLIF(pa.connector,''),'Unknown')`
		joins = " JOIN provider_accounts pa ON pa.id = a.provider_account_id"
	}
	args = append(args, limit)
	rows, err := q.QueryContext(ctx, `WITH outcomes AS (
		SELECT pa.attempt_id, pa.status, pa.method, pa.failure_code, pa.failure_message FROM provider_payment_attempts pa
		UNION ALL
		SELECT a0.id, a0.status, a0.method, a0.failure_code, a0.failure_message FROM payment_attempts a0
		WHERE NOT EXISTS (SELECT 1 FROM provider_payment_attempts pp WHERE pp.attempt_id = a0.id)
	) SELECT `+key+`, `+label+`, COUNT(*),
		COALESCE(SUM(CASE WHEN o.status = 'captured' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN o.status = 'failed' THEN 1 ELSE 0 END),0)
		FROM outcomes o JOIN payment_attempts a ON a.id = o.attempt_id JOIN payments p ON p.id = a.payment_id
		LEFT JOIN customers c ON c.id = p.customer_id`+joins+` WHERE `+where+`
		GROUP BY `+key+`, `+label+` ORDER BY 4 DESC, 3 DESC, 1 LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DimensionOutcome
	for rows.Next() {
		var d DimensionOutcome
		if err := rows.Scan(&d.Key, &d.Label, &d.Attempts, &d.Paid, &d.Failed); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ProviderOutcomes returns attempt-level performance by provider account.
func ProviderOutcomes(ctx context.Context, q Queryer, f AnalyticsFilter, limit int) ([]DimensionOutcome, error) {
	return dimensionOutcomes(ctx, q, f, "provider", limit)
}

// MethodOutcomes returns attempt-level performance by provider payment method.
func MethodOutcomes(ctx context.Context, q Queryer, f AnalyticsFilter, limit int) ([]DimensionOutcome, error) {
	return dimensionOutcomes(ctx, q, f, "method", limit)
}

// FailureOutcomes returns the most frequent provider-side failed outcomes.
func FailureOutcomes(ctx context.Context, q Queryer, f AnalyticsFilter, limit int) ([]FailureOutcome, error) {
	where, args := analyticsWhere(f, "p")
	args = append(args, limit)
	rows, err := q.QueryContext(ctx, `WITH outcomes AS (
		SELECT pa.attempt_id, pa.status, pa.failure_code, pa.failure_message FROM provider_payment_attempts pa
		UNION ALL
		SELECT a0.id, a0.status, a0.failure_code, a0.failure_message FROM payment_attempts a0
		WHERE NOT EXISTS (SELECT 1 FROM provider_payment_attempts pp WHERE pp.attempt_id = a0.id)
	) SELECT COALESCE(NULLIF(o.failure_code,''),'unknown'), COALESCE(NULLIF(o.failure_message,''),'No provider reason'), COUNT(*)
		FROM outcomes o JOIN payment_attempts a ON a.id = o.attempt_id JOIN payments p ON p.id = a.payment_id
		LEFT JOIN customers c ON c.id = p.customer_id WHERE o.status = 'failed' AND `+where+`
		GROUP BY 1, 2 ORDER BY 3 DESC, 1 LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FailureOutcome
	for rows.Next() {
		var f FailureOutcome
		if err := rows.Scan(&f.Code, &f.Message, &f.Count); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// AnalyticsCurrencies lists currencies present in the selected cohort.
func AnalyticsCurrencies(ctx context.Context, q Queryer, f AnalyticsFilter) ([]string, error) {
	f.Currency = ""
	where, args := analyticsWhere(f, "p")
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT p.currency FROM payments p LEFT JOIN customers c ON c.id = p.customer_id
		WHERE `+where+` ORDER BY p.currency`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var currency string
		if err := rows.Scan(&currency); err != nil {
			return nil, err
		}
		out = append(out, currency)
	}
	return out, rows.Err()
}

// AnalyticsCountries lists canonical customer countries present in a cohort.
func AnalyticsCountries(ctx context.Context, q Queryer, f AnalyticsFilter) ([]string, error) {
	f.Country = ""
	where, args := analyticsWhere(f, "p")
	country := `COALESCE(NULLIF(p.country,''), NULLIF(c.billing_country,''), 'Unknown')`
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT `+country+` FROM payments p LEFT JOIN customers c ON c.id = p.customer_id
		WHERE `+where+` ORDER BY 1`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var country string
		if err := rows.Scan(&country); err != nil {
			return nil, err
		}
		out = append(out, country)
	}
	return out, rows.Err()
}

// FirstPaymentAt returns the oldest payment creation time for an org and mode.
func FirstPaymentAt(ctx context.Context, q Queryer, orgID, mode string) (time.Time, bool, error) {
	var value *int64
	if err := q.QueryRowContext(ctx, `SELECT MIN(created_at) FROM payments WHERE org_id = ? AND mode = ?`, orgID, mode).Scan(&value); err != nil {
		return time.Time{}, false, err
	}
	if value == nil {
		return time.Time{}, false, nil
	}
	return fromMS(*value), true, nil
}
