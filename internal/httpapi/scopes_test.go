package httpapi_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/authz"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/httpapi"
	"github.com/paymux/paymux/internal/store"
)

func (f *fixture) scopedKey(mode string, scopes ...string) string {
	f.t.Helper()
	tok, _, err := f.svc.CreateAPIKeyWith(ctx, audit.System, core.OrgScope{OrgID: store.DefaultOrgID}, f.app.ID, mode, core.KeyOptions{Scopes: scopes})
	if err != nil {
		f.t.Fatal(err)
	}
	return tok
}

func TestScopedKeysAreCheckedPerRoute(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		body := map[string]any{"amount": 49900, "currency": "INR"}
		readOnly := f.scopedKey("test", authz.ScopePaymentsRead)
		writer := f.scopedKey("test", authz.ScopePaymentsWrite)
		refunds := f.scopedKey("test", authz.ScopeRefundsWrite)
		invoices := f.scopedKey("test", authz.ScopeInvoicesRead)

		// payments:read reads but does not create or cancel.
		r := f.do("POST", "/v1/payments", readOnly, body)
		if r.status != 403 || r.errType(t) != "insufficient_scope" || !strings.Contains(r.header.Get("WWW-Authenticate"), `error="insufficient_scope"`) ||
			!strings.Contains(r.header.Get("WWW-Authenticate"), "payments:write") {
			t.Fatalf("read-only key created a payment: %d %s %v", r.status, r.raw, r.header)
		}
		if f.count("SELECT COUNT(*) FROM payments") != 0 {
			t.Fatal("a payment was created by a key without the scope")
		}
		// payments:write creates, and reads (write covers read).
		r = f.do("POST", "/v1/payments", writer, body)
		if r.status != 201 && r.status != 200 {
			t.Fatalf("writer create: %d %s", r.status, r.raw)
		}
		id := r.json(t)["id"].(string)
		for _, path := range []string{"/v1/payments/" + id, "/v1/payments"} {
			if r := f.do("GET", path, writer, nil); r.status != 200 {
				t.Errorf("write key reading %s: %d", path, r.status)
			}
			if r := f.do("GET", path, readOnly, nil); r.status != 200 {
				t.Errorf("read key reading %s: %d", path, r.status)
			}
		}
		if r := f.do("POST", "/v1/payments/"+id+"/cancel", readOnly, nil); r.status != 403 {
			t.Errorf("read key cancelled a payment: %d", r.status)
		}
		// A payments key cannot reach refunds, subscriptions or invoices; the
		// refund scope does not read payments.
		if r := f.do("POST", "/v1/payments/"+id+"/refunds", writer, map[string]any{"amount": 100}); r.status != 403 {
			t.Errorf("payments:write created a refund: %d", r.status)
		}
		if r := f.do("GET", "/v1/payments/"+id, refunds, nil); r.status != 403 {
			t.Errorf("refunds:write read a payment: %d", r.status)
		}
		if r := f.do("POST", "/v1/subscriptions", writer, map[string]any{}); r.status != 403 {
			t.Errorf("payments:write reached subscriptions: %d", r.status)
		}
		if r := f.do("GET", "/v1/subscriptions", readOnly, nil); r.status != 403 {
			t.Errorf("payments:read listed subscriptions: %d", r.status)
		}
		for _, path := range []string{"/v1/invoices", "/v1/credit_notes"} {
			if r := f.do("GET", path, readOnly, nil); r.status != 403 {
				t.Errorf("payments:read reached %s: %d", path, r.status)
			}
			if r := f.do("GET", path, invoices, nil); r.status == 403 || r.status == 401 {
				t.Errorf("invoices:read refused at %s: %d", path, r.status)
			}
		}
		if r := f.do("GET", "/v1/refunds/ref_x", refunds, nil); r.status != 404 { // allowed (refunds:write covers read), nothing there
			t.Errorf("refunds:write reading a refund: %d", r.status)
		}

		// Existing and unrestricted keys have every scope.
		for _, path := range []string{"/v1/payments", "/v1/subscriptions", "/v1/invoices"} {
			if r := f.do("GET", path, f.testKey, nil); r.status == 403 {
				t.Errorf("full-access key refused at %s", path)
			}
		}
		// Scope is checked after authentication: a bad key is still 401.
		if r := f.do("POST", "/v1/payments", "pmx_test_sk_"+strings.Repeat("a", 43), body); r.status != 401 {
			t.Errorf("bad key: %d", r.status)
		}
		// An unknown route stays 404 for a scoped key.
		if r := f.do("GET", "/v1/nonesuch", readOnly, nil); r.status != 404 {
			t.Errorf("unknown route: %d", r.status)
		}
	})
}

func TestExpiredKeysAreRejectedByTheAPI(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		scope := core.OrgScope{OrgID: store.DefaultOrgID}
		oldTok, _, _ := f.svc.CreateAPIKey(ctx, audit.System, scope, f.app.ID, "live")
		newTok, _, _, err := f.svc.RotateAPIKey(ctx, audit.System, scope, f.app.ID, "live", time.Hour, core.KeyOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for name, tok := range map[string]string{"old (in its grace period)": oldTok, "new": newTok} {
			if r := f.do("GET", "/v1/payments", tok, nil); r.status != 200 {
				t.Errorf("%s: %d", name, r.status)
			}
		}
		if err := f.db.Write(ctx, func(tx store.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE api_keys SET expires_at = ? WHERE prefix = ?`, time.Now().Add(-time.Minute).UnixMilli(), oldTok[:len("pmx_live_sk_")+8])
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if r := f.do("GET", "/v1/payments", oldTok, nil); r.status != 401 {
			t.Errorf("old key after expiry: %d", r.status)
		}
		if r := f.do("GET", "/v1/payments", newTok, nil); r.status != 200 {
			t.Errorf("new key: %d", r.status)
		}
	})
}

// docs/openapi.yaml names the scope of every operation (x-required-scope); it
// must be the scope the router enforces.
func TestOpenAPIScopesMatchTheRouter(t *testing.T) {
	raw, err := os.ReadFile("../../docs/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	pathRE := regexp.MustCompile(`^  (/v1/\S+):$`)
	methodRE := regexp.MustCompile(`^    (get|post):$`)
	scopeRE := regexp.MustCompile(`^      x-required-scope: (\S+)$`)
	documented := map[string]string{}
	path, method := "", ""
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case pathRE.MatchString(line):
			path = pathRE.FindStringSubmatch(line)[1]
		case methodRE.MatchString(line):
			method = strings.ToUpper(methodRE.FindStringSubmatch(line)[1])
		case scopeRE.MatchString(line):
			documented[method+" "+path] = scopeRE.FindStringSubmatch(line)[1]
		}
	}
	if len(documented) != len(httpapi.RouteScopes) {
		t.Errorf("documented %d scopes, router declares %d", len(documented), len(httpapi.RouteScopes))
	}
	for route, scope := range httpapi.RouteScopes {
		if documented[route] != scope {
			t.Errorf("%s: openapi says %q, router enforces %q", route, documented[route], scope)
		}
		if !authz.ValidScope(scope) {
			t.Errorf("%s: %q is not a scope", route, scope)
		}
	}
}
