package dashboard

import (
	"net/url"
	"testing"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

// An endpoint is added for one mode (live unless the form says test), and the
// page says which.
func TestWebhookEndpointModeFromDashboard(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app := e.seedApp("alpha")
		admin := e.roleClient("admin")
		page := "/dashboard/apps/" + app.ID
		tok := csrfOf(t, e.pageOK(admin, page+"?tab=webhooks"))
		for _, c := range []struct {
			url, mode string
			want      int
		}{{"https://hooks.example.com/t", "test", 200}, {"https://hooks.example.com/l", "live", 200}, {"https://hooks.example.com/d", "", 200}, {"https://hooks.example.com/x", "staging", 400}} {
			f := url.Values{"url": {c.url}, "csrf": {tok}}
			if c.mode != "" {
				f.Set("mode", c.mode)
			}
			if r, _ := e.post(admin, page+"/endpoints", f); r.StatusCode != c.want {
				t.Errorf("mode %q: %d", c.mode, r.StatusCode)
			}
		}
		got := map[string]string{}
		eps, _ := e.out.ListEndpoints(ctx, app.ID)
		for _, ep := range eps {
			got[ep.URL] = ep.Mode
		}
		want := map[string]string{"https://hooks.example.com/t": "test", "https://hooks.example.com/l": "live", "https://hooks.example.com/d": "live"}
		if len(got) != 3 {
			t.Fatalf("endpoints %v", got)
		}
		for u, m := range want {
			if got[u] != m {
				t.Errorf("%s mode = %q, want %q", u, got[u], m)
			}
		}
		body := e.pageOK(admin, page+"?tab=webhooks")
		has(t, body, "<th>Mode</th>", ">test<", ">live<", `name="mode"`)
	})
}
