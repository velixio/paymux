package dashboard

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/paymux/paymux/internal/authz"
	"github.com/paymux/paymux/internal/web"
)

// seedForm is one set of seed colours as the form shows it. An optional seed
// is saved only while its "use it" box is checked.
type seedForm struct {
	Prefix                             string // "light" or "dark": the form field prefix
	Accent, Background, Sidebar, Text  string
	SidebarText                        string
	SetBackground, SetSidebar, SetText bool
	SetSidebarText                     bool
}

// themeFormView is the create/edit form.
type themeFormView struct {
	Creating                   bool
	Name, Version, Description string
	From                       string // the theme this one starts from, when duplicating
	Scheme                     string
	Light, Dark                seedForm
	DarkEnabled                bool
	Overrides                  int // hand-set colours the form does not edit
}

// Starting colours for a seed that is not set, so the colour pickers have a value.
var (
	lightDefaults = seedForm{Accent: "#2563eb", Background: "#ffffff", Sidebar: "#f7f8fa", Text: "#1a1f36", SidebarText: "#5c6478"}
	darkDefaults  = seedForm{Accent: "#8b85ff", Background: "#0f1117", Sidebar: "#14161d", Text: "#e6e8ef", SidebarText: "#9aa1b2"}
)

func seedFormOf(prefix string, def seedForm, s *web.Seeds) seedForm {
	f := def
	f.Prefix = prefix
	if s == nil {
		return f
	}
	f.Accent = s.Accent
	pick := func(v string, into *string, set *bool) {
		if v != "" {
			*into, *set = v, true
		}
	}
	pick(s.Background, &f.Background, &f.SetBackground)
	pick(s.Sidebar, &f.Sidebar, &f.SetSidebar)
	pick(s.Text, &f.Text, &f.SetText)
	pick(s.SidebarText, &f.SidebarText, &f.SetSidebarText)
	return f
}

func newThemeForm() *themeFormView {
	return &themeFormView{Creating: true, Version: "1.0.0",
		Light: seedFormOf("light", lightDefaults, nil), Dark: seedFormOf("dark", darkDefaults, nil)}
}

// themeFormOf fills the form from an existing theme.
func themeFormOf(th *web.Theme) *themeFormView {
	f := newThemeForm()
	f.Creating = false
	f.Name, f.Version, f.Description = th.Name, th.Version, th.Description
	if dt := th.Dashboard; dt != nil {
		f.Scheme = dt.Scheme
		f.Light = seedFormOf("light", lightDefaults, &dt.Seeds)
		if dt.DarkSeeds != nil {
			f.DarkEnabled = true
			f.Dark = seedFormOf("dark", darkDefaults, dt.DarkSeeds)
		}
		f.Overrides = len(dt.Overrides["light"]) + len(dt.Overrides["dark"])
	}
	return f
}

// readSeeds reads one set of seeds from the posted form, and the form to show
// again if it is refused.
func readSeeds(post url.Values, prefix string, def seedForm) (web.Seeds, seedForm) {
	get := func(k string) string { return strings.TrimSpace(post.Get(prefix + "_" + k)) }
	set := func(k string) bool { return post.Get("set_"+prefix+"_"+k) != "" }
	f := seedForm{Prefix: prefix, Accent: get("accent"),
		Background: get("background"), Sidebar: get("sidebar"), Text: get("text"), SidebarText: get("sidebar_text"),
		SetBackground: set("background"), SetSidebar: set("sidebar"), SetText: set("text"), SetSidebarText: set("sidebar_text")}
	s := web.Seeds{Accent: f.Accent}
	if f.SetBackground {
		s.Background = f.Background
	}
	if f.SetSidebar {
		s.Sidebar = f.Sidebar
	}
	if f.SetText {
		s.Text = f.Text
	}
	if f.SetSidebarText {
		s.SidebarText = f.SidebarText
	}
	// Unused pickers show the default again rather than an empty value.
	for _, p := range []struct {
		v   *string
		set bool
		def string
	}{{&f.Background, f.SetBackground, def.Background}, {&f.Sidebar, f.SetSidebar, def.Sidebar},
		{&f.Text, f.SetText, def.Text}, {&f.SidebarText, f.SetSidebarText, def.SidebarText}} {
		if !p.set && *p.v == "" {
			*p.v = p.def
		}
	}
	return s, f
}

// readThemeForm reads the posted form into the pieces of a manifest and the
// view to redisplay.
func readThemeForm(r *http.Request) (version, description, scheme string, dash *web.DashboardTheme, view *themeFormView) {
	post := r.PostForm
	light, lf := readSeeds(post, "light", lightDefaults)
	dash = &web.DashboardTheme{Seeds: light, Scheme: post.Get("scheme")}
	view = &themeFormView{Creating: true, Version: strings.TrimSpace(post.Get("version")),
		Description: strings.TrimSpace(post.Get("description")), Name: strings.TrimSpace(post.Get("name")),
		From: strings.TrimSpace(post.Get("from")), Scheme: post.Get("scheme"), Light: lf,
		Dark: seedFormOf("dark", darkDefaults, nil)}
	if post.Get("dark_enabled") != "" {
		dark, df := readSeeds(post, "dark", darkDefaults)
		dash.DarkSeeds = &dark
		view.DarkEnabled, view.Dark = true, df
	}
	return view.Version, view.Description, dash.Scheme, dash, view
}

func (d *Dashboard) routesThemeEditor() {
	m := d.mux
	m.HandleFunc("GET "+prefix+"/appearance/themes/new", d.authed(authz.SettingsWrite, d.themeNewPage))
	m.HandleFunc("POST "+prefix+"/appearance/themes", d.authed(authz.SettingsWrite, d.recent(d.themeCreate)))
	m.HandleFunc("GET "+prefix+"/appearance/themes/{name}/edit", d.authed(authz.SettingsWrite, d.themeEditPage))
	m.HandleFunc("POST "+prefix+"/appearance/themes/{name}", d.authed(authz.SettingsWrite, d.recent(d.themeSave)))
}

func (d *Dashboard) renderThemeForm(w http.ResponseWriter, r *http.Request, s *session, status int, f *themeFormView, msg string) {
	title := "New theme"
	if !f.Creating {
		title = "Edit " + f.Name
	}
	v := d.shell(r, s, "appearance", title)
	v.ThemeForm, v.Error = f, msg
	d.render(w, status, "dash_theme_edit", v)
}

func (d *Dashboard) themeNewPage(w http.ResponseWriter, r *http.Request, s *session) {
	f := newThemeForm()
	if from := r.URL.Query().Get("from"); from != "" {
		th, err := d.loadTheme(from)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		f.From = from
		// Duplicating starts from the seeds: the new theme is named by the user.
		if dt := th.Dashboard; dt != nil {
			cp := themeFormOf(th)
			f.Scheme, f.Light, f.Dark, f.DarkEnabled = cp.Scheme, cp.Light, cp.Dark, cp.DarkEnabled
		}
	}
	d.renderThemeForm(w, r, s, http.StatusOK, f, "")
}

func (d *Dashboard) loadTheme(name string) (*web.Theme, error) {
	if d.o.Themes == nil || !web.ValidThemeName(name) {
		return nil, errors.New("no such theme")
	}
	return d.o.Themes.Load(name)
}

// themeErrMessage is what the form says about a refused write.
func themeErrMessage(err error) string {
	return strings.TrimPrefix(err.Error(), "theme.json: ")
}

func (d *Dashboard) themeCreate(w http.ResponseWriter, r *http.Request, s *session) {
	version, desc, _, dash, view := readThemeForm(r)
	if d.o.Themes == nil {
		d.renderThemeForm(w, r, s, http.StatusBadRequest, view, "Themes cannot be saved here.")
		return
	}
	m := web.Manifest{Name: view.Name, Version: version, Description: desc, Dashboard: dash}
	var from *web.Theme
	if view.From != "" {
		src, err := d.loadTheme(view.From)
		if err != nil {
			d.renderThemeForm(w, r, s, http.StatusBadRequest, view, "The theme to start from was not found.")
			return
		}
		from = src
		// Customer-page settings come along; the dashboard block is what the form says.
		m.Tokens, m.FontStack = src.Tokens, src.FontStack
	}
	if err := d.o.Themes.Create(m, from); err != nil {
		d.renderThemeForm(w, r, s, http.StatusBadRequest, view, themeErrMessage(err))
		return
	}
	if err := d.record(r.Context(), r, s, "theme.create", m.Name, nil, map[string]string{"from": view.From, "version": m.Version}); err != nil {
		d.o.Logger.Warn("dashboard: audit of theme create", "err", err)
	}
	http.Redirect(w, r, prefix+"/appearance", http.StatusSeeOther)
}

// customTheme loads name for editing: it must be a custom theme that loads.
// It answers the request itself when it is not (404 unknown, 403 built-in).
func (d *Dashboard) customTheme(w http.ResponseWriter, r *http.Request) (*web.Theme, bool) {
	name := r.PathValue("name")
	if d.o.Themes == nil || !web.ValidThemeName(name) {
		http.NotFound(w, r)
		return nil, false
	}
	if d.o.Themes.IsBundled(name) {
		http.Error(w, "built-in themes are read-only: duplicate it to change it", http.StatusForbidden)
		return nil, false
	}
	th, err := d.o.Themes.Load(name)
	if err != nil {
		http.NotFound(w, r)
		return nil, false
	}
	if th.Bundled {
		http.Error(w, "built-in themes are read-only: duplicate it to change it", http.StatusForbidden)
		return nil, false
	}
	return th, true
}

func (d *Dashboard) themeEditPage(w http.ResponseWriter, r *http.Request, s *session) {
	th, ok := d.customTheme(w, r)
	if !ok {
		return
	}
	d.renderThemeForm(w, r, s, http.StatusOK, themeFormOf(th), "")
}

func (d *Dashboard) themeSave(w http.ResponseWriter, r *http.Request, s *session) {
	th, ok := d.customTheme(w, r)
	if !ok {
		return
	}
	version, desc, _, dash, view := readThemeForm(r)
	view.Creating, view.Name, view.From = false, th.Name, ""
	view.Overrides = 0
	if th.Dashboard != nil {
		view.Overrides = len(th.Dashboard.Overrides["light"]) + len(th.Dashboard.Overrides["dark"])
	}
	var before, after *web.DashboardTheme
	before = th.Dashboard
	err := d.o.Themes.Update(th.Name, func(m *web.Manifest) error {
		m.Version = version
		m.Description = desc
		if m.Dashboard != nil {
			// Overrides, and anything else the form does not edit, are kept.
			dash.Overrides = m.Dashboard.Overrides
		}
		m.Dashboard = dash
		after = dash
		return nil
	})
	switch {
	case errors.Is(err, web.ErrThemeBundled):
		http.Error(w, "built-in themes are read-only", http.StatusForbidden)
		return
	case errors.Is(err, web.ErrThemeMissing):
		http.NotFound(w, r)
		return
	case err != nil:
		d.renderThemeForm(w, r, s, http.StatusBadRequest, view, themeErrMessage(err))
		return
	}
	if err := d.record(r.Context(), r, s, "theme.update", th.Name, before, after); err != nil {
		d.o.Logger.Warn("dashboard: audit of theme update", "err", err)
	}
	http.Redirect(w, r, prefix+"/appearance", http.StatusSeeOther)
}
