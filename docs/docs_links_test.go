package docs

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	mdLinkRE    = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	mdHeadingRE = regexp.MustCompile(`(?m)^#{1,6}\s+(.+?)\s*$`)
)

// githubSlug is how GitHub turns a heading into an anchor.
func githubSlug(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.NewReplacer("`", "", "*", "").Replace(h)
	var b strings.Builder
	for _, r := range h {
		switch {
		case r == ' ' || r == '-':
			b.WriteRune('-')
		case r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r > 127:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// TestMarkdownLinksPointAtRealFilesAndHeadings keeps the README and the docs
// honest: a renamed file or heading breaks the build instead of the reader.
func TestMarkdownLinksPointAtRealFilesAndHeadings(t *testing.T) {
	files := []string{"README.md"}
	more, _ := filepath.Glob("docs/*.md")
	files = append(files, more...)
	checked := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range mdLinkRE.FindAllStringSubmatch(string(raw), -1) {
			target := m[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") || strings.HasPrefix(target, "<") {
				continue
			}
			path, anchor, _ := strings.Cut(target, "#")
			dest := f
			if path != "" {
				dest = filepath.Join(filepath.Dir(f), path)
			}
			info, err := os.Stat(dest)
			if err != nil {
				t.Errorf("%s links to %s, which does not exist", f, target)
				continue
			}
			checked++
			if anchor == "" || info.IsDir() || !strings.HasSuffix(dest, ".md") {
				continue
			}
			body, _ := os.ReadFile(dest)
			found := false
			for _, h := range mdHeadingRE.FindAllStringSubmatch(string(body), -1) {
				if githubSlug(h[1]) == anchor {
					found = true
				}
			}
			if !found {
				t.Errorf("%s links to %s, but %s has no such heading", f, target, dest)
			}
		}
	}
	if checked < 10 {
		t.Errorf("only %d links checked; the README should link to the docs", checked)
	}
}

func TestREADMEListsTheDocsAndTheBuiltConnectors(t *testing.T) {
	raw, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme := string(raw)
	for _, want := range []string{"docs/runbook.md", "docs/extending.md", "docs/connectors.md", "docs/capabilities.md", "docs/openapi.yaml", "#upgrades",
		"Razorpay", "PayU", "Zoho", "Whop", "Remote", "Payment links", "SQLite", "Postgres", "GST", "Subscriptions", "themes", "Hindi", "setup wizard", "framework", "Litestream", "/docs"} {
		if !strings.Contains(strings.ToLower(readme), strings.ToLower(want)) {
			t.Errorf("README does not mention %q", want)
		}
	}
	// The connector table in the README is a summary of docs/capabilities.md.
	caps, err := os.ReadFile("docs/capabilities.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"payu", "razorpay", "whop", "zoho", "remote"} {
		if !strings.Contains(string(caps), "["+name+"]") {
			t.Errorf("docs/capabilities.md lacks %s", name)
		}
	}
}

// The checks below name files from the repository root (README.md, docs/...),
// so the tests run from there.
func init() {
	if err := os.Chdir(".."); err != nil {
		panic(err)
	}
}
