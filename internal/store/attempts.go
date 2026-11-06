package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Attempt is a row of payment_attempts.
type Attempt struct {
	ID, OrgID, Mode, PaymentID, ProviderAccountID, ProviderRef string
	ProviderOrderID, ProviderPaymentID, Status, Method         string
	FailureCode, FailureMessage                                string
	AmountReceived                                             int64
	CurrencyReceived                                           string
	MethodDetails                                              string
	RiskDetails                                                string // connector.PaymentRisk as JSON
	Duplicate                                                  bool   // an extra success on an already-succeeded payment
	CreatedAt, UpdatedAt                                       time.Time
	// NextAction is the checkout's connector.NextAction as JSON, kept so a
	// reload of the pay page can show it again; CheckoutExpiresAt is when the
	// checkout stops being worth showing. Both are set by SetAttemptCheckout.
	NextAction        string
	CheckoutExpiresAt time.Time
	// FinishBy is when a started attempt stops holding its payment open. Zero
	// for a renewal or an older attempt, which keep the 72h polling window.
	FinishBy time.Time
}

// ProviderPaymentAttempt is one provider-side payment made against a Paymux
// checkout attempt. Providers such as Razorpay can accept a successful retry
// on the same order after an earlier payment failed.
type ProviderPaymentAttempt struct {
	ProviderAccountID, ProviderPaymentID, AttemptID, ProviderOrderID string
	Status, Method, FailureCode, FailureMessage, Currency            string
	Amount                                                           int64
	CreatedAt, UpdatedAt                                             time.Time
	// TryID keys the row within its account. Empty means ProviderPaymentID
	// identifies the try, as it does for every provider but Stripe.
	TryID string
	// MethodDetails and RiskDetails are JSON of the display-safe connector
	// types. ThreeDS is one of the connector.ThreeDS values, or empty.
	MethodDetails, RiskDetails, ThreeDS string
}

const attemptCols = `id, org_id, mode, payment_id, provider_account_id, provider_ref, COALESCE(provider_order_id,''),
	COALESCE(provider_payment_id,''), status, COALESCE(method,''), COALESCE(failure_code,''),
	COALESCE(failure_message,''), amount_received, COALESCE(currency_received,''), COALESCE(method_details,'{}'), COALESCE(risk_details,'{}'), duplicate, created_at, updated_at,
	COALESCE(next_action,''), COALESCE(checkout_expires_at,0), COALESCE(finish_by,0)`

func scanAttempt(row interface{ Scan(...any) error }) (Attempt, error) {
	var a Attempt
	var dup int
	var created, updated, checkoutExp, finishBy int64
	err := row.Scan(&a.ID, &a.OrgID, &a.Mode, &a.PaymentID, &a.ProviderAccountID, &a.ProviderRef, &a.ProviderOrderID,
		&a.ProviderPaymentID, &a.Status, &a.Method, &a.FailureCode, &a.FailureMessage, &a.AmountReceived,
		&a.CurrencyReceived, &a.MethodDetails, &a.RiskDetails, &dup, &created, &updated, &a.NextAction, &checkoutExp, &finishBy)
	if err != nil {
		return Attempt{}, notFound(err)
	}
	a.Duplicate = dup == 1
	a.CreatedAt, a.UpdatedAt = fromMS(created), fromMS(updated)
	if checkoutExp != 0 {
		a.CheckoutExpiresAt = fromMS(checkoutExp)
	}
	if finishBy != 0 {
		a.FinishBy = fromMS(finishBy)
	}
	return a, nil
}

// InsertAttempt inserts a.
func InsertAttempt(ctx context.Context, tx Tx, a Attempt) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO payment_attempts (id, org_id, mode, payment_id, provider_account_id,
		provider_ref, provider_order_id, provider_payment_id, status, method, failure_code, failure_message,
		amount_received, currency_received, method_details, risk_details, duplicate, created_at, updated_at, finish_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.OrgID, a.Mode, a.PaymentID, a.ProviderAccountID, a.ProviderRef, nullIfEmpty(a.ProviderOrderID),
		nullIfEmpty(a.ProviderPaymentID), a.Status, nullIfEmpty(a.Method), nullIfEmpty(a.FailureCode),
		nullIfEmpty(a.FailureMessage), a.AmountReceived, nullIfEmpty(a.CurrencyReceived), nonEmptyJSON(a.MethodDetails), nonEmptyJSON(a.RiskDetails), boolInt(a.Duplicate),
		ms(a.CreatedAt), ms(a.UpdatedAt), zeroMS(a.FinishBy))
	return err
}

// GetAttempt loads an attempt by id.
func GetAttempt(ctx context.Context, q Queryer, id string) (Attempt, error) {
	return scanAttempt(q.QueryRowContext(ctx, "SELECT "+attemptCols+" FROM payment_attempts WHERE id = ?", id))
}

// ListAttempts returns the attempts of a payment, oldest first.
func ListAttempts(ctx context.Context, q Queryer, paymentID string) ([]Attempt, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+attemptCols+" FROM payment_attempts WHERE payment_id = ? ORDER BY id", paymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Attempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListAttemptsForPayments returns every attempt for the requested payments in
// one query. Dashboard list pages use it to show provider, method and attempt
// count without issuing one query per payment.
func ListAttemptsForPayments(ctx context.Context, q Queryer, paymentIDs []string) ([]Attempt, error) {
	if len(paymentIDs) == 0 {
		return []Attempt{}, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(paymentIDs)), ",")
	args := make([]any, len(paymentIDs))
	for i, id := range paymentIDs {
		args[i] = id
	}
	rows, err := q.QueryContext(ctx, "SELECT "+attemptCols+" FROM payment_attempts WHERE payment_id IN ("+marks+") ORDER BY created_at, id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Attempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CountProviderPaymentAttemptsForPayments returns the number of distinct
// provider-side payment tries attached to each Paymux payment. One Paymux
// checkout attempt can contain several provider tries when a customer retries
// a failed payment on the same provider order.
func CountProviderPaymentAttemptsForPayments(ctx context.Context, q Queryer, paymentIDs []string) (map[string]int, error) {
	out := make(map[string]int, len(paymentIDs))
	if len(paymentIDs) == 0 {
		return out, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(paymentIDs)), ",")
	args := make([]any, len(paymentIDs))
	for i := range paymentIDs {
		args[i] = paymentIDs[i]
	}
	rows, err := q.QueryContext(ctx, `SELECT a.payment_id, COUNT(*)
		FROM provider_payment_attempts p JOIN payment_attempts a ON a.id = p.attempt_id
		WHERE a.payment_id IN (`+marks+`) GROUP BY a.payment_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var paymentID string
		var count int
		if err := rows.Scan(&paymentID, &count); err != nil {
			return nil, err
		}
		out[paymentID] = count
	}
	return out, rows.Err()
}

// UpsertProviderPaymentAttempt records the latest known state of one provider
// payment without changing the Paymux attempt or its ledger effects.
func UpsertProviderPaymentAttempt(ctx context.Context, tx Tx, p ProviderPaymentAttempt) error {
	tryID := p.TryID
	if tryID == "" {
		tryID = p.ProviderPaymentID
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO provider_payment_attempts
		(provider_account_id, try_id, provider_payment_id, attempt_id, provider_order_id, status, method, failure_code,
		 failure_message, amount, currency, method_details, risk_details, three_d_secure, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (provider_account_id, try_id) DO UPDATE SET
		attempt_id = excluded.attempt_id, provider_order_id = excluded.provider_order_id, status = excluded.status,
		method = COALESCE(excluded.method, provider_payment_attempts.method),
		failure_code = COALESCE(excluded.failure_code, provider_payment_attempts.failure_code),
		failure_message = COALESCE(excluded.failure_message, provider_payment_attempts.failure_message),
		amount = CASE WHEN excluded.amount > 0 THEN excluded.amount ELSE provider_payment_attempts.amount END,
		currency = COALESCE(excluded.currency, provider_payment_attempts.currency),
		method_details = COALESCE(excluded.method_details, provider_payment_attempts.method_details),
		risk_details = COALESCE(excluded.risk_details, provider_payment_attempts.risk_details),
		three_d_secure = COALESCE(excluded.three_d_secure, provider_payment_attempts.three_d_secure),
		updated_at = excluded.updated_at`,
		p.ProviderAccountID, tryID, p.ProviderPaymentID, p.AttemptID, nullIfEmpty(p.ProviderOrderID), p.Status,
		nullIfEmpty(p.Method), nullIfEmpty(p.FailureCode), nullIfEmpty(p.FailureMessage), p.Amount,
		nullIfEmpty(p.Currency), nullIfEmpty(p.MethodDetails), nullIfEmpty(p.RiskDetails), nullIfEmpty(p.ThreeDS),
		ms(p.CreatedAt), ms(p.UpdatedAt))
	return err
}

// HasTryOfProviderPayment reports whether any try names this provider payment.
func HasTryOfProviderPayment(ctx context.Context, q Queryer, accountID, providerPaymentID string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM provider_payment_attempts
		WHERE provider_account_id = ? AND provider_payment_id = ?`, accountID, providerPaymentID).Scan(&n)
	return n > 0, err
}

// ListProviderPaymentAttempts returns every provider-side payment of a Paymux
// payment, oldest first.
func ListProviderPaymentAttempts(ctx context.Context, q Queryer, paymentID string) ([]ProviderPaymentAttempt, error) {
	rows, err := q.QueryContext(ctx, `SELECT p.provider_account_id, p.provider_payment_id, p.attempt_id,
		COALESCE(p.provider_order_id,''), p.status, COALESCE(p.method,''), COALESCE(p.failure_code,''),
		COALESCE(p.failure_message,''), p.amount, COALESCE(p.currency,''), p.created_at, p.updated_at,
		p.try_id, COALESCE(p.method_details,''), COALESCE(p.risk_details,''), COALESCE(p.three_d_secure,'')
		FROM provider_payment_attempts p JOIN payment_attempts a ON a.id = p.attempt_id
		WHERE a.payment_id = ? ORDER BY p.created_at, p.try_id`, paymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProviderPaymentAttempt
	for rows.Next() {
		var p ProviderPaymentAttempt
		var created, updated int64
		if err := rows.Scan(&p.ProviderAccountID, &p.ProviderPaymentID, &p.AttemptID, &p.ProviderOrderID,
			&p.Status, &p.Method, &p.FailureCode, &p.FailureMessage, &p.Amount, &p.Currency, &created, &updated,
			&p.TryID, &p.MethodDetails, &p.RiskDetails, &p.ThreeDS); err != nil {
			return nil, err
		}
		p.CreatedAt, p.UpdatedAt = fromMS(created), fromMS(updated)
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdateAttempt writes the mutable fields of a. Capturing it with a provider
// payment that another captured, non-duplicate attempt of the account already
// holds is ErrConflict.
func UpdateAttempt(ctx context.Context, tx Tx, a Attempt, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE payment_attempts SET status = ?, method = ?, failure_code = ?, failure_message = ?,
		provider_payment_id = ?, provider_order_id = ?, amount_received = ?, currency_received = ?, method_details = ?, risk_details = ?, duplicate = ?, updated_at = ? WHERE id = ?`,
		a.Status, nullIfEmpty(a.Method), nullIfEmpty(a.FailureCode), nullIfEmpty(a.FailureMessage),
		nullIfEmpty(a.ProviderPaymentID), nullIfEmpty(a.ProviderOrderID), a.AmountReceived, nullIfEmpty(a.CurrencyReceived), nonEmptyJSON(a.MethodDetails), nonEmptyJSON(a.RiskDetails), boolInt(a.Duplicate),
		ms(now), a.ID)
	if IsUniqueViolation(err) {
		return fmt.Errorf("%w: the provider payment settles another attempt", ErrConflict)
	}
	return err
}

func nonEmptyJSON(v string) string {
	if v == "" {
		return "{}"
	}
	return v
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// SetAttemptCheckout records what CreateCheckout returned: the provider order,
// the next action (JSON) and when the checkout expires.
//
// firstPoll is when the provider is first asked about the attempt: a customer
// can pay and never come back, and the provider may send no webhook, so
// nothing else would ever look.
func SetAttemptCheckout(ctx context.Context, tx Tx, id, providerOrderID, nextAction string, expiresAt, now, firstPoll time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE payment_attempts SET provider_order_id = ?, next_action = ?, checkout_expires_at = ?,
		poll_count = 0, next_poll_at = ?, updated_at = ? WHERE id = ?`,
		nullIfEmpty(providerOrderID), nullIfEmpty(nextAction), ms(expiresAt), ms(firstPoll), ms(now), id)
	return err
}

// LatestOpenAttempt returns the newest attempt of the payment that is still
// waiting for the customer (created or pending) and has a stored next
// action; ErrNotFound when there is none.
func LatestOpenAttempt(ctx context.Context, q Queryer, paymentID string) (Attempt, error) {
	return scanAttempt(q.QueryRowContext(ctx, "SELECT "+attemptCols+` FROM payment_attempts WHERE payment_id = ?
		AND status IN ('created','pending') AND next_action IS NOT NULL ORDER BY created_at DESC, id DESC LIMIT 1`, paymentID))
}

// RiskCheckItem is a settled attempt the risk backfill has not asked about yet.
type RiskCheckItem struct{ ID, ProviderPaymentID string }

// ListAttemptsToRiskCheck returns an account's settled attempts, last settled
// before settledBefore, that have a provider payment and no risk check yet,
// most recent first.
func ListAttemptsToRiskCheck(ctx context.Context, q Queryer, accountID string, settledBefore time.Time, limit int) ([]RiskCheckItem, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, provider_payment_id FROM payment_attempts
		WHERE provider_account_id = ? AND risk_checked_at = 0 AND updated_at < ? AND status IN ('captured', 'failed')
		AND provider_payment_id IS NOT NULL AND provider_payment_id <> ''
		ORDER BY updated_at DESC, id LIMIT ?`, accountID, ms(settledBefore), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RiskCheckItem
	for rows.Next() {
		var it RiskCheckItem
		if err := rows.Scan(&it.ID, &it.ProviderPaymentID); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// SetAttemptRiskDetails stores an attempt's risk review, leaving it unchecked
// unless it already was, and leaves updated_at alone.
func SetAttemptRiskDetails(ctx context.Context, tx Tx, id, risk string) error {
	_, err := tx.ExecContext(ctx, "UPDATE payment_attempts SET risk_details = ? WHERE id = ?", nonEmptyJSON(risk), id)
	return err
}

// SetAttemptRiskChecked stores an attempt's risk review and marks it checked.
// It leaves updated_at alone: this is display data, not a change of the attempt.
func SetAttemptRiskChecked(ctx context.Context, tx Tx, id, risk string, checkedAt time.Time) error {
	_, err := tx.ExecContext(ctx, "UPDATE payment_attempts SET risk_details = ?, risk_checked_at = ? WHERE id = ?",
		nonEmptyJSON(risk), ms(checkedAt), id)
	return err
}
