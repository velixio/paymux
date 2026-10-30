package hub

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/config"
)

// With PAYMUX_OTLP_LOGS_ENDPOINT set, the warnings and errors that are stored
// are also posted to the collector, with the configured headers. The header
// values are secrets: they never reach the stored rows.
func TestStoredRowsAreAlsoShippedOverOTLP(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	var auth string
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies, auth = append(bodies, string(b)), r.Header.Get("Authorization")
		mu.Unlock()
	}))
	defer collector.Close()
	dir := t.TempDir()
	cfg := testConfig(t, "sqlite://"+dir+"/paymux.db", dir)
	cfg.OTLPLogsEndpoint = collector.URL + "/v1/logs"
	cfg.OTLPLogsHeaders = map[string]config.Secret{"Authorization": "Bearer collector-secret-token"}
	r := startApp(t, cfg)
	waitReady(t, r)
	if code := getWith(t, r.app.Addr(), "/v1/payments", "X-Request-Id", "req-otlp-1"); code != 401 {
		t.Fatalf("status %d", code)
	}
	waitFor(t, "the OTLP request", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, b := range bodies {
			if strings.Contains(b, "req-otlp-1") {
				return true
			}
		}
		return false
	})
	mu.Lock()
	got := auth
	mu.Unlock()
	if got != "Bearer collector-secret-token" {
		t.Errorf("Authorization = %q", got)
	}
	for _, row := range logRows(t, r) {
		if strings.Contains(row.Msg+row.Attrs+row.IDs+row.Request+row.Response, "collector-secret-token") {
			t.Errorf("the collector's token reached a row: %+v", row)
		}
	}
	shutdown(t, r)
	time.Sleep(10 * time.Millisecond)
}

// Audit R-A10: a configured secret (here the collector's own token) that is
// logged by accident is redacted in the OTLP request as it is in the stored row.
func TestOTLPPayloadNeverCarriesAConfiguredSecret(t *testing.T) {
	const token = "collector-secret-token-2"
	var mu sync.Mutex
	var bodies []string
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
	}))
	defer collector.Close()
	dir := t.TempDir()
	cfg := testConfig(t, "sqlite://"+dir+"/paymux.db", dir)
	cfg.OTLPLogsEndpoint = collector.URL + "/v1/logs"
	cfg.OTLPLogsHeaders = map[string]config.Secret{"Authorization": config.Secret("Bearer " + token)}
	r := startApp(t, cfg)
	waitReady(t, r)
	r.app.log.Warn("leak marker otlp-redact-1: header was Bearer "+token, "auth", token)
	waitFor(t, "the OTLP request", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, b := range bodies {
			if strings.Contains(b, "otlp-redact-1") {
				return true
			}
		}
		return false
	})
	mu.Lock()
	for _, b := range bodies {
		if strings.Contains(b, token) {
			t.Errorf("the token reached the collector:\n%s", b)
		}
	}
	mu.Unlock()
	shutdown(t, r)
}
