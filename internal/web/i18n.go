package web

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
)

// Message catalogs live in i18n/<lang>.json (flat key -> message), in the
// embedded assets and optionally in custom/i18n/. A custom catalog is merged
// over the embedded one of the same language, key by key, so an operator can
// reword one message or add a whole language.
//
// A message missing in the page's language falls back to English; one missing
// there too shows its key.

const defaultLang = "en"

var langRE = regexp.MustCompile(`^[a-z]{2,3}$`)

// catalogLayers are the file systems that may hold i18n/, lowest priority first.
func (g *registry) catalogLayers() []fs.FS {
	if g.static != nil {
		return []fs.FS{g.static}
	}
	var out []fs.FS
	for _, l := range []fs.FS{g.base, g.custom} {
		if l != nil {
			out = append(out, l)
		}
	}
	return out
}

func (g *registry) languages() []string {
	g.cmu.Lock()
	defer g.cmu.Unlock()
	if g.langs != nil {
		return g.langs
	}
	seen := map[string]bool{}
	for _, l := range g.catalogLayers() {
		es, err := fs.ReadDir(l, "i18n")
		if err != nil {
			continue
		}
		for _, e := range es {
			if name, ok := strings.CutSuffix(e.Name(), ".json"); ok && !e.IsDir() && langRE.MatchString(name) {
				seen[name] = true
			}
		}
	}
	g.langs = make([]string, 0, len(seen))
	for l := range seen {
		g.langs = append(g.langs, l)
	}
	sort.Strings(g.langs)
	return g.langs
}

// messages returns the merged catalog of lang (no English fallback).
func (g *registry) messages(lang string) map[string]string {
	g.cmu.Lock()
	defer g.cmu.Unlock()
	if m, ok := g.catalogs[lang]; ok {
		return m
	}
	m := map[string]string{}
	for _, l := range g.catalogLayers() {
		raw, err := fs.ReadFile(l, "i18n/"+lang+".json")
		if err != nil {
			continue
		}
		var layer map[string]string
		if err := json.Unmarshal(raw, &layer); err != nil {
			g.notify("i18n/"+lang+".json", fmt.Errorf("catalog %s is not valid: %w", lang, err))
			continue
		}
		for k, v := range layer {
			m[k] = v
		}
	}
	if g.catalogs == nil {
		g.catalogs = map[string]map[string]string{}
	}
	g.catalogs[lang] = m
	return m
}

func (g *registry) translate(lang, key string, args ...any) string {
	msg, ok := g.messages(lang)[key]
	if !ok && lang != defaultLang {
		msg, ok = g.messages(defaultLang)[key]
	}
	if !ok {
		return key
	}
	if len(args) > 0 {
		return fmt.Sprintf(msg, args...)
	}
	return msg
}

// HasLang reports whether a catalog for lang is installed. It is the
// allow-list for ?lang=.
func (r *Renderer) HasLang(lang string) bool {
	if !langRE.MatchString(lang) {
		return false
	}
	for _, l := range r.reg.languages() {
		if l == lang {
			return true
		}
	}
	return false
}

// Languages lists the installed catalogs.
func (r *Renderer) Languages() []string { return append([]string(nil), r.reg.languages()...) }

// T translates key into lang, formatting args with fmt.Sprintf.
func (r *Renderer) T(lang, key string, args ...any) string {
	if !r.HasLang(lang) {
		lang = defaultLang
	}
	return r.reg.translate(lang, key, args...)
}
