package dashboard

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/paylinks"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

func (e *env) seedLink(app store.App, mode string, mut func(*paylinks.CreateInput)) store.PaymentLink {
	e.t.Helper()
	in := paylinks.CreateInput{Scope: core.Scope{AppID: app.ID, Mode: mode}, Amount: 49900, Currency: "INR", Description: "Course fee"}
	if mut != nil {
		mut(&in)
	}
	l, err := e.links.Create(ctx, audit.System, in)
	if err != nil {
		e.t.Fatal(err)
	}
	return l
}

func TestPaymentLinksPageListsTheSessionModesLinksOfThisOrg(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		alpha, beta := e.seedApp("alpha"), e.seedApp("beta")
		e.otherOrg()
		fixed := e.seedLink(alpha, "test", nil)
		choose := e.seedLink(beta, "test", func(in *paylinks.CreateInput) {
			in.Amount, in.MinAmount, in.MaxAmount, in.Description = 0, 10000, 500000, "Donation"
		})
		single := e.seedLink(alpha, "test", func(in *paylinks.CreateInput) { in.SingleUse = true; in.Description = "One seat" })
		off := e.seedLink(alpha, "test", func(in *paylinks.CreateInput) { in.Description = "Old offer" })
		timed := e.seedLink(alpha, "test", func(in *paylinks.CreateInput) { in.Description = "Timed"; in.ExpiresAt = e.clk.Now().Add(time.Hour) })
		live := e.seedLink(alpha, "live", func(in *paylinks.CreateInput) { in.Description = "Live only" })
		if _, err := e.links.DeactivateInOrg(ctx, audit.System, store.DefaultOrgID, off.ID); err != nil {
			t.Fatal(err)
		}
		// A single-use link that a payment used up.
		used := e.seedLink(alpha, "test", func(in *paylinks.CreateInput) { in.SingleUse = true; in.Description = "Spent" })
		_ = e.db.Write(ctx, func(tx store.Tx) error {
			_, err := store.DeactivatePaymentLink(ctx, tx, used.ID, "used", e.clk.Now())
			return err
		})
		e.clk.Add(2 * time.Hour) // timed has now expired

		c := e.roleClient("viewer")
		body := e.pageOK(c, "/dashboard/payment-links")
		has(t, body, e.links.URL(fixed), e.links.URL(choose), e.links.URL(single), "Course fee", "Donation", "INR 499.00", "INR 100.00", "INR 5000.00",
			"Alpha", "Beta", "single-use", "reusable", "Payment links", `data-copy="link-url-`+fixed.ID+`"`)
		lacks(t, body, e.links.URL(live), "Live only", "foreign", `action="/dashboard/payment-links`)
		row := func(id string) string { return regexpFind(t, `(?s)(<tr id="link-`+id+`".*?</tr>)`, body) }
		has(t, row(fixed.ID), "active")
		has(t, row(off.ID), "inactive")
		has(t, row(timed.ID), "expired")
		has(t, row(used.ID), "used")

		// Filters.
		onlyBeta := e.pageOK(c, "/dashboard/payment-links?app="+beta.ID)
		has(t, onlyBeta, "Donation")
		lacks(t, onlyBeta, "Course fee")
		activeOnly := e.pageOK(c, "/dashboard/payment-links?state=active")
		has(t, activeOnly, "Course fee", "Donation")
		lacks(t, activeOnly, "Old offer", "Spent", "Timed")
		inactive := e.pageOK(c, "/dashboard/payment-links?state=inactive")
		has(t, inactive, "Old offer", "Spent", "Timed")
		lacks(t, inactive, "Course fee")
		for _, q := range []string{"?state=bogus", "?app=app_nope"} {
			if r, _ := e.get(c, "/dashboard/payment-links"+q); r.StatusCode != 400 {
				t.Errorf("%s: %d", q, r.StatusCode)
			}
		}
		// The mode toggle switches to the live links.
		e.setMode(c, "live")
		lbody := e.pageOK(c, "/dashboard/payment-links")
		has(t, lbody, "Live only")
		lacks(t, lbody, "Course fee")
	})
}

func TestOperatorCreatesPaymentLinks(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		alpha := e.seedApp("alpha")
		e.otherOrg()
		op, viewer := e.roleClient("operator"), e.roleClient("viewer")
		body := e.pageOK(op, "/dashboard/payment-links")
		has(t, body, `action="/dashboard/payment-links"`, `name="app"`, `name="currency"`, `name="amount"`, `name="min_amount"`, `name="max_amount"`,
			`name="description"`, `name="single_use"`, `name="expires_at"`, `name="return_url"`, `name="metadata"`)
		lacks(t, body, "app_foreign", "Foreign App")
		tok := csrfOf(t, body)

		form := url.Values{"app": {alpha.ID}, "currency": {"inr"}, "amount": {"1,499.50"}, "description": {"Annual plan"}, "single_use": {"on"},
			"expires_at": {"2030-06-01T10:30"}, "return_url": {"https://shop.example.com/thanks"}, "metadata": {"sku=annual\nteam = growth \n\n"}, "csrf": {tok}}
		r, out := e.post(op, "/dashboard/payment-links", form)
		if r.StatusCode != 303 || r.Header.Get("Location") != "/dashboard/payment-links" {
			t.Fatalf("create: %d %s", r.StatusCode, out)
		}
		var ls []store.PaymentLink
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			ls, err = store.ListPaymentLinks(ctx, q, store.PaymentLinkFilter{OrgID: store.DefaultOrgID, Limit: 10})
			return
		})
		if len(ls) != 1 {
			t.Fatalf("links: %+v", ls)
		}
		l := ls[0]
		wantExp := time.Date(2030, 6, 1, 10, 30, 0, 0, time.UTC) // the test dashboard runs in UTC
		if l.Amount != 149950 || l.Currency != "INR" || l.Mode != "test" || l.AppID != alpha.ID || !l.SingleUse || !l.ExpiresAt.Equal(wantExp) ||
			l.Description != "Annual plan" || l.ReturnURL != "https://shop.example.com/thanks" || l.Metadata["sku"] != "annual" || l.Metadata["team"] != "growth" || len(l.Metadata) != 2 {
			t.Fatalf("link %+v", l)
		}
		var aud []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, "payment_link.create", 10)
			return
		})
		if len(aud) != 1 || aud[0].ActorID != "usr_operator" {
			t.Errorf("audit %+v", aud)
		}
		// The copy-able URL is on the page afterwards.
		has(t, e.pageOK(op, "/dashboard/payment-links"), e.links.URL(l))

		// A customer-chooses link in the live mode.
		e.setMode(op, "live")
		tok = csrfOf(t, e.pageOK(op, "/dashboard/payment-links"))
		r, out = e.post(op, "/dashboard/payment-links", url.Values{"app": {alpha.ID}, "currency": {"INR"}, "min_amount": {"100"}, "max_amount": {"5,000"}, "csrf": {tok}})
		if r.StatusCode != 303 {
			t.Fatalf("chooser: %d %s", r.StatusCode, out)
		}
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			ls, err = store.ListPaymentLinks(ctx, q, store.PaymentLinkFilter{OrgID: store.DefaultOrgID, Mode: "live", Limit: 10})
			return
		})
		if len(ls) != 1 || ls[0].Amount != 0 || ls[0].MinAmount != 10000 || ls[0].MaxAmount != 500000 {
			t.Errorf("chooser link %+v", ls)
		}

		// Viewers cannot create, and the CSRF token is required.
		vtok := csrfOf(t, e.pageOK(viewer, "/dashboard/payment-links"))
		if r, _ := e.post(viewer, "/dashboard/payment-links", url.Values{"app": {alpha.ID}, "currency": {"INR"}, "amount": {"100"}, "csrf": {vtok}}); r.StatusCode != 403 {
			t.Errorf("viewer: %d", r.StatusCode)
		}
		if r, _ := e.post(op, "/dashboard/payment-links", url.Values{"app": {alpha.ID}, "currency": {"INR"}, "amount": {"100"}}); r.StatusCode != 403 {
			t.Errorf("no csrf: %d", r.StatusCode)
		}
	})
}

func TestCreatePaymentLinkRejectsBadInputAndKeepsTheForm(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		alpha := e.seedApp("alpha")
		e.otherOrg()
		op := e.roleClient("operator")
		tok := csrfOf(t, e.pageOK(op, "/dashboard/payment-links"))
		base := func() url.Values {
			return url.Values{"app": {alpha.ID}, "currency": {"INR"}, "amount": {"499"}, "description": {"Keep me"}, "csrf": {tok}}
		}
		cases := map[string]struct {
			mod    func(url.Values)
			status int
		}{
			"no amount":           {func(f url.Values) { f.Del("amount") }, 400},
			"amount and min":      {func(f url.Values) { f.Set("min_amount", "100") }, 400},
			"amount not a number": {func(f url.Values) { f.Set("amount", "lots") }, 400},
			"too many decimals":   {func(f url.Values) { f.Set("amount", "4.999") }, 400},
			"bad currency":        {func(f url.Values) { f.Set("currency", "ZZZ") }, 400},
			"bad expiry":          {func(f url.Values) { f.Set("expires_at", "soon") }, 400},
			"past expiry":         {func(f url.Values) { f.Set("expires_at", "2001-01-01T00:00") }, 400},
			"bad return url":      {func(f url.Values) { f.Set("return_url", "javascript:alert(1)") }, 400},
			"metadata no equals":  {func(f url.Values) { f.Set("metadata", "justakey") }, 400},
			"reserved metadata":   {func(f url.Values) { f.Set("metadata", "payment_link_id=x") }, 400},
			"unknown app":         {func(f url.Values) { f.Set("app", "app_nope") }, 404},
			"another orgs app":    {func(f url.Values) { f.Set("app", "app_foreign") }, 404},
		}
		for name, tc := range cases {
			f := base()
			tc.mod(f)
			r, out := e.post(op, "/dashboard/payment-links", f)
			if r.StatusCode != tc.status {
				t.Errorf("%s: %d, want %d", name, r.StatusCode, tc.status)
				continue
			}
			if tc.status == 400 && (!strings.Contains(out, `class="error"`) || !strings.Contains(out, "Keep me")) {
				t.Errorf("%s: the form should explain the problem and keep what was typed", name)
			}
		}
		var n int
		_ = db.Read(ctx, func(q store.Querier) error {
			return q.QueryRowContext(ctx, `SELECT COUNT(*) FROM payment_links`).Scan(&n)
		})
		if n != 0 {
			t.Errorf("%d links created from bad input", n)
		}
	})
}

func TestOperatorDeactivatesAPaymentLink(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		alpha := e.seedApp("alpha")
		e.otherOrg()
		l := e.seedLink(alpha, "test", nil)
		foreign := store.PaymentLink{ID: "plink_foreign", OrgID: "org_other", Mode: "test", AppID: "app_foreign", Slug: "foreignslug1", Amount: 100, Currency: "INR",
			Active: true, Version: 1, CreatedAt: e.clk.Now(), UpdatedAt: e.clk.Now()}
		_ = db.Write(ctx, func(tx store.Tx) error { return store.InsertPaymentLink(ctx, tx, foreign) })

		op, viewer := e.roleClient("operator"), e.roleClient("viewer")
		body := e.pageOK(op, "/dashboard/payment-links")
		has(t, body, `action="/dashboard/payment-links/`+l.ID+`/deactivate"`)
		tok := csrfOf(t, body)
		vtok := csrfOf(t, e.pageOK(viewer, "/dashboard/payment-links"))
		if r, _ := e.post(viewer, "/dashboard/payment-links/"+l.ID+"/deactivate", url.Values{"csrf": {vtok}}); r.StatusCode != 403 {
			t.Errorf("viewer: %d", r.StatusCode)
		}
		if r, _ := e.post(op, "/dashboard/payment-links/"+l.ID+"/deactivate", url.Values{}); r.StatusCode != 403 {
			t.Errorf("no csrf: %d", r.StatusCode)
		}
		if r, _ := e.post(op, "/dashboard/payment-links/"+foreign.ID+"/deactivate", url.Values{"csrf": {tok}}); r.StatusCode != 404 {
			t.Errorf("another org's link: %d", r.StatusCode)
		}
		var f store.PaymentLink
		_ = db.Read(ctx, func(q store.Querier) (err error) { f, err = store.GetPaymentLink(ctx, q, foreign.ID); return })
		if !f.Active {
			t.Error("another org's link was switched off")
		}
		if r, _ := e.post(op, "/dashboard/payment-links/plink_nope/deactivate", url.Values{"csrf": {tok}}); r.StatusCode != 404 {
			t.Errorf("unknown link: %d", r.StatusCode)
		}
		r, _ := e.post(op, "/dashboard/payment-links/"+l.ID+"/deactivate", url.Values{"csrf": {tok}})
		if r.StatusCode != 303 {
			t.Fatalf("deactivate: %d", r.StatusCode)
		}
		var got store.PaymentLink
		_ = db.Read(ctx, func(q store.Querier) (err error) { got, err = store.GetPaymentLink(ctx, q, l.ID); return })
		if got.Active || got.DeactivatedReason != "manual" {
			t.Errorf("link %+v", got)
		}
		var aud []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, "payment_link.deactivate", 10)
			return
		})
		if len(aud) != 1 || aud[0].ActorID != "usr_operator" {
			t.Errorf("audit %+v", aud)
		}
		// Repeating is harmless.
		if r, _ := e.post(op, "/dashboard/payment-links/"+l.ID+"/deactivate", url.Values{"csrf": {tok}}); r.StatusCode != 303 {
			t.Errorf("second deactivate: %d", r.StatusCode)
		}
		lacks(t, e.pageOK(op, "/dashboard/payment-links"), `action="/dashboard/payment-links/`+l.ID+`/deactivate"`)
	})
}
