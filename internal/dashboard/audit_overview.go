package dashboard

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/paymux/paymux/internal/authz"
	"github.com/paymux/paymux/internal/store"
)

type auditFilter struct{ Actor, Action, From, To string }

type auditRow struct {
	ID, At, Actor, Action, Target, IP, Before, After string
}

type periodView struct {
	ID, Label string
	Rows      []totalRow
}

type totalRow struct{ App, Currency, Gross, Refunds, Net string }

func (d *Dashboard) routesAuditOverview() {
	d.mux.HandleFunc("GET "+prefix+"/audit", d.authed(authz.AuditRead, d.auditPage))
	d.mux.HandleFunc("GET "+prefix+"/overview", d.authed(authz.PaymentRead, d.overviewPage))
}

func (d *Dashboard) auditPage(w http.ResponseWriter, r *http.Request, s *session) {
	ctx := r.Context()
	qv := r.URL.Query()
	f := auditFilter{Actor: strings.TrimSpace(qv.Get("actor")), Action: strings.TrimSpace(qv.Get("action")), From: qv.Get("from"), To: qv.Get("to")}
	org, err := d.orgID(ctx)
	if err != nil {
		d.fail(w, err)
		return
	}
	sf := store.AuditFilter{OrgID: org, Actor: f.Actor, Action: f.Action, After: qv.Get("after")}
	if f.From != "" {
		t, err := d.parseDay(f.From)
		if err != nil {
			badRequest(w, "from must be a date like 2026-10-01")
			return
		}
		sf.From = t
	}
	if f.To != "" {
		t, err := d.parseDay(f.To)
		if err != nil {
			badRequest(w, "to must be a date like 2026-10-01")
			return
		}
		sf.To = t.AddDate(0, 0, 1)
	}
	var rows []store.AuditRow
	if err := d.o.DB.Read(ctx, func(q store.Querier) (err error) { rows, err = store.SearchAudit(ctx, q, sf, d.o.PageSize+1); return }); err != nil {
		d.fail(w, err)
		return
	}
	v := d.shell(r, s, "audit", "Audit log")
	v.AuditFilter = f
	more := len(rows) > d.o.PageSize
	if more {
		rows = rows[:d.o.PageSize]
	}
	for _, a := range rows {
		actor := a.ActorType
		if a.ActorID != "" {
			actor += " " + a.ActorID
		}
		v.AuditRows = append(v.AuditRows, auditRow{ID: a.ID, At: d.fmtTime(a.At), Actor: actor, Action: a.Action, Target: a.Target,
			IP: a.IP, Before: a.Before, After: a.After})
	}
	if more {
		next := url.Values{}
		for k, val := range map[string]string{"actor": f.Actor, "action": f.Action, "from": f.From, "to": f.To} {
			if val != "" {
				next.Set(k, val)
			}
		}
		next.Set("after", rows[len(rows)-1].ID)
		v.NextURL = prefix + "/audit?" + next.Encode()
	}
	d.render(w, http.StatusOK, "dash_audit", v)
}

func (d *Dashboard) overviewPage(w http.ResponseWriter, r *http.Request, s *session) {
	ctx := r.Context()
	mode := d.mode(r, s)
	now := d.o.Now().In(d.o.Location)
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, d.o.Location)
	periods := []struct {
		id, label string
		since     time.Time
	}{
		{"today", "Today", midnight},
		{"7d", "Last 7 days", now.Add(-7 * 24 * time.Hour)},
		{"30d", "Last 30 days", now.Add(-30 * 24 * time.Hour)},
	}
	org, err := d.orgID(ctx)
	if err != nil {
		d.fail(w, err)
		return
	}
	names := map[string]string{}
	totals := make([][]store.LedgerTotal, len(periods))
	err = d.o.DB.Read(ctx, func(q store.Querier) error {
		apps, err := store.ListApps(ctx, q, org)
		if err != nil {
			return err
		}
		for _, a := range apps {
			names[a.ID] = a.Name
		}
		for i, p := range periods {
			if totals[i], err = store.LedgerTotals(ctx, q, org, mode, p.since); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		d.fail(w, err)
		return
	}
	v := d.shell(r, s, "overview", "Overview")
	for i, p := range periods {
		pv := periodView{ID: p.id, Label: p.label}
		for _, t := range totals[i] {
			name, known := names[t.AppID]
			if !known {
				continue // defence in depth: only this org's apps are shown
			}
			pv.Rows = append(pv.Rows, totalRow{App: name, Currency: t.Currency, Gross: fmtMoney(t.Gross, t.Currency),
				Refunds: fmtMoney(t.Refunds, t.Currency), Net: fmtMoney(t.Gross-t.Refunds, t.Currency)})
		}
		v.Periods = append(v.Periods, pv)
	}
	d.render(w, http.StatusOK, "dash_overview", v)
}
