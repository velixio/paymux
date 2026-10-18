package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebsiteRoutes(t *testing.T) {
	h := handler()
	for _, path := range []string{"/", "/integrations", "/docs", "/self-hosting"} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
			if w.Code != 200 || !strings.Contains(w.Body.String(), "<main") || strings.Contains(w.Body.String(), "ZgotmplZ") {
				t.Fatalf("page failed: %d", w.Code)
			}
			if w.Header().Get("Content-Security-Policy") == "" {
				t.Fatal("missing CSP")
			}
		})
	}
	for _, path := range []string{"/missing", "/static/missing.css"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 404 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
}
