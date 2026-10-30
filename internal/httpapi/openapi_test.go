package httpapi_test

import (
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/velixio/paymux/internal/httpapi"
)

type captureMux struct{ patterns []string }

func (c *captureMux) HandleFunc(p string, _ http.HandlerFunc) { c.patterns = append(c.patterns, p) }

// TestOpenAPIListsExactlyTheRegisteredRoutes keeps docs/openapi.yaml true: a
// route added without documentation, or documented but missing, fails.
func TestOpenAPIListsExactlyTheRegisteredRoutes(t *testing.T) {
	m := &captureMux{}
	(&httpapi.API{}).Register(m) // Register only binds method values
	var registered []string
	for _, p := range m.patterns {
		if strings.Contains(p, " ") { // skip the method-less /v1/ catch-all
			registered = append(registered, p)
		}
	}

	raw, err := os.ReadFile("../../docs/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var documented []string
	path := ""
	inPaths := false
	pathRE := regexp.MustCompile(`^  (/v1/\S+):$`)
	methodRE := regexp.MustCompile(`^    (get|post|put|patch|delete):$`)
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case line == "paths:":
			inPaths = true
		case inPaths && line != "" && !strings.HasPrefix(line, " "):
			inPaths = false
		case inPaths && pathRE.MatchString(line):
			path = pathRE.FindStringSubmatch(line)[1]
		case inPaths && methodRE.MatchString(line):
			documented = append(documented, strings.ToUpper(methodRE.FindStringSubmatch(line)[1])+" "+path)
		}
	}
	sort.Strings(registered)
	sort.Strings(documented)
	if strings.Join(registered, "\n") != strings.Join(documented, "\n") {
		t.Fatalf("openapi.yaml out of sync\nregistered:\n%s\ndocumented:\n%s", strings.Join(registered, "\n"), strings.Join(documented, "\n"))
	}
	if !strings.HasPrefix(string(raw), "openapi: 3.1") {
		t.Error("spec must be OpenAPI 3.1")
	}
}
