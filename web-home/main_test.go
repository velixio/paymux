package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestWebsiteRoutes(t *testing.T) {
	h := websiteHandler(fstest.MapFS{"index.html": {Data: []byte(`<html><div id="root"></div><script src="/assets/main-test.js"></script></html>`)}, "assets/main-test.js": {Data: []byte("console.log('test')")}})
	for _, path := range []string{"/", "/integrations", "/docs", "/self-hosting"} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
			if w.Code != 200 || !strings.Contains(w.Body.String(), `id="root"`) {
				t.Fatalf("page failed: %d", w.Code)
			}
			if w.Header().Get("Content-Security-Policy") == "" {
				t.Fatal("missing CSP")
			}
		})
	}
	for _, path := range []string{"/missing", "/assets/missing.js"} {
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
