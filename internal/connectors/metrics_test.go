package connectors_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/connectors"
	"github.com/paymux/paymux/internal/fakeconn"
	"github.com/paymux/paymux/internal/metrics"
	"github.com/paymux/paymux/internal/secrets"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/pkg/connector"
)

func TestProviderRequestsAreObservedPerConnector(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		reg := metrics.NewRegistry()
		box, _ := secrets.NewBox(bytes.Repeat([]byte{3}, 32))
		base, _ := url.Parse("https://pay.example.com")
		e.svc = connectors.New(connectors.Options{DB: db, Box: box, BaseURL: base, Now: e.clk.Now, Observe: connectors.ProviderMetrics(reg)})
		pa := e.create(fakeconn.ValidConfig)
		if _, err := e.svc.Get(ctx, pa.ID); err != nil {
			t.Fatal(err)
		}
		var deps connector.Deps
		e.fake.Locked(func() { deps = e.fake.Deps[len(e.fake.Deps)-1] })

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/limited" {
				w.WriteHeader(429)
			}
		}))
		defer srv.Close()
		for _, p := range []string{"/ok", "/ok", "/limited"} {
			resp, err := deps.HTTP.Post(srv.URL+p, "text/plain", strings.NewReader("x"))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
		}
		rec := httptest.NewRecorder()
		reg.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
		out := rec.Body.String()
		name := e.fake.Name
		for _, want := range []string{
			`paymux_provider_requests_total{connector="` + name + `",outcome="ok"} 2`,
			`paymux_provider_requests_total{connector="` + name + `",outcome="rate_limited"} 1`,
			`paymux_provider_request_duration_seconds_count{connector="` + name + `"} 3`,
			`paymux_provider_request_duration_seconds_bucket{connector="` + name + `",le="+Inf"} 3`,
		} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q in\n%s", want, out)
			}
		}
		if strings.Contains(out, srv.URL) || strings.Contains(out, "/limited") {
			t.Errorf("a URL leaked into the metrics:\n%s", out)
		}
	})
}
