package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// IPCheckoutPayments counts the other payments of app appID that ip started a
// checkout for (an attempt exists) since since.
func IPCheckoutPayments(ctx context.Context, q Queryer, appID, ip, exceptPaymentID string, since time.Time) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COUNT(DISTINCT c.payment_id) FROM checkout_ips c
		JOIN payments p ON p.id = c.payment_id
		WHERE c.ip = ? AND p.app_id = ? AND c.payment_id <> ? AND c.last_seen >= ?
		AND EXISTS (SELECT 1 FROM payment_attempts a WHERE a.payment_id = c.payment_id)`,
		ip, appID, exceptPaymentID, ms(since)).Scan(&n)
	return n, err
}

// IPDeclinedCards counts the different cards declined since since on the
// payments of app appID that ip opened a checkout for.
func IPDeclinedCards(ctx context.Context, q Queryer, appID, ip string, since time.Time) (int, error) {
	rows, err := q.QueryContext(ctx, `SELECT pp.provider_payment_id, COALESCE(pp.method_details,'')
		FROM provider_payment_attempts pp JOIN payment_attempts a ON a.id = pp.attempt_id
		JOIN payments p ON p.id = a.payment_id
		WHERE p.app_id = ? AND pp.status = 'failed' AND a.created_at >= ?
		AND a.payment_id IN (SELECT payment_id FROM checkout_ips WHERE ip = ?)`, appID, ms(since), ip)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	cards := map[string]bool{}
	for rows.Next() {
		var providerPaymentID, details string
		if err := rows.Scan(&providerPaymentID, &details); err != nil {
			return 0, err
		}
		cards[CardKey(details, providerPaymentID)] = true
	}
	return len(cards), rows.Err()
}

// EmailPaidPayments counts the payments of app appID paid (succeeded or held
// for review) since since by any customer with email (compared without case).
func EmailPaidPayments(ctx context.Context, q Queryer, appID, email string, since time.Time) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM payments p JOIN customers c ON c.id = p.customer_id
		WHERE p.app_id = ? AND LOWER(c.email) = LOWER(?) AND p.status IN ('succeeded','needs_review') AND p.updated_at >= ?`,
		appID, email, ms(since)).Scan(&n)
	return n, err
}

// FraudLinked returns a payment of org orgID tagged fraud that ip opened a
// checkout for (when ip is set) or whose customer has email (when set), or "".
func FraudLinked(ctx context.Context, q Queryer, orgID, ip, email string) (string, error) {
	var id string
	var err error
	if ip != "" {
		err = q.QueryRowContext(ctx, `SELECT p.id FROM checkout_ips c JOIN payments p ON p.id = c.payment_id
			WHERE c.ip = ? AND p.org_id = ? AND p.flags LIKE '%"fraud"%' LIMIT 1`, ip, orgID).Scan(&id)
		if err == nil || !errors.Is(err, sql.ErrNoRows) {
			return id, err
		}
	}
	if email != "" {
		err = q.QueryRowContext(ctx, `SELECT p.id FROM payments p JOIN customers c ON c.id = p.customer_id
			WHERE p.org_id = ? AND LOWER(c.email) = LOWER(?) AND p.flags LIKE '%"fraud"%' LIMIT 1`, orgID, email).Scan(&id)
		if err == nil || !errors.Is(err, sql.ErrNoRows) {
			return id, err
		}
	}
	return "", nil
}
