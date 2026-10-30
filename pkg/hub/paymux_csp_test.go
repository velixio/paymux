package hub

import (
	"net/http"
	"strings"
	"testing"

	"github.com/velixio/paymux/internal/domains"
)

// Browsers post violation reports of the hosted pages' enforced policy to
// /csp-report on the host that served the page, base host or custom domain.
func TestCSPReportsAreReceivedAndCounted(t *testing.T) {
	dir := t.TempDir()
	r := startApp(t, testConfig(t, "sqlite://"+dir+"/paymux.db", dir))
	waitReady(t, r)
	addr := r.app.Addr()
	if !domains.IsCustomerRoute("/csp-report") {
		t.Error("/csp-report must be served on custom domains")
	}
	report := `{"csp-report":{"effective-directive":"script-src-elem","blocked-uri":"https://evil.example.org/x.js?t=secret"}}`
	resp, err := http.Post("http://"+addr+"/csp-report", "application/csp-report", strings.NewReader(report))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if code, body := fetch(t, addr, "/metrics"); code == 200 && !strings.Contains(body, `paymux_csp_violations_total{directive="script-src-elem"} 1`) {
		t.Errorf("violation not counted:\n%s", body)
	}
	shutdown(t, r)
}
