package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/paymux/paymux/internal/authz"
	"github.com/paymux/paymux/internal/routing"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
)

type ruleRow struct{ Currency, Connectors string }

// routingView is the org routing setting laid out for the editor.
type routingView struct {
	Rules     []ruleRow
	BlankRows []int
	CatchAll  string
	Reject    bool
	CanWrite  bool
}

// testForm echoes the "test routing" inputs.
type testForm struct {
	App, Mode, Currency, Country, Amount, Purpose string
}

// routeResult is the outcome of a routing test.
type routeResult struct {
	Matched  bool
	Rule     string
	Accounts []string // display names, in preference order
	NoRoute  bool
	Rejected bool
	Message  string
}

func (d *Dashboard) routesSettings() {
	m := d.mux
	m.HandleFunc("GET "+prefix+"/settings", d.authed(authz.SettingsRead, d.settingsPage))
	m.HandleFunc("POST "+prefix+"/settings/routing", d.authed(authz.SettingsWrite, d.routingSave))
	m.HandleFunc("POST "+prefix+"/settings/{key}", d.authed(authz.SettingsWrite, d.orgSettingSet))
}

var currencyRE = regexp.MustCompile(`^[A-Z]{3}$`)

func (d *Dashboard) settingsPage(w http.ResponseWriter, r *http.Request, s *session) {
	d.renderSettings(w, r, s, http.StatusOK, "")
}

func (d *Dashboard) renderSettings(w http.ResponseWriter, r *http.Request, s *session, status int, errMsg string) {
	ctx := r.Context()
	org, err := d.orgID(ctx)
	if err != nil {
		d.fail(w, err)
		return
	}
	v := d.shell(r, s, "settings", "Settings")
	v.Error = errMsg

	rows, err := d.settingRows(ctx, "", org, settings.LevelOrg)
	if err != nil {
		d.fail(w, err)
		return
	}
	rows = slices.DeleteFunc(rows, func(sr settingRow) bool { return sr.Key == "routing" }) // has its own editor
	v.Table = &settingsTable{Rows: rows, Base: prefix + "/settings", CanWrite: s.can(authz.SettingsWrite)}

	rv, err := d.routingFor(ctx, "", org)
	if err != nil {
		d.fail(w, err)
		return
	}
	rv.CanWrite = s.can(authz.SettingsWrite)
	for i := 0; i < 2; i++ {
		rv.BlankRows = append(rv.BlankRows, i)
	}
	v.Routing = rv

	var apps []store.App
	if err := d.o.DB.Read(ctx, func(q store.Querier) (err error) { apps, err = store.ListApps(ctx, q, org); return }); err != nil {
		d.fail(w, err)
		return
	}
	for _, a := range apps {
		v.AppOpts = append(v.AppOpts, appOpt{ID: a.ID, Name: a.Name})
	}
	v.TestForm = testForm{Mode: v.Mode, Purpose: routing.PurposeOneTime, Currency: "INR"}
	if q := r.URL.Query(); q.Get("test") != "" {
		tf := testForm{App: q.Get("app"), Mode: q.Get("mode"), Currency: strings.ToUpper(strings.TrimSpace(q.Get("currency"))),
			Country: strings.ToUpper(strings.TrimSpace(q.Get("country"))), Amount: strings.TrimSpace(q.Get("amount")), Purpose: q.Get("purpose")}
		if tf.Mode == "" {
			tf.Mode = v.Mode
		}
		if tf.Purpose == "" {
			tf.Purpose = routing.PurposeOneTime
		}
		v.TestForm = tf
		res, msg := d.testRouting(r, org, tf)
		if msg != "" {
			badRequest(w, msg)
			return
		}
		v.Route = res
	}
	d.render(w, status, "dash_settings", v)
}

// routingFor reads the effective routing setting of an app (or the org).
func (d *Dashboard) routingFor(ctx context.Context, appID, orgID string) (*routingView, error) {
	var val settings.Value
	var err error
	if appID != "" {
		val, err = d.o.Settings.Resolve(ctx, appID, "routing")
	} else {
		val, err = d.o.Settings.ResolveOrg(ctx, orgID, "routing")
	}
	if err != nil {
		return nil, err
	}
	var doc routingDoc
	if err := val.Decode(&doc); err != nil {
		return nil, err
	}
	rv := &routingView{CatchAll: strings.Join(doc.CatchAll.Connectors, ", "), Reject: doc.CatchAll.Reject}
	for _, r := range doc.Rules {
		rv.Rules = append(rv.Rules, ruleRow{Currency: r.Currency, Connectors: strings.Join(r.Connectors, ", ")})
	}
	return rv, nil
}

// routingDoc is the stored shape of the routing setting.
type routingDoc struct {
	Rules    []ruleDoc `json:"rules"`
	CatchAll struct {
		Connectors []string `json:"connectors"`
		Reject     bool     `json:"reject"`
	} `json:"catch_all"`
}

type ruleDoc struct {
	Currency   string   `json:"currency"`
	Connectors []string `json:"connectors"`
}

// testRouting runs routing.Choose for the form. The second result is a
// validation message when the input itself is wrong.
func (d *Dashboard) testRouting(r *http.Request, orgID string, tf testForm) (*routeResult, string) {
	ctx := r.Context()
	if !currencyRE.MatchString(tf.Currency) {
		return nil, "currency must be a 3-letter ISO code like INR"
	}
	if tf.Mode != "test" && tf.Mode != "live" {
		return nil, "mode must be test or live"
	}
	if tf.Purpose != routing.PurposeOneTime && tf.Purpose != routing.PurposeSubscription {
		return nil, "purpose must be one_time or subscription"
	}
	var amount int64
	if tf.Amount != "" {
		n, err := strconv.ParseInt(tf.Amount, 10, 64)
		if err != nil || n < 0 {
			return nil, "amount must be a whole number of minor units (paise, cents)"
		}
		amount = n
	}

	var accts []store.ProviderAccountInfo
	var allowed []string
	var val settings.Value
	err := d.o.DB.Read(ctx, func(q store.Querier) (err error) {
		if accts, err = store.ListProviderAccounts(ctx, q, orgID, tf.Mode); err != nil {
			return err
		}
		if tf.App != "" {
			if _, err = store.GetApp(ctx, q, tf.App); err != nil {
				return err
			}
			allowed, err = store.ListAppProviderAccountIDs(ctx, q, tf.App)
		}
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil, "unknown app"
	}
	if err == nil {
		if tf.App != "" {
			val, err = d.o.Settings.Resolve(ctx, tf.App, "routing")
		} else {
			val, err = d.o.Settings.ResolveOrg(ctx, orgID, "routing")
		}
	}
	if err != nil {
		d.o.Logger.Error("dashboard: routing test", "err", err)
		return &routeResult{NoRoute: true, Message: "could not load the routing setting"}, ""
	}
	cfg, err := routingConfig(val)
	if err != nil {
		return &routeResult{NoRoute: true, Message: err.Error()}, ""
	}

	names := map[string]string{}
	var pool []routing.Account
	for _, a := range accts {
		if tf.App != "" && !slices.Contains(allowed, a.ID) {
			continue
		}
		names[a.ID] = a.Name
		pool = append(pool, routing.Account{ID: a.ID, Connector: a.Connector, Currencies: a.Currencies, Purposes: a.Purposes, Disabled: a.Status != "active"})
	}
	res, err := routing.Choose(cfg, routing.Request{Currency: tf.Currency, Country: tf.Country, Amount: amount, Purpose: tf.Purpose}, pool, nil)
	var rej *routing.RejectedError
	switch {
	case errors.As(err, &rej):
		return &routeResult{Rejected: true, Rule: rej.Rule, Message: rej.Message}, ""
	case err != nil:
		return &routeResult{NoRoute: true, Message: err.Error()}, ""
	}
	out := &routeResult{Matched: true, Rule: res.Rule}
	for _, id := range res.Accounts {
		out.Accounts = append(out.Accounts, names[id])
	}
	return out, ""
}

// routingConfig turns the stored routing setting (currency rules plus a
// catch-all) into the rule hierarchy routing.Choose evaluates. An app-level
// override replaces the org setting, so the effective value is the org layer.
func routingConfig(val settings.Value) (routing.Config, error) {
	var doc routingDoc
	if err := val.Decode(&doc); err != nil {
		return routing.Config{}, err
	}
	var cfg routing.Config
	for _, r := range doc.Rules {
		cfg.Org = append(cfg.Org, routing.Rule{Name: "rule:" + r.Currency, When: routing.When{Currency: []string{r.Currency}},
			Action: routing.Route(r.Connectors...)})
	}
	if doc.CatchAll.Reject {
		cfg.CatchAll = routing.Rule{Name: "catch_all", Action: routing.Reject("This payment cannot be accepted.")}
	} else {
		cfg.CatchAll = routing.Rule{Name: "catch_all", Action: routing.Route(doc.CatchAll.Connectors...)}
	}
	return cfg, nil
}

func splitList(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (d *Dashboard) routingSave(w http.ResponseWriter, r *http.Request, s *session) {
	var doc routingDoc
	doc.Rules = []ruleDoc{}
	cur, con := r.PostForm["currency"], r.PostForm["connectors"]
	for i, c := range cur {
		c = strings.ToUpper(strings.TrimSpace(c))
		var list []string
		if i < len(con) {
			list = splitList(con[i])
		}
		if c == "" && len(list) == 0 {
			continue // a blank row
		}
		doc.Rules = append(doc.Rules, ruleDoc{Currency: c, Connectors: list})
	}
	doc.CatchAll.Reject = r.PostForm.Get("catch_all_reject") != ""
	doc.CatchAll.Connectors = splitList(r.PostForm.Get("catch_all"))
	if doc.CatchAll.Reject {
		doc.CatchAll.Connectors = []string{}
	}
	raw, _ := json.Marshal(doc)
	org, err := d.orgID(r.Context())
	if err != nil {
		d.fail(w, err)
		return
	}
	err = d.o.Settings.SetOrg(r.Context(), s.auditActor(), org, "routing", raw)
	if msg, bad := settingErr(err); bad {
		d.renderSettings(w, r, s, http.StatusBadRequest, msg)
		return
	}
	if err != nil {
		d.fail(w, err)
		return
	}
	http.Redirect(w, r, prefix+"/settings", http.StatusSeeOther)
}

func (d *Dashboard) orgSettingSet(w http.ResponseWriter, r *http.Request, s *session) {
	org, err := d.orgID(r.Context())
	if err != nil {
		d.fail(w, err)
		return
	}
	err = d.o.Settings.SetOrg(r.Context(), s.auditActor(), org, r.PathValue("key"), json.RawMessage(strings.TrimSpace(r.PostForm.Get("value"))))
	if msg, bad := settingErr(err); bad {
		d.renderSettings(w, r, s, http.StatusBadRequest, msg)
		return
	}
	if err != nil {
		d.fail(w, err)
		return
	}
	http.Redirect(w, r, prefix+"/settings", http.StatusSeeOther)
}
