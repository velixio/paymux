package dashboard

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

func TestTimeZoneSettingMovesDashboardDates(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		admin := e.roleClient("admin")
		at := time.Date(2026, 1, 2, 20, 0, 0, 0, time.UTC)

		body := e.pageOK(admin, "/dashboard/settings")
		has(t, body, `action="/dashboard/settings/time_zone"`, `<select class="field" name="value" aria-label="time_zone">`, `<option value="Asia/Tokyo">`)
		has(t, body, "(UTC)") // "server" names the zone it stands for
		if got := e.d.fmtTime(at); got != "2026-01-02 20:00:00" {
			t.Errorf("server zone: %s", got)
		}

		tok := regexpFind(t, `name="csrf" value="([^"]+)"`, body)
		if r, _ := e.post(admin, "/dashboard/settings/time_zone", url.Values{"csrf": {tok}, "value": {"Mars/Base"}}); r.StatusCode != 400 {
			t.Errorf("unknown zone: %d", r.StatusCode)
		}
		if r, _ := e.post(admin, "/dashboard/settings/time_zone", url.Values{"csrf": {tok}, "value": {"Asia/Tokyo"}}); r.StatusCode != 303 {
			t.Errorf("save: %d", r.StatusCode)
		}
		e.pageOK(admin, "/dashboard/settings") // a request picks up the change
		if got := e.d.fmtTime(at); got != "2026-01-03 05:00:00" {
			t.Errorf("Tokyo: %s", got)
		}
		day, err := e.d.parseDay("2026-01-03")
		if err != nil || !day.Equal(time.Date(2026, 1, 2, 15, 0, 0, 0, time.UTC)) {
			t.Errorf("date filter: %v %v", day, err)
		}
		if strings.Contains(e.pageOK(admin, "/dashboard/settings"), "(UTC)") {
			t.Error("the server zone is still named after picking another")
		}

		if r, _ := e.post(admin, "/dashboard/settings/time_zone", url.Values{"csrf": {tok}, "value": {"server"}}); r.StatusCode != 303 {
			t.Errorf("back to server: %d", r.StatusCode)
		}
		e.pageOK(admin, "/dashboard/settings")
		if got := e.d.fmtTime(at); got != "2026-01-02 20:00:00" {
			t.Errorf("back to server: %s", got)
		}
	})
}
