package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	webassets "github.com/paymux/paymux/web"
)

// A theme is a directory <data>/themes/<name>/ (or a bundled one) holding:
//
//	theme.json      name, version, CSS token overrides, font stack
//	css/*.css       extra stylesheets, served after the tokens in name order
//	templates/...   optional template overrides (same names as the embedded ones)
//	assets/...      images and fonts, served under /theme/<name>/assets/
//
// The overlay order of a page is: embedded -> active theme -> custom/.

// Bundled returns the themes shipped inside the binary: one directory each.
func Bundled() fs.FS {
	sub, err := fs.Sub(webassets.FS, "themes")
	if err != nil {
		return emptyFS{}
	}
	return sub
}

type emptyFS struct{}

func (emptyFS) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }

// Manifest is theme.json.
type Manifest struct {
	Name        string            `json:"name"`
	Version     string            `json:"version"`
	Description string            `json:"description,omitempty"`
	Tokens      map[string]string `json:"tokens,omitempty"`
	FontStack   string            `json:"font_stack,omitempty"`
	// Dashboard themes the React dashboard (customer pages use Tokens).
	Dashboard *DashboardTheme `json:"dashboard,omitempty"`
}

// DashboardTheme is the "dashboard" block of theme.json: a few seed colours
// (everything else is derived in the dashboard) plus optional per-token overrides.
type DashboardTheme struct {
	Seeds     Seeds                        `json:"seeds"`
	DarkSeeds *Seeds                       `json:"dark_seeds,omitempty"`
	Scheme    string                       `json:"scheme,omitempty"`    // "", "light" or "dark"
	Overrides map[string]map[string]string `json:"overrides,omitempty"` // "light"|"dark" -> token -> #rrggbb
}

// Seeds are the colours a dashboard theme is made from, all #rrggbb. Only
// Accent is required.
type Seeds struct {
	Accent      string `json:"accent,omitempty"`
	Background  string `json:"background,omitempty"`
	Sidebar     string `json:"sidebar,omitempty"`
	Text        string `json:"text,omitempty"`
	SidebarText string `json:"sidebar_text,omitempty"`
}

// DashboardTokens are the dashboard colour tokens a theme may override. The
// status colours (--status-*) are deliberately absent: a theme cannot change
// what a status colour means.
var DashboardTokens = []string{
	"--background", "--foreground", "--card", "--card-foreground",
	"--popover", "--popover-foreground", "--primary", "--primary-foreground",
	"--secondary", "--secondary-foreground", "--muted", "--muted-foreground",
	"--ui-accent", "--accent-foreground", "--destructive", "--border",
	"--input", "--ring", "--sidebar", "--sidebar-foreground",
}

var hexColourRE = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func (s Seeds) validate(field string, needAccent bool) error {
	if needAccent && s.Accent == "" {
		return fmt.Errorf("%s.accent is required", field)
	}
	for _, f := range []struct{ name, v string }{
		{"accent", s.Accent}, {"background", s.Background}, {"sidebar", s.Sidebar},
		{"text", s.Text}, {"sidebar_text", s.SidebarText},
	} {
		if f.v != "" && !hexColourRE.MatchString(f.v) {
			return fmt.Errorf("%s.%s %q must be a #rrggbb colour", field, f.name, f.v)
		}
	}
	return nil
}

func (d *DashboardTheme) validate() error {
	if err := d.Seeds.validate("dashboard.seeds", true); err != nil {
		return err
	}
	if d.DarkSeeds != nil {
		if err := d.DarkSeeds.validate("dashboard.dark_seeds", true); err != nil {
			return err
		}
	}
	switch d.Scheme {
	case "", "light", "dark":
	default:
		return fmt.Errorf("dashboard.scheme %q must be light or dark", d.Scheme)
	}
	for mode, toks := range d.Overrides {
		if mode != "light" && mode != "dark" {
			return fmt.Errorf("dashboard.overrides: %q must be light or dark", mode)
		}
		for k, v := range toks {
			if !slices.Contains(DashboardTokens, k) {
				return fmt.Errorf("dashboard.overrides.%s: %s is not a themeable dashboard token", mode, k)
			}
			if !hexColourRE.MatchString(v) {
				return fmt.Errorf("dashboard.overrides.%s.%s: %q must be a #rrggbb colour", mode, k, v)
			}
		}
	}
	return nil
}

var (
	themeNameRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	themeVersionRE = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+-]{0,31}$`)
	tokenNameRE    = regexp.MustCompile(`^--[a-z][a-z0-9-]{0,40}$`)
)

// ValidThemeName reports whether s can name a theme.
func ValidThemeName(s string) bool { return themeNameRE.MatchString(s) }

// ParseManifest validates theme.json. dirName is the name of the directory
// that holds it; the two must agree.
func ParseManifest(raw []byte, dirName string) (Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("theme.json: %w", err)
	}
	if dec.More() {
		return Manifest{}, errors.New("theme.json: trailing data")
	}
	switch {
	case !themeNameRE.MatchString(m.Name):
		return Manifest{}, fmt.Errorf("theme.json: name %q must be 1-40 lower-case letters, digits or hyphens", m.Name)
	case m.Name != dirName:
		return Manifest{}, fmt.Errorf("theme.json: name %q differs from its directory %q", m.Name, dirName)
	case !themeVersionRE.MatchString(m.Version):
		return Manifest{}, errors.New("theme.json: version is required (for example 1.0.0)")
	case len(m.Description) > 200:
		return Manifest{}, errors.New("theme.json: description is longer than 200 characters")
	case len(m.Tokens) > 64:
		return Manifest{}, errors.New("theme.json: more than 64 tokens")
	}
	for k, v := range m.Tokens {
		if !tokenNameRE.MatchString(k) {
			return Manifest{}, fmt.Errorf("theme.json: token %q must be a CSS custom property such as --color-accent", k)
		}
		if err := safeCSSValue(v); err != nil {
			return Manifest{}, fmt.Errorf("theme.json: token %q: value %v", k, err)
		}
	}
	if m.Dashboard != nil {
		if err := m.Dashboard.validate(); err != nil {
			return Manifest{}, fmt.Errorf("theme.json: %w", err)
		}
	}
	if m.FontStack != "" {
		if err := safeCSSValue(m.FontStack); err != nil {
			return Manifest{}, fmt.Errorf("theme.json: font_stack: value %v", err)
		}
	}
	return m, nil
}

// safeCSSValue accepts plain values (colours, lengths, font names) and
// rejects anything that could end the declaration or load something.
func safeCSSValue(v string) error {
	if strings.TrimSpace(v) == "" || len(v) > 200 {
		return errors.New("must be 1-200 characters")
	}
	if strings.ContainsAny(v, ";{}<>\\\n\r\x00") {
		return errors.New("contains a character that is not allowed (; { } < > \\ or a line break)")
	}
	l := strings.ToLower(v)
	for _, bad := range []string{"/*", "*/", "url(", "@import", "expression(", "javascript:"} {
		if strings.Contains(l, bad) {
			return fmt.Errorf("contains %q, which is not allowed", bad)
		}
	}
	return nil
}

// Theme is a validated, loaded theme.
type Theme struct {
	Manifest
	// FS has theme.json, css/, templates/ and assets/ at its root.
	FS fs.FS
	// Fingerprint changes whenever any file of the theme changes.
	Fingerprint string
	Bundled     bool
}

// ThemeInfo describes one installed theme for pickers.
type ThemeInfo struct {
	Name, Version, Description string
	Bundled                    bool
	Err                        string // why it cannot be used; empty when valid
}

const (
	maxThemeFiles = 400
	maxThemeBytes = 8 << 20
)

// Themes finds themes in the bundled set and in a data directory. A theme in
// the data directory shadows a bundled one of the same name.
type Themes struct {
	bundled fs.FS
	dir     string

	mu    sync.Mutex
	ttl   time.Duration
	cache map[string]cachedTheme

	wmu sync.Mutex // serialises the write methods (theme_write.go)
}

type cachedTheme struct {
	th  *Theme
	err error
	at  time.Time
}

// NewThemes returns a theme source. dir may be empty (bundled themes only).
func NewThemes(bundled fs.FS, dir string) *Themes {
	return &Themes{bundled: bundled, dir: dir, cache: map[string]cachedTheme{}}
}

// CacheFor makes Load reuse a result for d (0 reads the disk every time). The
// binary uses one second, so an edit is live within a second without a restart.
func (t *Themes) CacheFor(d time.Duration) { t.mu.Lock(); t.ttl = d; t.mu.Unlock() }

// Load returns the named theme, validated.
func (t *Themes) Load(name string) (*Theme, error) {
	if !themeNameRE.MatchString(name) {
		return nil, fmt.Errorf("theme: %q is not a valid theme name", name)
	}
	t.mu.Lock()
	if c, ok := t.cache[name]; ok && t.ttl > 0 && time.Since(c.at) < t.ttl {
		t.mu.Unlock()
		return c.th, c.err
	}
	t.mu.Unlock()
	th, err := t.load(name)
	t.mu.Lock()
	t.cache[name] = cachedTheme{th, err, time.Now()}
	t.mu.Unlock()
	return th, err
}

func (t *Themes) load(name string) (*Theme, error) {
	if t.dir != "" {
		root, err := os.OpenRoot(filepath.Join(t.dir, name))
		switch {
		case err == nil:
			defer root.Close()
			return readTheme(root.FS(), name, false)
		case !errors.Is(err, fs.ErrNotExist):
			return nil, fmt.Errorf("theme %s: %w", name, err)
		}
	}
	if t.bundled != nil {
		if sub, err := fs.Sub(t.bundled, name); err == nil {
			if _, err := fs.Stat(sub, "theme.json"); err == nil {
				return readTheme(sub, name, true)
			}
		}
	}
	return nil, fmt.Errorf("theme %q not found", name)
}

// readTheme copies the theme into memory, so nothing holds a file handle and
// the fingerprint covers exactly what is served.
func readTheme(src fs.FS, name string, bundled bool) (*Theme, error) {
	files := map[string][]byte{}
	var total int64
	err := fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // symlinks and devices are never followed
		}
		if len(files) >= maxThemeFiles {
			return fmt.Errorf("more than %d files", maxThemeFiles)
		}
		f, err := src.Open(p)
		if err != nil {
			return nil // a link that leaves the directory
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, maxThemeBytes+1))
		if err != nil {
			return err
		}
		if total += int64(len(b)); total > maxThemeBytes {
			return fmt.Errorf("larger than %d MiB", maxThemeBytes>>20)
		}
		files[p] = b
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("theme %s: %w", name, err)
	}
	raw, ok := files["theme.json"]
	if !ok {
		return nil, fmt.Errorf("theme %s: theme.json is missing", name)
	}
	m, err := ParseManifest(raw, name)
	if err != nil {
		return nil, fmt.Errorf("theme %s: %w", name, err)
	}
	h := sha256.New()
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(h, "%s\x00%d\x00", n, len(files[n]))
		h.Write(files[n])
	}
	return &Theme{Manifest: m, FS: memFS(files), Fingerprint: hex.EncodeToString(h.Sum(nil))[:12], Bundled: bundled}, nil
}

// List returns every installed theme, valid or not, by name.
func (t *Themes) List() []ThemeInfo {
	names := map[string]bool{}
	if t.bundled != nil {
		if es, err := fs.ReadDir(t.bundled, "."); err == nil {
			for _, e := range es {
				if e.IsDir() {
					names[e.Name()] = true
				}
			}
		}
	}
	if t.dir != "" {
		if es, err := os.ReadDir(t.dir); err == nil {
			for _, e := range es {
				if e.IsDir() {
					names[e.Name()] = true
				}
			}
		}
	}
	var out []ThemeInfo
	for n := range names {
		if !themeNameRE.MatchString(n) {
			continue
		}
		th, err := t.Load(n)
		if err != nil {
			out = append(out, ThemeInfo{Name: n, Err: err.Error()})
			continue
		}
		out = append(out, ThemeInfo{Name: n, Version: th.Version, Description: th.Description, Bundled: th.Bundled})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// CSS renders the stylesheet served as /theme/<name>/theme.css: the token
// overrides, then css/*.css in name order.
func (th *Theme) CSS() []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "/* paymux theme %s %s */\n:root{", th.Name, th.Version)
	keys := make([]string, 0, len(th.Tokens))
	for k := range th.Tokens {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s:%s;", k, th.Tokens[k])
	}
	if th.FontStack != "" {
		fmt.Fprintf(&b, "--font:%s;", th.FontStack)
	}
	b.WriteString("}\n")
	es, _ := fs.ReadDir(th.FS, "css")
	for _, e := range es {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".css") {
			continue
		}
		if raw, err := fs.ReadFile(th.FS, path.Join("css", e.Name())); err == nil {
			b.Write(raw)
			b.WriteByte('\n')
		}
	}
	return b.Bytes()
}

var assetTypes = map[string]string{
	".svg": "image/svg+xml", ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp", ".ico": "image/x-icon", ".woff2": "font/woff2", ".woff": "font/woff",
	".ttf": "font/ttf", ".otf": "font/otf",
}

// Handler serves /theme/<name>/theme.css, /theme/<name>/dashboard.json and /theme/<name>/assets/....
func (t *Themes) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		rest, ok := strings.CutPrefix(r.URL.Path, "/theme/")
		name, file, _ := strings.Cut(rest, "/")
		if !ok || !themeNameRE.MatchString(name) {
			http.NotFound(w, r)
			return
		}
		th, err := t.Load(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		switch {
		case file == "theme.css":
			serveThemeBytes(w, r, th, file, "text/css; charset=utf-8", th.CSS(), "no-cache")
		case strings.HasPrefix(file, "assets/") && fs.ValidPath(file) && path.Clean(file) == file:
			typ := assetTypes[strings.ToLower(path.Ext(file))]
			raw, err := fs.ReadFile(th.FS, file)
			if typ == "" || err != nil {
				http.NotFound(w, r)
				return
			}
			if typ == "image/svg+xml" {
				w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
			}
			serveThemeBytes(w, r, th, file, typ, raw, "public, max-age=300")
		case file == "dashboard.json":
			serveThemeBytes(w, r, th, file, "application/json; charset=utf-8", th.dashboardJSON(), "no-cache")
		default:
			http.NotFound(w, r)
		}
	})
}

// dashboardJSON renders /theme/<name>/dashboard.json. A theme without a
// dashboard block answers with empty seeds, so the dashboard keeps its defaults.
func (th *Theme) dashboardJSON() []byte {
	out := struct {
		Name      string                       `json:"name"`
		Scheme    string                       `json:"scheme"`
		Seeds     Seeds                        `json:"seeds"`
		DarkSeeds *Seeds                       `json:"dark_seeds"`
		Overrides map[string]map[string]string `json:"overrides"`
	}{Name: th.Name, Overrides: map[string]map[string]string{}}
	if d := th.Dashboard; d != nil {
		out.Scheme, out.Seeds, out.DarkSeeds = d.Scheme, d.Seeds, d.DarkSeeds
		if d.Overrides != nil {
			out.Overrides = d.Overrides
		}
	}
	b, _ := json.Marshal(out)
	return b
}

func serveThemeBytes(w http.ResponseWriter, r *http.Request, th *Theme, file, typ string, body []byte, cache string) {
	sum := sha256.Sum256([]byte(file))
	etag := `"` + th.Fingerprint + "-" + hex.EncodeToString(sum[:4]) + `"`
	h := w.Header()
	h.Set("ETag", etag)
	h.Set("Cache-Control", cache)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", typ)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// ---- in-memory FS ----

// memFS is a read-only file tree held in memory: file path -> content.
type memFS map[string][]byte

type memInfo struct {
	name string
	size int64
	dir  bool
}

func (i memInfo) Name() string { return i.name }
func (i memInfo) Size() int64  { return i.size }
func (i memInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}
func (i memInfo) ModTime() time.Time { return time.Time{} }
func (i memInfo) IsDir() bool        { return i.dir }
func (i memInfo) Sys() any           { return nil }

type memFile struct {
	*bytes.Reader
	info memInfo
}

func (f *memFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *memFile) Close() error               { return nil }

type memDir struct {
	info    memInfo
	entries []fs.DirEntry
	pos     int
}

func (d *memDir) Stat() (fs.FileInfo, error) { return d.info, nil }
func (d *memDir) Close() error               { return nil }
func (d *memDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.info.name, Err: errors.New("is a directory")}
}
func (d *memDir) ReadDir(n int) ([]fs.DirEntry, error) {
	rest := d.entries[d.pos:]
	if n <= 0 {
		d.pos = len(d.entries)
		return rest, nil
	}
	if len(rest) == 0 {
		return nil, io.EOF
	}
	if n > len(rest) {
		n = len(rest)
	}
	d.pos += n
	return rest[:n], nil
}

func (m memFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if b, ok := m[name]; ok {
		return &memFile{Reader: bytes.NewReader(b), info: memInfo{name: path.Base(name), size: int64(len(b))}}, nil
	}
	prefix := name + "/"
	if name == "." {
		prefix = ""
	}
	seen := map[string]fs.DirEntry{}
	for p, b := range m {
		rest, ok := strings.CutPrefix(p, prefix)
		if !ok {
			continue
		}
		first, more, nested := strings.Cut(rest, "/")
		if nested && more != "" {
			seen[first] = fs.FileInfoToDirEntry(memInfo{name: first, dir: true})
		} else {
			seen[first] = fs.FileInfoToDirEntry(memInfo{name: first, size: int64(len(b))})
		}
	}
	if len(seen) == 0 {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	es := make([]fs.DirEntry, 0, len(seen))
	for _, e := range seen {
		es = append(es, e)
	}
	sort.Slice(es, func(i, j int) bool { return es[i].Name() < es[j].Name() })
	return &memDir{info: memInfo{name: path.Base(name), dir: true}, entries: es}, nil
}
