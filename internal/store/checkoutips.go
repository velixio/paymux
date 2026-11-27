package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Checkout IP sources: the pay page saw the browser, or the app sent the IP.
const (
	CheckoutIPPayPage = "pay_page"
	CheckoutIPApp     = "app"
)

// CheckoutIP is a buyer's IP at a payment's checkout. Source is where it was
// first seen (CheckoutIPPayPage or CheckoutIPApp).
type CheckoutIP struct {
	IP, Source          string
	FirstSeen, LastSeen time.Time
}

// NoteCheckoutIP records that ip was the buyer's at payment paymentID's
// checkout at now, as source saw it.
func NoteCheckoutIP(ctx context.Context, tx Tx, paymentID, ip, source string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO checkout_ips (payment_id, ip, first_seen, last_seen, source) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (payment_id, ip) DO UPDATE SET last_seen = excluded.last_seen`, paymentID, ip, ms(now), ms(now), source)
	return err
}

// CheckoutIPs lists the IPs that opened payment paymentID's checkout, first seen first.
func CheckoutIPs(ctx context.Context, q Queryer, paymentID string) ([]CheckoutIP, error) {
	rows, err := q.QueryContext(ctx, `SELECT ip, source, first_seen, last_seen FROM checkout_ips WHERE payment_id = ? ORDER BY first_seen, ip`, paymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CheckoutIP
	for rows.Next() {
		var c CheckoutIP
		var first, last int64
		if err := rows.Scan(&c.IP, &c.Source, &first, &last); err != nil {
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
