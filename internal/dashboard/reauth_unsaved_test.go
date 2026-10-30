package dashboard

import (
	"net/url"
	"strings"
	"testing"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

// A write that needs a recent sign-in is not replayed after the confirmation,
// so the confirmation page must say the change was not saved: otherwise the
// person confirms, lands back on the page and believes it took effect.
func TestReauthAfterAWriteSaysTheChangeWasNotSaved(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		c := staleAdmin(t, e)
		tok := csrfOf(t, e.pageOK(c, "/dashboard/appearance"))
		r, _ := e.post(c, "/dashboard/appearance/theme", url.Values{"theme": {"default"}, "csrf": {tok}})
		loc := r.Header.Get("Location")
		if r.StatusCode != 303 || !strings.HasPrefix(loc, "/dashboard/reauth?") {
			t.Fatalf("stale write: %d %q, want a redirect to the confirmation page", r.StatusCode, loc)
		}
		has(t, e.pageOK(c, loc), "was not saved")

		// Opening the confirmation page by itself (a stale GET, or a link) says no such thing.
		if p := e.pageOK(c, "/dashboard/reauth?next=%2Fdashboard%2Foverview"); strings.Contains(p, "was not saved") {
			t.Errorf("a plain confirmation page claims a change was lost")
		}

		// The dashboard's router rewrites the query it was given and quotes the
		// value (unsaved=%221%22); the notice still shows.
		has(t, e.pageOK(c, "/dashboard/reauth?next=%2Fdashboard%2Foverview&unsaved=%221%22"), "was not saved")
	})
}
