package dashboard

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/alertcenter"
	"github.com/velixio/paymux/internal/notify"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

func (e *env) raise(sev notify.Severity, kind, msg string, refs map[string]string) {
	e.t.Helper()
	if err := e.alerts.Record(ctx, notify.Alert{Severity: sev, Kind: kind, Message: msg, Refs: refs}, e.clk.Now()); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) openAlertID(kind string) string {
	e.t.Helper()
	as, err := e.alerts.List(ctx, store.DefaultOrgID, stateFilter("open"))
	if err != nil {
		e.t.Fatal(err)
	}
	for _, a := range as {
		if a.Kind == kind {
			return a.ID
		}
	}
	e.t.Fatalf("no open alert of kind %s", kind)
	return ""
}

func TestNavShowsTheCountOfOpenAlerts(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		viewer := e.roleClient("viewer")
		body := e.pageOK(viewer, "/dashboard/overview")
		has(t, body, `href="/dashboard/alerts"`, "Alerts")
		lacks(t, body, `class="badge`)

		e.raise(notify.SeverityWarning, "domain.failed", "pay.shop.example.org failed", map[string]string{"domain": "d1"})
		e.raise(notify.SeverityWarning, "domain.failed", "pay.shop.example.org failed", map[string]string{"domain": "d1"}) // a repeat: still one alert
		body = e.pageOK(viewer, "/dashboard/overview")
		has(t, body, `<span class="badge">1</span>`)
		lacks(t, body, "badge-critical")

		e.raise(notify.SeverityCritical, "recon.failed", "Reconciliation failed", map[string]string{"provider_account": "pa_1"})
		for _, page := range []string{"/dashboard/overview", "/dashboard/payments", "/dashboard/alerts", "/dashboard/settings"} {
			has(t, e.pageOK(viewer, page), `<span class="badge badge-critical">2</span>`)
		}
		// Another org's alerts are not counted.
		e.otherOrg()
		if err := db.Write(ctx, func(tx store.Tx) error {
			_, err := store.RecordAlert(ctx, tx, store.Alert{ID: "alt_foreign", OrgID: "org_other", Severity: "critical", Kind: "x", Message: "foreign", Fingerprint: "fp_foreign"}, e.clk.Now())
			return err
		}); err != nil {
			t.Fatal(err)
		}
		has(t, e.pageOK(viewer, "/dashboard/overview"), `<span class="badge badge-critical">2</span>`)
	})
}

func TestAlertsPageListsFiltersAndExplains(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.raise(notify.SeverityWarning, "domain.failed", "pay.shop.example.org failed its checks", map[string]string{"domain": "d1"})
		e.raise(notify.SeverityCritical, "recon.failed", "Reconciliation of razorpay failed", map[string]string{"provider_account": "pa_1", "run": "rcr_1"})
		e.raise(notify.SeverityCritical, "recon.failed", "Reconciliation of razorpay failed", map[string]string{"provider_account": "pa_1", "run": "rcr_1"})
		e.raise(notify.SeverityInfo, "hook.failed", "OnPaymentSucceeded failed", map[string]string{"hook": "OnPaymentSucceeded"})
		op := e.roleClient("operator")
		if r, _ := e.post(op, "/dashboard/alerts/"+e.openAlertID("hook.failed")+"/acknowledge", url.Values{"csrf": {csrfOf(t, e.pageOK(op, "/dashboard/alerts"))}}); r.StatusCode != 303 {
			t.Fatalf("ack: %d", r.StatusCode)
		}

		viewer := e.roleClient("viewer")
		body := e.pageOK(viewer, "/dashboard/alerts")
		has(t, body, "pay.shop.example.org failed its checks", "Reconciliation of razorpay failed", "domain.failed", "recon.failed", "critical", "warning", "pa_1", "rcr_1", "domain=d1")
		lacks(t, body, "OnPaymentSucceeded failed", `/acknowledge"`) // acknowledged ones are hidden by default; viewers cannot acknowledge
		// the repeated one shows its count
		row := func(b, kind string) string {
			for _, seg := range strings.Split(b, `<tr id="alert-`)[1:] {
				if strings.Contains(seg, kind) {
					return seg
				}
			}
			t.Fatalf("no row for %s", kind)
			return ""
		}
		has(t, row(body, "recon.failed"), "2 times")
		table := func(b string) string { return b[strings.Index(b, "<tbody>"):] } // the filter lists every kind; the table only the matches

		has(t, e.pageOK(viewer, "/dashboard/alerts?state=acknowledged"), "OnPaymentSucceeded failed", "usr_operator")
		all := e.pageOK(viewer, "/dashboard/alerts?state=all")
		has(t, all, "OnPaymentSucceeded failed", "Reconciliation of razorpay failed")
		onlyCritical := e.pageOK(viewer, "/dashboard/alerts?severity=critical")
		has(t, table(onlyCritical), "recon.failed")
		lacks(t, table(onlyCritical), "domain.failed")
		onlyKind := e.pageOK(viewer, "/dashboard/alerts?kind=domain.failed")
		has(t, table(onlyKind), "domain.failed")
		lacks(t, table(onlyKind), "recon.failed")
		// the filter offers the kinds that exist
		has(t, body, `<option value="domain.failed"`, `<option value="hook.failed"`)
		for _, q := range []string{"?state=bogus", "?severity=loud"} {
			if r, _ := e.get(viewer, "/dashboard/alerts"+q); r.StatusCode != 400 {
				t.Errorf("%s: %d", q, r.StatusCode)
			}
		}
		fresh := newEnvOnlyViewer(t, e)
		has(t, fresh, "No alerts")
	})
}

// newEnvOnlyViewer returns the alerts page of a hub without open alerts of a kind that does not exist.
func newEnvOnlyViewer(t *testing.T, e *env) string {
	t.Helper()
	return e.pageOK(e.roleClient("admin"), "/dashboard/alerts?kind=nothing.like.this")
}

func TestOperatorAcknowledgesAnAlert(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.raise(notify.SeverityCritical, "recon.failed", "Reconciliation failed", map[string]string{"provider_account": "pa_1"})
		id := e.openAlertID("recon.failed")
		op, viewer := e.roleClient("operator"), e.roleClient("viewer")

		body := e.pageOK(op, "/dashboard/alerts")
		has(t, body, `action="/dashboard/alerts/`+id+`/acknowledge"`)
		tok := csrfOf(t, body)

		vt := csrfOf(t, e.pageOK(viewer, "/dashboard/alerts"))
		if r, _ := e.post(viewer, "/dashboard/alerts/"+id+"/acknowledge", url.Values{"csrf": {vt}}); r.StatusCode != 403 {
			t.Errorf("viewer: %d", r.StatusCode)
		}
		if r, _ := e.post(op, "/dashboard/alerts/"+id+"/acknowledge", url.Values{}); r.StatusCode != 403 {
			t.Errorf("no csrf: %d", r.StatusCode)
		}
		if r, _ := e.post(op, "/dashboard/alerts/alt_nope/acknowledge", url.Values{"csrf": {tok}}); r.StatusCode != 404 {
			t.Errorf("unknown: %d", r.StatusCode)
		}
		e.otherOrg()
		_ = db.Write(ctx, func(tx store.Tx) error {
			_, err := store.RecordAlert(ctx, tx, store.Alert{ID: "alt_foreign", OrgID: "org_other", Severity: "warning", Kind: "x", Message: "foreign", Fingerprint: "fp_foreign"}, e.clk.Now())
			return err
		})
		if r, _ := e.post(op, "/dashboard/alerts/alt_foreign/acknowledge", url.Values{"csrf": {tok}}); r.StatusCode != 404 {
			t.Errorf("another org's alert: %d", r.StatusCode)
		}

		r, _ := e.post(op, "/dashboard/alerts/"+id+"/acknowledge", url.Values{"csrf": {tok}})
		if r.StatusCode != 303 || r.Header.Get("Location") != "/dashboard/alerts" {
			t.Fatalf("acknowledge: %d %q", r.StatusCode, r.Header.Get("Location"))
		}
		after := e.pageOK(op, "/dashboard/alerts")
		lacks(t, after, "Reconciliation failed", `class="badge`)
		has(t, e.pageOK(op, "/dashboard/alerts?state=acknowledged"), "Reconciliation failed", "usr_operator")
		var aud []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, "alert.acknowledge", 5)
			return
		})
		if len(aud) != 1 || aud[0].ActorID != "usr_operator" || aud[0].Target != id {
			t.Errorf("audit %+v", aud)
		}
		if r, _ := e.post(op, "/dashboard/alerts/"+id+"/acknowledge", url.Values{"csrf": {tok}}); r.StatusCode != 303 {
			t.Errorf("acknowledging twice: %d", r.StatusCode)
		}
		// The condition returns: a new open alert, and the count is back.
		e.clk.Add(time.Hour)
		e.raise(notify.SeverityCritical, "recon.failed", "Reconciliation failed", map[string]string{"provider_account": "pa_1"})
		has(t, e.pageOK(op, "/dashboard/overview"), `<span class="badge badge-critical">1</span>`)
	})
}

func TestEveryRaiseThroughNotifyShowsUp(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		raiser := notify.NewAlerts(notify.AlertsOptions{Emails: []string{"ops@example.com"}, Mailer: e.mail, Now: e.clk.Now, Recorder: e.alerts})
		if err := raiser.Raise(ctx, notify.Alert{Severity: notify.SeverityCritical, Kind: "provider.silent", Message: "No verified webhooks from pa_1 in 6h",
			Refs: map[string]string{"provider_account": "pa_1"}}); err != nil {
			t.Fatal(err)
		}
		has(t, e.pageOK(e.roleClient("viewer"), "/dashboard/alerts"), "No verified webhooks from pa_1 in 6h", "provider.silent")
		if e.mail.count() != 1 {
			t.Errorf("email delivery is unchanged: %d", e.mail.count())
		}
	})
}

func stateFilter(state string) alertcenter.Filter { return alertcenter.Filter{State: state} }
