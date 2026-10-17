// Package alertcenter keeps the ops alerts Paymux raises, so that the dashboard
// can list and acknowledge them. It is the notify.Recorder of
// notify.Alerts: delivery by email and webhook is unchanged.
package alertcenter

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/notify"
	"github.com/paymux/paymux/internal/store"
)

// Errors, matched with errors.Is.
var (
	ErrNotFound = errors.New("alertcenter: no such alert")
	ErrInvalid  = errors.New("alertcenter: invalid input")
)

// Service stores and reads alerts.
type Service struct {
	db  *store.DB
	now func() time.Time
}

// New builds a Service. now defaults to time.Now.
func New(db *store.DB, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{db: db, now: now}
}

// Record keeps a raised alert (notify.Recorder). Alerts belong to the install's
// org; a repeat of a condition that is still open adds an occurrence to it.
func (s *Service) Record(ctx context.Context, al notify.Alert, at time.Time) error {
	if al.Severity == "" {
		al.Severity = notify.SeverityWarning
	}
	return s.db.Write(ctx, func(tx store.Tx) error {
		org, err := store.FirstOrgID(ctx, tx)
		if err != nil {
			return err
		}
		_, err = store.RecordAlert(ctx, tx, store.Alert{ID: ids.New("alt"), OrgID: org, Severity: string(al.Severity), Kind: al.Kind, Message: al.Message,
			Refs: al.Refs, Fingerprint: al.Fingerprint()}, at.UTC())
		return err
	})
}

// Filter selects alerts. State is open (default), acknowledged or all.
type Filter struct {
	State, Kind, Severity string
	Limit                 int
}

// List returns the org's alerts matching f, most recently raised first.
func (s *Service) List(ctx context.Context, orgID string, f Filter) ([]store.Alert, error) {
	switch f.State {
	case "", "open", "acknowledged", "all":
	default:
		return nil, fmt.Errorf("%w: state must be open, acknowledged or all", ErrInvalid)
	}
	switch f.Severity {
	case "", "info", "warning", "critical":
	default:
		return nil, fmt.Errorf("%w: severity must be info, warning or critical", ErrInvalid)
	}
	var out []store.Alert
	err := s.db.Read(ctx, func(q store.Querier) (err error) {
		out, err = store.ListAlerts(ctx, q, store.AlertFilter{OrgID: orgID, State: f.State, Kind: f.Kind, Severity: f.Severity, Limit: f.Limit})
		return
	})
	return out, err
}

// Kinds lists the kinds of alert the org has, for the filter.
func (s *Service) Kinds(ctx context.Context, orgID string) ([]string, error) {
	var out []string
	err := s.db.Read(ctx, func(q store.Querier) (err error) { out, err = store.AlertKinds(ctx, q, orgID); return })
	return out, err
}

// Counts returns the number of open alerts and how many of them are critical.
func (s *Service) Counts(ctx context.Context, orgID string) (open, critical int, err error) {
	err = s.db.Read(ctx, func(q store.Querier) (err error) {
		open, critical, err = store.CountOpenAlerts(ctx, q, orgID)
		return
	})
	return
}

// Acknowledge marks an alert seen. It is audited when it changes something;
// acknowledging an alert that already is acknowledged changes nothing.
func (s *Service) Acknowledge(ctx context.Context, actor audit.Actor, orgID, id string) (store.Alert, error) {
	var out store.Alert
	err := s.db.Write(ctx, func(tx store.Tx) error {
		changed, err := store.AcknowledgeAlert(ctx, tx, orgID, id, actor.ID, s.now().UTC())
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if out, err = store.GetAlert(ctx, tx, orgID, id); err != nil {
			return err
		}
		if !changed {
			return nil
		}
		return audit.Record(ctx, tx, audit.Entry{OrgID: orgID, Actor: actor, Action: "alert.acknowledge", Target: id,
			After: map[string]any{"kind": out.Kind, "severity": out.Severity, "occurrences": out.Occurrences}})
	})
	return out, err
}
