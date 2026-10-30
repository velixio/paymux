package invoice_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
	"github.com/velixio/paymux/internal/web"
)

func TestHostedInvoiceUsesTheAppsTheme(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		rend, err := web.NewThemedRenderer(web.ThemedOptions{Base: web.Embedded(), Themes: web.NewThemes(web.Bundled(), ""), Recheck: -1})
		if err != nil {
			t.Fatal(err)
		}
		e.inv.SetPages(rend)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /i/{id}", e.inv.ServeHosted)

		p, _ := e.pay(118000, "INR", intraB2C, "pa_1")
		hosted, _ := urlsOf(t, e.eventsOf("invoice.issued")[0].Data)
		if _, body := get(t, mux, hosted); !strings.Contains(body, "/theme/default/theme.css") {
			t.Fatalf("default theme not linked: %s", body)
		}
		if err := e.set.SetApp(ctx, audit.System, store.DefaultOrgID, p.AppID, "theme", json.RawMessage(`"minimal-dark"`)); err != nil {
			t.Fatal(err)
		}
		res, body := get(t, mux, hosted)
		if !strings.Contains(body, "/theme/minimal-dark/theme.css") {
			t.Fatalf("the app's theme should be linked: %s", body)
		}
		// Theme CSS is served from the same origin, so the page's CSP still holds.
		if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "style-src 'self'") {
			t.Errorf("csp = %q", csp)
		}
	})
}
