package server

import (
	"net/http"
	"strings"
	"testing"
)

// A plain-text 5xx page says which request failed, so a person can quote it
// and an operator can find the log row.
func TestPlainTextServerErrorsCarryTheRequestID(t *testing.T) {
	s := New(Options{})
	s.HandleFunc("GET /plain", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "internal error", http.StatusInternalServerError) })
	s.HandleFunc("GET /missing", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "not found", http.StatusNotFound) })
	s.HandleFunc("GET /json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"x"}`))
	})
	s.HandleFunc("GET /html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("<p>oops</p>"))
	})
	h := s.Handler()
	rec := get(h, "/plain", "X-Request-Id", "req-abc-1")
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "internal error") || !strings.Contains(rec.Body.String(), "request id: req-abc-1") {
		t.Errorf("plain 500: %d %q", rec.Code, rec.Body.String())
	}
	if rec := get(h, "/missing", "X-Request-Id", "req-abc-2"); strings.Contains(rec.Body.String(), "request id") {
		t.Errorf("a 404 is not a server error: %q", rec.Body.String())
	}
	if rec := get(h, "/json", "X-Request-Id", "req-abc-3"); rec.Body.String() != `{"error":"x"}` {
		t.Errorf("JSON bodies are left alone: %q", rec.Body.String())
	}
	if rec := get(h, "/html", "X-Request-Id", "req-abc-4"); rec.Body.String() != "<p>oops</p>" {
		t.Errorf("HTML bodies are left alone: %q", rec.Body.String())
	}
}
