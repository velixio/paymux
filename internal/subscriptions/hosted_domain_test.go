package subscriptions_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// A custom domain serves only its own app's pages. A checkout link of another
// app opened on it is a 404 and starts nothing at the provider.
func TestHostedCheckoutOnAnotherAppsDomainIsNotFound(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /s/{id}", e.subs.ServeHosted)
		router := &domains.Router{BaseHost: "pay.example.com", Lookup: hostLookup{"pay.mine.example.org": "app_1", "pay.other.example.org": "app_2"},
			Base: mux, Customer: mux}
		srv := httptest.NewServer(router)
		t.Cleanup(srv.Close)
		c := e.mustCreate(nil)
		u, _ := url.Parse(c.CheckoutURL)
		get := func(host string) int {
			req, _ := http.NewRequest("GET", srv.URL+u.RequestURI(), nil)
			req.Host = host
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			return res.StatusCode
		}
		if code := get("pay.other.example.org"); code != 404 {
			t.Errorf("another app's domain: %d", code)
		}
		if len(e.r.mandateReqs) != 0 {
			t.Errorf("a mandate was registered for a request that was refused: %d", len(e.r.mandateReqs))
		}
		if code := get("pay.mine.example.org"); code != 200 {
			t.Errorf("own domain: %d", code)
		}
		if code := get("pay.example.com"); code != 200 {
			t.Errorf("base host: %d", code)
		}
	})
}
