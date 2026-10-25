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
	"github.com/paymux/paymux/internal/checkout"
	"github.com/paymux/paymux/internal/routing"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
)

// ruleEdit is one routing rule laid out as form fields.
type ruleEdit struct {
	Order                                                            string
	Name, Currency, Country, Purpose, AmountMin, AmountMax, Metadata string
	Action, Targets, Message                                         string
}

// routingView is the org routing setting laid out for the editor: ordered
// rules plus the catch-all.
type routingView struct {
	Rules     []ruleEdit
	BlankRows []int
	CatchAll  ruleEdit // Action is "route", "reject" or "none"
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
	m.HandleFunc("POST "+prefix+"/settings/routing", d.authed(authz.SettingsWrite, d.recent(d.routingSave)))
	m.HandleFunc("POST "+prefix+"/settings/{key}", d.authed(authz.SettingsWrite, d.recent(d.orgSettingSet)))
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

	rv, err := d.routingFor(ctx, org)
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

// routingFor reads the org's routing setting for the editor.
func (d *Dashboard) routingFor(ctx context.Context, orgID string) (*routingView, error) {
	val, err := d.o.Settings.ResolveOrg(ctx, orgID, "routing")
	if err != nil {
		return nil, err
	}
	var doc routingDoc
	if err := val.Decode(&doc); err != nil {
		return nil, err
	}
	rv := &routingView{CatchAll: ruleEdit{Action: "none"}}
	for i, r := range doc.Rules {
		e := toEdit(r)
		e.Order = strconv.Itoa(i + 1)
		rv.Rules = append(rv.Rules, e)
	}
	if doc.CatchAll != nil {
		rv.CatchAll = toEdit(*doc.CatchAll)
	}
	return rv, nil
}

// routingDoc is the stored shape of the routing setting (the same document
// the payment runtime loads).
type routingDoc struct {
	Rules    []routing.Rule `json:"rules"`
	CatchAll *routing.Rule  `json:"catch_all,omitempty"`
}

func joinInts(p *int64) string {
	if p == nil {
		return ""
	}
	return strconv.FormatInt(*p, 10)
}

func toEdit(r routing.Rule) ruleEdit {
	e := ruleEdit{Name: r.Name, Currency: strings.Join(r.When.Currency, ", "), Country: strings.Join(r.When.Country, ", "),
		Purpose: strings.Join(r.When.Purpose, ", "), AmountMin: joinInts(r.When.AmountMin), AmountMax: joinInts(r.When.AmountMax),
		Action: r.Action.Kind, Message: r.Action.Message}
	keys := make([]string, 0, len(r.When.Metadata))
	for k := range r.When.Metadata {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for i, k := range keys {
		if i > 0 {
			e.Metadata += ", "
		}
		e.Metadata += k + "=" + r.When.Metadata[k]
	}
	for i, t := range r.Action.Targets {
		if i > 0 {
			e.Targets += ", "
		}
		if t.AccountID != "" {
			e.Targets += "account:" + t.AccountID
		} else {
			e.Targets += t.Connector
		}
	}
	return e
}

// fieldErr is a form mistake to show the operator.
type fieldErr string

func (e fieldErr) Error() string { return string(e) }

func parseAmount(label, s string) (*int64, error) {
	if s = strings.TrimSpace(s); s == "" {
		return nil, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return nil, fieldErr(label + " must be a whole number of minor units (paise, cents) of 0 or more")
	}
	return &n, nil
}

func parseTargets(s string) []routing.Target {
	var out []routing.Target
	for _, tok := range splitList(s) {
		if id, ok := strings.CutPrefix(tok, "account:"); ok {
			out = append(out, routing.Target{AccountID: strings.TrimSpace(id)})
		} else {
			out = append(out, routing.Target{Connector: tok})
		}
	}
	return out
}

func parseMetadata(s string) (map[string]string, error) {
	list := splitList(s)
	if len(list) == 0 {
		return nil, nil
	}
	m := map[string]string{}
	for _, kv := range list {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fieldErr("metadata conditions must look like key=value, key2=value2")
		}
		m[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return m, nil
}

func upperList(s string) []string {
	l := splitList(s)
	for i := range l {
		l[i] = strings.ToUpper(l[i])
	}
	return l
}

// testRouting runs routing.Choose for the form, with the configuration and
// accounts the payment runtime uses. The second result is a validation
// message when the input itself is wrong.
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

	var cfg routing.Config
	var pas []store.ProviderAccount
	err := d.o.DB.Read(ctx, func(q store.Querier) (err error) {
		if tf.App != "" {
			app, err := store.GetApp(ctx, q, tf.App)
			if err != nil {
				return err
			}
			if cfg, err = checkout.AppRoutingConfig(ctx, q, app); err != nil {
				return err
			}
			pas, err = store.ListAppProviderAccounts(ctx, q, app.ID, tf.Mode)
			return err
		}
		if cfg, err = checkout.OrgRoutingConfig(ctx, q, orgID); err != nil {
			return err
		}
		infos, err := store.ListProviderAccounts(ctx, q, orgID, tf.Mode)
		for _, a := range infos {
			pas = append(pas, store.ProviderAccount{ID: a.ID, Connector: a.Connector, Name: a.Name, Status: a.Status,
				AcceptedCurrencies: a.Currencies, EnabledPurposes: a.Purposes})
		}
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil, "unknown app"
	}
	if err != nil {
		d.o.Logger.Error("dashboard: routing test", "err", err)
		return &routeResult{NoRoute: true, Message: "could not load the routing setting"}, ""
	}
	names := map[string]string{}
	for _, pa := range pas {
		names[pa.ID] = pa.Name
	}
	res, err := routing.Choose(cfg, routing.Request{Currency: tf.Currency, Country: tf.Country, Amount: amount, Purpose: tf.Purpose},
		checkout.RoutingAccounts(pas), nil)
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
	f := r.PostForm
	get := func(k string, i int) string {
		if v := f[k]; i < len(v) {
			return strings.TrimSpace(v[i])
		}
		return ""
	}
	doc := routingDoc{Rules: []routing.Rule{}}
	var orders []int
	for i := range f["r_name"] {
		if get("r_name", i)+get("r_currency", i)+get("r_country", i)+get("r_purpose", i)+get("r_amin", i)+get("r_amax", i)+
			get("r_meta", i)+get("r_targets", i)+get("r_message", i) == "" {
			continue // a blank row
		}
		order := i + 1 // blank: keep the written position
		if o := get("r_order", i); o != "" {
			n, err := strconv.Atoi(o)
			if err != nil || n < 0 {
				d.renderSettings(w, r, s, http.StatusBadRequest, "The order of a rule must be a whole number like 1, 2, 3.")
				return
			}
			order = n
		}
		rule := routing.Rule{Name: get("r_name", i)}
		if rule.Name == "" {
			rule.Name = "rule-" + strconv.Itoa(i+1)
		}
		rule.When.Currency, rule.When.Country = upperList(get("r_currency", i)), upperList(get("r_country", i))
		rule.When.Purpose = splitList(get("r_purpose", i))
		var err error
		if rule.When.AmountMin, err = parseAmount("Minimum amount", get("r_amin", i)); err == nil {
			rule.When.AmountMax, err = parseAmount("Maximum amount", get("r_amax", i))
		}
		if err == nil {
			rule.When.Metadata, err = parseMetadata(get("r_meta", i))
		}
		if err != nil {
			d.renderSettings(w, r, s, http.StatusBadRequest, "Rule "+rule.Name+": "+err.Error())
			return
		}
		if len(rule.When.Currency) == 0 {
			rule.When.Currency = nil
		}
		if len(rule.When.Country) == 0 {
			rule.When.Country = nil
		}
		if len(rule.When.Purpose) == 0 {
			rule.When.Purpose = nil
		}
		rule.Action = routing.Action{Kind: get("r_action", i), Targets: parseTargets(get("r_targets", i)), Message: get("r_message", i)}
		if rule.Action.Kind == "reject" {
			rule.Action.Targets = nil
		}
		doc.Rules = append(doc.Rules, rule)
		orders = append(orders, order)
	}
	// Rules are tried in the order of their numbers; equal numbers keep the
	// order they were written in.
	idx := make([]int, len(doc.Rules))
	for i := range idx {
		idx[i] = i
	}
	slices.SortStableFunc(idx, func(a, b int) int { return orders[a] - orders[b] })
	sorted := make([]routing.Rule, len(doc.Rules))
	for i, j := range idx {
		sorted[i] = doc.Rules[j]
	}
	doc.Rules = sorted
	switch kind := strings.TrimSpace(f.Get("ca_action")); kind {
	case "none":
	default:
		ca := routing.Rule{Name: "catch-all", Action: routing.Action{Kind: kind, Message: strings.TrimSpace(f.Get("ca_message"))}}
		if kind != "reject" {
			ca.Action.Targets = parseTargets(f.Get("ca_targets"))
		}
		doc.CatchAll = &ca
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
	err = d.o.Settings.SetOrg(r.Context(), s.auditActor(), org, r.PathValue("key"), settingJSON(r.PathValue("key"), r.PostForm["value"]))
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
