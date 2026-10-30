package invoice_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/velixio/paymux/internal/domains"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

type hostLookup map[string]string // host -> app id

func (h hostLookup) LookupHost(_ context.Context, host string) (domains.Domain, bool, error) {
	app, ok := h[host]
	return domains.Domain{AppID: app, Status: domains.StatusActive}, ok, nil
}

// A custom domain serves only its own app's pages: an invoice of another app,
// even with its right token, is a 404 there (and still served on the base host
// and on the app's own domain).
func TestHostedInvoiceOnAnotherAppsDomainIsNotFound(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /i/{id}", e.inv.ServeHosted)
		router := &domains.Router{BaseHost: "pay.example.com", Lookup: hostLookup{"pay.mine.example.org": "app_1", "pay.other.example.org": "app_2"},
			Base: mux, Customer: mux}
		srv := httptest.NewServer(router)
		t.Cleanup(srv.Close)

		e.pay(118000, "INR", intraB2C, "pa_1")
		hosted, pdf := urlsOf(t, e.eventsOf("invoice.issued")[0].Data)
		get := func(host, path string) int {
			req, _ := http.NewRequest("GET", srv.URL+path, nil)
			req.Host = host
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			return res.StatusCode
		}
		for _, path := range []string{hosted, pdf} {
			if code := get("pay.other.example.org", path); code != 404 {
				t.Errorf("%s on another app's domain: %d", path, code)
			}
		}
		if code := get("pay.mine.example.org", hosted); code != 200 {
			t.Errorf("own domain: %d", code)
		}
		if code := get("pay.example.com", hosted); code != 200 {
			t.Errorf("base host: %d", code)
		}
	})
}
