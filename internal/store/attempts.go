package store

import (
	"context"
	"fmt"
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
	Duplicate                                                  bool // an extra success on an already-succeeded payment
	CreatedAt, UpdatedAt                                       time.Time
	// NextAction is the checkout's connector.NextAction as JSON, kept so a
	// reload of the pay page can show it again; CheckoutExpiresAt is when the
	// checkout stops being worth showing. Both are set by SetAttemptCheckout.
	NextAction        string
	CheckoutExpiresAt time.Time
}

const attemptCols = `id, org_id, mode, payment_id, provider_account_id, provider_ref, COALESCE(provider_order_id,''),
	COALESCE(provider_payment_id,''), status, COALESCE(method,''), COALESCE(failure_code,''),
	COALESCE(failure_message,''), amount_received, COALESCE(currency_received,''), COALESCE(method_details,'{}'), duplicate, created_at, updated_at,
	COALESCE(next_action,''), COALESCE(checkout_expires_at,0)`

func scanAttempt(row interface{ Scan(...any) error }) (Attempt, error) {
	var a Attempt
	var dup int
	var created, updated, checkoutExp int64
	err := row.Scan(&a.ID, &a.OrgID, &a.Mode, &a.PaymentID, &a.ProviderAccountID, &a.ProviderRef, &a.ProviderOrderID,
		&a.ProviderPaymentID, &a.Status, &a.Method, &a.FailureCode, &a.FailureMessage, &a.AmountReceived,
		&a.CurrencyReceived, &a.MethodDetails, &dup, &created, &updated, &a.NextAction, &checkoutExp)
	if err != nil {
		return Attempt{}, notFound(err)
	}
	a.Duplicate = dup == 1
	a.CreatedAt, a.UpdatedAt = fromMS(created), fromMS(updated)
	if checkoutExp != 0 {
		a.CheckoutExpiresAt = fromMS(checkoutExp)
	}
	return a, nil
}

// InsertAttempt inserts a.
func InsertAttempt(ctx context.Context, tx Tx, a Attempt) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO payment_attempts (id, org_id, mode, payment_id, provider_account_id,
		provider_ref, provider_order_id, provider_payment_id, status, method, failure_code, failure_message,
		amount_received, currency_received, method_details, duplicate, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.OrgID, a.Mode, a.PaymentID, a.ProviderAccountID, a.ProviderRef, nullIfEmpty(a.ProviderOrderID),
		nullIfEmpty(a.ProviderPaymentID), a.Status, nullIfEmpty(a.Method), nullIfEmpty(a.FailureCode),
		nullIfEmpty(a.FailureMessage), a.AmountReceived, nullIfEmpty(a.CurrencyReceived), nonEmptyJSON(a.MethodDetails), boolInt(a.Duplicate),
		ms(a.CreatedAt), ms(a.UpdatedAt))
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

// UpdateAttempt writes the mutable fields of a. Capturing it with a provider
// payment that another captured, non-duplicate attempt of the account already
// holds is ErrConflict.
func UpdateAttempt(ctx context.Context, tx Tx, a Attempt, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE payment_attempts SET status = ?, method = ?, failure_code = ?, failure_message = ?,
		provider_payment_id = ?, provider_order_id = ?, amount_received = ?, currency_received = ?, method_details = ?, duplicate = ?, updated_at = ? WHERE id = ?`,
		a.Status, nullIfEmpty(a.Method), nullIfEmpty(a.FailureCode), nullIfEmpty(a.FailureMessage),
		nullIfEmpty(a.ProviderPaymentID), nullIfEmpty(a.ProviderOrderID), a.AmountReceived, nullIfEmpty(a.CurrencyReceived), nonEmptyJSON(a.MethodDetails), boolInt(a.Duplicate),
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
