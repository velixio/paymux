package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	files["go.mod"] = "module example.com/m\n\ngo 1.27\n"
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const base = `package a

import "context"

// Doc comments are not API.
type Thing struct {
	Name string ` + "`json:\"name\"`" + `
	secret int
}

func (t Thing) Do(ctx context.Context) error { return nil }
func (t Thing) hidden()                       {}

type Doer interface{ Do(ctx context.Context) error }

const Max = 3

var ErrX = context.Canceled

func New() *Thing { return nil }
func helper()     {}
`

func snap(t *testing.T, root string) string {
	t.Helper()
	s, err := Snapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSnapshotHasExportedAPIOnly(t *testing.T) {
	s := snap(t, tree(t, map[string]string{"pkg/a/a.go": base, "pkg/a/a_test.go": "package a\nfunc TestOnly() {}\n", "pkg/a/testdata/x.go": "package x\nfunc Hidden() {}\n"}))
	for _, want := range []string{`package a // import "example.com/m/pkg/a"`, "type Thing struct", "Name string", "func (t Thing) Do(ctx context.Context) error",
		"type Doer interface", "Max = 3", "ErrX = context.Canceled", "func New() *Thing"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	for _, not := range []string{"secret", "hidden", "helper", "TestOnly", "Hidden", "Doc comments", "return nil"} {
		if strings.Contains(s, not) {
			t.Errorf("unexpected %q in\n%s", not, s)
		}
	}
}

func TestSnapshotIgnoresInternalsAndDocsButNotTheAPI(t *testing.T) {
	ref := snap(t, tree(t, map[string]string{"pkg/a/a.go": base}))

	same := strings.NewReplacer("// Doc comments are not API.", "// Something else entirely.", "secret int", "other string", "func helper()     {}", "func helper2() { _ = 1 }",
		"return nil }\nfunc (t Thing) hidden", "return context.Canceled }\nfunc (t Thing) hidden").Replace(base)
	if got := snap(t, tree(t, map[string]string{"pkg/a/a.go": same})); got != ref {
		t.Errorf("internals changed the snapshot:\n%s", got)
	}

	for name, changed := range map[string]string{
		"new method":      base + "\nfunc (t Thing) More() {}\n",
		"new field":       strings.Replace(base, "secret int", "Extra int", 1),
		"signature":       strings.Replace(base, "func New() *Thing", "func New(x int) *Thing", 1),
		"removed const":   strings.Replace(base, "const Max = 3", "", 1),
		"interface":       strings.Replace(base, "Do(ctx context.Context) error }", "Do(ctx context.Context) error; Stop() }", 1),
		"tag":             strings.Replace(base, `json:\"name\"`, `json:\"n\"`, 1),
		"new package":     base,
		"changed const":   strings.Replace(base, "Max = 3", "Max = 4", 1),
		"new exported fn": base + "\nfunc Extra() {}\n",
	} {
		files := map[string]string{"pkg/a/a.go": changed}
		if name == "new package" {
			files["pkg/b/b.go"] = "package b\nfunc B() {}\n"
		}
		if name == "tag" {
			files["pkg/a/a.go"] = strings.Replace(base, `json:"name"`, `json:"n"`, 1)
		}
		if got := snap(t, tree(t, files)); got == ref {
			t.Errorf("%s: snapshot did not change", name)
		}
	}
}

func TestSnapshotCoversTheHubPackage(t *testing.T) {
	root := `package m

type App struct{ n int }

func (a *App) OnThing(f func(string)) {}
func (a *App) internal()            {}
func New() *App                     { return nil }
`
	files := map[string]string{"pkg/a/a.go": base, "pkg/hub/m.go": root, "pkg/hub/m_test.go": "package m\nfunc TestRootOnly() {}\n",
		"internal/x/x.go": "package x\nfunc Inner() {}\n"}
	ref := snap(t, tree(t, files))
	for _, want := range []string{`package m // import "example.com/m/pkg/hub"`, "func New() *App", "func (a *App) OnThing(f func(string))", `package a // import "example.com/m/pkg/a"`} {
		if !strings.Contains(ref, want) {
			t.Errorf("missing %q in\n%s", want, ref)
		}
	}
	for _, not := range []string{"internal()", "TestRootOnly", "Inner"} {
		if strings.Contains(ref, not) {
			t.Errorf("unexpected %q in\n%s", not, ref)
		}
	}
	files["pkg/hub/m.go"] = root + "\nfunc (a *App) OnOther() {}\n"
	if got := snap(t, tree(t, files)); got == ref {
		t.Error("a new method on the hub package did not change the snapshot")
	}
}

func TestCheck(t *testing.T) {
	root := tree(t, map[string]string{"pkg/a/a.go": base})
	s := snap(t, root)
	file := filepath.Join(root, "scripts", "apisnap", "pkg.txt")

	if err := Check(s, file); err == nil || !strings.Contains(err.Error(), "-w") {
		t.Errorf("missing file: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Check(s, file); err != nil {
		t.Errorf("up to date: %v", err)
	}
	err := Check(snap(t, tree(t, map[string]string{"pkg/a/a.go": base + "\nfunc Added() {}\n"})), file)
	if err == nil || !strings.Contains(err.Error(), "+ func Added()") || !strings.Contains(err.Error(), "out of date") {
		t.Errorf("changed API: %v", err)
	}
	err = Check(snap(t, tree(t, map[string]string{"pkg/a/a.go": strings.Replace(base, "func New() *Thing { return nil }", "", 1)})), file)
	if err == nil || !strings.Contains(err.Error(), "- func New() *Thing") {
		t.Errorf("removed API: %v", err)
	}
}

// The committed snapshot of this repository must be current: the same check CI runs.
func TestCommittedSnapshotIsCurrent(t *testing.T) {
	root := filepath.Join("..", "..")
	s, err := Snapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := Check(s, filepath.Join(root, "scripts", "apisnap", "pkg.txt")); err != nil {
		t.Fatal(err)
	}
}
