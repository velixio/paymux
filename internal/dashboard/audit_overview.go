package dashboard

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/velixio/paymux/internal/authz"
	"github.com/velixio/paymux/internal/store"
)

type auditFilter struct{ Actor, Action, From, To string }

type auditRow struct {
	ID, At, Actor, Action, Target, IP, Before, After string
}

func (d *Dashboard) routesAuditOverview() {
	d.mux.HandleFunc("GET "+prefix+"/audit", d.authed(authz.AuditRead, d.auditPage))
	d.mux.HandleFunc("GET "+prefix+"/overview", d.authed(authz.PaymentRead, d.overviewPage))
	d.mux.HandleFunc("GET "+prefix+"/overview/revenue.csv", d.authed(authz.PaymentRead, d.revenueCSV))
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
	org, err := d.orgID(ctx)
	if err != nil {
		d.fail(w, err)
		return
	}
	rev, bad, err := d.revenueData(ctx, r, org, mode)
	if err != nil {
		d.fail(w, err)
		return
	}
	if bad != "" {
		badRequest(w, bad)
		return
	}
	v := d.shell(r, s, "overview", "Overview")
	v.Revenue = rev
	analytics, analyticsBad, analyticsErr := d.analyticsData(ctx, r, org, mode)
	if analyticsErr != nil {
		d.fail(w, analyticsErr)
		return
	}
	if analyticsBad != "" {
		badRequest(w, analyticsBad)
		return
	}
	if len(analytics.CountryRows) > 5 {
		analytics.CountryRows = analytics.CountryRows[:5]
	}
	v.Analytics = &analytics
	if ents, err := d.listEntities(ctx); err == nil {
		v.Warnings = d.lutWarnings(ents)
	}
	d.render(w, http.StatusOK, "dash_overview", v)
}
