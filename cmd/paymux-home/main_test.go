package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestWebsiteRoutes(t *testing.T) {
	h := websiteHandler(fstest.MapFS{
		"index.html":          {Data: []byte(`<html><div id="root"></div><script src="/assets/main-test.js"></script></html>`)},
		"assets/main-test.js": {Data: []byte("console.log('test')")},
		"llms.txt":            {Data: []byte("# Paymux\n")},
		"llms-full.txt":       {Data: []byte("# Paymux merchant integration guide for coding agents\n")},
	})
	paths := []string{"/", "/integrations", "/docs", "/self-hosting", "/healthz"}
	var catalog []struct {
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(docsCatalog, &catalog); err != nil {
		t.Fatal(err)
	}
	for _, guide := range catalog {
		if guide.Slug != "" {
			paths = append(paths, "/docs/"+guide.Slug, "/docs/"+guide.Slug+"/")
		}
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
			if w.Code != 200 || (path != "/healthz" && !strings.Contains(w.Body.String(), `id="root"`)) {
				t.Fatalf("page failed: %d", w.Code)
			}
			if w.Header().Get("Content-Security-Policy") == "" {
				t.Fatal("missing CSP")
			}
		})
	}
	for _, path := range []string{"/missing", "/assets/missing.js", "/docs/not-a-guide", "/docs/payments/invalid"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 404 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/assets/main-test.js", nil))
	if w.Code != 200 || !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
		t.Fatal("static asset is not served with immutable caching")
	}
	for _, path := range []string{"/llms.txt", "/llms-full.txt"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || !strings.HasPrefix(w.Body.String(), "# Paymux") {
			t.Fatalf("%s: expected LLM documentation, got %d %q", path, w.Code, w.Body.String())
		}
		if got := w.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
			t.Fatalf("%s: content type %q", path, got)
		}
	}
}
func TestMissingFrontendIsNotReportedHealthy(t *testing.T) {
	h := websiteHandler(fstest.MapFS{})
	for _, path := range []string{"/", "/healthz"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 503 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
}
