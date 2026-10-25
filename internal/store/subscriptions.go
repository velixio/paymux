package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Mandate is a row of mandates.
type Mandate struct {
	ID, OrgID, Mode, AppID, CustomerID, ProviderAccountID string
	ProviderRef, ProviderMandateID, Method, Status        string
	MaxAmount                                             int64
	Currency, Frequency                                   string
	ValidUntil, ExpiryNotifiedAt                          time.Time // zero = unset
	CreatedAt, UpdatedAt                                  time.Time
}

const mandateCols = `id, org_id, mode, app_id, customer_id, provider_account_id, provider_ref, COALESCE(provider_mandate_id,''),
	method, status, max_amount, currency, frequency, COALESCE(valid_until,0), COALESCE(expiry_notified_at,0), created_at, updated_at`

func scanMandate(row interface{ Scan(...any) error }) (Mandate, error) {
	var m Mandate
	var valid, notified, created, updated int64
	err := row.Scan(&m.ID, &m.OrgID, &m.Mode, &m.AppID, &m.CustomerID, &m.ProviderAccountID, &m.ProviderRef,
		&m.ProviderMandateID, &m.Method, &m.Status, &m.MaxAmount, &m.Currency, &m.Frequency, &valid, &notified, &created, &updated)
	if err != nil {
		return Mandate{}, notFound(err)
	}
	m.ValidUntil, m.ExpiryNotifiedAt = fromMSZero(valid), fromMSZero(notified)
	m.CreatedAt, m.UpdatedAt = fromMS(created), fromMS(updated)
	return m, nil
}

// InsertMandate inserts m.
func InsertMandate(ctx context.Context, tx Tx, m Mandate) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO mandates (id, org_id, mode, app_id, customer_id, provider_account_id,
		provider_ref, provider_mandate_id, method, status, max_amount, currency, frequency, valid_until, expiry_notified_at,
		created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.OrgID, m.Mode, m.AppID, m.CustomerID, m.ProviderAccountID, m.ProviderRef, nullIfEmpty(m.ProviderMandateID),
		m.Method, m.Status, m.MaxAmount, m.Currency, m.Frequency, zeroMS(m.ValidUntil), zeroMS(m.ExpiryNotifiedAt),
		ms(m.CreatedAt), ms(m.UpdatedAt))
	return err
}

// GetMandate loads a mandate by id.
func GetMandate(ctx context.Context, q Queryer, id string) (Mandate, error) {
	return scanMandate(q.QueryRowContext(ctx, "SELECT "+mandateCols+" FROM mandates WHERE id = ?", id))
}

// GetMandateForUpdate loads a mandate, locking it on Postgres.
func GetMandateForUpdate(ctx context.Context, tx Tx, id string) (Mandate, error) {
	return scanMandate(tx.QueryRowContext(ctx, "SELECT "+mandateCols+" FROM mandates WHERE id = ?"+tx.Dialect().ForUpdate(), id))
}

// GetMandateByRef loads a mandate by the short reference sent to the provider.
func GetMandateByRef(ctx context.Context, q Queryer, ref string) (Mandate, error) {
	return scanMandate(q.QueryRowContext(ctx, "SELECT "+mandateCols+" FROM mandates WHERE provider_ref = ?", ref))
}

// GetMandateByProviderID loads a mandate by the provider's own id.
func GetMandateByProviderID(ctx context.Context, q Queryer, accountID, providerMandateID string) (Mandate, error) {
	return scanMandate(q.QueryRowContext(ctx, "SELECT "+mandateCols+" FROM mandates WHERE provider_account_id = ? AND provider_mandate_id = ?",
		accountID, providerMandateID))
}

// UpdateMandate writes the mutable fields of m.
func UpdateMandate(ctx context.Context, tx Tx, m Mandate, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE mandates SET provider_mandate_id = ?, status = ?, max_amount = ?, valid_until = ?,
		expiry_notified_at = ?, updated_at = ? WHERE id = ?`,
		nullIfEmpty(m.ProviderMandateID), m.Status, m.MaxAmount, zeroMS(m.ValidUntil), zeroMS(m.ExpiryNotifiedAt), ms(now), m.ID)
	return err
}

// AdjustLine is one line of a prorated adjustment: a charge when Amount is
// positive, a credit when negative (minor units of the subscription currency).
type AdjustLine struct {
	Description string `json:"description"`
	Amount      int64  `json:"amount"`
}

func marshalLines(l []AdjustLine) string {
	if len(l) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(l)
	return string(b)
}

func unmarshalLines(raw string) ([]AdjustLine, error) {
	var l []AdjustLine
	if err := json.Unmarshal([]byte(raw), &l); err != nil {
		return nil, err
	}
	return l, nil
}

// Subscription is a row of subscriptions.
type Subscription struct {
	ID, OrgID, Mode, AppID, CustomerID, Status, BillingMode string
	ProviderAccountID, ProviderSubscriptionID, ProviderRef  string
	MandateID, PendingMandateID                             string
	Amount                                                  int64
	Currency, Interval                                      string
	IntervalCount                                           int
	AnchorAt, CurrentPeriodStart, CurrentPeriodEnd          time.Time
	TrialEnd                                                time.Time // zero = no trial
	TotalCount, RenewalsDone                                int       // TotalCount 0 = until cancelled
	CancelAtPeriodEnd                                       bool
	CanceledAt, EndedAt                                     time.Time
	PendingAmount                                           int64 // 0 = none
	// ProrationAdjust is what the next renewal changes by after a prorated
	// plan change (a charge when positive, a credit when negative), explained
	// by ProrationLines. BilledAmount is the rate the current period is paid
	// at once a prorated change has been made (0: Amount).
	ProrationAdjust, BilledAmount int64
	ProrationLines                []AdjustLine
	RequiresReauthorization       bool
	DunningAttempt                int
	NextRetryAt                   time.Time
	Description                   string
	Metadata                      map[string]string
	ReturnURL, CheckoutTokenHash  string
	CheckoutAction                string // JSON of the cached connector.NextAction
	// AllowedConnectors and Country are the routing restrictions the
	// subscription was created with (like a payment's).
	AllowedConnectors    []string
	Country              string
	ExpiresAt            time.Time
	Version              int64
	CreatedAt, UpdatedAt time.Time
}

const subCols = `id, org_id, mode, app_id, customer_id, status, billing_mode, provider_account_id,
	COALESCE(provider_subscription_id,''), provider_ref, COALESCE(mandate_id,''), COALESCE(pending_mandate_id,''), amount,
	currency, interval, interval_count, anchor_at, current_period_start, current_period_end, COALESCE(trial_end,0),
	COALESCE(total_count,0), renewals_done, cancel_at_period_end, COALESCE(canceled_at,0), COALESCE(ended_at,0),
	COALESCE(pending_amount,0), requires_reauthorization, dunning_attempt, COALESCE(next_retry_at,0), description, metadata,
	return_url, checkout_token_hash, checkout_action, expires_at, version, created_at, updated_at,
	proration_adjust, billed_amount, proration_lines, allowed_connectors, country`

func scanSub(row interface{ Scan(...any) error }) (Subscription, error) {
	var s Subscription
	var anchor, ps, pe, trial, canceled, ended, retry, exp, created, updated int64
	var cape, reauth int
	var meta, lines, allowed string
	err := row.Scan(&s.ID, &s.OrgID, &s.Mode, &s.AppID, &s.CustomerID, &s.Status, &s.BillingMode, &s.ProviderAccountID,
		&s.ProviderSubscriptionID, &s.ProviderRef, &s.MandateID, &s.PendingMandateID, &s.Amount, &s.Currency, &s.Interval,
		&s.IntervalCount, &anchor, &ps, &pe, &trial, &s.TotalCount, &s.RenewalsDone, &cape, &canceled, &ended,
		&s.PendingAmount, &reauth, &s.DunningAttempt, &retry, &s.Description, &meta, &s.ReturnURL, &s.CheckoutTokenHash, &s.CheckoutAction,
		&exp, &s.Version, &created, &updated, &s.ProrationAdjust, &s.BilledAmount, &lines, &allowed, &s.Country)
	if err != nil {
		return Subscription{}, notFound(err)
	}
	if err := json.Unmarshal([]byte(allowed), &s.AllowedConnectors); err != nil {
		return Subscription{}, fmt.Errorf("subscription %s allowed_connectors: %w", s.ID, err)
	}
	if s.ProrationLines, err = unmarshalLines(lines); err != nil {
		return Subscription{}, fmt.Errorf("subscription %s proration lines: %w", s.ID, err)
	}
	if err := json.Unmarshal([]byte(meta), &s.Metadata); err != nil {
		return Subscription{}, fmt.Errorf("subscription %s metadata: %w", s.ID, err)
	}
	s.AnchorAt, s.CurrentPeriodStart, s.CurrentPeriodEnd = fromMS(anchor), fromMS(ps), fromMS(pe)
	s.TrialEnd, s.CanceledAt, s.EndedAt, s.NextRetryAt = fromMSZero(trial), fromMSZero(canceled), fromMSZero(ended), fromMSZero(retry)
	s.CancelAtPeriodEnd, s.RequiresReauthorization = cape == 1, reauth == 1
	s.ExpiresAt, s.CreatedAt, s.UpdatedAt = fromMS(exp), fromMS(created), fromMS(updated)
	return s, nil
}

// InsertSubscription inserts s.
func InsertSubscription(ctx context.Context, tx Tx, s Subscription) error {
	meta, _ := json.Marshal(nonNilMap(s.Metadata))
	allowed, _ := json.Marshal(nonNilSlice(s.AllowedConnectors))
	_, err := tx.ExecContext(ctx, `INSERT INTO subscriptions (id, org_id, mode, app_id, customer_id, status, billing_mode,
		provider_account_id, provider_subscription_id, provider_ref, mandate_id, pending_mandate_id, amount, currency, interval,
		interval_count, anchor_at, current_period_start, current_period_end, trial_end, total_count, renewals_done,
		cancel_at_period_end, canceled_at, ended_at, pending_amount, requires_reauthorization, dunning_attempt, next_retry_at,
		description, metadata, return_url, checkout_token_hash, checkout_action, expires_at, version, created_at, updated_at,
		allowed_connectors, country)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.OrgID, s.Mode, s.AppID, s.CustomerID, s.Status, s.BillingMode, s.ProviderAccountID,
		nullIfEmpty(s.ProviderSubscriptionID), s.ProviderRef, nullIfEmpty(s.MandateID), nullIfEmpty(s.PendingMandateID),
		s.Amount, s.Currency, s.Interval, s.IntervalCount, ms(s.AnchorAt), ms(s.CurrentPeriodStart), ms(s.CurrentPeriodEnd),
		zeroMS(s.TrialEnd), zeroInt(s.TotalCount), s.RenewalsDone, boolInt(s.CancelAtPeriodEnd), zeroMS(s.CanceledAt),
		zeroMS(s.EndedAt), zeroInt64(s.PendingAmount), boolInt(s.RequiresReauthorization), s.DunningAttempt,
		zeroMS(s.NextRetryAt), s.Description, string(meta), s.ReturnURL, s.CheckoutTokenHash, s.CheckoutAction, ms(s.ExpiresAt), s.Version,
		ms(s.CreatedAt), ms(s.UpdatedAt), string(allowed), s.Country)
	return err
}

func zeroInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

func zeroInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// GetSubscription loads a subscription by id.
func GetSubscription(ctx context.Context, q Queryer, id string) (Subscription, error) {
	return scanSub(q.QueryRowContext(ctx, "SELECT "+subCols+" FROM subscriptions WHERE id = ?", id))
}

// GetSubscriptionForUpdate loads a subscription and locks it on Postgres
// (SQLite's single writer already serialises). Everything that changes a
// subscription or its cycles does so under this lock.
func GetSubscriptionForUpdate(ctx context.Context, tx Tx, id string) (Subscription, error) {
	return scanSub(tx.QueryRowContext(ctx, "SELECT "+subCols+" FROM subscriptions WHERE id = ?"+tx.Dialect().ForUpdate(), id))
}

// GetSubscriptionByProviderRef loads a subscription by its short provider reference.
func GetSubscriptionByProviderRef(ctx context.Context, q Queryer, ref string) (Subscription, error) {
	return scanSub(q.QueryRowContext(ctx, "SELECT "+subCols+" FROM subscriptions WHERE provider_ref = ?", ref))
}

// GetSubscriptionByProviderID loads a subscription by the provider's id.
func GetSubscriptionByProviderID(ctx context.Context, q Queryer, accountID, providerSubscriptionID string) (Subscription, error) {
	return scanSub(q.QueryRowContext(ctx, "SELECT "+subCols+" FROM subscriptions WHERE provider_account_id = ? AND provider_subscription_id = ?",
		accountID, providerSubscriptionID))
}

// SubscriptionsByMandate lists the subscriptions using (or about to use) a mandate.
func SubscriptionsByMandate(ctx context.Context, q Queryer, mandateID string) ([]Subscription, error) {
	return listSubs(ctx, q, "mandate_id = ? OR pending_mandate_id = ? ORDER BY id", mandateID, mandateID)
}

func listSubs(ctx context.Context, q Queryer, where string, args ...any) ([]Subscription, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+subCols+" FROM subscriptions WHERE "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Subscription
	for rows.Next() {
		s, err := scanSub(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListSubscriptions returns an app's subscriptions in one mode, newest first.
// customerID and status filter when set; afterID is an exclusive cursor.
func ListSubscriptions(ctx context.Context, q Queryer, appID, mode, customerID, status, afterID string, limit int) ([]Subscription, error) {
	where, args := "app_id = ? AND mode = ?", []any{appID, mode}
	if customerID != "" {
		where += " AND customer_id = ?"
		args = append(args, customerID)
	}
	if status != "" {
		where += " AND status = ?"
		args = append(args, status)
	}
	if afterID != "" {
		where += " AND id < ?"
		args = append(args, afterID)
	}
	return listSubs(ctx, q, where+" ORDER BY id DESC LIMIT ?", append(args, limit)...)
}

// UpdateSubscription writes the mutable fields of s if its stored version is
// still s.Version, then bumps the version. A stale version returns ErrConflict.
func UpdateSubscription(ctx context.Context, tx Tx, s *Subscription, now time.Time) error {
	res, err := tx.ExecContext(ctx, `UPDATE subscriptions SET status = ?, provider_subscription_id = ?, mandate_id = ?,
		pending_mandate_id = ?, amount = ?, current_period_start = ?, current_period_end = ?, trial_end = ?, anchor_at = ?,
		renewals_done = ?, cancel_at_period_end = ?, canceled_at = ?, ended_at = ?, pending_amount = ?,
		requires_reauthorization = ?, dunning_attempt = ?, next_retry_at = ?, checkout_token_hash = ?, checkout_action = ?, expires_at = ?,
		proration_adjust = ?, billed_amount = ?, proration_lines = ?,
		version = version + 1, updated_at = ? WHERE id = ? AND version = ?`,
		s.Status, nullIfEmpty(s.ProviderSubscriptionID), nullIfEmpty(s.MandateID), nullIfEmpty(s.PendingMandateID), s.Amount,
		ms(s.CurrentPeriodStart), ms(s.CurrentPeriodEnd), zeroMS(s.TrialEnd), ms(s.AnchorAt), s.RenewalsDone,
		boolInt(s.CancelAtPeriodEnd), zeroMS(s.CanceledAt), zeroMS(s.EndedAt), zeroInt64(s.PendingAmount),
		boolInt(s.RequiresReauthorization), s.DunningAttempt, zeroMS(s.NextRetryAt), s.CheckoutTokenHash, s.CheckoutAction, ms(s.ExpiresAt),
		s.ProrationAdjust, s.BilledAmount, marshalLines(s.ProrationLines), ms(now), s.ID, s.Version)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrConflict
	}
	s.Version++
	s.UpdatedAt = now
	return nil
}

// Cycle is a row of subscription_cycles.
type Cycle struct {
	ID, SubscriptionID                      string
	PeriodIndex                             int // -1 for provider-managed charges (keyed by ProviderPaymentID)
	ProviderPaymentID, PaymentID, AttemptID string
	Amount                                  int64
	// Adjustments are the prorated lines included in Amount (the rest is the
	// period's own amount), kept for the invoice.
	Adjustments                       []AdjustLine
	State                             string
	Retries, JobSeq                   int
	NotifiedAt, NextRunAt, LeaseUntil time.Time
	CreatedAt, UpdatedAt              time.Time
}

const cycleCols = `id, subscription_id, COALESCE(period_index,-1), COALESCE(provider_payment_id,''), COALESCE(payment_id,''),
	COALESCE(attempt_id,''), amount, state, retries, job_seq, COALESCE(notified_at,0), COALESCE(next_run_at,0),
	COALESCE(lease_until,0), created_at, updated_at, adjustments`

func scanCycle(row interface{ Scan(...any) error }) (Cycle, error) {
	var c Cycle
	var notified, next, lease, created, updated int64
	var adj string
	err := row.Scan(&c.ID, &c.SubscriptionID, &c.PeriodIndex, &c.ProviderPaymentID, &c.PaymentID, &c.AttemptID, &c.Amount,
		&c.State, &c.Retries, &c.JobSeq, &notified, &next, &lease, &created, &updated, &adj)
	if err != nil {
		return Cycle{}, notFound(err)
	}
	if c.Adjustments, err = unmarshalLines(adj); err != nil {
		return Cycle{}, fmt.Errorf("cycle %s adjustments: %w", c.ID, err)
	}
	c.NotifiedAt, c.NextRunAt, c.LeaseUntil = fromMSZero(notified), fromMSZero(next), fromMSZero(lease)
	c.CreatedAt, c.UpdatedAt = fromMS(created), fromMS(updated)
	return c, nil
}

// InsertCycle inserts c. Another cycle for the same period or provider
// payment returns ErrConflict.
func InsertCycle(ctx context.Context, tx Tx, c Cycle) error {
	var period any
	if c.PeriodIndex >= 0 {
		period = c.PeriodIndex
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO subscription_cycles (id, subscription_id, period_index, provider_payment_id,
		payment_id, attempt_id, amount, state, retries, job_seq, notified_at, next_run_at, lease_until, created_at, updated_at, adjustments)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.SubscriptionID, period, nullIfEmpty(c.ProviderPaymentID), nullIfEmpty(c.PaymentID), nullIfEmpty(c.AttemptID),
		c.Amount, c.State, c.Retries, c.JobSeq, zeroMS(c.NotifiedAt), zeroMS(c.NextRunAt), zeroMS(c.LeaseUntil),
		ms(c.CreatedAt), ms(c.UpdatedAt), marshalLines(c.Adjustments))
	if IsUniqueViolation(err) {
		return fmt.Errorf("%w: cycle exists", ErrConflict)
	}
	return err
}

// GetCycle loads the cycle of a billed period.
func GetCycle(ctx context.Context, q Queryer, subID string, period int) (Cycle, error) {
	return scanCycle(q.QueryRowContext(ctx, "SELECT "+cycleCols+" FROM subscription_cycles WHERE subscription_id = ? AND period_index = ?", subID, period))
}

// GetCycleByProviderPayment loads the cycle of a provider-managed charge.
func GetCycleByProviderPayment(ctx context.Context, q Queryer, subID, providerPaymentID string) (Cycle, error) {
	return scanCycle(q.QueryRowContext(ctx, "SELECT "+cycleCols+" FROM subscription_cycles WHERE subscription_id = ? AND provider_payment_id = ?",
		subID, providerPaymentID))
}

// GetCycleByPayment loads the cycle that produced a payment.
func GetCycleByPayment(ctx context.Context, q Queryer, paymentID string) (Cycle, error) {
	return scanCycle(q.QueryRowContext(ctx, "SELECT "+cycleCols+" FROM subscription_cycles WHERE payment_id = ?", paymentID))
}

// ListCycles lists a subscription's cycles in creation order.
func ListCycles(ctx context.Context, q Queryer, subID string) ([]Cycle, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+cycleCols+" FROM subscription_cycles WHERE subscription_id = ? ORDER BY created_at, id", subID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Cycle
	for rows.Next() {
		c, err := scanCycle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateCycle writes the mutable fields of c.
func UpdateCycle(ctx context.Context, tx Tx, c Cycle, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE subscription_cycles SET provider_payment_id = ?, payment_id = ?, attempt_id = ?,
		amount = ?, state = ?, retries = ?, job_seq = ?, notified_at = ?, next_run_at = ?, lease_until = ?, adjustments = ?, updated_at = ? WHERE id = ?`,
		nullIfEmpty(c.ProviderPaymentID), nullIfEmpty(c.PaymentID), nullIfEmpty(c.AttemptID), c.Amount, c.State, c.Retries,
		c.JobSeq, zeroMS(c.NotifiedAt), zeroMS(c.NextRunAt), zeroMS(c.LeaseUntil), marshalLines(c.Adjustments), ms(now), c.ID)
	return err
}

// ListOpenMandates returns the account's mandates that are not final
// (pending or active) and already have the provider's id.
func ListOpenMandates(ctx context.Context, q Queryer, accountID string) ([]Mandate, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+mandateCols+` FROM mandates WHERE provider_account_id = ? AND status IN ('pending','active')
		AND provider_mandate_id IS NOT NULL ORDER BY created_at, id`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Mandate
	for rows.Next() {
		m, err := scanMandate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListOpenProviderSubscriptions returns the account's provider-managed
// subscriptions that are not final and already have the provider's id.
func ListOpenProviderSubscriptions(ctx context.Context, q Queryer, accountID string) ([]Subscription, error) {
	return listSubs(ctx, q, `provider_account_id = ? AND billing_mode = 'provider_managed' AND provider_subscription_id IS NOT NULL
		AND status IN ('incomplete','trialing','active','past_due') ORDER BY created_at, id`, accountID)
}
