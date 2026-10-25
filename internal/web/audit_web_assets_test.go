package web

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAuditWebAssetsNoDirectoryListing: /assets/ is served by
// http.FileServerFS, which renders an HTML index of any directory that has no
// index.html. The listing is public (base host and every custom domain) and
// names every bundled and operator-overlaid file.
func TestAuditWebAssetsNoDirectoryListing(t *testing.T) {
	h := AssetHandler(Embedded())
	for _, p := range []string{"/assets/", "/assets/js/", "/assets/css/"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code == 200 && strings.Contains(w.Body.String(), "<a href=") {
			t.Errorf("GET %s lists the directory (%d): %.120q", p, w.Code, w.Body.String())
		}
	}
}
