package dashboard

import (
	"encoding/json"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/audit"
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
		has(t, row, "60 minutes", "org")
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
		has(t, row, `value="90"`)

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

// routeForm builds the editor's form: rules is a list of rule field maps.
func routeForm(tok string, rules []map[string]string, catchAll map[string]string) url.Values {
	f := url.Values{"csrf": {tok}}
	for _, k := range []string{"r_order", "r_name", "r_currency", "r_country", "r_purpose", "r_amin", "r_amax", "r_meta", "r_action", "r_targets", "r_message"} {
		for _, r := range rules {
			v := r[k]
			if k == "r_action" && v == "" {
				v = "route"
			}
			f.Add(k, v)
		}
	}
	for k, v := range catchAll {
		f.Set(k, v)
	}
	if f.Get("ca_action") == "" {
		f.Set("ca_action", "route")
	}
	return f
}

func normJSON(t *testing.T, raw string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("bad json %s: %v", raw, err)
	}
	return v
}

func TestRoutingEditorReadsAndWritesCanonicalFormat(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		admin := e.roleClient("admin")
		body := e.pageOK(admin, "/dashboard/settings")
		tok := csrfOf(t, body)
		// the seeded defaults are shown as ordered rules with a catch-all
		has(t, body, `action="/dashboard/settings/routing"`, `value="inr-default"`, `value="bdt-default"`, `value="razorpay, zoho, payu"`,
			`value="razorpay, whop"`, "Catch-all")

		rules := []map[string]string{
			{"r_name": "inr-pinned", "r_currency": "inr", "r_country": "in", "r_purpose": "one_time", "r_amin": "1000", "r_amax": "5000000",
				"r_meta": "plan=pro", "r_targets": "account:pa_inr, razorpay"},
			{"r_name": "block-jpy", "r_currency": "JPY", "r_action": "reject", "r_message": "Yen not supported"},
			{}, {}, // blank rows are ignored
		}
		f := routeForm(tok, rules, map[string]string{"ca_targets": "razorpay, whop"})
		if r, b := e.post(admin, "/dashboard/settings/routing", f); r.StatusCode != 303 {
			t.Fatalf("save: %d\n%s", r.StatusCode, b)
		}
		v, _ := e.set.ResolveOrg(ctx, store.DefaultOrgID, "routing")
		want := `{"rules":[
			{"name":"inr-pinned","when":{"currency":["INR"],"country":["IN"],"purpose":["one_time"],"amount_min":1000,"amount_max":5000000,"metadata":{"plan":"pro"}},
			 "action":{"kind":"route","targets":[{"account_id":"pa_inr"},{"connector":"razorpay"}]}},
			{"name":"block-jpy","when":{"currency":["JPY"]},"action":{"kind":"reject","message":"Yen not supported"}}],
			"catch_all":{"name":"catch-all","when":{},"action":{"kind":"route","targets":[{"connector":"razorpay"},{"connector":"whop"}]}}}`
		if !reflect.DeepEqual(normJSON(t, string(v.Raw)), normJSON(t, want)) {
			t.Fatalf("stored routing\n got %s\nwant %s", v.Raw, want)
		}

		// reload: every field comes back, in order
		body = e.pageOK(admin, "/dashboard/settings")
		has(t, body, `value="inr-pinned"`, `value="INR"`, `value="IN"`, `value="one_time"`, `value="1000"`, `value="5000000"`,
			`value="plan=pro"`, `value="account:pa_inr, razorpay"`, `value="block-jpy"`, `value="Yen not supported"`)
		if strings.Index(body, "inr-pinned") > strings.Index(body, "block-jpy") {
			t.Error("rules are not shown in order")
		}
		// saving the page unchanged keeps the rules (including metadata)
		again := routeForm(tok, []map[string]string{
			{"r_name": "inr-pinned", "r_currency": "INR", "r_country": "IN", "r_purpose": "one_time", "r_amin": "1000", "r_amax": "5000000",
				"r_meta": "plan=pro", "r_targets": "account:pa_inr, razorpay"},
			{"r_name": "block-jpy", "r_currency": "JPY", "r_action": "reject", "r_message": "Yen not supported"}},
			map[string]string{"ca_targets": "razorpay, whop"})
		e.post(admin, "/dashboard/settings/routing", again)
		v2, _ := e.set.ResolveOrg(ctx, store.DefaultOrgID, "routing")
		if !reflect.DeepEqual(normJSON(t, string(v2.Raw)), normJSON(t, string(v.Raw))) {
			t.Fatalf("re-saving changed the setting: %s", v2.Raw)
		}

		// a catch-all that rejects, with a message
		f = routeForm(tok, rules[:1], map[string]string{"ca_action": "reject", "ca_message": "We cannot take this payment."})
		if r, _ := e.post(admin, "/dashboard/settings/routing", f); r.StatusCode != 303 {
			t.Fatalf("reject catch-all: %d", r.StatusCode)
		}
		v, _ = e.set.ResolveOrg(ctx, store.DefaultOrgID, "routing")
		has(t, string(v.Raw), `"kind":"reject"`, "We cannot take this payment.")
		has(t, e.pageOK(admin, "/dashboard/settings"), `value="We cannot take this payment."`)

		// rejections come from the settings validator
		for name, f := range map[string]url.Values{
			"route without targets": routeForm(tok, []map[string]string{{"r_name": "x", "r_currency": "INR"}}, map[string]string{"ca_targets": "razorpay"}),
			"catch-all no targets":  routeForm(tok, nil, nil),
			"bad amount":            routeForm(tok, []map[string]string{{"r_name": "x", "r_currency": "INR", "r_amin": "abc", "r_targets": "razorpay"}}, map[string]string{"ca_targets": "razorpay"}),
			"negative amount":       routeForm(tok, []map[string]string{{"r_name": "x", "r_currency": "INR", "r_amax": "-1", "r_targets": "razorpay"}}, map[string]string{"ca_targets": "razorpay"}),
			"bad metadata":          routeForm(tok, []map[string]string{{"r_name": "x", "r_meta": "novalue", "r_targets": "razorpay"}}, map[string]string{"ca_targets": "razorpay"}),
			"bad action":            routeForm(tok, []map[string]string{{"r_name": "x", "r_action": "explode", "r_targets": "razorpay"}}, map[string]string{"ca_targets": "razorpay"}),
			"empty account pin":     routeForm(tok, []map[string]string{{"r_name": "x", "r_targets": "account:"}}, map[string]string{"ca_targets": "razorpay"}),
		} {
			if r, _ := e.post(admin, "/dashboard/settings/routing", f); r.StatusCode != 400 {
				t.Errorf("%s: %d", name, r.StatusCode)
			}
		}
		if v3, _ := e.set.ResolveOrg(ctx, store.DefaultOrgID, "routing"); string(v3.Raw) != string(v.Raw) {
			t.Fatalf("a rejected save changed the setting")
		}

		viewer := e.roleClient("viewer")
		vb := e.pageOK(viewer, "/dashboard/settings")
		lacks(t, vb, `action="/dashboard/settings/routing"`)
		has(t, vb, "inr-pinned", "account:pa_inr, razorpay")
		vt := csrfOf(t, vb)
		if r, _ := e.post(viewer, "/dashboard/settings/routing", routeForm(vt, rules[:1], map[string]string{"ca_targets": "razorpay"})); r.StatusCode != 403 {
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
		try := func(q string) string { // just the result box, not the editor around it
			return regexpFind(t, `(?s)(<div class="route-result">.*?</div>)`, e.pageOK(viewer, "/dashboard/settings?test=1&"+q))
		}
		// a currency with its own rule (the seeded defaults)
		body := try("mode=live&currency=INR&country=IN&amount=100000&purpose=one_time")
		has(t, body, "Matched rule", "inr-default", "Razorpay INR")
		lacks(t, body, "Whop USD", "Razorpay test")
		// falls through to the catch-all (razorpay cannot take USD, whop can)
		body = try("mode=live&currency=USD&amount=500&purpose=one_time")
		has(t, body, "Matched rule", "catch-all", "Whop USD")
		lacks(t, body, "Razorpay INR")
		// the test-mode accounts are separate
		body = try("mode=test&currency=INR&amount=100&purpose=one_time")
		has(t, body, "inr-default", "Razorpay test")
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
		has(t, body, "Whop USD", "catch-all")

		// an app-level override puts its rules ahead of the org rules, exactly as at runtime
		admin := e.roleClient("admin")
		tok := csrfOf(t, e.pageOK(admin, "/dashboard/apps/"+app.ID+"?tab=settings"))
		override := `{"rules":[{"name":"usd-pinned","when":{"currency":["USD"]},"action":{"kind":"route","targets":[{"account_id":"pa_usd"}]}}]}`
		if r, _ := e.post(admin, "/dashboard/apps/"+app.ID+"/settings/routing", url.Values{"value": {override}, "csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("app routing override: %d", r.StatusCode)
		}
		body = try("app=" + app.ID + "&mode=live&currency=USD&amount=100&purpose=one_time")
		has(t, body, "usd-pinned", "Whop USD")
		lacks(t, body, "catch-all")
		// the same request for the org (no app) still follows the org rules
		body = try("mode=live&currency=USD&amount=100&purpose=one_time")
		has(t, body, "catch-all")

		// a rejecting catch-all with its message
		reject := `{"rules":[],"catch_all":{"name":"catch-all","when":{},"action":{"kind":"reject","message":"Only INR and USD, sorry."}}}`
		if err := e.set.SetOrg(ctx, audit.System, store.DefaultOrgID, "routing", json.RawMessage(reject)); err != nil {
			t.Fatal(err)
		}
		body = try("mode=live&currency=EUR&amount=100&purpose=one_time")
		has(t, body, "Rejected", "Only INR and USD, sorry.")

		// input validation
		for _, q := range []string{"currency=us", "currency=USD&amount=abc", "currency=USD&amount=-5", "currency=USD&mode=staging", "currency=USD&purpose=barter", "currency=USD&app=app_nope"} {
			if r, _ := e.get(viewer, "/dashboard/settings?test=1&"+q); r.StatusCode != 400 {
				t.Errorf("%s: %d", q, r.StatusCode)
			}
		}
	})
}

func TestRoutingEditorRulesAreCardsWithOrderNumbers(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		admin := e.roleClient("admin")
		body := e.pageOK(admin, "/dashboard/settings")
		tok := csrfOf(t, body)
		// each rule is a labelled block with an order number, not a table row
		cards := strings.Count(body, `class="rule-card"`)
		if cards < 4 { // two seeded rules, the catch-all and blank rows
			t.Fatalf("rule cards: %d", cards)
		}
		has(t, body, `name="r_order"`, `value="1"`, `value="2"`, `<label>Name`, `<label>Targets`, `<label>Currencies`, `class="rule-card catch-all"`)
		lacks(t, regexpFind(t, `(?s)(<form[^>]*routing-editor.*?</form>)`, body), "<table")

		order := func(a, b string) []string {
			f := routeForm(tok, []map[string]string{
				{"r_order": a, "r_name": "first-written", "r_currency": "INR", "r_targets": "razorpay"},
				{"r_order": b, "r_name": "second-written", "r_currency": "BDT", "r_targets": "whop"}}, map[string]string{"ca_targets": "razorpay"})
			if r, out := e.post(admin, "/dashboard/settings/routing", f); r.StatusCode != 303 {
				t.Fatalf("save %s/%s: %d\n%s", a, b, r.StatusCode, out)
			}
			v, _ := e.set.ResolveOrg(ctx, store.DefaultOrgID, "routing")
			var doc struct {
				Rules []struct{ Name string } `json:"rules"`
			}
			_ = json.Unmarshal(v.Raw, &doc)
			var names []string
			for _, r := range doc.Rules {
				names = append(names, r.Name)
			}
			return names
		}
		if got := order("1", "2"); strings.Join(got, ",") != "first-written,second-written" {
			t.Errorf("1,2 -> %v", got)
		}
		if got := order("2", "1"); strings.Join(got, ",") != "second-written,first-written" {
			t.Errorf("2,1 -> %v", got)
		}
		if got := order("", ""); strings.Join(got, ",") != "first-written,second-written" {
			t.Errorf("blank orders keep the written order: %v", got)
		}
		if got := order("5", "5"); strings.Join(got, ",") != "first-written,second-written" {
			t.Errorf("equal orders keep the written order: %v", got)
		}
		f := routeForm(tok, []map[string]string{{"r_order": "first", "r_name": "x", "r_currency": "INR", "r_targets": "razorpay"}}, map[string]string{"ca_targets": "razorpay"})
		if r, _ := e.post(admin, "/dashboard/settings/routing", f); r.StatusCode != 400 {
			t.Errorf("non-numeric order: %d", r.StatusCode)
		}
		// the page numbers the stored rules 1..n, in order
		page := e.pageOK(admin, "/dashboard/settings")
		if strings.Index(page, `value="second-written"`) > strings.Index(page, `value="first-written"`) && strings.Contains(page, "first-written") {
			t.Log("rules shown in stored order")
		}
	})
}
