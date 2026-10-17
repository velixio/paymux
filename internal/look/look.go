// Package look decides how a page looks: which theme and which language.
// Customer pages use their app's settings (app value, else the org's); the
// dashboard uses the org's theme.
package look

import (
	"context"

	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/web"
)

// Look is the theme and language of one page.
type Look struct{ Theme, Lang string }

// Settings is the part of *settings.Service this package needs.
type Settings interface {
	Resolve(ctx context.Context, appID, key string) (settings.Value, error)
}

const (
	defaultTheme = "default"
	defaultLang  = "en"
)

// Resolve returns the look of appID's customer pages. queryLang is the raw
// ?lang= value: it is used only when an installed catalog has exactly that
// code, so it can never select a path or inject markup. A setting that cannot
// be read leaves the defaults.
func Resolve(ctx context.Context, s Settings, rend *web.Renderer, appID, queryLang string) Look {
	l := Look{Theme: defaultTheme, Lang: defaultLang}
	var v string
	if val, err := s.Resolve(ctx, appID, "theme"); err == nil && val.Decode(&v) == nil && web.ValidThemeName(v) {
		l.Theme = v
	}
	if val, err := s.Resolve(ctx, appID, "customer_language"); err == nil && val.Decode(&v) == nil && rend.HasLang(v) {
		l.Lang = v
	}
	if queryLang != "" && rend.HasLang(queryLang) {
		l.Lang = queryLang
	}
	return l
}

// Dashboard returns the dashboard's theme (the org value).
func Dashboard(ctx context.Context, s Settings) string {
	var v string
	if val, err := s.Resolve(ctx, "", "theme"); err == nil && val.Decode(&v) == nil && web.ValidThemeName(v) {
		return v
	}
	return defaultTheme
}

// Bind returns r drawing in l.
func (l Look) Bind(r *web.Renderer) *web.Renderer { return r.With(l.Theme, l.Lang) }
