package dashboard

import (
	"testing"

	"github.com/velixio/paymux/internal/server"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

// Audit H-15: the export routes are served outside the timeout buffer, by the
// path list of internal/server. Each of them must exist in the dashboard and
// stay behind the session (a signed-out request is sent to the sign-in page,
// not served), so renaming one cannot silently put it back under the buffer.
func TestExportRoutesAreStreamedAndStayBehindSignIn(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		for _, p := range []string{prefix + "/logs.jsonl", prefix + "/reports/zip", prefix + "/reports/file"} {
			if !server.IsStreamed("GET", p) {
				t.Errorf("%s is not served as a stream", p)
			}
			r, _ := e.get(e.client(), p)
			if r.StatusCode != 303 || r.Header.Get("Location") != prefix+"/login" {
				t.Errorf("signed out GET %s: %d %q", p, r.StatusCode, r.Header.Get("Location"))
			}
		}
	})
}
