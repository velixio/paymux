package hub

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/domains"
)

func TestAPIReferenceIsServedOnTheBaseHost(t *testing.T) {
	dir := t.TempDir()
	r := startApp(t, testConfig(t, "sqlite://"+dir+"/paymux.db", dir))
	waitReady(t, r)
	addr := r.app.Addr()

	code, page := fetch(t, addr, "/docs")
	if code != 200 {
		t.Fatalf("/docs: %d", code)
	}
	for _, want := range []string{"Paymux API", `id="op-createPayment"`, `id="op-createPaymentLink"`, `id="schema-Payment"`, "/docs/openapi.yaml", "/theme/default/theme.css"} {
		if !strings.Contains(page, want) {
			t.Errorf("/docs lacks %q", want)
		}
	}
	if strings.Contains(page, "<script") {
		t.Error("/docs must not need JavaScript")
	}

	spec, err := os.ReadFile("../../docs/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get("http://" + addr + "/docs/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if resp.StatusCode != 200 || sb.String() != string(spec) || !strings.Contains(resp.Header.Get("Content-Type"), "yaml") {
		t.Errorf("/docs/openapi.yaml: %d %q, %d bytes of %d", resp.StatusCode, resp.Header.Get("Content-Type"), sb.Len(), len(spec))
	}

	// Custom domains serve customer pages only, not the API reference.
	for _, p := range []string{"/docs", "/docs/openapi.yaml"} {
		if domains.IsCustomerRoute(p) {
			t.Errorf("%s must stay on the base host", p)
		}
	}
	shutdown(t, r)
}
