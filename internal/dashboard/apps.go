package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // logo validation
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/authz"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/outbound"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
)

const (
	maxLogoBytes = 512 * 1024
	appTabKeys   = "keys"
)

var appTabs = []string{"keys", "webhooks", "branding", "providers", "settings"}

type appRow struct{ ID, Name, Slug, ReturnURL, Created string }

type keyRow struct {
	ID, Mode, Prefix, Created, LastUsed string
	Revoked                             bool
}

type endpointRow struct {
	ID, URL, Events string
	Enabled         bool
	Created         string
}

type brandView struct {
	Name, Accent, ReturnURL string
	HasLogo                 bool
}

type accountRow struct {
	ID, Name, Connector, Mode, Status, Currencies string
	Allowed                                       bool
}

type settingRow struct {
	Key, Value, Source string
	Overridden         bool
}

// settingsTable is what the shared settings partial renders: the rows, the
// URL prefix of the per-key forms, and whether the viewer may change them.
type settingsTable struct {
	Rows     []settingRow
	Base     string
	CanWrite bool
}

type appView struct {
	ID, Name, Slug, ReturnURL, Tab string
	Tabs                           []string

	Keys               []keyRow
	Endpoints          []endpointRow
	EndpointID         string
	EndpointDeliveries []deliveryRow
	Brand              brandView
	Accounts           []accountRow

	CanKeys, CanEndpoints, CanBrand, CanProviders, CanSettings, CanReplay bool
}

// secretReveal is a secret shown exactly once, right after creation.
type secretReveal struct{ Kind, Label, Value string }

func (d *Dashboard) routesApps() {
	m := d.mux
	m.HandleFunc("GET "+prefix+"/apps", d.authed(authz.SettingsRead, d.appsPage))
	m.HandleFunc("POST "+prefix+"/apps", d.authed(authz.AppCreate, d.appCreate))
	m.HandleFunc("GET "+prefix+"/apps/{id}", d.authed(authz.SettingsRead, d.appPage))
	m.HandleFunc("GET "+prefix+"/apps/{id}/logo", d.authed(authz.SettingsRead, d.logoGet))
	m.HandleFunc("POST "+prefix+"/apps/{id}/keys", d.authed(authz.APIKeyCreate, d.keyCreate))
	m.HandleFunc("POST "+prefix+"/apps/{id}/keys/{kid}/revoke", d.authed(authz.APIKeyCreate, d.keyRevoke))
	m.HandleFunc("POST "+prefix+"/apps/{id}/endpoints", d.authed(authz.SettingsWrite, d.endpointCreate))
	m.HandleFunc("POST "+prefix+"/apps/{id}/endpoints/{eid}/disable", d.authed(authz.SettingsWrite, d.endpointDisable))
	m.HandleFunc("POST "+prefix+"/apps/{id}/branding", d.authed(authz.SettingsWrite, d.brandingSave))
	m.HandleFunc("POST "+prefix+"/apps/{id}/providers", d.authed(authz.ProviderAccountManage, d.providersSave))
	m.HandleFunc("POST "+prefix+"/apps/{id}/settings/{key}", d.authed(authz.SettingsWrite, d.appSettingSet))
	m.HandleFunc("POST "+prefix+"/apps/{id}/settings/{key}/reset", d.authed(authz.SettingsWrite, d.appSettingReset))
}

func (d *Dashboard) appsPage(w http.ResponseWriter, r *http.Request, s *session) {
	d.renderApps(w, r, s, http.StatusOK, "")
}

func (d *Dashboard) renderApps(w http.ResponseWriter, r *http.Request, s *session, status int, errMsg string) {
	var apps []store.App
	err := d.o.DB.Read(r.Context(), func(q store.Querier) error {
		org, err := store.FirstOrgID(r.Context(), q)
		if err != nil {
			return err
		}
		apps, err = store.ListApps(r.Context(), q, org)
		return err
	})
	if err != nil {
		d.fail(w, err)
		return
	}
	v := d.shell(r, s, "apps", "Apps")
	v.Error = errMsg
	v.CanManage = s.can(authz.AppCreate)
	for _, a := range apps {
		v.Apps = append(v.Apps, appRow{ID: a.ID, Name: a.Name, Slug: a.Slug, ReturnURL: a.DefaultReturnURL, Created: d.fmtTime(a.CreatedAt)})
	}
	d.render(w, status, "dash_apps", v)
}

func validHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != ""
}

func (d *Dashboard) appCreate(w http.ResponseWriter, r *http.Request, s *session) {
	app, err := d.o.Core.CreateApp(r.Context(), s.auditActor(), core.CreateAppInput{
		Name: r.PostForm.Get("name"), Slug: strings.TrimSpace(r.PostForm.Get("slug")), DefaultReturnURL: strings.TrimSpace(r.PostForm.Get("return_url"))})
	switch {
	case err == nil:
		http.Redirect(w, r, prefix+"/apps/"+app.ID, http.StatusSeeOther)
	case errors.Is(err, core.ErrConflict):
		d.renderApps(w, r, s, http.StatusConflict, err.Error())
	case errors.Is(err, core.ErrInvalid):
		d.renderApps(w, r, s, http.StatusBadRequest, err.Error())
	default:
		d.fail(w, err)
	}
}

// loadApp resolves {id} to an app, answering 404 itself when it is missing.
func (d *Dashboard) loadApp(w http.ResponseWriter, r *http.Request) (store.App, bool) {
	var app store.App
	err := d.o.DB.Read(r.Context(), func(q store.Querier) (err error) { app, err = store.GetApp(r.Context(), q, r.PathValue("id")); return })
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return app, false
	}
	if err != nil {
		d.fail(w, err)
		return app, false
	}
	return app, true
}

func (d *Dashboard) appPage(w http.ResponseWriter, r *http.Request, s *session) {
	app, ok := d.loadApp(w, r)
	if !ok {
		return
	}
	tab := r.URL.Query().Get("tab")
	if !slices.Contains(appTabs, tab) {
		tab = appTabKeys
	}
	d.renderApp(w, r, s, app, http.StatusOK, tab, "", nil)
}

type brandingDoc struct {
	Name       string `json:"name,omitempty"`
	Accent     string `json:"accent,omitempty"`
	LogoBlobID string `json:"logo_blob_id,omitempty"`
}

func parseBranding(raw string) brandingDoc {
	var b brandingDoc
	_ = json.Unmarshal([]byte(raw), &b) // an unreadable document shows as empty
	return b
}

// renderApp renders one tab of the app page. reveal is a secret to show once.
func (d *Dashboard) renderApp(w http.ResponseWriter, r *http.Request, s *session, app store.App, status int, tab, errMsg string, reveal *secretReveal) {
	ctx := r.Context()
	v := d.shell(r, s, "apps", app.Name)
	v.Error = errMsg
	v.Reveal = reveal
	av := &appView{ID: app.ID, Name: app.Name, Slug: app.Slug, ReturnURL: app.DefaultReturnURL, Tab: tab, Tabs: appTabs,
		CanKeys: s.can(authz.APIKeyCreate), CanEndpoints: s.can(authz.SettingsWrite), CanBrand: s.can(authz.SettingsWrite),
		CanProviders: s.can(authz.ProviderAccountManage), CanSettings: s.can(authz.SettingsWrite), CanReplay: s.can(authz.WebhookReplay)}
	v.App = av

	var err error
	switch tab {
	case "keys":
		var keys []store.APIKey
		err = d.o.DB.Read(ctx, func(q store.Querier) (err error) { keys, err = store.ListAPIKeys(ctx, q, app.ID); return })
		for _, k := range keys {
			av.Keys = append(av.Keys, keyRow{ID: k.ID, Mode: k.Mode, Prefix: k.Prefix, Created: d.fmtTime(k.CreatedAt),
				LastUsed: d.fmtTime(k.LastUsedAt), Revoked: !k.RevokedAt.IsZero()})
		}
	case "webhooks":
		var eps []outbound.Endpoint
		eps, err = d.o.Outbound.ListEndpoints(ctx, app.ID)
		for _, ep := range eps {
			av.Endpoints = append(av.Endpoints, endpointRow{ID: ep.ID, URL: ep.URL, Events: strings.Join(ep.EventTypes, ", "), Enabled: ep.Enabled})
			if ep.ID == r.URL.Query().Get("endpoint") {
				av.EndpointID = ep.ID
			}
		}
		if err == nil && av.EndpointID != "" {
			var dels []store.DeliveryListRow
			err = d.o.DB.Read(ctx, func(q store.Querier) (err error) {
				dels, err = store.ListEndpointDeliveries(ctx, q, av.EndpointID, 50)
				return
			})
			for _, dl := range dels {
				av.EndpointDeliveries = append(av.EndpointDeliveries, deliveryRow{ID: dl.DeliveryID, EventType: dl.EventType, Endpoint: dl.EndpointURL,
					Status: dl.Status, Attempts: dl.AttemptCount, LastStatus: dl.LastStatus, LastError: dl.LastError, At: d.fmtTime(dl.UpdatedAt)})
			}
		}
	case "branding":
		var raw string
		err = d.o.DB.Read(ctx, func(q store.Querier) (err error) { raw, err = store.GetAppBranding(ctx, q, app.ID); return })
		b := parseBranding(raw)
		av.Brand = brandView{Name: b.Name, Accent: b.Accent, ReturnURL: app.DefaultReturnURL, HasLogo: b.LogoBlobID != ""}
	case "providers":
		var accts []store.ProviderAccountInfo
		var allowed []string
		err = d.o.DB.Read(ctx, func(q store.Querier) (err error) {
			if accts, err = store.ListProviderAccounts(ctx, q, app.OrgID, ""); err != nil {
				return
			}
			allowed, err = store.ListAppProviderAccountIDs(ctx, q, app.ID)
			return
		})
		for _, a := range accts {
			av.Accounts = append(av.Accounts, accountRow{ID: a.ID, Name: a.Name, Connector: a.Connector, Mode: a.Mode, Status: a.Status,
				Currencies: strings.Join(a.Currencies, ", "), Allowed: slices.Contains(allowed, a.ID)})
		}
	case "settings":
		var rows []settingRow
		rows, err = d.settingRows(ctx, app.ID, app.OrgID, settings.LevelApp)
		v.Table = &settingsTable{Rows: rows, Base: prefix + "/apps/" + app.ID + "/settings", CanWrite: s.can(authz.SettingsWrite)}
	}
	if err != nil {
		d.fail(w, err)
		return
	}
	d.render(w, status, "dash_app", v)
}

func (d *Dashboard) appRedirect(w http.ResponseWriter, r *http.Request, app store.App, tab string) {
	http.Redirect(w, r, prefix+"/apps/"+app.ID+"?tab="+tab, http.StatusSeeOther)
}

func (d *Dashboard) keyCreate(w http.ResponseWriter, r *http.Request, s *session) {
	app, ok := d.loadApp(w, r)
	if !ok {
		return
	}
	token, _, err := d.o.Core.CreateAPIKey(r.Context(), s.auditActor(), app.ID, r.PostForm.Get("mode"))
	switch {
	case err == nil:
		d.renderApp(w, r, s, app, http.StatusOK, "keys", "", &secretReveal{Kind: "key", Label: "API key (" + r.PostForm.Get("mode") + ")", Value: token})
	case errors.Is(err, core.ErrInvalid):
		d.renderApp(w, r, s, app, http.StatusBadRequest, "keys", err.Error(), nil)
	default:
		d.fail(w, err)
	}
}

func (d *Dashboard) keyRevoke(w http.ResponseWriter, r *http.Request, s *session) {
	app, ok := d.loadApp(w, r)
	if !ok {
		return
	}
	kid := r.PathValue("kid")
	org, err := d.orgID(r.Context())
	if err != nil {
		d.fail(w, err)
		return
	}
	revoked := false
	err = d.o.DB.Write(r.Context(), func(tx store.Tx) error {
		var err error
		if revoked, err = store.RevokeAPIKey(r.Context(), tx, app.ID, kid, d.o.Now()); err != nil || !revoked {
			return err
		}
		return audit.Record(r.Context(), tx, audit.Entry{OrgID: org, Actor: s.auditActor(), Action: "api_key.revoke", Target: kid,
			After: map[string]string{"app_id": app.ID}, IP: clientIP(r)})
	})
	if err != nil {
		d.fail(w, err)
		return
	}
	if !revoked {
		http.NotFound(w, r)
		return
	}
	d.appRedirect(w, r, app, "keys")
}

func (d *Dashboard) endpointCreate(w http.ResponseWriter, r *http.Request, s *session) {
	app, ok := d.loadApp(w, r)
	if !ok {
		return
	}
	raw := strings.TrimSpace(r.PostForm.Get("url"))
	if !validHTTPURL(raw) {
		d.renderApp(w, r, s, app, http.StatusBadRequest, "webhooks", "The endpoint URL must be an absolute http(s) URL.", nil)
		return
	}
	var types []string
	for _, t := range strings.Split(r.PostForm.Get("events"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			types = append(types, t)
		}
	}
	ep, secret, err := d.o.Outbound.CreateEndpoint(r.Context(), outbound.CreateEndpointInput{OrgID: app.OrgID, AppID: app.ID, URL: raw, EventTypes: types})
	if err != nil {
		d.fail(w, err)
		return
	}
	if err := d.record(r.Context(), r, s, "webhook_endpoint.create", ep.ID, nil, map[string]any{"app_id": app.ID, "url": ep.URL, "event_types": ep.EventTypes}); err != nil {
		d.fail(w, err)
		return
	}
	d.renderApp(w, r, s, app, http.StatusOK, "webhooks", "", &secretReveal{Kind: "webhook", Label: "Signing secret for " + ep.URL, Value: secret})
}

func (d *Dashboard) endpointDisable(w http.ResponseWriter, r *http.Request, s *session) {
	app, ok := d.loadApp(w, r)
	if !ok {
		return
	}
	eid := r.PathValue("eid")
	ep, err := d.o.DB.GetEndpoint(r.Context(), eid)
	if errors.Is(err, store.ErrNotFound) || err == nil && ep.AppID != app.ID {
		http.NotFound(w, r)
		return
	}
	if err == nil {
		err = d.o.Outbound.DisableEndpoint(r.Context(), eid)
	}
	if err == nil {
		err = d.record(r.Context(), r, s, "webhook_endpoint.disable", eid, map[string]any{"enabled": true}, map[string]any{"enabled": false})
	}
	if err != nil {
		d.fail(w, err)
		return
	}
	d.appRedirect(w, r, app, "webhooks")
}

var accentRE = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// sniffLogo returns the content type of a PNG or JPEG logo, verifying that the
// bytes really decode as such an image.
func sniffLogo(data []byte) (string, error) {
	ct := http.DetectContentType(data)
	if ct != "image/png" && ct != "image/jpeg" {
		return "", errors.New("the logo must be a PNG or JPEG image")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width == 0 || cfg.Height == 0 || "image/"+format != ct {
		return "", errors.New("the logo must be a PNG or JPEG image")
	}
	return ct, nil
}

func (d *Dashboard) brandingSave(w http.ResponseWriter, r *http.Request, s *session) {
	app, ok := d.loadApp(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	reject := func(msg string) { d.renderApp(w, r, s, app, http.StatusBadRequest, "branding", msg, nil) }
	name := strings.TrimSpace(r.PostForm.Get("display_name"))
	accent := strings.TrimSpace(r.PostForm.Get("accent"))
	ret := strings.TrimSpace(r.PostForm.Get("return_url"))
	switch {
	case len([]rune(name)) > 100:
		reject("The display name can be at most 100 characters.")
		return
	case accent != "" && !accentRE.MatchString(accent):
		reject("The accent colour must look like #1a2b3c.")
		return
	case ret != "" && !validHTTPURL(ret):
		reject("The default return URL must be an absolute http(s) URL.")
		return
	}

	var rawBefore string
	if err := d.o.DB.Read(ctx, func(q store.Querier) (err error) { rawBefore, err = store.GetAppBranding(ctx, q, app.ID); return }); err != nil {
		d.fail(w, err)
		return
	}
	before := parseBranding(rawBefore)
	after := brandingDoc{Name: name, Accent: accent, LogoBlobID: before.LogoBlobID}
	if r.PostForm.Get("remove_logo") != "" {
		after.LogoBlobID = ""
	}

	var blob *store.Blob
	if r.MultipartForm != nil {
		if fhs := r.MultipartForm.File["logo"]; len(fhs) > 0 && fhs[0].Filename != "" {
			if fhs[0].Size > maxLogoBytes {
				reject("The logo must be 512 KB or smaller.")
				return
			}
			f, err := fhs[0].Open()
			if err != nil {
				d.fail(w, err)
				return
			}
			data, err := io.ReadAll(io.LimitReader(f, maxLogoBytes+1))
			f.Close()
			if err != nil {
				d.fail(w, err)
				return
			}
			if len(data) > maxLogoBytes {
				reject("The logo must be 512 KB or smaller.")
				return
			}
			ct, err := sniffLogo(data)
			if err != nil {
				reject(err.Error())
				return
			}
			blob = &store.Blob{ID: ids.New("blob"), OrgID: app.OrgID, Kind: "logo", ContentType: ct, Data: data, CreatedAt: d.o.Now()}
			after.LogoBlobID = blob.ID
		}
	}
	doc, _ := json.Marshal(after)
	err := d.o.DB.Write(ctx, func(tx store.Tx) error {
		if blob != nil {
			if err := store.InsertBlob(ctx, tx, *blob); err != nil {
				return err
			}
		}
		if err := store.SetAppBranding(ctx, tx, app.ID, string(doc), d.o.Now()); err != nil {
			return err
		}
		if err := store.SetAppReturnURL(ctx, tx, app.ID, ret, d.o.Now()); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{OrgID: app.OrgID, Actor: s.auditActor(), Action: "app.update", Target: app.ID,
			Before: map[string]any{"branding": before, "default_return_url": app.DefaultReturnURL},
			After:  map[string]any{"branding": after, "default_return_url": ret}, IP: clientIP(r)})
	})
	if err != nil {
		d.fail(w, err)
		return
	}
	d.appRedirect(w, r, app, "branding")
}

func (d *Dashboard) logoGet(w http.ResponseWriter, r *http.Request, s *session) {
	app, ok := d.loadApp(w, r)
	if !ok {
		return
	}
	var blob store.Blob
	err := d.o.DB.Read(r.Context(), func(q store.Querier) error {
		raw, err := store.GetAppBranding(r.Context(), q, app.ID)
		if err != nil {
			return err
		}
		id := parseBranding(raw).LogoBlobID
		if id == "" {
			return store.ErrNotFound
		}
		blob, err = store.GetBlob(r.Context(), q, id)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		d.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", blob.ContentType)
	w.Header().Set("Content-Length", fmt.Sprint(len(blob.Data)))
	_, _ = w.Write(blob.Data)
}

func (d *Dashboard) providersSave(w http.ResponseWriter, r *http.Request, s *session) {
	app, ok := d.loadApp(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	want := r.PostForm["account"]
	var accts []store.ProviderAccountInfo
	var before []string
	err := d.o.DB.Read(ctx, func(q store.Querier) (err error) {
		if accts, err = store.ListProviderAccounts(ctx, q, app.OrgID, ""); err != nil {
			return
		}
		before, err = store.ListAppProviderAccountIDs(ctx, q, app.ID)
		return
	})
	if err != nil {
		d.fail(w, err)
		return
	}
	known := map[string]bool{}
	for _, a := range accts {
		known[a.ID] = true
	}
	for _, id := range want {
		if !known[id] {
			d.renderApp(w, r, s, app, http.StatusBadRequest, "providers", "Unknown provider account "+id+".", nil)
			return
		}
	}
	slices.Sort(want)
	want = slices.Compact(want)
	err = d.o.DB.Write(ctx, func(tx store.Tx) error {
		if err := store.SetAppProviderAccounts(ctx, tx, app.ID, want); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{OrgID: app.OrgID, Actor: s.auditActor(), Action: "app.providers.update", Target: app.ID,
			Before: map[string]any{"provider_accounts": before}, After: map[string]any{"provider_accounts": want}, IP: clientIP(r)})
	})
	if err != nil {
		d.fail(w, err)
		return
	}
	d.appRedirect(w, r, app, "providers")
}

// settingRows lists every setting settable at level with its effective value
// and where it comes from. appID empty means the org page.
func (d *Dashboard) settingRows(ctx context.Context, appID, orgID string, level settings.Level) ([]settingRow, error) {
	var rows []settingRow
	for _, def := range settings.Defs() {
		if def.Levels&level == 0 {
			continue
		}
		var val settings.Value
		var err error
		if appID != "" {
			val, err = d.o.Settings.Resolve(ctx, appID, def.Key)
		} else {
			val, err = d.o.Settings.ResolveOrg(ctx, orgID, def.Key)
		}
		if err != nil {
			return nil, err
		}
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, val.Raw, "", "  "); err != nil {
			pretty.Reset()
			pretty.Write(val.Raw)
		}
		rows = append(rows, settingRow{Key: def.Key, Value: pretty.String(), Source: string(val.Source),
			Overridden: val.Source == settings.SourceApp})
	}
	return rows, nil
}

// settingErr maps settings errors to a message for the form; ok is false for
// errors that are not the operator's doing.
func settingErr(err error) (string, bool) {
	if errors.Is(err, settings.ErrInvalid) || errors.Is(err, settings.ErrUnknownKey) || errors.Is(err, settings.ErrWrongLevel) {
		return err.Error(), true
	}
	return "", false
}

func (d *Dashboard) appSettingSet(w http.ResponseWriter, r *http.Request, s *session) {
	app, ok := d.loadApp(w, r)
	if !ok {
		return
	}
	err := d.o.Settings.SetApp(r.Context(), s.auditActor(), app.ID, r.PathValue("key"), json.RawMessage(strings.TrimSpace(r.PostForm.Get("value"))))
	if msg, bad := settingErr(err); bad {
		d.renderApp(w, r, s, app, http.StatusBadRequest, "settings", msg, nil)
		return
	}
	if err != nil {
		d.fail(w, err)
		return
	}
	d.appRedirect(w, r, app, "settings")
}

func (d *Dashboard) appSettingReset(w http.ResponseWriter, r *http.Request, s *session) {
	app, ok := d.loadApp(w, r)
	if !ok {
		return
	}
	err := d.o.Settings.ResetApp(r.Context(), s.auditActor(), app.ID, r.PathValue("key"))
	if msg, bad := settingErr(err); bad {
		d.renderApp(w, r, s, app, http.StatusBadRequest, "settings", msg, nil)
		return
	}
	if err != nil {
		d.fail(w, err)
		return
	}
	d.appRedirect(w, r, app, "settings")
}
