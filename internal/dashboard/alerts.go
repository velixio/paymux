package dashboard

import (
	"errors"
	"net/http"
	"sort"

	"github.com/velixio/paymux/internal/alertcenter"
	"github.com/velixio/paymux/internal/authz"
	"github.com/velixio/paymux/internal/store"
)

// The alerts center: every alert Paymux raises (the ones also sent by email
// and webhook) with a way to acknowledge it.

type alertRow struct {
	ID, Severity, Kind, Message, First, Last string
	Refs                                     []string
	Occurrences                              int
	Acknowledged                             bool
	By, At                                   string
	CanAck                                   bool
}

type alertFilter struct{ State, Kind, Severity string }

func (d *Dashboard) routesAlerts() {
	m := d.mux
	m.HandleFunc("GET "+prefix+"/alerts", d.authed(authz.PaymentRead, d.alertsPage))
	// Acknowledging is an operator action (the same level as handling disputes).
	m.HandleFunc("POST "+prefix+"/alerts/{id}/acknowledge", d.authed(authz.DisputeManage, d.alertAcknowledge))
}

// alertCounts is what the navigation badge shows; a failure to count shows nothing.
func (d *Dashboard) alertCounts(r *http.Request) (open, critical int) {
	org, err := d.orgID(r.Context())
	if err != nil {
		return 0, 0
	}
	open, critical, err = d.o.Alerts.Counts(r.Context(), org)
	if err != nil {
		return 0, 0
	}
	return open, critical
}

func (d *Dashboard) alertsPage(w http.ResponseWriter, r *http.Request, s *session) {
	qv := r.URL.Query()
	f := alertFilter{State: qv.Get("state"), Kind: qv.Get("kind"), Severity: qv.Get("severity")}
	org, err := d.orgID(r.Context())
	if err != nil {
		d.fail(w, err)
		return
	}
	list, err := d.o.Alerts.List(r.Context(), org, alertcenter.Filter{State: f.State, Kind: f.Kind, Severity: f.Severity})
	if errors.Is(err, alertcenter.ErrInvalid) {
		badRequest(w, err.Error())
		return
	}
	if err != nil {
		d.fail(w, err)
		return
	}
	kinds, err := d.o.Alerts.Kinds(r.Context(), org)
	if err != nil {
		d.fail(w, err)
		return
	}
	v := d.shell(r, s, "alerts", "Alerts")
	canAck := s.can(authz.DisputeManage)
	for _, a := range list {
		row := alertRow{ID: a.ID, Severity: a.Severity, Kind: a.Kind, Message: a.Message, First: d.fmtTime(a.RaisedAt), Last: d.fmtTime(a.LastRaisedAt),
			Occurrences: a.Occurrences, Acknowledged: !a.Open(), By: a.AcknowledgedBy, CanAck: canAck && a.Open()}
		if !a.Open() {
			row.At = d.fmtTime(a.AcknowledgedAt)
		}
		keys := make([]string, 0, len(a.Refs))
		for k := range a.Refs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			row.Refs = append(row.Refs, k+"="+a.Refs[k])
		}
		v.AlertRows = append(v.AlertRows, row)
	}
	v.AlertFilter, v.AlertKinds = f, kinds
	d.render(w, http.StatusOK, "dash_alerts", v)
}

func (d *Dashboard) alertAcknowledge(w http.ResponseWriter, r *http.Request, s *session) {
	org, err := d.orgID(r.Context())
	if err != nil {
		d.fail(w, err)
		return
	}
	if _, err := d.o.Alerts.Acknowledge(r.Context(), s.auditActor(), org, r.PathValue("id")); err != nil {
		if errors.Is(err, alertcenter.ErrNotFound) || errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		d.fail(w, err)
		return
	}
	http.Redirect(w, r, prefix+"/alerts", http.StatusSeeOther)
}
