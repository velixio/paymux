// Package seams enforces architectural import rules by scanning Go sources.
package seams

import (
	"bufio"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Violation is one broken rule.
type Violation struct{ File, Rule, Detail string }

// settingsStoreCall matches direct use of the settings store functions. Policy
// values are read and written only through internal/settings.
var settingsStoreCall = regexp.MustCompile(`store\.(Get|Set|Seed|Delete)(Org|App)Setting\b`)

const maxMainLines = 25

// Check scans root (a module root) for seam violations. It skips testdata/
// directories (relative to root) and _test.go files.
func Check(root, modulePath string) ([]Violation, error) {
	var out []Violation
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if d.Name() == "testdata" || (rel != "." && strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return fmt.Errorf("seams: parse %s: %w", rel, err)
		}
		inStore := strings.HasPrefix(rel, "internal/store/")
		inConnector := strings.HasPrefix(rel, "connectors/")
		for _, spec := range f.Imports {
			imp, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if !inStore && isDBImport(imp) {
				out = append(out, Violation{rel, "sql-outside-store", "imports " + imp})
			}
			if inConnector && !isStdlib(imp) && !strings.HasPrefix(imp, modulePath+"/pkg/") {
				out = append(out, Violation{rel, "connector-imports", "imports " + imp})
			}
		}
		if !inStore && !strings.HasPrefix(rel, "internal/settings/") {
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if m := settingsStoreCall.Find(src); m != nil {
				out = append(out, Violation{rel, "settings-via-resolver", "calls " + string(m)})
			}
		}
		if rel == "cmd/paymux/main.go" {
			n, err := codeLines(path)
			if err != nil {
				return err
			}
			if n > maxMainLines {
				out = append(out, Violation{rel, "thin-main", fmt.Sprintf("%d code lines, max %d", n, maxMainLines)})
			}
		}
		return nil
	})
	return out, err
}

func isDBImport(imp string) bool {
	return imp == "database/sql" || strings.HasPrefix(imp, "database/sql/") ||
		imp == "modernc.org/sqlite" || strings.HasPrefix(imp, "modernc.org/sqlite/") ||
		imp == "github.com/jackc/pgx" || strings.HasPrefix(imp, "github.com/jackc/pgx/")
}

func isStdlib(imp string) bool {
	first, _, _ := strings.Cut(imp, "/")
	return !strings.Contains(first, ".")
}

// codeLines counts non-blank lines that are not comments.
func codeLines(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n, inBlock := 0, false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		switch {
		case inBlock:
			if strings.Contains(l, "*/") {
				inBlock = false
			}
		case l == "" || strings.HasPrefix(l, "//"):
		case strings.HasPrefix(l, "/*"):
			if !strings.Contains(l, "*/") {
				inBlock = true
			}
		default:
			n++
		}
	}
	return n, sc.Err()
}
