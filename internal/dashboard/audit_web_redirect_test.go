package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAuditWebSafeNextStaysOnSite: safeNext only checks that the raw value
// starts with /dashboard/; http.Redirect then path.Cleans it, so dot segments
// can climb out of /dashboard and leave "/\host", which browsers read as the
// protocol-relative "//host" (an open redirect after mode switch, reauth,
// payment actions).
func TestAuditWebSafeNextStaysOnSite(t *testing.T) {
	for _, next := range []string{
		`/dashboard/../\evil.example`,
		`/dashboard/..\..\/evil.example`,
		`/dashboard/%2e%2e/\evil.example`,
	} {
		got := safeNext(next, prefix+"/overview")
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/dashboard/mode", nil)
		http.Redirect(w, r, got, http.StatusSeeOther)
		loc := w.Header().Get("Location")
		if strings.HasPrefix(loc, `/\`) || strings.HasPrefix(loc, "//") || strings.HasPrefix(loc, `\`) || !strings.HasPrefix(loc, prefix+"/") {
			t.Errorf("next=%q: safeNext=%q, Location=%q leaves the dashboard", next, got, loc)
		}
	}
}
