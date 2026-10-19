package hub

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

// logRows returns every stored log row, bodies included.
func logRows(t *testing.T, r *running) []store.LogRow {
	t.Helper()
	l, rec := r.app.logStore()
	if l == nil || rec == nil {
		t.Fatal("the app has no database log")
	}
	rec.Flush(context.Background())
	var out []store.LogRow
	if err := l.Read(context.Background(), func(q store.Querier) (err error) {
		out, err = store.SearchLogs(context.Background(), q, store.LogFilter{WithBodies: true, Limit: 5000})
		return
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func getWith(t *testing.T, addr, path string, hdr ...string) int {
	t.Helper()
	req, _ := http.NewRequest("GET", "http://"+addr+path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestServeKeepsErrorsAndWarningsInTheDatabase(t *testing.T) {
	for _, name := range []string{"sqlite", "postgres"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			url := "sqlite://" + dir + "/paymux.db"
			if name == "postgres" {
				url = storetest.PostgresURL(t)
			}
			r := startApp(t, testConfig(t, url, dir))
			waitReady(t, r)
			if name == "sqlite" {
				if _, err := os.Stat(filepath.Join(dir, "logs.db")); err != nil {
					t.Errorf("logs.db: %v", err)
				}
			}
			// A rejected API request is a warning; a healthy probe and a page 404 are nothing.
			if code := getWith(t, r.app.Addr(), "/v1/payments", "X-Request-Id", "req-warn-1"); code != 401 {
				t.Fatalf("status %d", code)
			}
			getWith(t, r.app.Addr(), "/healthz", "X-Request-Id", "req-ok-1")
			getWith(t, r.app.Addr(), "/no/such/page", "X-Request-Id", "req-404-1")
			var warn *store.LogRow
			for _, row := range logRows(t, r) {
				switch row.RequestID {
				case "req-ok-1", "req-404-1":
					t.Errorf("ordinary traffic stored: %+v", row)
				case "req-warn-1":
					row := row
					warn = &row
				}
				if row.Level != "warn" && row.Level != "error" {
					t.Errorf("a %s row was stored: %+v", row.Level, row)
				}
			}
			if warn == nil || warn.Kind != "http" || warn.Level != "warn" || warn.Status != 401 {
				t.Fatalf("the rejected request was not stored: %+v", warn)
			}
			// Metrics for the writer.
			code, body := fetch(t, r.app.InternalAddr(), "/metrics")
			if code != 200 || !strings.Contains(body, "paymux_logs_stored_total") || !strings.Contains(body, "paymux_logs_dropped_total") {
				t.Errorf("log metrics missing:\n%s", body)
			}
			shutdown(t, r)
			time.Sleep(10 * time.Millisecond)
		})
	}
}
