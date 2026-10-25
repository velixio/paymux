package dashboard

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/connectors"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/fakeconn"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/pkg/connector"
)

const providerSchema = `{"type":"object","required":["key_id","key_secret"],"additionalProperties":false,"properties":{
"key_id":{"type":"string","title":"Key id","minLength":1},
"key_secret":{"type":"string","title":"Key secret","minLength":1,"x-secret":true},
"region":{"type":"string","title":"Region","enum":["in","sg"]},
"test_mode":{"type":"boolean","title":"Use the sandbox"},
"retries":{"type":"integer","title":"Retries"}}}`

// openConfig decrypts the stored config of a provider account.
func (e *env) openConfig(id string) map[string]any {
	e.t.Helper()
	var pa store.ProviderAccount
	if err := e.db.Read(ctx, func(q store.Querier) (err error) { pa, err = store.GetProviderAccount(ctx, q, id); return }); err != nil {
		e.t.Fatal(err)
	}
	raw, err := e.box.Open(pa.ConfigEnc, []byte("provider_accounts.config_enc:"+id))
	if err != nil {
		e.t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		e.t.Fatal(err)
	}
	return m
}

func auditUser(id string) audit.Actor { return audit.Actor{Type: audit.ActorUser, ID: id} }

func connectorsInputCfg(name, cfg string) connectors.CreateAccountInput {
	return connectors.CreateAccountInput{Connector: name, Name: "Test account", Mode: "live", Config: json.RawMessage(cfg)}
}

func TestProviderCreateRendersTheSchemaForm(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		st := fakeconn.RegisterSchema("dashfake", providerSchema)
		app := e.seedApp("alpha")
		for _, role := range []string{"viewer", "operator"} {
			c := e.roleClient(role)
			if r, _ := e.get(c, "/dashboard/providers/new?connector="+st.Name); r.StatusCode != 403 {
				t.Errorf("%s: new form %d", role, r.StatusCode)
			}
			tok := csrfOf(t, e.pageOK(c, "/dashboard/providers"))
			if r, _ := e.post(c, "/dashboard/providers", url.Values{"connector": {st.Name}, "name": {"x"}, "mode": {"test"}, "csrf": {tok}}); r.StatusCode != 403 {
				t.Errorf("%s: create %d", role, r.StatusCode)
			}
		}
		admin := e.roleClient("admin")
		list := e.pageOK(admin, "/dashboard/providers")
		has(t, list, st.Name, `/dashboard/providers/new?connector=`+st.Name)
		body := e.pageOK(admin, "/dashboard/providers/new?connector="+st.Name)
		has(t, body, `name="cfg_key_id"`, `Key id`, `name="cfg_key_secret"`, `type="password"`, `name="cfg_region"`, `<option value="in"`, `<option value="sg"`,
			`name="cfg_test_mode"`, `type="checkbox"`, `name="cfg_retries"`, `name="currency" value="INR"`, `name="currency" value="USD"`,
			`name="purpose" value="one_time"`, `name="purpose" value="subscription"`, `name="app_id"`, app.ID)
		if r, _ := e.get(admin, "/dashboard/providers/new?connector=nope"); r.StatusCode != 404 {
			t.Errorf("unknown connector: %d", r.StatusCode)
		}

		tok := csrfOf(t, body)
		form := url.Values{"connector": {st.Name}, "name": {"Main account"}, "mode": {"test"}, "cfg_key_id": {"key_123"}, "cfg_key_secret": {"s3cr3t-value"},
			"cfg_region": {"sg"}, "cfg_test_mode": {"1"}, "cfg_retries": {"3"}, "currency": {"INR", "USD"}, "purpose": {"one_time", "subscription"},
			"statement_descriptor": {"VOICESTUDIO"}, "provider_gstin": {"27AAPFU0939F1ZV"}, "app_id": {app.ID}, "csrf": {tok}}
		r, b := e.post(admin, "/dashboard/providers", form)
		if r.StatusCode != 303 || !strings.HasPrefix(r.Header.Get("Location"), "/dashboard/providers/pa_") {
			t.Fatalf("create: %d %s\n%s", r.StatusCode, r.Header.Get("Location"), b)
		}
		id := strings.TrimPrefix(r.Header.Get("Location"), "/dashboard/providers/")
		cfg := e.openConfig(id)
		if cfg["key_id"] != "key_123" || cfg["key_secret"] != "s3cr3t-value" || cfg["region"] != "sg" || cfg["test_mode"] != true || cfg["retries"] != float64(3) {
			t.Fatalf("stored config %v", cfg)
		}
		var pa store.ProviderAccount
		var linked []string
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			pa, err = store.GetProviderAccount(ctx, q, id)
			if err != nil {
				return
			}
			linked, err = store.ListAppProviderAccountIDs(ctx, q, app.ID)
			return
		})
		if pa.Mode != "test" || pa.Name != "Main account" || strings.Join(pa.AcceptedCurrencies, ",") != "INR,USD" || strings.Join(pa.EnabledPurposes, ",") != "one_time,subscription" ||
			pa.StatementDescriptor != "VOICESTUDIO" || len(linked) != 1 || linked[0] != id {
			t.Fatalf("account %+v linked %v", pa, linked)
		}
		var aud []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, "provider_account.create", 5)
			return
		})
		if len(aud) != 1 || aud[0].ActorID != "usr_admin" || strings.Contains(aud[0].After, "s3cr3t") {
			t.Fatalf("audit %+v", aud)
		}

		for name, o := range map[string]map[string]string{
			"missing required":  {"cfg_key_id": ""},
			"bad number":        {"cfg_retries": "many"},
			"no name":           {"name": " "},
			"bad mode":          {"mode": "staging"},
			"bad enum":          {"cfg_region": "mars"},
			"unknown currency":  {"currency": "XXX"},
			"bad purpose":       {"purpose": "barter"},
			"unknown app":       {"app_id": "app_nope"},
			"unknown connector": {"connector": "nope"},
		} {
			f := url.Values{}
			for k, v := range form {
				f[k] = v
			}
			for k, v := range o {
				f.Set(k, v)
			}
			r, b := e.post(admin, "/dashboard/providers", f)
			if r.StatusCode != 400 && r.StatusCode != 404 {
				t.Errorf("%s: %d", name, r.StatusCode)
			}
			if r.StatusCode == 400 && strings.Contains(b, "s3cr3t-value") {
				t.Errorf("%s: the secret was echoed back", name)
			}
		}
		if n := countAccounts(t, e); n != 1 {
			t.Fatalf("rejected creates left accounts behind (%d)", n)
		}
	})
}

func countAccounts(t *testing.T, e *env) int {
	t.Helper()
	var accts []store.ProviderAccountInfo
	_ = e.db.Read(ctx, func(q store.Querier) (err error) {
		accts, err = store.ListProviderAccounts(ctx, q, store.DefaultOrgID, "")
		return
	})
	return len(accts)
}

func TestProviderSecretsAreWriteOnly(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		st := fakeconn.RegisterSchema("dashfake", providerSchema)
		// A sandbox switch is only allowed on a test-mode account.
		in := connectorsInputCfg(st.Name, `{"key_id":"key_123","key_secret":"s3cr3t-value","region":"in","test_mode":true,"retries":2}`)
		in.Mode = "test"
		pa, err := e.conn.CreateAccount(ctx, auditUser("usr_seed"), core.OrgScope{OrgID: store.DefaultOrgID}, in)
		if err != nil {
			t.Fatal(err)
		}
		admin := e.roleClient("admin")
		page := "/dashboard/providers/" + pa.ID
		body := e.pageOK(admin, page)
		has(t, body, `value="key_123"`, `name="cfg_key_secret"`, `placeholder="`, "leave blank to keep", `<option value="in" selected`, `name="cfg_test_mode" value="1" checked`, `value="2"`)
		lacks(t, body, "s3cr3t-value")
		tok := csrfOf(t, body)

		form := func(over map[string]string) url.Values {
			f := url.Values{"name": {"Renamed"}, "status": {"active"}, "cfg_key_id": {"key_123"}, "cfg_key_secret": {""}, "cfg_region": {"in"}, "cfg_test_mode": {"1"},
				"cfg_retries": {"2"}, "currency": {"INR"}, "purpose": {"one_time", "subscription"}, "csrf": {tok}}
			for k, v := range over {
				f.Set(k, v)
			}
			return f
		}
		// a blank secret keeps the stored one
		if r, b := e.post(admin, page, form(map[string]string{"cfg_region": "sg"})); r.StatusCode != 303 {
			t.Fatalf("update: %d\n%s", r.StatusCode, b)
		}
		cfg := e.openConfig(pa.ID)
		if cfg["key_secret"] != "s3cr3t-value" || cfg["region"] != "sg" || cfg["key_id"] != "key_123" {
			t.Fatalf("config after blank-secret update: %v", cfg)
		}
		var got store.ProviderAccount
		_ = db.Read(ctx, func(q store.Querier) (err error) { got, err = store.GetProviderAccount(ctx, q, pa.ID); return })
		if got.Name != "Renamed" || strings.Join(got.AcceptedCurrencies, ",") != "INR" || strings.Join(got.EnabledPurposes, ",") != "one_time,subscription" {
			t.Fatalf("account %+v", got)
		}
		// an unticked checkbox means false
		if r, _ := e.post(admin, page, form(map[string]string{"cfg_test_mode": ""})); r.StatusCode != 303 {
			t.Fatalf("untick: %d", r.StatusCode)
		}
		if cfg = e.openConfig(pa.ID); cfg["test_mode"] != false {
			t.Fatalf("test_mode %v", cfg["test_mode"])
		}
		// a new secret replaces it
		if r, _ := e.post(admin, page, form(map[string]string{"cfg_key_secret": "n3w-secret"})); r.StatusCode != 303 {
			t.Fatalf("new secret: %d", r.StatusCode)
		}
		if cfg = e.openConfig(pa.ID); cfg["key_secret"] != "n3w-secret" {
			t.Fatalf("secret not replaced: %v", cfg)
		}
		// disabling the account
		if r, _ := e.post(admin, page, form(map[string]string{"status": "disabled"})); r.StatusCode != 303 {
			t.Fatalf("disable: %d", r.StatusCode)
		}
		_ = db.Read(ctx, func(q store.Querier) (err error) { got, err = store.GetProviderAccount(ctx, q, pa.ID); return })
		if got.Status != "disabled" {
			t.Fatalf("status %s", got.Status)
		}
		// invalid changes are refused and change nothing
		before := e.openConfig(pa.ID)
		for name, o := range map[string]map[string]string{
			"clear required": {"cfg_key_id": ""},
			"bad enum":       {"cfg_region": "mars"},
			"bad status":     {"status": "paused"},
			"bad purpose":    {"purpose": "barter"},
		} {
			r, b := e.post(admin, page, form(o))
			if r.StatusCode != 400 || strings.Contains(b, "n3w-secret") {
				t.Errorf("%s: %d", name, r.StatusCode)
			}
		}
		after := e.openConfig(pa.ID)
		bj, _ := json.Marshal(before)
		aj, _ := json.Marshal(after)
		if string(bj) != string(aj) {
			t.Fatalf("a refused update changed the config")
		}
		// the audit log records the change but never a secret
		var aud []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, "provider_account.update", 20)
			return
		})
		if len(aud) < 4 {
			t.Fatalf("audit %+v", aud)
		}
		for _, a := range aud {
			if strings.Contains(a.After+a.Before, "secret") && strings.Contains(a.After+a.Before, "n3w") {
				t.Errorf("secret in audit: %+v", a)
			}
		}

		// read-only users see no form and no secrets
		for _, role := range []string{"viewer", "operator"} {
			c := e.roleClient(role)
			vb := e.pageOK(c, page)
			lacks(t, vb, `action="`+page+`"`, "n3w-secret", `name="cfg_key_secret"`)
			has(t, vb, "Renamed")
			if r, _ := e.post(c, page, form(map[string]string{"csrf": csrfOf(t, vb)})); r.StatusCode != 403 {
				t.Errorf("%s update: %d", role, r.StatusCode)
			}
		}
	})
}

// createProvider stores an account directly through the connector service.
func (e *env) createProvider(st *fakeconn.State, cfg string) store.ProviderAccount {
	e.t.Helper()
	pa, err := e.conn.CreateAccount(ctx, auditUser("usr_seed"), core.OrgScope{OrgID: store.DefaultOrgID}, connectorsInputCfg(st.Name, cfg))
	if err != nil {
		e.t.Fatal(err)
	}
	return pa
}

func TestProviderDetailWebhookURLAndAppLinking(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		st := fakeconn.RegisterSchema("dashfake", providerSchema)
		pa := e.createProvider(st, `{"key_id":"k","key_secret":"s"}`)
		alpha, beta := e.seedApp("alpha"), e.seedApp("beta")
		e.otherOrg()
		err := db.Write(ctx, func(tx store.Tx) error {
			return store.InsertProviderAccountInfo(ctx, tx, store.ProviderAccountInfo{ID: "pa_foreign", OrgID: "org_other", Mode: "live", Connector: st.Name,
				Name: "Foreign account", Status: "active", Currencies: []string{"INR"}, Purposes: []string{"one_time"}}, e.clk.Now())
		})
		if err != nil {
			t.Fatal(err)
		}
		admin := e.roleClient("admin")
		list := e.pageOK(admin, "/dashboard/providers")
		hook := "http://localhost:8080/hooks/" + pa.ID
		has(t, list, pa.Name, st.Name)
		lacks(t, list, "Webhook URL", hook, `data-copy="`+hook+`"`)
		lacks(t, list, "pa_foreign", "Foreign account")
		if r, _ := e.get(admin, "/dashboard/providers/pa_foreign"); r.StatusCode != 404 {
			t.Errorf("another org's account: %d", r.StatusCode)
		}

		page := "/dashboard/providers/" + pa.ID
		body := e.pageOK(admin, page)
		has(t, body, hook, `data-copy="`+hook+`"`, `name="app_id"`)
		tok := csrfOf(t, body)
		if r, _ := e.post(admin, page+"/apps", url.Values{"app_id": {alpha.ID}, "csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("link: %d", r.StatusCode)
		}
		var ids []string
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			ids, err = store.ListAppProviderAccountIDs(ctx, q, alpha.ID)
			return
		})
		if len(ids) != 1 || ids[0] != pa.ID {
			t.Fatalf("linked %v", ids)
		}
		body = e.pageOK(admin, page)
		has(t, body, "Alpha")
		lacks(t, body, `<option value="`+alpha.ID+`"`) // already linked
		has(t, body, `<option value="`+beta.ID+`"`)
		for name, id := range map[string]string{"unknown app": "app_nope", "other org's app": "app_foreign"} {
			if r, _ := e.post(admin, page+"/apps", url.Values{"app_id": {id}, "csrf": {tok}}); r.StatusCode != 404 {
				t.Errorf("%s: %d", name, r.StatusCode)
			}
		}
		if r, _ := e.get(admin, "/dashboard/providers/pa_nope"); r.StatusCode != 404 {
			t.Errorf("unknown account: %d", r.StatusCode)
		}
		op := e.roleClient("operator")
		if r, _ := e.post(op, page+"/apps", url.Values{"app_id": {beta.ID}, "csrf": {csrfOf(t, e.pageOK(op, page))}}); r.StatusCode != 403 {
			t.Errorf("operator link: %d", r.StatusCode)
		}
	})
}

func TestProviderTestConnection(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		st := fakeconn.RegisterSchema("dashfake", providerSchema)
		pa := e.createProvider(st, `{"key_id":"k","key_secret":"s"}`)
		admin := e.roleClient("admin")
		page := "/dashboard/providers/" + pa.ID
		body := e.pageOK(admin, page)
		has(t, body, `action="`+page+`/test"`, "Test connection")
		tok := csrfOf(t, body)

		r, b := e.post(admin, page+"/test", url.Values{"csrf": {tok}})
		if r.StatusCode != 200 || !strings.Contains(b, "accepted the credentials") {
			t.Fatalf("ok: %d", r.StatusCode)
		}
		if len(st.Fetches) != 1 {
			t.Fatalf("fetches %+v", st.Fetches)
		}
		st.Locked(func() { st.FetchErr = connector.ErrInvalidCredentials })
		if _, b = e.post(admin, page+"/test", url.Values{"csrf": {tok}}); !strings.Contains(b, "rejected the credentials") {
			t.Fatalf("invalid credentials not reported: %s", b)
		}
		st.Locked(func() { st.FetchErr = errors.New("connection refused") })
		if _, b = e.post(admin, page+"/test", url.Values{"csrf": {tok}}); !strings.Contains(b, "connection refused") || strings.Contains(b, "accepted the credentials") {
			t.Fatalf("outage reported as a pass: %s", b)
		}
		op := e.roleClient("operator")
		if r, _ := e.post(op, page+"/test", url.Values{"csrf": {csrfOf(t, e.pageOK(op, page))}}); r.StatusCode != 403 {
			t.Errorf("operator: %d", r.StatusCode)
		}
		if r, _ := e.post(admin, "/dashboard/providers/pa_nope/test", url.Values{"csrf": {tok}}); r.StatusCode != 404 {
			t.Errorf("unknown: %d", r.StatusCode)
		}
	})
}
