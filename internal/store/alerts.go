package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Alert is a row of alerts: a condition the operator should know about.
// Repeats of a still-open condition raise Occurrences instead of adding rows.
type Alert struct {
	ID, OrgID, Severity, Kind, Message, Fingerprint string
	Refs                                            map[string]string
	Occurrences                                     int
	RaisedAt, LastRaisedAt                          time.Time
	AcknowledgedBy                                  string
	AcknowledgedAt                                  time.Time
}

// Open reports whether nobody has acknowledged the alert yet.
func (a Alert) Open() bool { return a.AcknowledgedAt.IsZero() }

const alertCols = `id, org_id, severity, kind, message, refs, fingerprint, occurrences, raised_at, last_raised_at,
	COALESCE(acknowledged_by,''), COALESCE(acknowledged_at,0)`

func scanAlert(row interface{ Scan(...any) error }) (Alert, error) {
	var a Alert
	var refs string
	var raised, last, ack int64
	if err := row.Scan(&a.ID, &a.OrgID, &a.Severity, &a.Kind, &a.Message, &refs, &a.Fingerprint, &a.Occurrences, &raised, &last, &a.AcknowledgedBy, &ack); err != nil {
		return Alert{}, notFound(err)
	}
	a.RaisedAt, a.LastRaisedAt, a.AcknowledgedAt = fromMS(raised), fromMS(last), fromMSZero(ack)
	if err := json.Unmarshal([]byte(refs), &a.Refs); err != nil {
		return Alert{}, fmt.Errorf("alert %s refs: %w", a.ID, err)
	}
	return a, nil
}

var severityRank = map[string]int{"info": 1, "warning": 2, "critical": 3}

// RecordAlert stores a raised alert. If the same condition (org and
// fingerprint) is still open, that alert gets one more occurrence, the latest
// message and the higher severity; otherwise a new one is inserted from a.
func RecordAlert(ctx context.Context, tx Tx, a Alert, at time.Time) (Alert, error) {
	if _, ok := severityRank[a.Severity]; !ok {
		return Alert{}, fmt.Errorf("store: unknown alert severity %q", a.Severity)
	}
	refs, err := json.Marshal(nonNilMap(a.Refs))
	if err != nil {
		return Alert{}, err
	}
	for attempt := 0; attempt < 3; attempt++ {
		var id, sev string
		err := tx.QueryRowContext(ctx, `SELECT id, severity FROM alerts WHERE org_id = ? AND fingerprint = ? AND acknowledged_at IS NULL`,
			a.OrgID, a.Fingerprint).Scan(&id, &sev)
		switch {
		case err == nil:
			if severityRank[a.Severity] > severityRank[sev] {
				sev = a.Severity
			}
			res, err := tx.ExecContext(ctx, `UPDATE alerts SET occurrences = occurrences + 1, last_raised_at = ?, message = ?, severity = ?, refs = ?
				WHERE id = ? AND acknowledged_at IS NULL`, ms(at), a.Message, sev, string(refs), id)
			if err != nil {
				return Alert{}, err
			}
			if n, _ := res.RowsAffected(); n == 1 {
				return GetAlert(ctx, tx, a.OrgID, id)
			}
			continue // acknowledged meanwhile: start a new one
		case errors.Is(err, sql.ErrNoRows):
			_, err := tx.ExecContext(ctx, `INSERT INTO alerts (id, org_id, severity, kind, message, refs, fingerprint, occurrences, raised_at, last_raised_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`, a.ID, a.OrgID, a.Severity, a.Kind, a.Message, string(refs), a.Fingerprint, ms(at), ms(at))
			if IsUniqueViolation(err) {
				continue // another raise won the race: count this one on its alert
			}
			if err != nil {
				return Alert{}, err
			}
			return GetAlert(ctx, tx, a.OrgID, a.ID)
		default:
			return Alert{}, err
		}
	}
	return Alert{}, errors.New("store: could not record the alert (contention)")
}

// GetAlert loads an alert of the org.
func GetAlert(ctx context.Context, q Queryer, orgID, id string) (Alert, error) {
	return scanAlert(q.QueryRowContext(ctx, `SELECT `+alertCols+` FROM alerts WHERE org_id = ? AND id = ?`, orgID, id))
}

// AlertFilter selects alerts of one org, most recently raised first. State is
// open, acknowledged or all (default open).
type AlertFilter struct {
	OrgID, State, Kind, Severity string
	Limit                        int
}

// ListAlerts returns the alerts matching f.
func ListAlerts(ctx context.Context, q Queryer, f AlertFilter) ([]Alert, error) {
	where, args := "org_id = ?", []any{f.OrgID}
	switch f.State {
	case "acknowledged":
		where += " AND acknowledged_at IS NOT NULL"
	case "all":
	default:
		where += " AND acknowledged_at IS NULL"
	}
	if f.Kind != "" {
		where += " AND kind = ?"
		args = append(args, f.Kind)
	}
	if f.Severity != "" {
		where += " AND severity = ?"
		args = append(args, f.Severity)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	rows, err := q.QueryContext(ctx, `SELECT `+alertCols+` FROM alerts WHERE `+where+` ORDER BY last_raised_at DESC, id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Alert
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AlertKinds lists the distinct kinds the org has alerts of.
func AlertKinds(ctx context.Context, q Queryer, orgID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT kind FROM alerts WHERE org_id = ? ORDER BY kind`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// CountOpenAlerts returns how many alerts are open, and how many of those are critical.
func CountOpenAlerts(ctx context.Context, q Queryer, orgID string) (open, critical int, err error) {
	err = q.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN severity = 'critical' THEN 1 ELSE 0 END), 0)
		FROM alerts WHERE org_id = ? AND acknowledged_at IS NULL`, orgID).Scan(&open, &critical)
	return
}

// AcknowledgeAlert marks an open alert of the org acknowledged by a user. It
// reports whether anything changed: an alert acknowledged already is left as it
// is. An unknown alert is ErrNotFound.
func AcknowledgeAlert(ctx context.Context, tx Tx, orgID, id, by string, at time.Time) (bool, error) {
	res, err := tx.ExecContext(ctx, `UPDATE alerts SET acknowledged_by = ?, acknowledged_at = ? WHERE org_id = ? AND id = ? AND acknowledged_at IS NULL`,
		by, ms(at), orgID, id)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return true, nil
	}
	if _, err := GetAlert(ctx, tx, orgID, id); err != nil {
		return false, err
	}
	return false, nil
}
