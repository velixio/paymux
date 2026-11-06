package dashboard

import (
	"net/url"
	"testing"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

func TestTerminalLoggingSettings(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		admin := e.roleClient("admin")
		page := e.pageOK(admin, "/dashboard/settings")
		has(t, page, "Terminal logging")
		has(t, settingRowHTML(t, page, "terminal_log_level"), "Terminal log level", "<select", `value="error"`, `value="debug"`)
		has(t, settingRowHTML(t, page, "terminal_log_requests"), "Log HTTP requests to terminal", `type="checkbox"`, "checked")

		raw := func(key string) string {
			t.Helper()
			v, err := e.set.ResolveOrg(ctx, store.DefaultOrgID, key)
			if err != nil {
				t.Fatal(err)
			}
			return string(v.Raw)
		}
		tok := csrfOf(t, page)
		post := func(key string, vals ...string) int {
			r, _ := e.post(admin, "/dashboard/settings/"+key, url.Values{"value": vals, "csrf": {tok}})
			return r.StatusCode
		}
		if code := post("terminal_log_level", "warn"); code != 303 {
			t.Errorf("save level: %d", code)
		}
		if got := raw("terminal_log_level"); got != `"warn"` {
			t.Errorf("level = %s", got)
		}
		// An unticked checkbox posts only the hidden false.
		if code := post("terminal_log_requests", "false"); code != 303 {
			t.Errorf("save requests: %d", code)
		}
		if got := raw("terminal_log_requests"); got != "false" {
			t.Errorf("requests = %s", got)
		}
		if code := post("terminal_log_level", "trace"); code != 400 {
			t.Errorf("an unknown level saved: %d", code)
		}
	})
}
