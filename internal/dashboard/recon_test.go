package dashboard

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/recon"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

type reconWorld struct {
	acct1, acct2 store.ProviderAccount
	run1, run2   store.ReconRun
	i1, i2, i3   store.ReconItem // acct1: open status, open amount, resolved unknown
	i4           store.ReconItem // acct2: open unknown
}

func (e *env) reconWorld() reconWorld {
	e.t.Helper()
	alpha, beta := e.seedApp("alpha"), e.seedApp("beta")
	acct1, _ := e.disputeAccount(alpha, true)
	acct2, _ := e.disputeAccount(beta, true)
	now := e.clk.Now()
	w := reconWorld{acct1: acct1, acct2: acct2}
	w.run1 = store.ReconRun{ID: "rcr_1", OrgID: store.DefaultOrgID, ProviderAccountID: acct1.ID, WindowFrom: now.Add(-72 * time.Hour), WindowTo: now,
		StartedAt: now.Add(-time.Hour), FinishedAt: now.Add(-59 * time.Minute), Status: "ok", Checked: 40, Items: 2, Fixed: 1}
	w.run2 = store.ReconRun{ID: "rcr_2", OrgID: store.DefaultOrgID, ProviderAccountID: acct2.ID, WindowFrom: now.Add(-72 * time.Hour), WindowTo: now,
		StartedAt: now.Add(-30 * time.Minute), FinishedAt: now.Add(-29 * time.Minute), Status: "failed", Error: "provider timed out"}
	item := func(id, run, acct, kind, provider, st string) store.ReconItem {
		return store.ReconItem{ID: id, RunID: run, OrgID: store.DefaultOrgID, ProviderAccountID: acct, Kind: kind, ProviderID: provider,
			Ours: `{"status":"pending"}`, Theirs: `{"status":"captured"}`, State: st, CreatedAt: now.Add(-50 * time.Minute)}
	}
	w.i1 = item("rci_1", "rcr_1", acct1.ID, recon.KindStatusMismatch, "pay_prov_1", "open")
	w.i2 = item("rci_2", "rcr_1", acct1.ID, recon.KindAmountMismatch, "pay_prov_2", "open")
	w.i3 = item("rci_3", "rcr_1", acct1.ID, recon.KindUnknownProviderObject, "pay_prov_3", "resolved")
	w.i4 = item("rci_4", "rcr_2", acct2.ID, recon.KindUnknownProviderObject, "pay_prov_4", "open")
	err := e.db.Write(ctx, func(tx store.Tx) error {
		for _, r := range []store.ReconRun{w.run1, w.run2} {
			if err := store.InsertReconRun(ctx, tx, r); err != nil {
				return err
			}
			if err := store.FinishReconRun(ctx, tx, r); err != nil {
				return err
			}
		}
		for _, i := range []store.ReconItem{w.i1, w.i2, w.i3, w.i4} {
			if err := store.InsertReconItem(ctx, tx, i); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return w
}

func TestReconciliationPageSummaryRunsAndItems(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		w := e.reconWorld()
		e.otherOrg()
		err := db.Write(ctx, func(tx store.Tx) error {
			return store.InsertReconItem(ctx, tx, store.ReconItem{ID: "rci_foreign", RunID: "rcr_f", OrgID: "org_other", ProviderAccountID: "pa_f", Kind: recon.KindStatusMismatch,
				ProviderID: "pay_foreign_prov", Theirs: "{}", State: "open", CreatedAt: e.clk.Now()})
		})
		if err != nil {
			t.Fatal(err)
		}
		c := e.roleClient("viewer")
		body := e.pageOK(c, "/dashboard/reconciliation")

		row := func(id string) string { return regexpFind(t, `(?s)(<tr id="acct-`+id+`".*?</tr>)`, body) }
		has(t, row(w.acct1.ID), w.acct1.Name, "ok", ">2<") // 2 open items
		has(t, row(w.acct2.ID), w.acct2.Name, "failed", "provider timed out", ">1<")
		has(t, body, w.run1.ID, w.run2.ID, ">40<")
		lacks(t, body, "rci_foreign", "pay_foreign_prov")

		// default: open items only
		has(t, body, w.i1.ProviderID, w.i2.ProviderID, w.i4.ProviderID, "status_mismatch", "amount_mismatch")
		lacks(t, body, w.i3.ProviderID)

		for name, c2 := range map[string]struct {
			q         string
			want, not []store.ReconItem
		}{
			"resolved": {"?state=resolved", []store.ReconItem{w.i3}, []store.ReconItem{w.i1, w.i2, w.i4}},
			"all":      {"?state=all", []store.ReconItem{w.i1, w.i2, w.i3, w.i4}, nil},
			"kind":     {"?kind=" + recon.KindStatusMismatch, []store.ReconItem{w.i1}, []store.ReconItem{w.i2, w.i4}},
			"account":  {"?account=" + w.acct1.ID, []store.ReconItem{w.i1, w.i2}, []store.ReconItem{w.i4, w.i3}},
		} {
			b := e.pageOK(c, "/dashboard/reconciliation"+c2.q)
			for _, i := range c2.want {
				if !strings.Contains(b, i.ProviderID) {
					t.Errorf("%s: missing %s", name, i.ID)
				}
			}
			for _, i := range c2.not {
				if strings.Contains(b, i.ProviderID) {
					t.Errorf("%s: unexpected %s", name, i.ID)
				}
			}
		}
		for _, q := range []string{"?state=bogus", "?kind=bogus", "?account=pa_nope"} {
			if r, _ := e.get(c, "/dashboard/reconciliation"+q); r.StatusCode != 400 {
				t.Errorf("%s: %d", q, r.StatusCode)
			}
		}
		// read-only users get no buttons
		lacks(t, body, `action="/dashboard/reconciliation/run"`, `/resolve"`)
	})
}

func TestResolveReconItemWithNote(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		w := e.reconWorld()
		e.otherOrg()
		_ = db.Write(ctx, func(tx store.Tx) error {
			return store.InsertReconItem(ctx, tx, store.ReconItem{ID: "rci_foreign", RunID: "rcr_f", OrgID: "org_other", ProviderAccountID: "pa_f", Kind: recon.KindStatusMismatch,
				ProviderID: "pay_foreign_prov", Theirs: "{}", State: "open", CreatedAt: e.clk.Now()})
		})
		op := e.roleClient("operator")
		body := e.pageOK(op, "/dashboard/reconciliation")
		has(t, body, `action="/dashboard/reconciliation/items/`+w.i1.ID+`/resolve"`, `name="note"`)
		tok := csrfOf(t, body)
		path := "/dashboard/reconciliation/items/" + w.i1.ID + "/resolve"

		if r, _ := e.post(op, path, url.Values{"note": {"  "}, "csrf": {tok}}); r.StatusCode != 400 {
			t.Fatalf("blank note: %d", r.StatusCode)
		}
		if r, _ := e.post(op, path, url.Values{"note": {"Provider fixed it"}}); r.StatusCode != 403 {
			t.Fatalf("no csrf: %d", r.StatusCode)
		}
		if r, b := e.post(op, path, url.Values{"note": {"Checked with Razorpay support, ticket 4411"}, "csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("resolve: %d\n%s", r.StatusCode, b)
		}
		var it store.ReconItem
		_ = db.Read(ctx, func(q store.Querier) (err error) { it, err = store.GetReconItem(ctx, q, w.i1.ID); return })
		if it.State != "resolved" || it.Note != "Checked with Razorpay support, ticket 4411" || it.ResolvedAt.IsZero() {
			t.Fatalf("item %+v", it)
		}
		lacks(t, e.pageOK(op, "/dashboard/reconciliation"), w.i1.ProviderID)
		has(t, e.pageOK(op, "/dashboard/reconciliation?state=resolved"), w.i1.ProviderID, "ticket 4411")
		var aud []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, "recon.resolve", 5)
			return
		})
		if len(aud) != 1 || aud[0].ActorType != "user" || aud[0].ActorID != "usr_operator" || aud[0].Target != w.i1.ID {
			t.Fatalf("audit %+v", aud)
		}
		for name, id := range map[string]string{"unknown": "rci_nope", "another org's": "rci_foreign"} {
			if r, _ := e.post(op, "/dashboard/reconciliation/items/"+id+"/resolve", url.Values{"note": {"x"}, "csrf": {tok}}); r.StatusCode != 404 {
				t.Errorf("%s item: %d", name, r.StatusCode)
			}
		}
		viewer := e.roleClient("viewer")
		if r, _ := e.post(viewer, "/dashboard/reconciliation/items/"+w.i2.ID+"/resolve", url.Values{"note": {"x"}, "csrf": {csrfOf(t, e.pageOK(viewer, "/dashboard/reconciliation"))}}); r.StatusCode != 403 {
			t.Errorf("viewer: %d", r.StatusCode)
		}
	})
}

func TestRunReconciliationNow(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		w := e.reconWorld()
		e.otherOrg()
		_ = db.Write(ctx, func(tx store.Tx) error {
			return store.InsertProviderAccountInfo(ctx, tx, store.ProviderAccountInfo{ID: "pa_foreign", OrgID: "org_other", Mode: "live", Connector: "x", Name: "F", Status: "active"}, e.clk.Now())
		})
		op := e.roleClient("operator")
		body := e.pageOK(op, "/dashboard/reconciliation")
		has(t, body, `action="/dashboard/reconciliation/run"`, `value="`+w.acct1.ID+`"`)
		tok := csrfOf(t, body)
		jobsOf := func() int {
			return e.count(`SELECT COUNT(*) FROM jobs WHERE kind = 'recon.account' AND state = 'pending'`)
		}
		runsOf := func(acct string) int {
			return e.count(`SELECT COUNT(*) FROM recon_runs WHERE provider_account_id = ?`, acct)
		}

		if r, _ := e.post(op, "/dashboard/reconciliation/run", url.Values{"account_id": {w.acct1.ID}, "csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("run now: %d", r.StatusCode)
		}
		// a second click while one is pending queues nothing more
		e.post(op, "/dashboard/reconciliation/run", url.Values{"account_id": {w.acct1.ID}, "csrf": {tok}})
		if jobsOf() != 1 {
			t.Fatalf("pending recon jobs: %d", jobsOf())
		}
		before := runsOf(w.acct1.ID)
		e.runJobs()
		if runsOf(w.acct1.ID) != before+1 {
			t.Fatalf("the job did not reconcile the account (runs %d -> %d)", before, runsOf(w.acct1.ID))
		}
		if runsOf(w.acct2.ID) != 1 {
			t.Fatal("another account was reconciled")
		}
		var aud []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, "recon.run", 5)
			return
		})
		if len(aud) == 0 || aud[0].ActorID != "usr_operator" || aud[0].Target != w.acct1.ID {
			t.Fatalf("audit %+v", aud)
		}
		for name, id := range map[string]string{"unknown": "pa_nope", "another org's": "pa_foreign"} {
			if r, _ := e.post(op, "/dashboard/reconciliation/run", url.Values{"account_id": {id}, "csrf": {tok}}); r.StatusCode != 404 {
				t.Errorf("%s account: %d", name, r.StatusCode)
			}
		}
		viewer := e.roleClient("viewer")
		if r, _ := e.post(viewer, "/dashboard/reconciliation/run", url.Values{"account_id": {w.acct1.ID}, "csrf": {csrfOf(t, e.pageOK(viewer, "/dashboard/reconciliation"))}}); r.StatusCode != 403 {
			t.Errorf("viewer: %d", r.StatusCode)
		}
	})
}

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.Read(ctx, func(q store.Querier) error { return q.QueryRowContext(ctx, query, args...).Scan(&n) }); err != nil {
		e.t.Fatal(err)
	}
	return n
}
