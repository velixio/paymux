package dashboard

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/paymux/paymux/internal/web"
)

// themeImportMax is the largest theme.json an import accepts.
const themeImportMax = 64 << 10

// reservedThemeName reports names that would collide with a route
// (/appearance/themes/new and /appearance/themes/import).
func reservedThemeName(name string) bool { return name == "new" || name == "import" }

// importFormView is the import form as it is shown again after a refusal: the
// file's content travels in a hidden field, so choosing a new name is enough.
type importFormView struct {
	Name, JSON, Error string
}

// themeExport downloads the theme's theme.json as it is (customer-page fields
// included). Only the manifest is exported: stylesheets and assets stay behind.
func (d *Dashboard) themeExport(w http.ResponseWriter, r *http.Request, s *session) {
	name := r.PathValue("name")
	th, err := d.loadTheme(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	raw, err := json.MarshalIndent(th.Manifest, "", "  ")
	if err != nil {
		d.fail(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Content-Disposition", `attachment; filename="`+th.Name+`.theme.json"`) // th.Name matches the theme-name pattern
	h.Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(append(raw, '\n'))
}

// themeImport creates a custom theme from an uploaded theme.json. Nothing else
// in the upload is read: a theme made by import has no stylesheets, templates or
// assets, so it cannot carry anything but validated colours and settings.
func (d *Dashboard) themeImport(w http.ResponseWriter, r *http.Request, s *session) {
	rename := strings.TrimSpace(r.PostForm.Get("name"))
	raw, errMsg := readImportedTheme(r)
	again := func(msg string) {
		imp := &importFormView{Name: rename, Error: msg}
		if errMsg == "" {
			imp.JSON = string(raw)
		}
		d.renderAppearanceWith(w, r, s, http.StatusBadRequest, "", imp)
	}
	if errMsg != "" {
		again(errMsg)
		return
	}
	var declared struct{ Name string }
	_ = json.Unmarshal(raw, &declared)
	m, err := web.ParseManifest(raw, declared.Name)
	if err != nil {
		again(themeErrMessage(err))
		return
	}
	if rename != "" {
		m.Name = rename
	}
	switch {
	case d.o.Themes == nil:
		again("Themes cannot be saved here.")
		return
	case reservedThemeName(m.Name):
		again("That name is reserved. Choose a new name below.")
		return
	}
	if err := d.o.Themes.Create(m, nil); err != nil {
		switch {
		case errors.Is(err, web.ErrThemeBundled):
			again("\u201c" + m.Name + "\u201d is the name of a built-in theme. Choose a new name below and import again.")
		case errors.Is(err, web.ErrThemeExists):
			again("A theme named \u201c" + m.Name + "\u201d already exists. Choose a new name below and import again.")
		default:
			again(themeErrMessage(err))
		}
		return
	}
	if err := d.record(r.Context(), r, s, "theme.import", m.Name, nil, map[string]string{"version": m.Version}); err != nil {
		d.o.Logger.Warn("dashboard: audit of theme import", "err", err)
	}
	http.Redirect(w, r, prefix+"/appearance", http.StatusSeeOther)
}

// readImportedTheme returns the theme.json bytes from the "theme" file part (or,
// when the form is shown again, from its hidden field), at most themeImportMax.
func readImportedTheme(r *http.Request) ([]byte, string) {
	const missing = "Choose a theme.json file to import."
	if f, _, err := r.FormFile("theme"); err == nil {
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, themeImportMax+1))
		switch {
		case err != nil:
			return nil, missing
		case len(b) > themeImportMax:
			return nil, "That file is larger than 64 KB. A theme.json is a few hundred bytes."
		}
		return b, ""
	}
	if j := r.PostForm.Get("theme_json"); j != "" {
		if len(j) > themeImportMax {
			return nil, "That file is larger than 64 KB. A theme.json is a few hundred bytes."
		}
		return []byte(j), ""
	}
	return nil, missing
}
