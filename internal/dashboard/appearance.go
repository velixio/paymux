package dashboard

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/paymux/paymux/internal/authz"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/web"
)

type themeRow struct {
	Name, Version, Description string
	Bundled, Usable, Current   bool
	Err                        string
	Tokens                     []tokenRow
}

type tokenRow struct{ Name, Value string }

type appearanceRow struct {
	ID, Name         string
	Theme, Language  string // the app's own values; empty: inherits
	EffectiveTheme   string
	EffectiveLang    string
	InheritsTheme    bool
	InheritsLanguage bool
}

func (d *Dashboard) routesAppearance() {
	m := d.mux
	m.HandleFunc("GET "+prefix+"/appearance", d.authed(authz.SettingsRead, d.appearancePage))
	m.HandleFunc("GET "+prefix+"/appearance/preview/{name}", d.authed(authz.SettingsRead, d.themePreview))
	m.HandleFunc("POST "+prefix+"/appearance/theme", d.authed(authz.SettingsWrite, d.themeSet))
	m.HandleFunc("POST "+prefix+"/appearance/apps/{id}", d.authed(authz.SettingsWrite, d.appAppearanceSet))
}

// usableThemes maps the names of themes that load cleanly.
func (d *Dashboard) installedThemes() []web.ThemeInfo {
	if d.o.Themes == nil {
		return nil
	}
	return d.o.Themes.List()
}

func (d *Dashboard) appearancePage(w http.ResponseWriter, r *http.Request, s *session) {
	d.renderAppearance(w, r, s, http.StatusOK, "")
}

func (d *Dashboard) renderAppearance(w http.ResponseWriter, r *http.Request, s *session, status int, errMsg string) {
	ctx := r.Context()
	org, err := d.orgID(ctx)
	if err != nil {
		d.fail(w, err)
		return
	}
	cur := d.dashboardTheme(ctx)
	v := d.shell(r, s, "appearance", "Appearance")
	v.Error = errMsg
	v.CanManage = s.can(authz.SettingsWrite)
	v.CurrentTheme = cur
	for _, ti := range d.installedThemes() {
		row := themeRow{Name: ti.Name, Version: ti.Version, Description: ti.Description, Bundled: ti.Bundled, Err: ti.Err,
			Usable: ti.Err == "", Current: ti.Name == cur}
		if row.Usable {
			if th, err := d.o.Themes.Load(ti.Name); err == nil {
				keys := make([]string, 0, len(th.Tokens))
				for k := range th.Tokens {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					row.Tokens = append(row.Tokens, tokenRow{k, th.Tokens[k]})
				}
			}
		}
		v.ThemeRows = append(v.ThemeRows, row)
	}
	v.Languages = d.o.Renderer.Languages()

	var apps []store.App
	if err := d.o.DB.Read(ctx, func(q store.Querier) (err error) { apps, err = store.ListApps(ctx, q, org); return }); err != nil {
		d.fail(w, err)
		return
	}
	for _, a := range apps {
		row := appearanceRow{ID: a.ID, Name: a.Name}
		for _, key := range []string{"theme", "customer_language"} {
			val, err := d.o.Settings.Resolve(ctx, a.ID, key)
			if err != nil {
				d.fail(w, err)
				return
			}
			var str string
			_ = val.Decode(&str)
			own := ""
			if val.Source == settings.SourceApp {
				own = str
			}
			if key == "theme" {
				row.Theme, row.EffectiveTheme = own, str
			} else {
				row.Language, row.EffectiveLang = own, str
			}
		}
		v.AppearanceRows = append(v.AppearanceRows, row)
	}
	d.render(w, status, "dash_appearance", v)
}

// themeUsable reports whether name is an installed theme that loads cleanly.
func (d *Dashboard) themeUsable(name string) bool {
	if !web.ValidThemeName(name) {
		return false
	}
	for _, ti := range d.installedThemes() {
		if ti.Name == name {
			return ti.Err == ""
		}
	}
	return false
}

func (d *Dashboard) themeSet(w http.ResponseWriter, r *http.Request, s *session) {
	name := r.PostForm.Get("theme")
	if !d.themeUsable(name) {
		d.renderAppearance(w, r, s, http.StatusBadRequest, "That theme is not installed or cannot be used.")
		return
	}
	org, err := d.orgID(r.Context())
	if err != nil {
		d.fail(w, err)
		return
	}
	raw, _ := json.Marshal(name)
	if err := d.o.Settings.SetOrg(r.Context(), s.auditActor(), org, "theme", raw); err != nil {
		if msg, bad := settingErr(err); bad {
			d.renderAppearance(w, r, s, http.StatusBadRequest, msg)
			return
		}
		d.fail(w, err)
		return
	}
	http.Redirect(w, r, prefix+"/appearance", http.StatusSeeOther)
}

func (d *Dashboard) appAppearanceSet(w http.ResponseWriter, r *http.Request, s *session) {
	app, ok := d.loadApp(w, r)
	if !ok {
		return
	}
	theme, lang := r.PostForm.Get("theme"), r.PostForm.Get("language")
	if theme != "" && !d.themeUsable(theme) {
		d.renderAppearance(w, r, s, http.StatusBadRequest, "That theme is not installed or cannot be used.")
		return
	}
	if lang != "" && !d.o.Renderer.HasLang(lang) {
		d.renderAppearance(w, r, s, http.StatusBadRequest, "That language is not installed.")
		return
	}
	for key, val := range map[string]string{"theme": theme, "customer_language": lang} {
		var err error
		if val == "" {
			err = d.o.Settings.ResetApp(r.Context(), s.auditActor(), app.OrgID, app.ID, key)
		} else {
			raw, _ := json.Marshal(val)
			err = d.o.Settings.SetApp(r.Context(), s.auditActor(), app.OrgID, app.ID, key, raw)
		}
		if msg, bad := settingErr(err); bad {
			d.renderAppearance(w, r, s, http.StatusBadRequest, msg)
			return
		}
		if err != nil {
			d.fail(w, err)
			return
		}
	}
	http.Redirect(w, r, prefix+"/appearance", http.StatusSeeOther)
}

// themePreview renders a sample page in the named theme: dashboard-style
// chrome and a customer-style card. A theme that cannot be used shows why,
// drawn in the fallback look, which is also what its pages would get.
func (d *Dashboard) themePreview(w http.ResponseWriter, r *http.Request, s *session) {
	name := r.PathValue("name")
	if !web.ValidThemeName(name) || d.o.Themes == nil {
		http.NotFound(w, r)
		return
	}
	var info *web.ThemeInfo
	for _, ti := range d.installedThemes() {
		if ti.Name == name {
			info = &ti
		}
	}
	if info == nil {
		http.NotFound(w, r)
		return
	}
	v := d.shell(r, s, "appearance", "Preview: "+name)
	v.PreviewTheme = themeRow{Name: info.Name, Version: info.Version, Description: info.Description, Bundled: info.Bundled,
		Usable: info.Err == "", Err: info.Err}
	d.renderWith(w, http.StatusOK, d.o.Renderer.With(name, ""), "dash_theme_preview", v)
}
