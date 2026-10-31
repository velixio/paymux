package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ConnectorKVGet returns the sealed value for (account, key) unless it has
// expired at now.
func ConnectorKVGet(ctx context.Context, q Queryer, accountID, key string, now time.Time) ([]byte, bool, error) {
	var enc []byte
	var exp sql.NullInt64
	err := q.QueryRowContext(ctx, "SELECT value_enc, expires_at FROM connector_kv WHERE provider_account_id = ? AND key = ?",
		accountID, key).Scan(&enc, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if exp.Valid && now.UnixMilli() >= exp.Int64 {
		return nil, false, nil
	}
	return enc, true, nil
}

// ConnectorKVSet upserts a sealed value; a zero expires means no expiry.
func ConnectorKVSet(ctx context.Context, tx Tx, accountID, key string, enc []byte, expires time.Time) error {
	var exp any
	if !expires.IsZero() {
		exp = ms(expires)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO connector_kv (provider_account_id, key, value_enc, expires_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (provider_account_id, key) DO UPDATE SET value_enc = excluded.value_enc, expires_at = excluded.expires_at`,
		accountID, key, enc, exp)
	return err
}

// ConnectorKVDelete removes a value.
func ConnectorKVDelete(ctx context.Context, tx Tx, accountID, key string) error {
	_, err := tx.ExecContext(ctx, "DELETE FROM connector_kv WHERE provider_account_id = ? AND key = ?", accountID, key)
	return err
}

// Domain is a row of app_domains.
type Domain struct {
	ID, OrgID, AppID, Host, Status, Token, LastError string
	IsPrimary                                        bool
	LastCheckAt, CreatedAt                           time.Time
	// TXTVerifiedAt is when the DNS challenge was last seen; GraceStartedAt is
	// when an active domain was first found without it. Zero when unset.
	TXTVerifiedAt, GraceStartedAt time.Time
}

// InsertDomain inserts d. A host that exists already returns ErrConflict.
func InsertDomain(ctx context.Context, tx Tx, d Domain) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO app_domains (id, org_id, app_id, host, is_primary, status, token, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, d.ID, d.OrgID, d.AppID, d.Host, boolInt(d.IsPrimary), d.Status, d.Token, ms(d.CreatedAt))
	if IsUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

// CountDomains returns how many domains an app has.
func CountDomains(ctx context.Context, q Queryer, appID string) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM app_domains WHERE app_id = ?", appID).Scan(&n)
	return n, err
}

// GetDomainByHost loads a domain by its (normalised) host.
func GetDomainByHost(ctx context.Context, q Queryer, host string) (Domain, error) {
	var d Domain
	var primary int
	var checked, txt, grace sql.NullInt64
	var created int64
	err := q.QueryRowContext(ctx, `SELECT id, org_id, app_id, host, is_primary, status, token, last_check_at,
		COALESCE(last_error,''), created_at, txt_verified_at, grace_started_at FROM app_domains WHERE host = ?`, host).
		Scan(&d.ID, &d.OrgID, &d.AppID, &d.Host, &primary, &d.Status, &d.Token, &checked, &d.LastError, &created, &txt, &grace)
	if err != nil {
		return Domain{}, notFound(err)
	}
	d.IsPrimary = primary == 1
	if checked.Valid {
		d.LastCheckAt = fromMS(checked.Int64)
	}
	d.TXTVerifiedAt = fromNullMS(txt)
	d.GraceStartedAt = fromNullMS(grace)
	d.CreatedAt = fromMS(created)
	return d, nil
}

// PrimaryActiveHost returns the app's primary domain when it is active.
func PrimaryActiveHost(ctx context.Context, q Queryer, appID string) (string, bool, error) {
	var h string
	err := q.QueryRowContext(ctx, "SELECT host FROM app_domains WHERE app_id = ? AND is_primary = 1 AND status = 'active'", appID).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return h, err == nil, err
}

// AppIDForAttemptRef returns the app that owns the attempt with providerRef.
func AppIDForAttemptRef(ctx context.Context, q Queryer, providerRef string) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `SELECT p.app_id FROM payment_attempts a JOIN payments p ON p.id = a.payment_id
		WHERE a.provider_ref = ?`, providerRef).Scan(&id)
	return id, notFound(err)
}

// UnknownObject is an inbound provider object Paymux could not match.
type UnknownObject struct {
	ID, ProviderAccountID, ProviderEventID, Kind, ProviderID, Detail string
	CreatedAt                                                        time.Time
}

// ProviderEventMatch records the local transaction resolved from an inbound
// provider event. It makes raw webhook history traceable without parsing the
// provider payload again.
type ProviderEventMatch struct {
	ProviderEventID, ObjectType, ObjectID, PaymentID, ProviderID string
	CreatedAt                                                    time.Time
}

func InsertProviderEventMatch(ctx context.Context, tx Tx, m ProviderEventMatch) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO provider_event_matches
		(provider_event_id, object_type, object_id, payment_id, provider_id, created_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (provider_event_id, object_type, object_id) DO NOTHING`,
		m.ProviderEventID, m.ObjectType, m.ObjectID, nullIfEmpty(m.PaymentID), nullIfEmpty(m.ProviderID), ms(m.CreatedAt))
	return err
}

func ListProviderEventMatches(ctx context.Context, q Queryer, eventID string) ([]ProviderEventMatch, error) {
	rows, err := q.QueryContext(ctx, `SELECT provider_event_id, object_type, object_id, COALESCE(payment_id,''),
		COALESCE(provider_id,''), created_at FROM provider_event_matches WHERE provider_event_id = ? ORDER BY created_at, object_type, object_id`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProviderEventMatch
	for rows.Next() {
		var m ProviderEventMatch
		var at int64
		if err := rows.Scan(&m.ProviderEventID, &m.ObjectType, &m.ObjectID, &m.PaymentID, &m.ProviderID, &at); err != nil {
			return nil, err
		}
		m.CreatedAt = fromMS(at)
		out = append(out, m)
	}
	return out, rows.Err()
}

// InsertUnknownObject records u once per (account, event, kind, id).
func InsertUnknownObject(ctx context.Context, tx Tx, u UnknownObject) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO unknown_provider_objects
		(id, provider_account_id, provider_event_id, kind, provider_id, detail, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (provider_account_id, provider_event_id, kind, provider_id) DO NOTHING`,
		u.ID, u.ProviderAccountID, u.ProviderEventID, u.Kind, u.ProviderID, nullIfEmpty(u.Detail), ms(u.CreatedAt))
	return err
}

// ListUnknownObjects returns the unmatched objects of an account, newest first.
func ListUnknownObjects(ctx context.Context, q Queryer, accountID string) ([]UnknownObject, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, provider_account_id, provider_event_id, kind, provider_id,
		COALESCE(detail,''), created_at FROM unknown_provider_objects WHERE provider_account_id = ? ORDER BY created_at DESC, id`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UnknownObject
	for rows.Next() {
		var u UnknownObject
		var at int64
		if err := rows.Scan(&u.ID, &u.ProviderAccountID, &u.ProviderEventID, &u.Kind, &u.ProviderID, &u.Detail, &at); err != nil {
			return nil, err
		}
		u.CreatedAt = fromMS(at)
		out = append(out, u)
	}
	return out, rows.Err()
}

const refundColsQualified = `r.id, r.org_id, r.app_id, r.mode, r.payment_id, r.attempt_id, r.amount, r.currency, r.status, COALESCE(r.reason,''),
	r.provider_ref, COALESCE(r.provider_refund_id,''), r.initiated_by, COALESCE(r.actor,''), r.created_at, r.updated_at,
	COALESCE(r.idempotency_key,'')`

// FindAttempt looks an attempt of the account up by any of its provider
// identifiers (empty ones are skipped). It returns ErrNotFound when none match.
func FindAttempt(ctx context.Context, q Queryer, accountID, providerRef, orderID, paymentID string) (Attempt, error) {
	for _, c := range []struct{ col, val string }{
		{"provider_ref", providerRef}, {"provider_payment_id", paymentID}, {"provider_order_id", orderID},
	} {
		if c.val == "" {
			continue
		}
		a, err := scanAttempt(q.QueryRowContext(ctx, "SELECT "+attemptCols+" FROM payment_attempts WHERE provider_account_id = ? AND "+
			c.col+" = ? ORDER BY created_at DESC, id DESC LIMIT 1", accountID, c.val))
		if !errors.Is(err, ErrNotFound) {
			return a, err
		}
	}
	return Attempt{}, ErrNotFound
}

// FindRefund looks a refund up by provider_ref or provider refund id, only
// among refunds of attempts that belong to the delivering provider account, so
// one account's identifiers can never reach another account's refunds.
func FindRefund(ctx context.Context, q Queryer, accountID, providerRef, providerRefundID string) (Refund, error) {
	for _, c := range []struct{ col, val string }{{"provider_ref", providerRef}, {"provider_refund_id", providerRefundID}} {
		if c.val == "" {
			continue
		}
		r, err := scanRefund(q.QueryRowContext(ctx, "SELECT "+refundColsQualified+` FROM refunds r JOIN payment_attempts a ON a.id = r.attempt_id
			WHERE a.provider_account_id = ? AND r.`+c.col+" = ?", accountID, c.val))
		if !errors.Is(err, ErrNotFound) {
			return r, err
		}
	}
	return Refund{}, ErrNotFound
}

// PollItem is a due attempt or refund and how often it was polled already.
type PollItem struct {
	ID        string
	PollCount int
	// Checkout marks an attempt that is only a started checkout: created, with
	// a next action but no provider order or payment id yet (a form posted to
	// the provider). It is polled on a shorter, bounded schedule.
	Checkout bool
}

// ListDueAttempts returns open attempts whose next poll is due: attempts
// created after cutoff that are pending, authorized or created and already
// known to the provider by an id; and started checkouts (created, only a next
// action) until checkoutGrace after their checkout expires. Attempts that never
// reached a provider checkout are skipped.
func ListDueAttempts(ctx context.Context, q Queryer, now, cutoff time.Time, checkoutGrace time.Duration, limit int) ([]PollItem, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, poll_count,
			CASE WHEN status = 'created' AND provider_order_id IS NULL AND provider_payment_id IS NULL THEN 1 ELSE 0 END
		FROM payment_attempts
		WHERE (
			(created_at > ? AND (status IN ('pending','authorized') OR (status = 'created' AND (provider_order_id IS NOT NULL OR provider_payment_id IS NOT NULL))))
			OR (status = 'created' AND provider_order_id IS NULL AND provider_payment_id IS NULL AND next_action IS NOT NULL
				AND COALESCE(checkout_expires_at, created_at) + ? > ?)
		) AND COALESCE(next_poll_at, created_at + 60000) <= ?
		ORDER BY COALESCE(next_poll_at, created_at + 60000) LIMIT ?`,
		ms(cutoff), checkoutGrace.Milliseconds(), ms(now), ms(now), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PollItem
	for rows.Next() {
		var it PollItem
		var co int
		if err := rows.Scan(&it.ID, &it.PollCount, &co); err != nil {
			return nil, err
		}
		it.Checkout = co == 1
		out = append(out, it)
	}
	return out, rows.Err()
}

// ListDueRefunds returns pending refunds created after cutoff whose next poll is due.
func ListDueRefunds(ctx context.Context, q Queryer, now, cutoff time.Time, limit int) ([]PollItem, error) {
	return listDue(ctx, q, `SELECT id, poll_count FROM refunds WHERE status = 'pending'
		AND created_at > ? AND COALESCE(next_poll_at, created_at + 60000) <= ? ORDER BY COALESCE(next_poll_at, created_at + 60000) LIMIT ?`,
		ms(cutoff), ms(now), limit)
}

func listDue(ctx context.Context, q Queryer, query string, args ...any) ([]PollItem, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PollItem
	for rows.Next() {
		var it PollItem
		if err := rows.Scan(&it.ID, &it.PollCount); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// BumpAttemptPoll records a poll and schedules the next one.
func BumpAttemptPoll(ctx context.Context, tx Tx, id string, count int, next time.Time) error {
	_, err := tx.ExecContext(ctx, "UPDATE payment_attempts SET poll_count = ?, next_poll_at = ? WHERE id = ?", count, ms(next), id)
	return err
}

// BumpRefundPoll records a poll and schedules the next one.
func BumpRefundPoll(ctx context.Context, tx Tx, id string, count int, next time.Time) error {
	_, err := tx.ExecContext(ctx, "UPDATE refunds SET poll_count = ?, next_poll_at = ? WHERE id = ?", count, ms(next), id)
	return err
}

// UpdateRefund writes the mutable fields of r.
func UpdateRefund(ctx context.Context, tx Tx, r Refund, now time.Time) error {
	_, err := tx.ExecContext(ctx, "UPDATE refunds SET status = ?, provider_refund_id = ?, updated_at = ? WHERE id = ?",
		r.Status, nullIfEmpty(r.ProviderRefundID), ms(now), r.ID)
	return err
}

// SetPaymentCustomer links a customer to a payment.
func SetPaymentCustomer(ctx context.Context, tx Tx, paymentID, customerID string, now time.Time) error {
	_, err := tx.ExecContext(ctx, "UPDATE payments SET customer_id = ?, updated_at = ? WHERE id = ?", customerID, ms(now), paymentID)
	return err
}

// GetProviderEventByID loads a provider event by its row id.
func GetProviderEventByID(ctx context.Context, q Queryer, id string) (ProviderEventRow, error) {
	var p ProviderEventRow
	var headers string
	var verified int
	var received int64
	var processed sql.NullInt64
	var errText sql.NullString
	err := q.QueryRowContext(ctx, `SELECT id, provider_account_id, provider_event_id, headers, body, verified, received_at,
		processed_at, error FROM provider_events WHERE id = ?`, id).
		Scan(&p.ID, &p.ProviderAccountID, &p.ProviderEventID, &headers, &p.Body, &verified, &received, &processed, &errText)
	if err != nil {
		return ProviderEventRow{}, notFound(err)
	}
	p.Headers, p.Verified = []byte(headers), verified != 0
	p.ReceivedAt, p.ProcessedAt, p.Error = fromMS(received), fromNullMS(processed), errText.String
	return p, nil
}

// ListProviderEvents returns recent raw inbound webhook events for an account.
func ListProviderEvents(ctx context.Context, q Queryer, accountID string, limit int) ([]ProviderEventRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, provider_account_id, provider_event_id, headers, body, verified,
		received_at, processed_at, error FROM provider_events WHERE provider_account_id = ?
		ORDER BY received_at DESC, id DESC LIMIT ?`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProviderEventRow
	for rows.Next() {
		var p ProviderEventRow
		var headers string
		var verified int
		var received int64
		var processed sql.NullInt64
		var errText sql.NullString
		if err := rows.Scan(&p.ID, &p.ProviderAccountID, &p.ProviderEventID, &headers, &p.Body, &verified,
			&received, &processed, &errText); err != nil {
			return nil, err
		}
		p.Headers, p.Verified, p.ReceivedAt, p.ProcessedAt, p.Error = []byte(headers), verified != 0, fromMS(received), fromNullMS(processed), errText.String
		out = append(out, p)
	}
	return out, rows.Err()
}

// MarkProviderEventProcessed stamps processed_at and the processing error ("" for none).
func MarkProviderEventProcessed(ctx context.Context, tx Tx, id string, at time.Time, errText string) error {
	_, err := tx.ExecContext(ctx, "UPDATE provider_events SET processed_at = ?, error = ? WHERE id = ?", ms(at), nullIfEmpty(errText), id)
	return err
}

// GetDomainByID loads a domain by id.
func GetDomainByID(ctx context.Context, q Queryer, id string) (Domain, error) {
	var host string
	if err := q.QueryRowContext(ctx, "SELECT host FROM app_domains WHERE id = ?", id).Scan(&host); err != nil {
		return Domain{}, notFound(err)
	}
	return GetDomainByHost(ctx, q, host)
}

// ListDomainsByStatus returns the domains in one status, oldest first.
func ListDomainsByStatus(ctx context.Context, q Queryer, status string) ([]Domain, error) {
	rows, err := q.QueryContext(ctx, "SELECT host FROM app_domains WHERE status = ? ORDER BY created_at, id", status)
	if err != nil {
		return nil, err
	}
	var hosts []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return nil, err
		}
		hosts = append(hosts, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Domain, 0, len(hosts))
	for _, h := range hosts {
		d, err := GetDomainByHost(ctx, q, h)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// UpdateDomainCheck records the result of an activation check: the new status,
// the error text ("" clears it) and the check time.
func UpdateDomainCheck(ctx context.Context, tx Tx, id, status, errText string, now time.Time) error {
	_, err := tx.ExecContext(ctx, "UPDATE app_domains SET status = ?, last_error = ?, last_check_at = ? WHERE id = ?",
		status, nullIfEmpty(errText), ms(now), id)
	return err
}

// ResetDomain puts a failed domain back to pending with a fresh activation
// grace period (created_at is the start of the grace period). The DNS proof
// has to be seen again.
func ResetDomain(ctx context.Context, tx Tx, id string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE app_domains SET status = 'pending', last_error = NULL, created_at = ?,
		txt_verified_at = NULL, grace_started_at = NULL WHERE id = ?`, ms(now), id)
	return err
}

// SetDomainTXT records the DNS challenge as seen (verified) or, with a zero
// time, as not seen.
func SetDomainTXT(ctx context.Context, tx Tx, id string, at time.Time) error {
	var v any
	if !at.IsZero() {
		v = ms(at)
	}
	_, err := tx.ExecContext(ctx, "UPDATE app_domains SET txt_verified_at = ? WHERE id = ?", v, id)
	return err
}

// SetDomainGrace records when an active domain was first found without its
// DNS proof; a zero time clears it.
func SetDomainGrace(ctx context.Context, tx Tx, id string, at time.Time) error {
	var v any
	if !at.IsZero() {
		v = ms(at)
	}
	_, err := tx.ExecContext(ctx, "UPDATE app_domains SET grace_started_at = ? WHERE id = ?", v, id)
	return err
}

// DemoteDomain puts an active domain back to pending (never deleting it) with
// a fresh activation grace period and no DNS proof.
func DemoteDomain(ctx context.Context, tx Tx, id, errText string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE app_domains SET status = 'pending', last_error = ?, last_check_at = ?, created_at = ?,
		txt_verified_at = NULL, grace_started_at = NULL WHERE id = ?`, nullIfEmpty(errText), ms(now), ms(now), id)
	return err
}

// CountAttemptsSince counts the account's attempts created at or after since.
func CountAttemptsSince(ctx context.Context, q Queryer, accountID string, since time.Time) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM payment_attempts WHERE provider_account_id = ? AND created_at >= ?", accountID, ms(since)).Scan(&n)
	return n, err
}

// CountVerifiedEventsSince counts verified inbound webhooks of the account
// received at or after since.
func CountVerifiedEventsSince(ctx context.Context, q Queryer, accountID string, since time.Time) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM provider_events WHERE provider_account_id = ? AND verified = 1 AND received_at >= ?", accountID, ms(since)).Scan(&n)
	return n, err
}
