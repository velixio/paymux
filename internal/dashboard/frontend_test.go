package dashboard

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

// The server page stays hidden while the React workspace loads (dash.css);
// without JavaScript a nonce'd <noscript> style shows it at once.
func TestFrontendServerPageShownWithoutJavaScript(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		body := e.pageOK(e.roleClient("viewer"), "/dashboard/overview")
		re := regexp.MustCompile(`<noscript><style nonce="[A-Za-z0-9_-]{32}">#paymux-server-page\{visibility:visible;animation:none\}</style></noscript>`)
		if !re.MatchString(body) {
			t.Fatal("dashboard pages need a nonce'd <noscript> style that shows the server page")
		}
	})
}

func TestFrontendStyleNonceKeepsScriptPolicyStrict(t *testing.T) {
	first, second := http.Header{}, http.Header{}
	setSecurityHeaders(first)
	setSecurityHeaders(second)
	re := regexp.MustCompile(`style-src 'self' 'nonce-([A-Za-z0-9_-]{32})'`)
	a, b := re.FindStringSubmatch(first.Get("Content-Security-Policy")), re.FindStringSubmatch(second.Get("Content-Security-Policy"))
	if len(a) != 2 || len(b) != 2 || a[1] == b[1] {
		t.Fatal("style tags need fresh per-response nonces")
	}
	for _, h := range []http.Header{first, second} {
		csp := h.Get("Content-Security-Policy")
		if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") || !strings.Contains(csp, "default-src 'self'") {
			t.Fatal("frontend must not relax script execution or arbitrary inline styles")
		}
	}
}
