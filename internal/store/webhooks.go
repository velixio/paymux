package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound means the requested row does not exist.
var ErrNotFound = errors.New("not found")

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

func fromNullMS(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return fromMS(v.Int64)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// EventRow is a row of the events table (outbound domain event).
type EventRow struct {
	ID, OrgID, AppID, Mode, Type, ObjectType, ObjectID string
	ObjectVersion                                      int64
	Data                                               json.RawMessage
	CreatedAt                                          time.Time
}

// InsertEvent inserts an event inside tx.
func (db *DB) InsertEvent(ctx context.Context, tx Tx, e EventRow) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO events
		(id, org_id, app_id, mode, type, object_type, object_id, object_version, data, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.OrgID, e.AppID, e.Mode, e.Type, e.ObjectType, e.ObjectID, e.ObjectVersion, string(e.Data), ms(e.CreatedAt))
	if err != nil {
		return fmt.Errorf("insert event: %w", err)
	}
	return nil
}

// EndpointRow is a row of webhook_endpoints. An empty EventTypes list means
// every event type.
type EndpointRow struct {
	ID, OrgID, AppID, URL string
	SigningSecretEnc      []byte
	EventTypes            []string
	Enabled               bool
	// Mode is "test" or "live": the endpoint receives the events of that mode only.
	Mode                 string
	CreatedAt, UpdatedAt time.Time
}

const endpointCols = `id, org_id, app_id, url, signing_secret_enc, event_types, enabled, mode, created_at, updated_at`

type scanner interface{ Scan(dest ...any) error }

func scanEndpoint(s scanner) (EndpointRow, error) {
	var e EndpointRow
	var types string
	var enabled int
	var created, updated int64
	if err := s.Scan(&e.ID, &e.OrgID, &e.AppID, &e.URL, &e.SigningSecretEnc, &types, &enabled, &e.Mode, &created, &updated); err != nil {
		return EndpointRow{}, err
	}
	if err := json.Unmarshal([]byte(types), &e.EventTypes); err != nil {
		return EndpointRow{}, fmt.Errorf("endpoint %s event_types: %w", e.ID, err)
	}
	e.Enabled = enabled != 0
	e.CreatedAt, e.UpdatedAt = fromMS(created), fromMS(updated)
	return e, nil
}

// InsertEndpoint inserts a webhook endpoint inside tx.
func (db *DB) InsertEndpoint(ctx context.Context, tx Tx, e EndpointRow) error {
	types, err := json.Marshal(e.EventTypes)
	if err != nil {
		return err
	}
	if e.EventTypes == nil {
		types = []byte("[]")
	}
	if e.Mode == "" {
		e.Mode = "live"
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO webhook_endpoints (`+endpointCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.OrgID, e.AppID, e.URL, e.SigningSecretEnc, string(types), b2i(e.Enabled), e.Mode, ms(e.CreatedAt), ms(e.UpdatedAt))
	if err != nil {
		return fmt.Errorf("insert webhook endpoint: %w", err)
	}
	return nil
}

// GetEndpoint returns one endpoint or ErrNotFound.
func (db *DB) GetEndpoint(ctx context.Context, id string) (EndpointRow, error) {
	var e EndpointRow
	err := db.Read(ctx, func(q Querier) (err error) {
		e, err = scanEndpoint(q.QueryRowContext(ctx, `SELECT `+endpointCols+` FROM webhook_endpoints WHERE id = ?`, id))
		return
	})
	if errors.Is(err, sql.ErrNoRows) {
		return EndpointRow{}, ErrNotFound
	}
	return e, err
}

func collectEndpoints(rows *sql.Rows, err error) ([]EndpointRow, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EndpointRow
	for rows.Next() {
		e, err := scanEndpoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListEndpoints lists the endpoints of an app (all apps when appID is empty).
func (db *DB) ListEndpoints(ctx context.Context, appID string) ([]EndpointRow, error) {
	var out []EndpointRow
	err := db.Read(ctx, func(q Querier) (err error) {
		if appID == "" {
			out, err = collectEndpoints(q.QueryContext(ctx, `SELECT `+endpointCols+` FROM webhook_endpoints ORDER BY created_at, id`))
		} else {
			out, err = collectEndpoints(q.QueryContext(ctx, `SELECT `+endpointCols+` FROM webhook_endpoints WHERE app_id = ? ORDER BY created_at, id`, appID))
		}
		return
	})
	return out, err
}

// ListEnabledEndpoints lists an app's enabled endpoints inside tx, so a
// just-created endpoint in the same transaction is visible.
func (db *DB) ListEnabledEndpoints(ctx context.Context, tx Tx, appID string) ([]EndpointRow, error) {
	return collectEndpoints(tx.QueryContext(ctx, `SELECT `+endpointCols+` FROM webhook_endpoints
		WHERE app_id = ? AND enabled = 1 ORDER BY created_at, id`, appID))
}

// EndpointUpdate carries the fields to change; nil fields are left alone.
type EndpointUpdate struct {
	URL        *string
	EventTypes *[]string
	Enabled    *bool
}

// UpdateEndpoint applies u to the endpoint, or returns ErrNotFound.
func (db *DB) UpdateEndpoint(ctx context.Context, id string, u EndpointUpdate, now time.Time) error {
	return db.Write(ctx, func(tx Tx) error {
		set, args := "updated_at = ?", []any{ms(now)}
		if u.URL != nil {
			set += ", url = ?"
			args = append(args, *u.URL)
		}
		if u.EventTypes != nil {
			b, err := json.Marshal(*u.EventTypes)
			if err != nil {
				return err
			}
			set += ", event_types = ?"
			args = append(args, string(b))
		}
		if u.Enabled != nil {
			set += ", enabled = ?"
			args = append(args, b2i(*u.Enabled))
		}
		res, err := tx.ExecContext(ctx, `UPDATE webhook_endpoints SET `+set+` WHERE id = ?`, append(args, id)...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// DeliveryRow is a row of webhook_deliveries. Status is pending, delivered or
// failed. Zero times and LastStatus 0 mean NULL.
type DeliveryRow struct {
	ID, EventID, EndpointID string
	Status                  string
	AttemptCount            int
	NextAttemptAt           time.Time
	LastStatus              int
	LastError               string
	DeliveredAt             time.Time
	CreatedAt, UpdatedAt    time.Time
}

// InsertDelivery inserts a delivery inside tx.
func (db *DB) InsertDelivery(ctx context.Context, tx Tx, d DeliveryRow) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO webhook_deliveries
		(id, event_id, endpoint_id, status, attempt_count, next_attempt_at, last_status, last_error, delivered_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.EventID, d.EndpointID, d.Status, d.AttemptCount, nullTime(d.NextAttemptAt),
		nullInt(d.LastStatus), nullStr(d.LastError), nullTime(d.DeliveredAt), ms(d.CreatedAt), ms(d.UpdatedAt))
	if err != nil {
		return fmt.Errorf("insert webhook delivery: %w", err)
	}
	return nil
}

func nullInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

// DeliveryDetail is a delivery with its event and endpoint.
type DeliveryDetail struct {
	Delivery DeliveryRow
	Event    EventRow
	Endpoint EndpointRow
}

// GetDelivery loads a delivery with its event and endpoint, or ErrNotFound.
func (db *DB) GetDelivery(ctx context.Context, id string) (DeliveryDetail, error) {
	var d DeliveryDetail
	err := db.Read(ctx, func(q Querier) error {
		var (
			next, delivered              sql.NullInt64
			lastStatus                   sql.NullInt64
			lastErr                      sql.NullString
			dCreated, dUpdated, evCreate int64
			evData                       string
			epTypes                      string
			epEnabled                    int
			epCreated, epUpdated         int64
		)
		e, w := &d.Event, &d.Endpoint
		x := &d.Delivery
		err := q.QueryRowContext(ctx, `SELECT
			d.id, d.event_id, d.endpoint_id, d.status, d.attempt_count, d.next_attempt_at, d.last_status, d.last_error, d.delivered_at, d.created_at, d.updated_at,
			e.id, e.org_id, e.app_id, e.mode, e.type, e.object_type, e.object_id, e.object_version, e.data, e.created_at,
			w.id, w.org_id, w.app_id, w.url, w.signing_secret_enc, w.event_types, w.enabled, w.created_at, w.updated_at
			FROM webhook_deliveries d
			JOIN events e ON e.id = d.event_id
			JOIN webhook_endpoints w ON w.id = d.endpoint_id
			WHERE d.id = ?`, id).Scan(
			&x.ID, &x.EventID, &x.EndpointID, &x.Status, &x.AttemptCount, &next, &lastStatus, &lastErr, &delivered, &dCreated, &dUpdated,
			&e.ID, &e.OrgID, &e.AppID, &e.Mode, &e.Type, &e.ObjectType, &e.ObjectID, &e.ObjectVersion, &evData, &evCreate,
			&w.ID, &w.OrgID, &w.AppID, &w.URL, &w.SigningSecretEnc, &epTypes, &epEnabled, &epCreated, &epUpdated)
		if err != nil {
			return err
		}
		x.NextAttemptAt, x.DeliveredAt = fromNullMS(next), fromNullMS(delivered)
		x.LastStatus, x.LastError = int(lastStatus.Int64), lastErr.String
		x.CreatedAt, x.UpdatedAt = fromMS(dCreated), fromMS(dUpdated)
		e.Data, e.CreatedAt = json.RawMessage(evData), fromMS(evCreate)
		if err := json.Unmarshal([]byte(epTypes), &w.EventTypes); err != nil {
			return fmt.Errorf("endpoint %s event_types: %w", w.ID, err)
		}
		w.Enabled, w.CreatedAt, w.UpdatedAt = epEnabled != 0, fromMS(epCreated), fromMS(epUpdated)
		return nil
	})
	if errors.Is(err, sql.ErrNoRows) {
		return DeliveryDetail{}, ErrNotFound
	}
	return d, err
}

// DeliveryUpdate replaces a delivery's mutable state. Zero times, LastStatus 0
// and empty LastError store NULL.
type DeliveryUpdate struct {
	Status        string
	AttemptCount  int
	NextAttemptAt time.Time
	LastStatus    int
	LastError     string
	DeliveredAt   time.Time
}

// UpdateDelivery overwrites the delivery's state inside tx.
func (db *DB) UpdateDelivery(ctx context.Context, tx Tx, id string, u DeliveryUpdate, now time.Time) error {
	res, err := tx.ExecContext(ctx, `UPDATE webhook_deliveries SET status = ?, attempt_count = ?, next_attempt_at = ?,
		last_status = ?, last_error = ?, delivered_at = ?, updated_at = ? WHERE id = ?`,
		u.Status, u.AttemptCount, nullTime(u.NextAttemptAt), nullInt(u.LastStatus), nullStr(u.LastError),
		nullTime(u.DeliveredAt), ms(now), id)
	if err != nil {
		return fmt.Errorf("update webhook delivery: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListFailedDeliveryIDs returns the ids of an endpoint's failed deliveries
// last updated at or after since, oldest first.
func (db *DB) ListFailedDeliveryIDs(ctx context.Context, endpointID string, since time.Time) ([]string, error) {
	var out []string
	err := db.Read(ctx, func(q Querier) error {
		rows, err := q.QueryContext(ctx, `SELECT id FROM webhook_deliveries
			WHERE endpoint_id = ? AND status = 'failed' AND updated_at >= ? ORDER BY updated_at, id`, endpointID, ms(since))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out = append(out, id)
		}
		return rows.Err()
	})
	return out, err
}

// ProviderEventRow is a row of provider_events (raw inbound webhook).
type ProviderEventRow struct {
	ID, ProviderAccountID, ProviderEventID string
	Headers                                json.RawMessage
	Body                                   []byte
	Verified                               bool
	ReceivedAt, ProcessedAt                time.Time
	Error                                  string
}

// InsertProviderEvent inserts the event, doing nothing when
// (provider_account_id, provider_event_id) already exists. It reports whether
// a row was inserted.
func (db *DB) InsertProviderEvent(ctx context.Context, tx Tx, p ProviderEventRow) (bool, error) {
	res, err := tx.ExecContext(ctx, `INSERT INTO provider_events
		(id, provider_account_id, provider_event_id, headers, body, verified, received_at, processed_at, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (provider_account_id, provider_event_id) DO NOTHING`,
		p.ID, p.ProviderAccountID, p.ProviderEventID, string(p.Headers), p.Body, b2i(p.Verified),
		ms(p.ReceivedAt), nullTime(p.ProcessedAt), nullStr(p.Error))
	if err != nil {
		return false, fmt.Errorf("insert provider event: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// GetProviderEvent returns the event for the account and provider event id,
// or ErrNotFound.
func (db *DB) GetProviderEvent(ctx context.Context, accountID, eventID string) (ProviderEventRow, error) {
	var p ProviderEventRow
	err := db.Read(ctx, func(q Querier) error {
		var headers string
		var verified int
		var received int64
		var processed sql.NullInt64
		var errText sql.NullString
		if err := q.QueryRowContext(ctx, `SELECT id, provider_account_id, provider_event_id, headers, body, verified, received_at, processed_at, error
			FROM provider_events WHERE provider_account_id = ? AND provider_event_id = ?`, accountID, eventID).
			Scan(&p.ID, &p.ProviderAccountID, &p.ProviderEventID, &headers, &p.Body, &verified, &received, &processed, &errText); err != nil {
			return err
		}
		p.Headers, p.Verified = json.RawMessage(headers), verified != 0
		p.ReceivedAt, p.ProcessedAt, p.Error = fromMS(received), fromNullMS(processed), errText.String
		return nil
	})
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderEventRow{}, ErrNotFound
	}
	return p, err
}
