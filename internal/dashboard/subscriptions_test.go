package dashboard

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/internal/subscriptions"
	"github.com/paymux/paymux/pkg/connector"
)

func (e *env) enableSubscriptions() *subscriptions.Service {
	e.t.Helper()
	subs := subscriptions.New(subscriptions.Options{DB: e.db, Core: e.core, Settings: e.set, Jobs: e.q, BaseURL: "https://pay.example.com", Now: e.clk.Now,
		Connectors: subscriptions.ConnectorsFunc(func(context.Context, string) (connector.Connector, error) {
			return nil, errors.New("no connectors in this test")
		})})
	e.d.o.Subscriptions = subs
	return subs
}

func (e *env) seedCustomer(id, appID, mode, email string) {
	e.t.Helper()
	err := e.db.Write(ctx, func(tx store.Tx) error {
		_, err := store.UpsertCustomer(ctx, tx, store.Customer{ID: id, OrgID: store.DefaultOrgID, AppID: appID, Mode: mode, Name: "Cust " + id, Email: email}, e.clk.Now())
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) seedMandate(id, appID, customerID, mode, status string) store.Mandate {
	e.t.Helper()
	now := e.clk.Now()
	m := store.Mandate{ID: id, OrgID: store.DefaultOrgID, Mode: mode, AppID: appID, CustomerID: customerID, ProviderAccountID: "pa_x",
		ProviderRef: "REF" + id, ProviderMandateID: "pm_" + id, Method: "upi", Status: status, MaxAmount: 74850, Currency: "INR",
		Frequency: "month", ValidUntil: now.AddDate(5, 0, 0), CreatedAt: now, UpdatedAt: now}
	if err := e.db.Write(ctx, func(tx store.Tx) error { return store.InsertMandate(ctx, tx, m) }); err != nil {
		e.t.Fatal(err)
	}
	return m
}

func (e *env) seedSub(id, appID, customerID, mode, status string, mut func(*store.Subscription)) store.Subscription {
	e.t.Helper()
	now := e.clk.Now()
	s := store.Subscription{ID: id, OrgID: store.DefaultOrgID, Mode: mode, AppID: appID, CustomerID: customerID, Status: status,
		BillingMode: "merchant_triggered", ProviderAccountID: "pa_x", ProviderRef: "SUB" + id, Amount: 49900, Currency: "INR", Interval: "month",
		IntervalCount: 1, AnchorAt: now, CurrentPeriodStart: now, CurrentPeriodEnd: now.AddDate(0, 1, 0), Description: "Pro", Metadata: map[string]string{},
		CheckoutTokenHash: "oldhash", ExpiresAt: now.Add(24 * time.Hour), Version: 1, CreatedAt: now, UpdatedAt: now}
	if mut != nil {
		mut(&s)
	}
	if err := e.db.Write(ctx, func(tx store.Tx) error { return store.InsertSubscription(ctx, tx, s) }); err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *env) seedCycle(subID string, period int, state, paymentID string, retries int) {
	e.t.Helper()
	now := e.clk.Now()
	c := store.Cycle{ID: "cyc_" + subID + string(rune('a'+period)), SubscriptionID: subID, PeriodIndex: period, PaymentID: paymentID, Amount: 49900,
		State: state, Retries: retries, NotifiedAt: now, NextRunAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}
	if err := e.db.Write(ctx, func(tx store.Tx) error { return store.InsertCycle(ctx, tx, c) }); err != nil {
		e.t.Fatal(err)
	}
}

func TestSubscriptionListFiltersModeAndPaging(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.enableSubscriptions()
		a, b := e.seedApp("alpha"), e.seedApp("beta")
		e.seedCustomer("cus_1", a.ID, "test", "ann@example.com")
		e.seedCustomer("cus_2", b.ID, "test", "bob@example.com")
		e.seedCustomer("cus_3", a.ID, "live", "liv@example.com")
		e.seedSub("sub_01", a.ID, "cus_1", "test", "active", nil)
		e.seedSub("sub_02", a.ID, "cus_1", "test", "past_due", func(s *store.Subscription) { s.DunningAttempt = 2 })
		e.seedSub("sub_03", a.ID, "cus_1", "test", "active", func(s *store.Subscription) { s.RequiresReauthorization = true; s.CancelAtPeriodEnd = true })
		e.seedSub("sub_04", b.ID, "cus_2", "test", "canceled", nil)
		e.seedSub("sub_05", a.ID, "cus_3", "live", "active", nil)
		c := e.roleClient("viewer")

		body := e.pageOK(c, "/dashboard/subscriptions") // live by default
		has(t, body, `href="/dashboard/subscriptions/sub_05"`, "liv@example.com")
		lacks(t, body, `href="/dashboard/subscriptions/sub_01"`)

		e.setMode(c, "test")
		body = e.pageOK(c, "/dashboard/subscriptions")
		has(t, body, `href="/dashboard/subscriptions/sub_04"`, `href="/dashboard/subscriptions/sub_03"`, "needs re-authorization", "cancels at period end", "INR / month")
		lacks(t, body, `href="/dashboard/subscriptions/sub_02"`, `href="/dashboard/subscriptions/sub_05"`)
		next := strings.ReplaceAll(regexpFind(t, `href="([^"]*after=sub_03[^"]*)"`, body), "&amp;", "&")
		body = e.pageOK(c, next)
		has(t, body, `href="/dashboard/subscriptions/sub_02"`, `href="/dashboard/subscriptions/sub_01"`, "retry 2")
		lacks(t, body, `href="/dashboard/subscriptions/sub_03"`, `class="next"`)

		for name, tc := range map[string]struct {
			query string
			want  []string
			not   []string
		}{
			"app":    {"?app=" + b.ID, []string{`sub_04`}, []string{`sub_01`, `sub_03`}},
			"status": {"?status=past_due", []string{`sub_02`}, []string{`sub_01`, `sub_04`}},
			"email":  {"?q=BOB@example", []string{`sub_04`}, []string{`sub_01`}},
			"id":     {"?q=sub_03", []string{`sub_03`}, []string{`sub_01`, `sub_02`}},
		} {
			body := e.pageOK(c, "/dashboard/subscriptions"+tc.query)
			for _, w := range tc.want {
				if !strings.Contains(body, `href="/dashboard/subscriptions/`+w+`"`) {
					t.Errorf("%s: missing %s", name, w)
				}
			}
			for _, w := range tc.not {
				if strings.Contains(body, `href="/dashboard/subscriptions/`+w+`"`) {
					t.Errorf("%s: unexpected %s", name, w)
				}
			}
		}
		if r, _ := e.get(c, "/dashboard/subscriptions?status=bogus"); r.StatusCode != 400 {
			t.Errorf("bad status: %d", r.StatusCode)
		}
		// Anonymous users are sent to the login page.
		if r, _ := e.get(e.client(), "/dashboard/subscriptions"); r.StatusCode != 303 {
			t.Errorf("anonymous: %d", r.StatusCode)
		}
	})
}

func TestSubscriptionDetailShowsPeriodsMandateAndDunning(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.enableSubscriptions()
		a := e.seedApp("alpha")
		e.seedCustomer("cus_1", a.ID, "test", "ann@example.com")
		m := e.seedMandate("mdt_1", a.ID, "cus_1", "test", "active")
		pend := e.seedMandate("mdt_2", a.ID, "cus_1", "test", "pending")
		e.seedPayment("pay_r0", a.ID, "test", "succeeded", "", 49900, e.clk.Now())
		e.seedPayment("pay_r1", a.ID, "test", "failed", "", 49900, e.clk.Now())
		e.seedSub("sub_01", a.ID, "cus_1", "test", "past_due", func(s *store.Subscription) {
			s.MandateID, s.PendingMandateID = m.ID, pend.ID
			s.RequiresReauthorization, s.DunningAttempt, s.NextRetryAt = true, 1, e.clk.Now().Add(24*time.Hour)
			s.PendingAmount, s.TotalCount, s.RenewalsDone = 60000, 12, 1
		})
		e.seedCycle("sub_01", 0, "succeeded", "pay_r0", 0)
		e.seedCycle("sub_01", 1, "scheduled", "pay_r1", 1)

		c := e.roleClient("viewer")
		e.setMode(c, "test")
		body := e.pageOK(c, "/dashboard/subscriptions/sub_01")
		has(t, body, "sub_01", "past_due", "ann@example.com", "499.00 INR / month", "changes to 600.00 INR next period",
			"1 of 12", "retry 1, next at", "required", "mdt_1", "pending replacement", "mdt_2", "REFmdt_1", "pm_mdt_1", "748.50 INR",
			`href="/dashboard/payments/pay_r0"`, `href="/dashboard/payments/pay_r1"`, "succeeded", "failed")
		// A viewer sees no actions.
		lacks(t, body, "Cancel subscription", "Create checkout link")
		if has2 := strings.Contains(body, `action="/dashboard/subscriptions/sub_01/cancel"`); has2 {
			t.Error("viewer sees the cancel form")
		}
		if r, _ := e.get(c, "/dashboard/subscriptions/sub_nosuch"); r.StatusCode != 404 {
			t.Errorf("unknown: %d", r.StatusCode)
		}

		op := e.roleClient("operator")
		e.setMode(op, "test")
		body = e.pageOK(op, "/dashboard/subscriptions/sub_01")
		has(t, body, `action="/dashboard/subscriptions/sub_01/cancel"`, `action="/dashboard/subscriptions/sub_01/checkout-link"`)
	})
}

func (e *env) auditActions() []string {
	e.t.Helper()
	var out []string
	err := e.db.Read(ctx, func(q store.Querier) error {
		rows, err := q.QueryContext(ctx, "SELECT action FROM audit_log WHERE action LIKE 'subscription.%' ORDER BY at, id")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a string
			if err := rows.Scan(&a); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *env) subOf(id string) store.Subscription {
	e.t.Helper()
	var s store.Subscription
	if err := e.db.Read(ctx, func(q store.Querier) (err error) { s, err = store.GetSubscription(ctx, q, id); return }); err != nil {
		e.t.Fatal(err)
	}
	return s
}

func TestSubscriptionCancelNeedsAnOperatorAndIsAudited(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.enableSubscriptions()
		a := e.seedApp("alpha")
		e.seedCustomer("cus_1", a.ID, "test", "ann@example.com")
		m := e.seedMandate("mdt_1", a.ID, "cus_1", "test", "active")
		for _, id := range []string{"sub_01", "sub_02"} {
			e.seedSub(id, a.ID, "cus_1", "test", "active", func(s *store.Subscription) { s.MandateID = m.ID })
		}
		viewer, op := e.roleClient("viewer"), e.roleClient("operator")
		e.setMode(viewer, "test")
		e.setMode(op, "test")

		// A viewer is forbidden and nothing changes.
		tok := csrfOf(t, e.pageOK(viewer, "/dashboard/subscriptions/sub_01"))
		if r, _ := e.post(viewer, "/dashboard/subscriptions/sub_01/cancel", url.Values{"when": {"now"}, "csrf": {tok}}); r.StatusCode != 403 {
			t.Fatalf("viewer cancel: %d", r.StatusCode)
		}
		if e.subOf("sub_01").Status != "active" {
			t.Fatal("a viewer canceled a subscription")
		}
		// No CSRF token: forbidden.
		if r, _ := e.post(op, "/dashboard/subscriptions/sub_01/cancel", url.Values{"when": {"now"}}); r.StatusCode != 403 {
			t.Errorf("without csrf: %d", r.StatusCode)
		}

		tok = csrfOf(t, e.pageOK(op, "/dashboard/subscriptions/sub_01"))
		if r, _ := e.post(op, "/dashboard/subscriptions/sub_01/cancel", url.Values{"when": {"bogus"}, "csrf": {tok}}); r.StatusCode != 400 {
			t.Errorf("bad choice: %d", r.StatusCode)
		}
		// At period end: stays active, flag set, audited.
		if r, _ := e.post(op, "/dashboard/subscriptions/sub_01/cancel", url.Values{"when": {"period_end"}, "csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("period_end cancel: %d", r.StatusCode)
		}
		if s := e.subOf("sub_01"); s.Status != "active" || !s.CancelAtPeriodEnd {
			t.Errorf("sub_01 = %+v", s)
		}
		// Now: canceled, mandate revoked, audited.
		if r, _ := e.post(op, "/dashboard/subscriptions/sub_02/cancel", url.Values{"when": {"now"}, "csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("cancel now: %d", r.StatusCode)
		}
		if s := e.subOf("sub_02"); s.Status != "canceled" || s.CanceledAt.IsZero() {
			t.Errorf("sub_02 = %+v", s)
		}
		var mm store.Mandate
		_ = e.db.Read(ctx, func(q store.Querier) (err error) { mm, err = store.GetMandate(ctx, q, m.ID); return })
		if mm.Status != "revoked" {
			t.Errorf("mandate = %s", mm.Status)
		}
		if got := e.auditActions(); len(got) != 2 || got[0] != "subscription.cancel" || got[1] != "subscription.cancel" {
			t.Errorf("audit = %v", got)
		}
		body := e.pageOK(op, "/dashboard/subscriptions/sub_02")
		has(t, body, "canceled")
		lacks(t, body, `action="/dashboard/subscriptions/sub_02/cancel"`)
		if r, _ := e.post(op, "/dashboard/subscriptions/sub_nosuch/cancel", url.Values{"when": {"now"}, "csrf": {tok}}); r.StatusCode != 404 {
			t.Errorf("unknown: %d", r.StatusCode)
		}
	})
}

func TestSubscriptionCheckoutLinkIsShownOnceAndAudited(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.enableSubscriptions()
		a := e.seedApp("alpha")
		e.seedCustomer("cus_1", a.ID, "test", "ann@example.com")
		m := e.seedMandate("mdt_1", a.ID, "cus_1", "test", "active")
		pend := e.seedMandate("mdt_2", a.ID, "cus_1", "test", "pending")
		e.seedSub("sub_01", a.ID, "cus_1", "test", "active", func(s *store.Subscription) {
			s.MandateID, s.PendingMandateID, s.RequiresReauthorization = m.ID, pend.ID, true
		})
		e.seedSub("sub_02", a.ID, "cus_1", "test", "active", func(s *store.Subscription) { s.MandateID = m.ID }) // healthy
		op := e.roleClient("operator")
		e.setMode(op, "test")
		tok := csrfOf(t, e.pageOK(op, "/dashboard/subscriptions/sub_01"))

		r, body := e.post(op, "/dashboard/subscriptions/sub_01/checkout-link", url.Values{"csrf": {tok}})
		if r.StatusCode != 200 {
			t.Fatalf("link: %d %s", r.StatusCode, body)
		}
		link := regexpFind(t, `value="(https://pay\.example\.com/s/sub_01\?t=[A-Za-z0-9_-]+)"`, body)
		has(t, body, `data-copy="checkout-link"`, "Any earlier link no longer works")
		token := link[strings.Index(link, "t=")+2:]
		if h := e.subOf("sub_01").CheckoutTokenHash; h == "oldhash" || strings.Contains(h, token) {
			t.Errorf("stored hash = %q (must be a hash of the new token)", h)
		}
		// Reloading the page does not show the link again.
		lacks(t, e.pageOK(op, "/dashboard/subscriptions/sub_01"), token)
		// The audit entry has no token in it.
		var after string
		_ = e.db.Read(ctx, func(q store.Querier) error {
			return q.QueryRowContext(ctx, "SELECT COALESCE(after,'') FROM audit_log WHERE action = 'subscription.checkout_link'").Scan(&after)
		})
		if after == "" || strings.Contains(after, token) {
			t.Errorf("audit after = %q", after)
		}
		// A healthy subscription is not waiting for the customer.
		r, body = e.post(op, "/dashboard/subscriptions/sub_02/checkout-link", url.Values{"csrf": {tok}})
		if r.StatusCode != 409 || !strings.Contains(body, "not waiting for the customer") {
			t.Errorf("healthy sub: %d %s", r.StatusCode, body)
		}
		// Viewers cannot.
		viewer := e.roleClient("viewer")
		e.setMode(viewer, "test")
		vt := csrfOf(t, e.pageOK(viewer, "/dashboard/subscriptions/sub_01"))
		if r, _ := e.post(viewer, "/dashboard/subscriptions/sub_01/checkout-link", url.Values{"csrf": {vt}}); r.StatusCode != 403 {
			t.Errorf("viewer: %d", r.StatusCode)
		}
	})
}

func TestSubscriptionPagesAre404WhenNotEnabled(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		c := e.roleClient("operator")
		if r, _ := e.get(c, "/dashboard/subscriptions"); r.StatusCode != 404 {
			t.Errorf("list: %d", r.StatusCode)
		}
		if r, _ := e.get(c, "/dashboard/subscriptions/sub_x"); r.StatusCode != 404 {
			t.Errorf("detail: %d", r.StatusCode)
		}
	})
}
