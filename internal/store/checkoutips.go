package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// CheckoutIP is an IP that opened a payment's pay page or started its browser
// checkout.
type CheckoutIP struct {
	IP                  string
	FirstSeen, LastSeen time.Time
}

// NoteCheckoutIP records that ip opened payment paymentID's checkout at now.
func NoteCheckoutIP(ctx context.Context, tx Tx, paymentID, ip string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO checkout_ips (payment_id, ip, first_seen, last_seen) VALUES (?, ?, ?, ?)
		ON CONFLICT (payment_id, ip) DO UPDATE SET last_seen = excluded.last_seen`, paymentID, ip, ms(now), ms(now))
	return err
}

// CheckoutIPs lists the IPs that opened payment paymentID's checkout, first seen first.
func CheckoutIPs(ctx context.Context, q Queryer, paymentID string) ([]CheckoutIP, error) {
	rows, err := q.QueryContext(ctx, `SELECT ip, first_seen, last_seen FROM checkout_ips WHERE payment_id = ? ORDER BY first_seen, ip`, paymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CheckoutIP
	for rows.Next() {
		var c CheckoutIP
		var first, last int64
		if err := rows.Scan(&c.IP, &first, &last); err != nil {
			return nil, err
		}
		c.FirstSeen, c.LastSeen = fromMS(first), fromMS(last)
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetIPLookup returns the cached lookup of ip (JSON) and when it was made, or
// ErrNotFound.
func GetIPLookup(ctx context.Context, q Queryer, ip string) (string, time.Time, error) {
	var info string
	var at int64
	err := q.QueryRowContext(ctx, `SELECT info, checked_at FROM ip_lookups WHERE ip = ?`, ip).Scan(&info, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, ErrNotFound
	}
	if err != nil {
		return "", time.Time{}, err
	}
	return info, fromMS(at), nil
}

// PutIPLookup caches the lookup of ip.
func PutIPLookup(ctx context.Context, tx Tx, ip, info string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO ip_lookups (ip, info, checked_at) VALUES (?, ?, ?)
		ON CONFLICT (ip) DO UPDATE SET info = excluded.info, checked_at = excluded.checked_at`, ip, info, ms(now))
	return err
}
