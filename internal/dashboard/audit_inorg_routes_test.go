package dashboard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/velixio/paymux/internal/authz"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

// Audit H-12. The dashboard serves one org today, but a handler that loads an
// object by the id in its URL must check d.inOrg (directly, or through a
// helper such as loadApp) or build the org scope (d.orgScope) that the service
// it calls enforces, or the day a second org exists it serves that org's
// objects to this one. This test reads the route table from the source: every
// route whose pattern has a wildcard must reach inOrg through the calls its
// handler makes, unless it is listed here with the reason it needs no check.
var inOrgExempt = map[string]string{
	"/users/{id}":                           "users are not org objects: the dashboard has one user table",
	"/users/{id}/role":                      "users are not org objects",
	"/users/{id}/enrolment":                 "users are not org objects",
	"/users/{id}/sso-link":                  "users are not org objects",
	"/users/{id}/disable":                   "users are not org objects",
	"/users/{id}/sessions/revoke":           "users are not org objects",
	"/roles/{name}":                         "roles are global, like users",
	"/roles/{name}/delete":                  "roles are global, like users",
	"/{section}":                            "static sections, no object is loaded",
	"/{$}":                                  "redirects to the overview",
	"/appearance/themes/{name}/export.json": "themes are files of the installation, not org objects",
	"/appearance/preview/{name}":            "a bundled theme preview, no object is loaded",
	"/appearance/themes/{name}/edit":        "themes are files of the installation, not org objects",
	"/appearance/themes/{name}":             "themes are files of the installation, not org objects",
	"/appearance/themes/{name}/rename":      "themes are files of the installation, not org objects",
	"/appearance/themes/{name}/delete":      "themes are files of the installation, not org objects",
	"/account/sessions/{id}/revoke":         "scoped to the signed-in user's own sessions",
	"/settings/{key}":                       "the key names a setting of the dashboard's own org",
	"/alerts/{id}/acknowledge":              "Alerts.Acknowledge looks the alert up inside the org it is given",
	"/payment-links/{id}/deactivate":        "PaymentLinks.DeactivateInOrg refuses a link of another org",
	"/logs/{id}":                            "logPage compares the row's org with the dashboard's own",
}

type fnInfo struct {
	calls map[string]bool
}

// routeTable parses the package and returns pattern -> handler method name,
// plus the call graph by function name.
func routeTable(t *testing.T) (map[string]string, map[string]*fnInfo) {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	routes := map[string]string{}
	graph := map[string]*fnInfo{}
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, n, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			info := graph[fd.Name.Name]
			if info == nil {
				info = &fnInfo{calls: map[string]bool{}}
				graph[fd.Name.Name] = info
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					switch fun := x.Fun.(type) {
					case *ast.Ident:
						info.calls[fun.Name] = true
					case *ast.SelectorExpr:
						info.calls[fun.Sel.Name] = true
						if fun.Sel.Name == "HandleFunc" && len(x.Args) == 2 {
							if pat, h := registration(x.Args[0], x.Args[1]); pat != "" {
								routes[pat] = h
							}
						}
					}
				}
				return true
			})
		}
	}
	return routes, graph
}

// registration returns the path of the pattern (without the method and the
// "/dashboard" prefix) and the handler method named in the second argument.
func registration(pat, handler ast.Expr) (string, string) {
	var path strings.Builder
	ast.Inspect(pat, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			s, _ := strconv.Unquote(lit.Value)
			path.WriteString(s)
		}
		return true
	})
	p := path.String()
	if _, after, ok := strings.Cut(p, " "); ok {
		p = after
	}
	var h string
	ast.Inspect(handler, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "d" && sel.Sel.Name != "authed" && sel.Sel.Name != "recent" {
				h = sel.Sel.Name
			}
		}
		return true
	})
	return p, h
}

// reaches reports whether from calls target through the call graph.
func reaches(graph map[string]*fnInfo, from, target string) bool {
	seen := map[string]bool{}
	var walk func(string) bool
	walk = func(fn string) bool {
		if seen[fn] {
			return false
		}
		seen[fn] = true
		info := graph[fn]
		if info == nil {
			return false
		}
		for c := range info.calls {
			if c == target || walk(c) {
				return true
			}
		}
		return false
	}
	return walk(from)
}

func TestEveryRouteWithAnIDChecksTheOrg(t *testing.T) {
	routes, graph := routeTable(t)
	var missing []string
	checked := 0
	for pat, h := range routes {
		if !strings.Contains(pat, "{") {
			continue
		}
		if _, ok := inOrgExempt[strings.TrimPrefix(pat, "/dashboard")]; ok {
			continue
		}
		checked++
		if h == "" {
			missing = append(missing, pat+" (handler not found)")
		} else if !reaches(graph, h, "inOrg") && !reaches(graph, h, "orgScope") {
			missing = append(missing, pat+" -> "+h)
		}
	}
	sort.Strings(missing)
	if checked < 20 {
		t.Fatalf("only %d routes with a wildcard found: the route table reader is broken", checked)
	}
	for _, m := range missing {
		t.Errorf("no d.inOrg check reachable from %s", m)
	}
}

// The subscription pages loaded the subscription by id without asking whose it
// is: with a second org, its subscription page, cancel and checkout link
// answered to this org's users. A foreign object is a 404, as an unknown one.
func TestSubscriptionOfAnotherOrgIs404(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.enableSubscriptions()
		e.otherOrg()
		err := db.Write(ctx, func(tx store.Tx) error {
			_, err := store.UpsertCustomer(ctx, tx, store.Customer{ID: "cus_foreign", OrgID: "org_other", AppID: "app_foreign", Mode: "live", Name: "F", Email: "f@example.com"}, e.clk.Now())
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		e.seedSub("sub_foreign", "app_foreign", "cus_foreign", "live", "active", func(s *store.Subscription) { s.OrgID = "org_other" })
		admin := e.roleClient("admin")
		tok := csrfOf(t, e.pageOK(admin, "/dashboard/subscriptions"))
		if r, _ := e.get(admin, "/dashboard/subscriptions/sub_foreign"); r.StatusCode != 404 {
			t.Errorf("subscription page: %d, want 404", r.StatusCode)
		}
		for _, p := range []string{"/dashboard/subscriptions/sub_foreign/cancel", "/dashboard/subscriptions/sub_foreign/checkout-link", "/dashboard/subscriptions/sub_foreign/amount"} {
			if r, _ := e.post(admin, p, url.Values{"csrf": {tok}, "when": {"now"}, "amount": {"100"}}); r.StatusCode != 404 {
				t.Errorf("POST %s: %d, want 404", p, r.StatusCode)
			}
		}
	})
}

// The actor of a session carries the org, so authz refuses what belongs to
// another one.
func TestSessionActorCarriesTheOrg(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		c := e.roleClient("admin")
		u, _ := url.Parse(e.srv.URL + "/dashboard/overview")
		req := httptest.NewRequest("GET", "/dashboard/overview", nil)
		for _, ck := range c.Jar.Cookies(u) {
			req.AddCookie(ck)
		}
		s, ok := e.d.currentSession(req)
		if !ok {
			t.Fatal("no session")
		}
		if got := s.actor().OrgID; got != store.DefaultOrgID {
			t.Fatalf("actor org = %q, want %q", got, store.DefaultOrgID)
		}
		if authz.CanWith(s.roles, s.actor(), authz.PaymentRead, authz.Resource{OrgID: "org_other"}) || !authz.CanWith(s.roles, s.actor(), authz.PaymentRead, authz.Resource{OrgID: store.DefaultOrgID}) {
			t.Error("authz does not tell the orgs apart for this session")
		}
	})
}
