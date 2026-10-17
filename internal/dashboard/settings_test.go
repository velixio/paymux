package dashboard

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

func (e *env) seedAccounts() {
	e.t.Helper()
	err := e.db.Write(ctx, func(tx store.Tx) error {
		for _, pa := range []store.ProviderAccountInfo{
			{ID: "pa_inr", OrgID: store.DefaultOrgID, Mode: "live", Connector: "razorpay", Name: "Razorpay INR", Status: "active", Currencies: []string{"INR"}, Purposes: []string{"one_time"}},
			{ID: "pa_usd", OrgID: store.DefaultOrgID, Mode: "live", Connector: "whop", Name: "Whop USD", Status: "active", Currencies: []string{"USD"}, Purposes: []string{"one_time"}},
			{ID: "pa_test", OrgID: store.DefaultOrgID, Mode: "test", Connector: "razorpay", Name: "Razorpay test", Status: "active", Currencies: []string{"INR"}, Purposes: []string{"one_time"}},
		} {
			if err := store.InsertProviderAccountInfo(ctx, tx, pa, e.clk.Now()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func TestOrgSettingsPage(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		viewer := e.roleClient("viewer")
		body := e.pageOK(viewer, "/dashboard/settings")
		has(t, body, "payment_ttl", "mandate_afa_limit", "recon_time", "webhook_retry_schedule", "duplicate_payment_action")
		lacks(t, body, `id="setting-issue_invoices"`, `id="setting-sac_code"`) // app-only keys
		row := regexpFind(t, `(?s)(<tr[^>]*id="setting-payment_ttl".*?</tr>)`, body)
		has(t, row, ">60<", "org")
		lacks(t, body, `action="/dashboard/settings/payment_ttl"`)
		vt := csrfOf(t, body)
		if r, _ := e.post(viewer, "/dashboard/settings/payment_ttl", url.Values{"value": {"90"}, "csrf": {vt}}); r.StatusCode != 403 {
			t.Fatalf("viewer write: %d", r.StatusCode)
		}

		admin := e.roleClient("admin")
		body = e.pageOK(admin, "/dashboard/settings")
		has(t, body, `action="/dashboard/settings/payment_ttl"`)
		tok := csrfOf(t, body)
		if r, _ := e.post(admin, "/dashboard/settings/payment_ttl", url.Values{"value": {"90"}, "csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("write: %d", r.StatusCode)
		}
		v, _ := e.set.ResolveOrg(ctx, store.DefaultOrgID, "payment_ttl")
		if string(v.Raw) != "90" {
			t.Fatalf("value %s", v.Raw)
		}
		row = regexpFind(t, `(?s)(<tr[^>]*id="setting-payment_ttl".*?</tr>)`, e.pageOK(admin, "/dashboard/settings"))
		has(t, row, ">90<")

		for name, c := range map[string]struct{ key, val string }{
			"out of range":    {"payment_ttl", "1"},
			"not json":        {"payment_ttl", "ninety"},
			"app-only key":    {"sac_code", `"998315"`},
			"unknown":         {"nope", "1"},
			"bad clock":       {"recon_time", `"25:00"`},
			"retry schedule":  {"webhook_retry_schedule", `[0,"x"]`},
			"routing via raw": {"routing", `{"rules":[]}`},
		} {
			r, body := e.post(admin, "/dashboard/settings/"+c.key, url.Values{"value": {c.val}, "csrf": {tok}})
			if r.StatusCode != 400 {
				t.Errorf("%s: %d", name, r.StatusCode)
			}
			if name == "out of range" && !strings.Contains(body, "payment_ttl") {
				t.Errorf("error does not name the setting")
			}
		}
		if v, _ = e.set.ResolveOrg(ctx, store.DefaultOrgID, "payment_ttl"); string(v.Raw) != "90" {
			t.Fatalf("an invalid write changed the value: %s", v.Raw)
		}
		var aud []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, "setting.update", 10)
			return
		})
		if len(aud) != 1 || aud[0].ActorID != "usr_admin" || !strings.HasSuffix(aud[0].Target, "/payment_ttl") {
			t.Errorf("audit %+v", aud)
		}
	})
}

func TestRoutingEditor(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		admin := e.roleClient("admin")
		body := e.pageOK(admin, "/dashboard/settings")
		tok := csrfOf(t, body)
		// shows the current rules (seeded defaults) and a catch-all
		has(t, body, `action="/dashboard/settings/routing"`, "INR", "razorpay, zoho, payu", "BDT", "Catch-all")

		form := url.Values{"currency": {"inr", "BDT", "", ""}, "connectors": {"razorpay", "whop, razorpay", "", ""},
			"catch_all": {"razorpay"}, "csrf": {tok}}
		if r, b := e.post(admin, "/dashboard/settings/routing", form); r.StatusCode != 303 {
			t.Fatalf("save: %d\n%s", r.StatusCode, b)
		}
		v, _ := e.set.ResolveOrg(ctx, store.DefaultOrgID, "routing")
		var got map[string]any
		_ = json.Unmarshal(v.Raw, &got)
		want := `{"catch_all":{"connectors":["razorpay"],"reject":false},"rules":[{"connectors":["razorpay"],"currency":"INR"},{"connectors":["whop","razorpay"],"currency":"BDT"}]}`
		gotJSON, _ := json.Marshal(got)
		if string(gotJSON) != want {
			t.Fatalf("stored routing\n got %s\nwant %s", gotJSON, want)
		}

		// catch-all reject
		form = url.Values{"currency": {"INR"}, "connectors": {"razorpay"}, "catch_all_reject": {"1"}, "csrf": {tok}}
		if r, _ := e.post(admin, "/dashboard/settings/routing", form); r.StatusCode != 303 {
			t.Fatalf("reject catch-all: %d", r.StatusCode)
		}
		v, _ = e.set.ResolveOrg(ctx, store.DefaultOrgID, "routing")
		if !strings.Contains(string(v.Raw), `"reject":true`) {
			t.Fatalf("stored %s", v.Raw)
		}
		has(t, e.pageOK(admin, "/dashboard/settings"), "reject")

		for name, f := range map[string]url.Values{
			"bad currency":         {"currency": {"RUPEE"}, "connectors": {"razorpay"}, "catch_all": {"razorpay"}},
			"rule without targets": {"currency": {"INR"}, "connectors": {""}, "catch_all": {"razorpay"}},
			"no catch-all":         {"currency": {"INR"}, "connectors": {"razorpay"}},
		} {
			f.Set("csrf", tok)
			if r, _ := e.post(admin, "/dashboard/settings/routing", f); r.StatusCode != 400 {
				t.Errorf("%s: %d", name, r.StatusCode)
			}
		}
		viewer := e.roleClient("viewer")
		vt := csrfOf(t, e.pageOK(viewer, "/dashboard/settings"))
		lacks(t, e.pageOK(viewer, "/dashboard/settings"), `action="/dashboard/settings/routing"`)
		if r, _ := e.post(viewer, "/dashboard/settings/routing", url.Values{"currency": {"INR"}, "connectors": {"razorpay"}, "catch_all": {"razorpay"}, "csrf": {vt}}); r.StatusCode != 403 {
			t.Errorf("viewer: %d", r.StatusCode)
		}
	})
}

func TestRoutingTestBox(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.seedAccounts()
		app := e.seedApp("alpha")
		viewer := e.roleClient("viewer")
		try := func(q string) string {
			return e.pageOK(viewer, "/dashboard/settings?test=1&"+q)
		}
		// a currency with its own rule
		body := try("mode=live&currency=INR&country=IN&amount=100000&purpose=one_time")
		has(t, body, "Matched rule", "rule:INR", "Razorpay INR")
		lacks(t, body, "Whop USD", "Razorpay test")
		// falls through to the catch-all (razorpay cannot take USD, whop can)
		body = try("mode=live&currency=USD&amount=500&purpose=one_time")
		has(t, body, "Matched rule", "catch_all", "Whop USD")
		lacks(t, body, "Razorpay INR")
		// the test-mode accounts are separate
		body = try("mode=test&currency=INR&amount=100&purpose=one_time")
		has(t, body, "rule:INR", "Razorpay test")
		lacks(t, body, "Razorpay INR")
		// nothing can take it
		body = try("mode=live&currency=JPY&amount=100&purpose=one_time")
		has(t, body, "No route")
		lacks(t, body, "Matched rule")
		// an app only sees its allowed accounts
		err := db.Write(ctx, func(tx store.Tx) error { return store.SetAppProviderAccounts(ctx, tx, app.ID, []string{"pa_usd"}) })
		if err != nil {
			t.Fatal(err)
		}
		body = try("app=" + app.ID + "&mode=live&currency=INR&amount=100&purpose=one_time")
		has(t, body, "No route")
		body = try("app=" + app.ID + "&mode=live&currency=USD&amount=100&purpose=one_time")
		has(t, body, "Whop USD", "catch_all")
		// an app-level routing override replaces the org rules for that app
		admin := e.roleClient("admin")
		tok := csrfOf(t, e.pageOK(admin, "/dashboard/apps/"+app.ID+"?tab=settings"))
		override := `{"rules":[{"currency":"USD","connectors":["whop"]}],"catch_all":{"reject":true}}`
		if r, _ := e.post(admin, "/dashboard/apps/"+app.ID+"/settings/routing", url.Values{"value": {override}, "csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("app routing override: %d", r.StatusCode)
		}
		body = try("app=" + app.ID + "&mode=live&currency=USD&amount=100&purpose=one_time")
		has(t, body, "rule:USD", "Whop USD")
		body = try("app=" + app.ID + "&mode=live&currency=EUR&amount=100&purpose=one_time")
		has(t, body, "Rejected")

		// input validation
		for _, q := range []string{"currency=us", "currency=USD&amount=abc", "currency=USD&amount=-5", "currency=USD&mode=staging", "currency=USD&purpose=barter", "currency=USD&app=app_nope"} {
			if r, _ := e.get(viewer, "/dashboard/settings?test=1&"+q); r.StatusCode != 400 {
				t.Errorf("%s: %d", q, r.StatusCode)
			}
		}
	})
}
