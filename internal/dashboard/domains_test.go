package dashboard

import (
	"net/url"
	"strings"
	"testing"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

func TestDomainsTab(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app := e.seedApp("alpha")
		e.otherOrg()
		page := "/dashboard/apps/" + app.ID

		viewer := e.roleClient("viewer")
		vb := e.pageOK(viewer, page+"?tab=domains")
		lacks(t, vb, `action="`+page+`/domains"`)
		has(t, vb, "No custom domains")
		if r, _ := e.post(viewer, page+"/domains", url.Values{"host": {"pay.alpha.example"}, "csrf": {csrfOf(t, vb)}}); r.StatusCode != 403 {
			t.Fatalf("viewer: %d", r.StatusCode)
		}
		op := e.roleClient("operator")
		if r, _ := e.post(op, page+"/domains", url.Values{"host": {"pay.alpha.example"}, "csrf": {csrfOf(t, e.pageOK(op, page+"?tab=domains"))}}); r.StatusCode != 403 {
			t.Fatalf("operator: %d", r.StatusCode)
		}

		admin := e.roleClient("admin")
		body := e.pageOK(admin, page+"?tab=domains")
		has(t, body, `action="`+page+`/domains"`)
		tok := csrfOf(t, body)
		if r, b := e.post(admin, page+"/domains", url.Values{"host": {"Pay.Alpha.Example"}, "csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("add: %d\n%s", r.StatusCode, b)
		}
		e.post(admin, page+"/domains", url.Values{"host": {"checkout.alpha.example"}, "csrf": {tok}})
		var doms []store.Domain
		_ = db.Read(ctx, func(q store.Querier) (err error) { doms, err = store.ListDomains(ctx, q, app.ID); return })
		if len(doms) != 2 || doms[0].Host != "pay.alpha.example" || !doms[0].IsPrimary || doms[1].IsPrimary || doms[0].Status != "pending" || doms[0].Token == "" {
			t.Fatalf("domains %+v", doms)
		}

		body = e.pageOK(admin, page+"?tab=domains")
		has(t, body, "pay.alpha.example", "checkout.alpha.example", "pending", "primary",
			"https://pay.alpha.example/.well-known/paymux/"+doms[0].Token, "https://checkout.alpha.example/.well-known/paymux/"+doms[1].Token,
			"_paymux-challenge.pay.alpha.example", "paymux-verify="+doms[0].Token,
			"_paymux-challenge.checkout.alpha.example", "paymux-verify="+doms[1].Token)
		// the whitelist reminder, per provider and per domain
		for _, provider := range []string{"Razorpay", "PayU", "Zoho", "Whop"} {
			has(t, body, provider)
		}
		checklist := regexpFind(t, `(?s)(<ul class="checklist".*?</ul>)`, body)
		has(t, checklist, "pay.alpha.example", "Razorpay", "PayU")

		for name, c := range map[string]struct {
			host string
			code int
		}{
			"duplicate":     {"pay.alpha.example", 409},
			"not a host":    {"not a host!", 400},
			"with a scheme": {"https://pay.alpha.example", 400},
			"empty":         {" ", 400},
		} {
			if r, _ := e.post(admin, page+"/domains", url.Values{"host": {c.host}, "csrf": {tok}}); r.StatusCode != c.code {
				t.Errorf("%s: %d want %d", name, r.StatusCode, c.code)
			}
		}
		if r, _ := e.post(admin, "/dashboard/apps/app_foreign/domains", url.Values{"host": {"x.example"}, "csrf": {tok}}); r.StatusCode != 404 {
			t.Errorf("another org's app: %d", r.StatusCode)
		}
		var aud []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, "domain.add", 10)
			return
		})
		if len(aud) != 2 || aud[0].ActorType != "user" || aud[0].ActorID != "usr_admin" {
			t.Errorf("audit %+v", aud)
		}
		if n, _ := db.CountUsers(ctx); n == 0 || strings.Contains(body, "app_foreign") {
			t.Error("leak")
		}
	})
}
