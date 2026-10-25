package web

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
)

// Errors of the theme write path. Callers match them with errors.Is.
var (
	ErrThemeBundled = errors.New("that name belongs to a built-in theme, which cannot be changed or replaced")
	ErrThemeExists  = errors.New("a theme with that name already exists")
	ErrThemeMissing = errors.New("no custom theme has that name")
	errNoThemeDir   = errors.New("themes cannot be saved: no data directory is configured")
)

// Dir is the data directory holding custom themes ("" when there is none).
func (t *Themes) Dir() string { return t.dir }

// IsBundled reports whether name is a theme shipped inside the binary. A custom
// theme may never take such a name, even if the directory is free.
func (t *Themes) IsBundled(name string) bool {
	if t.bundled == nil || !themeNameRE.MatchString(name) {
		return false
	}
	_, err := fs.Stat(t.bundled, path.Join(name, "theme.json"))
	return err == nil
}

// Invalidate forgets every loaded theme, so the next Load reads the disk. The
// write methods call it; it is also what makes a change visible at once.
func (t *Themes) Invalidate() {
	t.mu.Lock()
	clear(t.cache)
	t.mu.Unlock()
}

func randSuffix() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// root opens the themes directory, creating it when asked.
func (t *Themes) root(create bool) (*os.Root, error) {
	if t.dir == "" {
		return nil, errNoThemeDir
	}
	if create {
		if err := os.MkdirAll(t.dir, 0o755); err != nil {
			return nil, err
		}
	}
	r, err := os.OpenRoot(t.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrThemeMissing
	}
	return r, err
}

func encodeManifest(m Manifest) ([]byte, error) {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	// The same rules a loaded theme.json must pass.
	if _, err := ParseManifest(raw, m.Name); err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// writeAtomic replaces dir/theme.json through a temporary file in the same
// directory, so a reader sees the old file or the new one, never half of one.
func writeAtomic(r *os.Root, dir string, data []byte) error {
	tmp := path.Join(dir, ".theme.json.tmp-"+randSuffix())
	if err := r.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := r.Rename(tmp, path.Join(dir, "theme.json")); err != nil {
		_ = r.Remove(tmp)
		return err
	}
	return nil
}

// custom opens the data directory and checks name is a custom theme there.
func (t *Themes) custom(name string) (*os.Root, error) {
	if !themeNameRE.MatchString(name) {
		return nil, fmt.Errorf("theme: %q is not a valid theme name", name)
	}
	if t.IsBundled(name) { // also a directory shadowing one: not ours to edit
		return nil, ErrThemeBundled
	}
	r, err := t.root(false)
	if err != nil {
		return nil, err
	}
	if fi, err := r.Lstat(name); err != nil || !fi.IsDir() {
		r.Close()
		return nil, ErrThemeMissing
	}
	return r, nil
}

// Create writes a new custom theme. The manifest is validated like any
// theme.json; the name may not be a built-in theme's or one in use. When from
// is given, its css, templates and assets are copied (its theme.json is not;
// the caller decides what the new manifest keeps). The theme appears in one
// step: it is built in a hidden directory and renamed into place.
func (t *Themes) Create(m Manifest, from *Theme) error {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	defer t.Invalidate()
	if !themeNameRE.MatchString(m.Name) {
		return fmt.Errorf("theme: %q is not a valid theme name: use 1-40 lower-case letters, digits or hyphens", m.Name)
	}
	if t.IsBundled(m.Name) {
		return ErrThemeBundled
	}
	raw, err := encodeManifest(m)
	if err != nil {
		return err
	}
	r, err := t.root(true)
	if err != nil {
		return err
	}
	defer r.Close()
	if _, err := r.Lstat(m.Name); err == nil {
		return ErrThemeExists
	}
	tmp := ".new-" + m.Name + "-" + randSuffix()
	if err := r.Mkdir(tmp, 0o755); err != nil {
		return err
	}
	fail := func(err error) error { _ = r.RemoveAll(tmp); return err }
	if from != nil {
		err := fs.WalkDir(from.FS, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || p == "." || p == "theme.json" {
				return err
			}
			if d.IsDir() {
				return r.MkdirAll(path.Join(tmp, p), 0o755)
			}
			b, err := fs.ReadFile(from.FS, p)
			if err != nil {
				return err
			}
			return r.WriteFile(path.Join(tmp, p), b, 0o644)
		})
		if err != nil {
			return fail(err)
		}
	}
	if err := r.WriteFile(path.Join(tmp, "theme.json"), raw, 0o644); err != nil {
		return fail(err)
	}
	if err := r.Rename(tmp, m.Name); err != nil {
		return fail(ErrThemeExists)
	}
	return nil
}

// Update changes a custom theme's manifest: edit receives the current one and
// whatever it leaves alone is kept. The result is validated before anything is
// written, and written atomically.
func (t *Themes) Update(name string, edit func(*Manifest) error) error {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	defer t.Invalidate()
	r, err := t.custom(name)
	if err != nil {
		return err
	}
	defer r.Close()
	cur, err := r.ReadFile(path.Join(name, "theme.json"))
	if err != nil {
		return fmt.Errorf("theme %s: %w", name, err)
	}
	m, err := ParseManifest(cur, name)
	if err != nil {
		return err
	}
	if err := edit(&m); err != nil {
		return err
	}
	raw, err := encodeManifest(m)
	if err != nil {
		return err
	}
	return writeAtomic(r, name, raw)
}

// Rename gives a custom theme a new name (its directory and the name inside
// theme.json). Whatever refers to the old name is the caller's to follow.
func (t *Themes) Rename(old, new string) error {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	defer t.Invalidate()
	r, err := t.custom(old)
	if err != nil {
		return err
	}
	defer r.Close()
	if !themeNameRE.MatchString(new) {
		return fmt.Errorf("theme: %q is not a valid theme name: use 1-40 lower-case letters, digits or hyphens", new)
	}
	if t.IsBundled(new) {
		return ErrThemeBundled
	}
	if _, err := r.Lstat(new); err == nil {
		return ErrThemeExists
	}
	cur, err := r.ReadFile(path.Join(old, "theme.json"))
	if err != nil {
		return err
	}
	m, err := ParseManifest(cur, old)
	if err != nil {
		return err
	}
	m.Name = new
	raw, err := encodeManifest(m)
	if err != nil {
		return err
	}
	if err := r.Rename(old, new); err != nil {
		return err
	}
	if err := writeAtomic(r, new, raw); err != nil {
		_ = r.Rename(new, old) // put it back as it was
		return err
	}
	return nil
}

// Delete removes a custom theme's directory.
func (t *Themes) Delete(name string) error {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	defer t.Invalidate()
	r, err := t.custom(name)
	if err != nil {
		return err
	}
	defer r.Close()
	return r.RemoveAll(name)
}
