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

	"github.com/velixio/paymux/internal/authz"
	"github.com/velixio/paymux/internal/connectors"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/secrets"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/pkg/connector"
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
	WebhookSecret     bool
	Generate          bool
	Enum              []enumOpt
	Value             string
	Checked           bool // booleans
	HasValue          bool // a secret that is stored (never shown)
	// Hidden fields are kept in the config but never shown (x-hidden).
	Hidden bool
	// OAuthRole says what the property is to the OAuth connect flow (x-oauth):
	// client_id, client_secret, refresh_token or scopes.
	OAuthRole string
}

type checkOpt struct {
	Value   string
	Checked bool
}

type linkedApp struct{ ID, Name string }

type providerEventView struct {
	ID, ProviderID, Received, Processed, Error, Headers, Payload string
	Verified                                                     bool
	Matches                                                      []providerEventMatchView
}

type providerEventMatchView struct{ Kind, ID, ProviderID, PaymentID string }

type provForm struct {
	New                                                bool
	ID, Connector, Display, Name, Mode, Status         string
	StatementDescriptor, ProviderGSTIN, Webhook, AppID string
	Fields                                             []cfgField
	WebhookField                                       *cfgField
	Currencies, Purposes                               []checkOpt
	WebhookEvents                                      []string
	Linked                                             []linkedApp
	Linkable                                           []appOpt
	CanManage                                          bool
	Test                                               string
	TestOK                                             bool
	CanAutoWebhook                                     bool
	WebhookPerCheckout                                 bool
	HasWebhookField                                    bool
	HasWebhookSecret                                   bool
	Unavailable                                        bool
	OAuth                                              *oauthPanel
	Inbound                                            []providerEventView
}

func (d *Dashboard) routesProviders() {
	m := d.mux
	m.HandleFunc("GET "+prefix+"/providers", d.authed(authz.SettingsRead, d.providersPage))
	m.HandleFunc("GET "+prefix+"/providers/new", d.authed(authz.ProviderAccountManage, d.providerNew))
	m.HandleFunc("POST "+prefix+"/providers", d.authed(authz.ProviderAccountManage, d.recent(d.providerCreate)))
	m.HandleFunc("GET "+prefix+"/providers/{id}", d.authed(authz.SettingsRead, d.providerPage))
	m.HandleFunc("POST "+prefix+"/providers/{id}", d.authed(authz.ProviderAccountManage, d.recent(d.providerUpdate)))
	m.HandleFunc("POST "+prefix+"/providers/{id}/apps", d.authed(authz.ProviderAccountManage, d.recent(d.providerLink)))
	m.HandleFunc("POST "+prefix+"/providers/{id}/test", d.authed(authz.ProviderAccountManage, d.providerTest))
	m.HandleFunc("POST "+prefix+"/providers/{id}/webhook/configure", d.authed(authz.ProviderAccountManage, d.recent(d.providerWebhookConfigure)))
	m.HandleFunc("POST "+prefix+"/providers/{id}/webhook/secret", d.authed(authz.ProviderAccountManage, d.recent(d.providerWebhookSecret)))
	d.routesOAuth()
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
			Type     string `json:"type"`
			Title    string `json:"title"`
			Enum     []any  `json:"enum"`
			Secret   bool   `json:"x-secret"`
			Hidden   bool   `json:"x-hidden"`
			OAuth    string `json:"x-oauth"`
			Webhook  bool   `json:"x-webhook-secret"`
			Generate bool   `json:"x-generate"`
			Default  any    `json:"default"`
		}
		if err := dec.Decode(&p); err != nil {
			break
		}
		name := kt.(string)
		f := cfgField{Name: name, Title: firstNonBlank(p.Title, name), Type: p.Type, Secret: p.Secret, Required: slices.Contains(doc.Required, name),
			Hidden: p.Hidden, OAuthRole: p.OAuth, WebhookSecret: p.Webhook, Generate: p.Generate}
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
		case f.Hidden:
		case f.Secret:
			_, f.HasValue = existing[f.Name]
			f.HasValue = f.HasValue && fmt.Sprint(existing[f.Name]) != ""
		case post != nil && f.Type == "boolean":
			f.Checked = post.Get(key) != ""
		case post != nil:
			f.Value = post.Get(key)
		case existing != nil && f.Type == "boolean":
			f.Checked, _ = existing[f.Name].(bool)
		case existing != nil && f.Type == "object":
			if v, ok := existing[f.Name]; ok {
				b, _ := json.MarshalIndent(v, "", "  ")
				f.Value = string(b)
			}
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
		case f.Hidden: // not on the form: whatever was stored stays
			if v, ok := existing[f.Name]; ok {
				cfg[f.Name] = v
			}
			continue
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
		case f.Type == "object":
			var obj map[string]any
			if err := json.Unmarshal([]byte(raw), &obj); err != nil || obj == nil {
				return nil, fieldErr(f.Title + " must be a JSON object")
			}
			cfg[f.Name] = obj
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
	if err := d.o.DB.Read(ctx, func(q store.Querier) (err error) {
		accts, err = store.ListProviderAccounts(ctx, q, org, d.mode(r, s))
		return
	}); err != nil {
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
	f := &provForm{New: true, Connector: info.Name, Display: firstNonBlank(info.DisplayName, info.Name), Mode: d.mode(r, s), CanManage: true,
		Fields: fillFields(schemaFields(info.ConfigSchema), nil, post)}
	on, onP := info.Currencies, []string{"one_time"}
	if post != nil {
		f.Name, f.Mode, f.StatementDescriptor, f.ProviderGSTIN, f.AppID = post.Get("name"), d.mode(r, s), post.Get("statement_descriptor"), post.Get("provider_gstin"), post.Get("app_id")
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
	if !postedModeMatches(r, d.mode(r, s)) {
		d.renderNewProvider(w, r, s, info, http.StatusBadRequest, "The requested mode does not match the dashboard mode. Switch modes before creating the account.", f)
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
			Connector: info.Name, Name: f.Get("name"), Mode: d.mode(r, s), Config: cfg, Currencies: f["currency"], Purposes: f["purpose"],
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
func (d *Dashboard) loadProvider(w http.ResponseWriter, r *http.Request, s *session) (store.ProviderAccount, core.OrgScope, bool) {
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
	if pa.Mode != d.mode(r, s) {
		http.NotFound(w, r)
		return pa, scope, false
	}
	return pa, scope, true
}

// storedConfig decrypts an account's config; ok is false when it cannot be read.
func (d *Dashboard) storedConfig(pa store.ProviderAccount) map[string]any {
	raw, err := d.o.Box.Open(pa.ConfigEnc, secrets.AADProviderConfig(pa.ID))
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
	pa, scope, ok := d.loadProvider(w, r, s)
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
		stored := d.storedConfig(pa)
		fields := fillFields(schemaFields(info.ConfigSchema), stored, post)
		f.CanAutoWebhook = info.Has(connector.CapWebhookConfig)
		f.WebhookEvents = append([]string(nil), info.WebhookEvents...)
		// Cashfree has no endpoint-registration step: its Orders API accepts a
		// notify_url and the connector sends this account URL on every checkout.
		f.WebhookPerCheckout = pa.Connector == "cashfree"
		for _, field := range fields {
			if field.WebhookSecret {
				f.HasWebhookField = true
				fieldCopy := field
				f.WebhookField = &fieldCopy
				if field.HasValue {
					f.HasWebhookSecret = true
				}
			}
		}
		if f.CanManage {
			f.Fields = fields
			if info.Has(connector.CapOAuth) {
				f.OAuth = d.buildOAuthPanel(r.Context(), pa, info, stored, post)
			}
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
		events, err := store.ListProviderEvents(r.Context(), q, pa.ID, 20)
		if err != nil {
			return err
		}
		for _, event := range events {
			row := providerEventView{ID: event.ID, ProviderID: event.ProviderEventID, Verified: event.Verified,
				Received: d.fmtTime(event.ReceivedAt), Processed: d.fmtTime(event.ProcessedAt), Error: event.Error,
				Headers: string(event.Headers), Payload: string(event.Body)}
			matches, err := store.ListProviderEventMatches(r.Context(), q, event.ID)
			if err != nil {
				return err
			}
			for _, match := range matches {
				row.Matches = append(row.Matches, providerEventMatchView{Kind: match.ObjectType, ID: match.ObjectID,
					ProviderID: match.ProviderID, PaymentID: match.PaymentID})
			}
			f.Inbound = append(f.Inbound, row)
		}
		return nil
	})
	if err != nil {
		d.fail(w, err)
		return
	}
	v := d.shell(r, s, "providers", pa.Name)
	v.Prov, v.Error = f, errMsg
	switch n := r.URL.Query().Get("n"); n {
	case "oauth_ok":
		v.Notice = oauthNotices[n]
	case "oauth_denied":
		v.Error = "The authorization was not approved at the provider, so nothing was changed."
	case "oauth_failed":
		v.Error = "The provider returned an error, so nothing was changed. Try Connect again."
	}
	d.render(w, status, "dash_provider", v)
}

func (d *Dashboard) providerUpdate(w http.ResponseWriter, r *http.Request, s *session) {
	pa, scope, ok := d.loadProvider(w, r, s)
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
	pa, scope, ok := d.loadProvider(w, r, s)
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
	pa, scope, ok := d.loadProvider(w, r, s)
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

func (d *Dashboard) providerWebhookConfigure(w http.ResponseWriter, r *http.Request, s *session) {
	pa, scope, ok := d.loadProvider(w, r, s)
	if !ok {
		return
	}
	res, err := d.o.Connectors.ConfigureWebhook(r.Context(), s.auditActor(), scope, pa.ID)
	if err != nil {
		d.renderProvider(w, r, s, http.StatusBadGateway, "", "Webhook configuration failed: "+err.Error(), false, nil)
		return
	}
	d.renderProvider(w, r, s, http.StatusOK, "", res.Message, true, nil)
}

func (d *Dashboard) providerWebhookSecret(w http.ResponseWriter, r *http.Request, s *session) {
	pa, scope, ok := d.loadProvider(w, r, s)
	if !ok {
		return
	}
	_, info, known := connector.Lookup(pa.Connector)
	if !known {
		d.renderProvider(w, r, s, http.StatusBadRequest, "", "This connector is not available in this build.", false, nil)
		return
	}
	name := ""
	for _, field := range schemaFields(info.ConfigSchema) {
		if field.WebhookSecret {
			name = field.Name
			break
		}
	}
	secret := strings.TrimSpace(r.PostForm.Get("webhook_secret"))
	if name == "" || secret == "" {
		d.renderProvider(w, r, s, http.StatusBadRequest, "", "Enter or generate a webhook signing secret before saving.", false, nil)
		return
	}
	cfg := d.storedConfig(pa)
	if cfg == nil {
		d.fail(w, errors.New("provider config could not be decrypted"))
		return
	}
	cfg[name] = secret
	raw, err := json.Marshal(cfg)
	if err == nil {
		err = d.o.Connectors.UpdateAccount(r.Context(), s.auditActor(), scope, pa.ID, connectors.UpdateAccountInput{Config: raw})
	}
	if err != nil {
		d.renderProvider(w, r, s, http.StatusBadRequest, "", "Webhook secret was not saved: "+err.Error(), false, nil)
		return
	}
	d.renderProvider(w, r, s, http.StatusOK, "", "Webhook signing secret saved securely.", true, nil)
}
