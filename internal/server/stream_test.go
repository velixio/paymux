package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"
)

// Audit H-15: http.TimeoutHandler buffers the whole response and cuts it at 30
// seconds. The export routes stream under a deadline of their own.

func TestExportRoutesAreNotBufferedByTheTimeoutHandler(t *testing.T) {
	s, _, _ := newServer(t)
	proceed := make(chan struct{})
	s.HandleFunc("GET /dashboard/logs.jsonl", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("first line\n"))
		_ = http.NewResponseController(w).Flush()
		select { // the client must see the first line while the handler is still running
		case <-proceed:
		case <-time.After(5 * time.Second):
		}
		_, _ = w.Write([]byte("last line\n"))
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/dashboard/logs.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	buf := make([]byte, 11)
	if _, err := io.ReadFull(res.Body, buf); err != nil || string(buf) != "first line\n" {
		t.Fatalf("first chunk = %q %v (the response is buffered until the handler ends)", buf, err)
	}
	close(proceed)
	rest, _ := io.ReadAll(res.Body)
	if string(rest) != "last line\n" || res.Header.Get("X-Request-Id") == "" {
		t.Errorf("rest = %q, headers %v", rest, res.Header)
	}
}

func TestDashboardEventRoutePreservesStreaming(t *testing.T) {
	s, _, _ := newServer(t)
	proceed := make(chan struct{})
	s.HandleFunc("GET /dashboard/events", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("data: ready\n\n"))
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush: %v", err)
			return
		}
		select {
		case <-proceed:
		case <-r.Context().Done():
		}
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/dashboard/events")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	buf := make([]byte, len("data: ready\n\n"))
	if _, err := io.ReadFull(res.Body, buf); err != nil || string(buf) != "data: ready\n\n" {
		t.Fatalf("first event = %q %v", buf, err)
	}
	close(proceed)
}

func TestStreamedRoutesAreExactlyTheExportsAndEvents(t *testing.T) {
	for _, c := range []struct {
		method, path string
		want         bool
	}{
		{"GET", "/dashboard/events", true}, {"GET", "/dashboard/logs.jsonl", true}, {"GET", "/dashboard/reports/zip", true}, {"GET", "/dashboard/reports/file", true},
		{"POST", "/dashboard/logs.jsonl", false}, {"GET", "/dashboard/logs", false}, {"GET", "/dashboard/reports/zip/", false},
		{"GET", "/v1/payments", false}, {"GET", "/dashboard/reports", false},
	} {
		if got := IsStreamed(c.method, c.path); got != c.want {
			t.Errorf("IsStreamed(%s %s) = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}

// Every other route keeps the time limit; an export outlives it.
func TestOtherRoutesStillTimeOut(t *testing.T) {
	old := requestTimeout
	requestTimeout = 50 * time.Millisecond
	defer func() { requestTimeout = old }()
	s, _, _ := newServer(t)
	slow := func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte("done"))
	}
	s.HandleFunc("GET /v1/slow", slow)
	s.HandleFunc("GET /dashboard/reports/zip", slow)
	h := s.Handler()
	if rec := get(h, "/v1/slow"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("slow ordinary route: %d %s", rec.Code, rec.Body)
	}
	if rec := get(h, "/dashboard/reports/zip"); rec.Code != 200 || rec.Body.String() != "done" {
		t.Errorf("slow export: %d %s", rec.Code, rec.Body)
	}
}

// A 50 MB export passes through without being held in memory.
func TestLargeExportStreamsWithinMemory(t *testing.T) {
	s, _, _ := newServer(t)
	const total = 50 << 20
	chunk := make([]byte, 64<<10)
	for i := range chunk {
		chunk[i] = 'x'
	}
	s.HandleFunc("GET /dashboard/logs.jsonl", func(w http.ResponseWriter, r *http.Request) {
		for n := 0; n < total; n += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	res, err := http.Get(srv.URL + "/dashboard/logs.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	n, err := io.Copy(io.Discard, res.Body)
	if err != nil || n != total {
		t.Fatalf("got %d bytes, err %v", n, err)
	}
	runtime.ReadMemStats(&after)
	if used := after.TotalAlloc - before.TotalAlloc; used > total/2 {
		t.Errorf("allocated %d MB for a %d MB export: the response is buffered", used>>20, total>>20)
	}
}
