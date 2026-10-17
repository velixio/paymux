package dashboard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/paymux/paymux/internal/authz"
	"github.com/paymux/paymux/internal/connectors"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/pkg/connector"
)

type connOpt struct{ Name, Display string }

type provRow struct {
	ID, Name, Connector, Display, Mode, Status, Currencies, Purposes, Webhook string
}

type enumOpt struct {
	Value    string
	Selected bool
}

// cfgField is one input of a connector's config form, drawn from the
// connector's JSON schema.
type cfgField struct {
	Name, Title, Type string
	Secret, Required  bool
	Enum              []enumOpt
	Value             string
	Checked           bool // booleans
	HasValue          bool // a secret that is stored (never shown)
}

type checkOpt struct {
	Value   string
	Checked bool
}

type linkedApp struct{ ID, Name string }

type provForm struct {
	New                                                bool
	ID, Connector, Display, Name, Mode, Status         string
	StatementDescriptor, ProviderGSTIN, Webhook, AppID string
	Fields                                             []cfgField
	Currencies, Purposes                               []checkOpt
	Linked                                             []linkedApp
	Linkable                                           []appOpt
	CanManage                                          bool
	Test                                               string
	TestOK                                             bool
	Unavailable                                        bool
}

func (d *Dashboard) routesProviders() {
	m := d.mux
	m.HandleFunc("GET "+prefix+"/providers", d.authed(authz.SettingsRead, d.providersPage))
	m.HandleFunc("GET "+prefix+"/providers/new", d.authed(authz.ProviderAccountManage, d.providerNew))
	m.HandleFunc("POST "+prefix+"/providers", d.authed(authz.ProviderAccountManage, d.providerCreate))
	m.HandleFunc("GET "+prefix+"/providers/{id}", d.authed(authz.SettingsRead, d.providerPage))
	m.HandleFunc("POST "+prefix+"/providers/{id}", d.authed(authz.ProviderAccountManage, d.providerUpdate))
	m.HandleFunc("POST "+prefix+"/providers/{id}/apps", d.authed(authz.ProviderAccountManage, d.providerLink))
	m.HandleFunc("POST "+prefix+"/providers/{id}/test", d.authed(authz.ProviderAccountManage, d.providerTest))
}

// schemaFields lists the config inputs of a connector's JSON schema, in the
// order the schema declares them.
func schemaFields(schema json.RawMessage) []cfgField {
	if len(bytes.TrimSpace(schema)) == 0 {
		return nil
	}
	var doc struct {
		Required   []string        `json:"required"`
		Properties json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(schema, &doc) != nil || len(doc.Properties) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(doc.Properties))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil
	}
	var out []cfgField
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			break
		}
		var p struct {
			Type    string `json:"type"`
			Title   string `json:"title"`
			Enum    []any  `json:"enum"`
			Secret  bool   `json:"x-secret"`
			Default any    `json:"default"`
		}
		if err := dec.Decode(&p); err != nil {
			break
		}
		name := kt.(string)
		f := cfgField{Name: name, Title: firstNonBlank(p.Title, name), Type: p.Type, Secret: p.Secret, Required: slices.Contains(doc.Required, name)}
		for _, ev := range p.Enum {
			f.Enum = append(f.Enum, enumOpt{Value: fmt.Sprint(ev)})
		}
		if p.Default != nil {
			switch dv := p.Default.(type) {
			case bool:
				f.Checked = dv
			default:
				f.Value = fmt.Sprint(dv)
			}
		}
		out = append(out, f)
	}
	return out
}

// fillFields sets each field's value from the stored config (existing) or, on
// a re-render after a mistake, from the posted form. Secrets are never filled.
func fillFields(fields []cfgField, existing map[string]any, post url.Values) []cfgField {
	out := make([]cfgField, len(fields))
	for i, f := range fields {
		key := "cfg_" + f.Name
		switch {
		case f.Secret:
			_, f.HasValue = existing[f.Name]
			f.HasValue = f.HasValue && fmt.Sprint(existing[f.Name]) != ""
		case post != nil && f.Type == "boolean":
			f.Checked = post.Get(key) != ""
		case post != nil:
			f.Value = post.Get(key)
		case existing != nil && f.Type == "boolean":
			f.Checked, _ = existing[f.Name].(bool)
		case existing != nil:
			if v, ok := existing[f.Name]; ok {
				f.Value = fmt.Sprint(v)
			}
		}
		if len(f.Enum) > 0 {
			opts := make([]enumOpt, len(f.Enum))
			for j, o := range f.Enum {
				opts[j] = enumOpt{Value: o.Value, Selected: o.Value == f.Value}
			}
			f.Enum = opts
		}
		out[i] = f
	}
	return out
}

// buildConfig assembles the config document from the posted form. A blank
// secret keeps the stored one (existing); other blank fields are left out.
func buildConfig(fields []cfgField, post url.Values, existing map[string]any) (json.RawMessage, error) {
	cfg := map[string]any{}
	for _, f := range fields {
		raw := strings.TrimSpace(post.Get("cfg_" + f.Name))
		switch {
		case f.Type == "boolean":
			cfg[f.Name] = raw != ""
			continue
		case raw == "":
			if s, ok := existing[f.Name].(string); ok && f.Secret && s != "" {
				cfg[f.Name] = s
			}
			continue
		}
		switch {
		case len(f.Enum) > 0:
			if !slices.ContainsFunc(f.Enum, func(o enumOpt) bool { return o.Value == raw }) {
				return nil, fieldErr(f.Title + " must be one of the listed values")
			}
			cfg[f.Name] = raw
		case f.Type == "integer":
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return nil, fieldErr(f.Title + " must be a whole number")
			}
			cfg[f.Name] = n
		case f.Type == "number":
			n, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				return nil, fieldErr(f.Title + " must be a number")
			}
			cfg[f.Name] = n
		default:
			cfg[f.Name] = raw
		}
	}
	return json.Marshal(cfg)
}

func (d *Dashboard) orgScope(r *http.Request) (core.OrgScope, error) {
	org, err := d.orgID(r.Context())
	return core.OrgScope{OrgID: org}, err
}

func (d *Dashboard) providersPage(w http.ResponseWriter, r *http.Request, s *session) {
	ctx := r.Context()
	org, err := d.orgID(ctx)
	if err != nil {
		d.fail(w, err)
		return
	}
	var accts []store.ProviderAccountInfo
	if err := d.o.DB.Read(ctx, func(q store.Querier) (err error) { accts, err = store.ListProviderAccounts(ctx, q, org, ""); return }); err != nil {
		d.fail(w, err)
		return
	}
	v := d.shell(r, s, "providers", "Provider accounts")
	v.CanManage = s.can(authz.ProviderAccountManage)
	for _, a := range accts {
		display := a.Connector
		if _, info, ok := connector.Lookup(a.Connector); ok {
			display = info.DisplayName
		}
		v.Providers = append(v.Providers, provRow{ID: a.ID, Name: a.Name, Connector: a.Connector, Display: display, Mode: a.Mode, Status: a.Status,
			Currencies: strings.Join(a.Currencies, ", "), Purposes: strings.Join(a.Purposes, ", "), Webhook: d.o.Connectors.WebhookURL(a.ID)})
	}
	for _, info := range connector.List() {
		v.ConnOpts = append(v.ConnOpts, connOpt{Name: info.Name, Display: firstNonBlank(info.DisplayName, info.Name)})
	}
	d.render(w, http.StatusOK, "dash_providers", v)
}

func checks(all, on []string) []checkOpt {
	out := make([]checkOpt, len(all))
	for i, a := range all {
		out[i] = checkOpt{Value: a, Checked: slices.Contains(on, a)}
	}
	return out
}

func (d *Dashboard) appChoices(r *http.Request, org string) ([]store.App, error) {
	var apps []store.App
	err := d.o.DB.Read(r.Context(), func(q store.Querier) (err error) { apps, err = store.ListApps(r.Context(), q, org); return })
	return apps, err
}

func (d *Dashboard) renderNewProvider(w http.ResponseWriter, r *http.Request, s *session, info connector.Info, status int, errMsg string, post url.Values) {
	org, err := d.orgID(r.Context())
	if err != nil {
		d.fail(w, err)
		return
	}
	apps, err := d.appChoices(r, org)
	if err != nil {
		d.fail(w, err)
		return
	}
	f := &provForm{New: true, Connector: info.Name, Display: firstNonBlank(info.DisplayName, info.Name), Mode: "test", CanManage: true,
		Fields: fillFields(schemaFields(info.ConfigSchema), nil, post)}
	on, onP := info.Currencies, []string{"one_time"}
	if post != nil {
		f.Name, f.Mode, f.StatementDescriptor, f.ProviderGSTIN, f.AppID = post.Get("name"), post.Get("mode"), post.Get("statement_descriptor"), post.Get("provider_gstin"), post.Get("app_id")
		on, onP = post["currency"], post["purpose"]
	}
	f.Currencies, f.Purposes = checks(info.Currencies, on), checks([]string{"one_time", "subscription"}, onP)
	for _, a := range apps {
		f.Linkable = append(f.Linkable, appOpt{ID: a.ID, Name: a.Name})
	}
	v := d.shell(r, s, "providers", "Add "+f.Display+" account")
	v.Prov, v.Error = f, errMsg
	d.render(w, status, "dash_provider", v)
}

func (d *Dashboard) providerNew(w http.ResponseWriter, r *http.Request, s *session) {
	_, info, ok := connector.Lookup(r.URL.Query().Get("connector"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	d.renderNewProvider(w, r, s, info, http.StatusOK, "", nil)
}

func (d *Dashboard) providerCreate(w http.ResponseWriter, r *http.Request, s *session) {
	f := r.PostForm
	_, info, ok := connector.Lookup(f.Get("connector"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	scope, err := d.orgScope(r)
	if err != nil {
		d.fail(w, err)
		return
	}
	cfg, err := buildConfig(schemaFields(info.ConfigSchema), f, nil)
	if err == nil {
		var pa store.ProviderAccount
		pa, err = d.o.Connectors.CreateAccount(r.Context(), s.auditActor(), scope, connectors.CreateAccountInput{
			Connector: info.Name, Name: f.Get("name"), Mode: f.Get("mode"), Config: cfg, Currencies: f["currency"], Purposes: f["purpose"],
			StatementDescriptor: strings.TrimSpace(f.Get("statement_descriptor")), ProviderGSTIN: strings.ToUpper(strings.TrimSpace(f.Get("provider_gstin"))),
			AppID: f.Get("app_id")})
		if err == nil {
			http.Redirect(w, r, prefix+"/providers/"+pa.ID, http.StatusSeeOther)
			return
		}
	}
	var fe fieldErr
	switch {
	case errors.As(err, &fe), errors.Is(err, core.ErrInvalid), errors.Is(err, core.ErrNotFound):
		d.renderNewProvider(w, r, s, info, http.StatusBadRequest, err.Error(), f)
	default:
		d.fail(w, err)
	}
}

// loadProvider answers 404 for an unknown account or one of another org.
func (d *Dashboard) loadProvider(w http.ResponseWriter, r *http.Request) (store.ProviderAccount, core.OrgScope, bool) {
	scope, err := d.orgScope(r)
	if err != nil {
		d.fail(w, err)
		return store.ProviderAccount{}, scope, false
	}
	pa, err := d.o.Connectors.Account(r.Context(), scope, r.PathValue("id"))
	if errors.Is(err, core.ErrNotFound) {
		http.NotFound(w, r)
		return pa, scope, false
	}
	if err != nil {
		d.fail(w, err)
		return pa, scope, false
	}
	return pa, scope, true
}

// storedConfig decrypts an account's config; ok is false when it cannot be read.
func (d *Dashboard) storedConfig(pa store.ProviderAccount) map[string]any {
	raw, err := d.o.Box.Open(pa.ConfigEnc, []byte("provider_accounts.config_enc:"+pa.ID))
	if err != nil {
		return nil
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}

func (d *Dashboard) providerPage(w http.ResponseWriter, r *http.Request, s *session) {
	d.renderProvider(w, r, s, http.StatusOK, "", "", false, nil)
}

func (d *Dashboard) renderProvider(w http.ResponseWriter, r *http.Request, s *session, status int, errMsg, test string, testOK bool, post url.Values) {
	pa, scope, ok := d.loadProvider(w, r)
	if !ok {
		return
	}
	apps, err := d.appChoices(r, scope.OrgID)
	if err != nil {
		d.fail(w, err)
		return
	}
	display := pa.Connector
	f := &provForm{ID: pa.ID, Connector: pa.Connector, Name: pa.Name, Mode: pa.Mode, Status: pa.Status, StatementDescriptor: pa.StatementDescriptor,
		ProviderGSTIN: pa.ProviderGSTIN, Webhook: d.o.Connectors.WebhookURL(pa.ID), CanManage: s.can(authz.ProviderAccountManage), Test: test, TestOK: testOK}
	_, info, known := connector.Lookup(pa.Connector)
	if known {
		display = firstNonBlank(info.DisplayName, info.Name)
		f.Currencies = checks(info.Currencies, pa.AcceptedCurrencies)
		if f.CanManage {
			f.Fields = fillFields(schemaFields(info.ConfigSchema), d.storedConfig(pa), post)
		}
	} else {
		f.Unavailable = true
	}
	f.Display = display
	f.Purposes = checks([]string{"one_time", "subscription"}, pa.EnabledPurposes)
	err = d.o.DB.Read(r.Context(), func(q store.Querier) error {
		for _, a := range apps {
			ids, err := store.ListAppProviderAccountIDs(r.Context(), q, a.ID)
			if err != nil {
				return err
			}
			if slices.Contains(ids, pa.ID) {
				f.Linked = append(f.Linked, linkedApp{ID: a.ID, Name: a.Name})
			} else {
				f.Linkable = append(f.Linkable, appOpt{ID: a.ID, Name: a.Name})
			}
		}
		return nil
	})
	if err != nil {
		d.fail(w, err)
		return
	}
	v := d.shell(r, s, "providers", pa.Name)
	v.Prov, v.Error = f, errMsg
	d.render(w, status, "dash_provider", v)
}

func (d *Dashboard) providerUpdate(w http.ResponseWriter, r *http.Request, s *session) {
	pa, scope, ok := d.loadProvider(w, r)
	if !ok {
		return
	}
	_, info, known := connector.Lookup(pa.Connector)
	if !known {
		d.renderProvider(w, r, s, http.StatusBadRequest, "This connector is not available in this build.", "", false, nil)
		return
	}
	f := r.PostForm
	cfg, err := buildConfig(schemaFields(info.ConfigSchema), f, d.storedConfig(pa))
	if err == nil {
		name, status := strings.TrimSpace(f.Get("name")), f.Get("status")
		err = d.o.Connectors.UpdateAccount(r.Context(), s.auditActor(), scope, pa.ID, connectors.UpdateAccountInput{
			Name: &name, Status: &status, Config: cfg, Currencies: f["currency"], Purposes: f["purpose"]})
		if err == nil {
			http.Redirect(w, r, prefix+"/providers/"+pa.ID, http.StatusSeeOther)
			return
		}
	}
	var fe fieldErr
	switch {
	case errors.As(err, &fe), errors.Is(err, core.ErrInvalid):
		d.renderProvider(w, r, s, http.StatusBadRequest, err.Error(), "", false, f)
	case errors.Is(err, core.ErrNotFound):
		http.NotFound(w, r)
	default:
		d.fail(w, err)
	}
}

func (d *Dashboard) providerLink(w http.ResponseWriter, r *http.Request, s *session) {
	pa, scope, ok := d.loadProvider(w, r)
	if !ok {
		return
	}
	err := d.o.Connectors.LinkApp(r.Context(), s.auditActor(), scope, pa.ID, r.PostForm.Get("app_id"))
	switch {
	case err == nil:
		http.Redirect(w, r, prefix+"/providers/"+pa.ID, http.StatusSeeOther)
	case errors.Is(err, core.ErrNotFound):
		http.NotFound(w, r)
	case errors.Is(err, core.ErrInvalid):
		d.renderProvider(w, r, s, http.StatusBadRequest, err.Error(), "", false, nil)
	default:
		d.fail(w, err)
	}
}

func (d *Dashboard) providerTest(w http.ResponseWriter, r *http.Request, s *session) {
	pa, scope, ok := d.loadProvider(w, r)
	if !ok {
		return
	}
	res, err := d.o.Connectors.TestConnection(r.Context(), scope, pa.ID)
	if err != nil {
		d.fail(w, err)
		return
	}
	d.renderProvider(w, r, s, http.StatusOK, "", res.Message, res.OK, nil)
}
