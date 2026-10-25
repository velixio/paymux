package web

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newWritable(t *testing.T) (*Themes, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "themes")
	th := NewThemes(Bundled(), dir)
	th.CacheFor(time.Hour) // a write must show without waiting for the cache
	return th, dir
}

func tealManifest(name string) Manifest {
	return Manifest{Name: name, Version: "1.0.0", Dashboard: &DashboardTheme{Seeds: Seeds{Accent: "#0d7d68"}}}
}

func noStrays(t *testing.T, dir string) {
	t.Helper()
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.Contains(d.Name(), ".tmp") {
			t.Errorf("leftover temporary file %s", p)
		}
		return nil
	})
}

func TestCreateWritesACustomThemeThatLoadsAtOnce(t *testing.T) {
	th, dir := newWritable(t)
	if _, err := th.Load("teal"); err == nil {
		t.Fatal("teal should not exist yet") // and the miss is now cached
	}
	if err := th.Create(tealManifest("teal"), nil); err != nil {
		t.Fatal(err)
	}
	got, err := th.Load("teal")
	if err != nil || got.Bundled || got.Dashboard.Seeds.Accent != "#0d7d68" {
		t.Fatalf("load after create: %+v %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "teal", "theme.json")); err != nil {
		t.Fatal(err)
	}
	var infos []string
	for _, i := range th.List() {
		infos = append(infos, i.Name)
	}
	if !strings.Contains(strings.Join(infos, ","), "teal") {
		t.Errorf("list: %v", infos)
	}
	noStrays(t, dir)
}

func TestCreateRefusesBundledExistingAndBadNames(t *testing.T) {
	th, dir := newWritable(t)
	for _, name := range []string{"stripe", "paddle", "midnight", "default", "minimal-dark"} {
		if err := th.Create(tealManifest(name), nil); !errors.Is(err, ErrThemeBundled) {
			t.Errorf("%s: want ErrThemeBundled, got %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s: a directory shadowing the bundled theme was written", name)
		}
	}
	if err := th.Create(tealManifest("teal"), nil); err != nil {
		t.Fatal(err)
	}
	if err := th.Create(tealManifest("teal"), nil); !errors.Is(err, ErrThemeExists) {
		t.Errorf("second create: %v", err)
	}
	for _, name := range []string{"", "../x", "a/b", `a\b`, ".hidden", "Upper", "x..y/../z", strings.Repeat("a", 41), "has space"} {
		if err := th.Create(tealManifest(name), nil); err == nil {
			t.Errorf("%q should be refused", name)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("only teal should exist, got %v", entries)
	}
	// An invalid manifest is refused and leaves nothing behind.
	bad := tealManifest("bad")
	bad.Dashboard.Seeds.Accent = "red"
	if err := th.Create(bad, nil); err == nil || !strings.Contains(err.Error(), "#rrggbb") {
		t.Errorf("bad seed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "bad")); err == nil {
		t.Error("an invalid theme was written")
	}
	noStrays(t, dir)
}

func TestCreateFromCopiesTheSourcesFilesButNotItsManifest(t *testing.T) {
	th, dir := newWritable(t)
	writeTheme(t, dir, "src", map[string]string{
		"theme.json":      `{"name":"src","version":"1.0.0","tokens":{"--radius":"2px"}}`,
		"css/a.css":       `.a{color:red}`,
		"assets/logo.svg": `<svg xmlns="http://www.w3.org/2000/svg"/>`,
	})
	src, err := th.Load("src")
	if err != nil {
		t.Fatal(err)
	}
	m := tealManifest("copy")
	m.Tokens = src.Tokens
	if err := th.Create(m, src); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"css/a.css", "assets/logo.svg", "theme.json"} {
		if _, err := os.Stat(filepath.Join(dir, "copy", filepath.FromSlash(f))); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "copy", "theme.json"))
	if !strings.Contains(string(raw), `"name": "copy"`) || !strings.Contains(string(raw), `"--radius": "2px"`) {
		t.Errorf("manifest: %s", raw)
	}
	// Duplicating a bundled theme works too (its files come from the binary).
	md, _ := th.Load("minimal-dark")
	if err := th.Create(tealManifest("mine"), md); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mine", "css")); err != nil {
		t.Errorf("minimal-dark's css was not copied: %v", err)
	}
	noStrays(t, dir)
}

func TestUpdateEditsOnlyWhatItIsGivenAndKeepsTheRest(t *testing.T) {
	th, dir := newWritable(t)
	writeTheme(t, dir, "mine", map[string]string{
		"theme.json": `{"name":"mine","version":"1.0.0","tokens":{"--radius":"2px"},"dashboard":{"seeds":{"accent":"#0d7d68"},"overrides":{"light":{"--border":"#112233"}}}}`,
		"css/a.css":  `.a{}`,
	})
	if _, err := th.Load("mine"); err != nil {
		t.Fatal(err)
	}
	err := th.Update("mine", func(m *Manifest) error {
		m.Version = "1.1.0"
		m.Dashboard.Seeds.Accent = "#ff0066"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := th.Load("mine")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "1.1.0" || got.Dashboard.Seeds.Accent != "#ff0066" || got.Tokens["--radius"] != "2px" ||
		got.Dashboard.Overrides["light"]["--border"] != "#112233" {
		t.Fatalf("after update: %+v / %+v", got.Manifest, got.Dashboard)
	}
	if _, err := os.Stat(filepath.Join(dir, "mine", "css", "a.css")); err != nil {
		t.Errorf("css lost: %v", err)
	}
	noStrays(t, dir)
}

func TestUpdateRefusesBundledMissingInvalidAndEscapingThemes(t *testing.T) {
	th, dir := newWritable(t)
	ok := func(m *Manifest) error { m.Version = "9"; return nil }
	if err := th.Update("stripe", ok); !errors.Is(err, ErrThemeBundled) {
		t.Errorf("bundled: %v", err)
	}
	if err := th.Update("nope", ok); !errors.Is(err, ErrThemeMissing) {
		t.Errorf("missing: %v", err)
	}
	if err := th.Update("../etc", ok); err == nil {
		t.Error("traversal accepted")
	}
	if err := th.Create(tealManifest("teal"), nil); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "teal", "theme.json"))
	if err := th.Update("teal", func(m *Manifest) error { m.Dashboard.Seeds.Accent = "nope"; return nil }); err == nil {
		t.Error("invalid result accepted")
	}
	boom := errors.New("boom")
	if err := th.Update("teal", func(m *Manifest) error { return boom }); !errors.Is(err, boom) {
		t.Errorf("edit error: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "teal", "theme.json"))
	if string(before) != string(after) {
		t.Errorf("a refused update changed the file:\n%s\n%s", before, after)
	}
	// A symlinked theme directory pointing outside the themes directory is not followed.
	outside := t.TempDir()
	writeTheme(t, outside, "x", map[string]string{"theme.json": `{"name":"x","version":"1"}`})
	if err := os.Symlink(filepath.Join(outside, "x"), filepath.Join(dir, "x")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if err := th.Update("x", ok); err == nil {
		t.Error("a symlink out of the themes directory was followed")
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "x", "theme.json")); strings.Contains(string(b), `"9"`) {
		t.Error("wrote through the symlink")
	}
	noStrays(t, dir)
}

func TestConcurrentUpdatesNeverLeaveAHalfWrittenFile(t *testing.T) {
	th, dir := newWritable(t)
	if err := th.Create(tealManifest("teal"), nil); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop, readerDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			raw, err := os.ReadFile(filepath.Join(dir, "teal", "theme.json"))
			if err != nil {
				t.Errorf("read: %v", err)
				return
			}
			var m Manifest
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Errorf("half-written theme.json: %v: %q", err, raw)
				return
			}
		}
	}()
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = th.Update("teal", func(m *Manifest) error { m.Description = strings.Repeat("x", 150); return nil })
		}()
	}
	wg.Wait()
	close(stop)
	<-readerDone // the reader must not report after the test is over
	noStrays(t, dir)
}

func TestRenameMovesTheThemeAndRewritesItsName(t *testing.T) {
	th, dir := newWritable(t)
	if err := th.Create(tealManifest("teal"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := th.Load("teal"); err != nil {
		t.Fatal(err)
	}
	if err := th.Rename("teal", "green"); err != nil {
		t.Fatal(err)
	}
	if _, err := th.Load("teal"); err == nil {
		t.Error("old name still loads")
	}
	got, err := th.Load("green")
	if err != nil || got.Name != "green" {
		t.Fatalf("new name: %+v %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "teal")); err == nil {
		t.Error("old directory remains")
	}
	// Refusals leave everything as it was.
	_ = th.Create(tealManifest("other"), nil)
	for _, c := range []struct {
		old, new string
		want     error
	}{
		{"green", "other", ErrThemeExists},
		{"green", "stripe", ErrThemeBundled},
		{"stripe", "mine", ErrThemeBundled},
		{"nope", "mine", ErrThemeMissing},
	} {
		if err := th.Rename(c.old, c.new); !errors.Is(err, c.want) {
			t.Errorf("%s -> %s: %v, want %v", c.old, c.new, err, c.want)
		}
	}
	for _, bad := range []string{"../out", "A b", ""} {
		if err := th.Rename("green", bad); err == nil {
			t.Errorf("rename to %q accepted", bad)
		}
	}
	if _, err := th.Load("green"); err != nil {
		t.Errorf("green broke: %v", err)
	}
	noStrays(t, dir)
}

func TestDeleteRemovesCustomThemesOnly(t *testing.T) {
	th, dir := newWritable(t)
	if err := th.Create(tealManifest("teal"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := th.Load("teal"); err != nil {
		t.Fatal(err)
	}
	if err := th.Delete("teal"); err != nil {
		t.Fatal(err)
	}
	if _, err := th.Load("teal"); err == nil {
		t.Error("deleted theme still loads")
	}
	if _, err := os.Stat(filepath.Join(dir, "teal")); err == nil {
		t.Error("directory remains")
	}
	if err := th.Delete("teal"); !errors.Is(err, ErrThemeMissing) {
		t.Errorf("again: %v", err)
	}
	if err := th.Delete("stripe"); !errors.Is(err, ErrThemeBundled) {
		t.Errorf("bundled: %v", err)
	}
	if err := th.Delete("../.."); err == nil {
		t.Error("traversal accepted")
	}
}

func TestWritesNeedADataDirectory(t *testing.T) {
	th := NewThemes(Bundled(), "")
	if err := th.Create(tealManifest("teal"), nil); err == nil {
		t.Error("create without a data directory")
	}
}
