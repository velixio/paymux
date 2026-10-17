package dashboard

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

func TestAuditPage(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		day := func(d, h int) time.Time { return time.Date(2026, 10, d, h, 0, 0, 0, time.UTC) }
		err := db.Write(ctx, func(tx store.Tx) error {
			for _, r := range []store.AuditRow{
				{ID: "aud_01", ActorType: "user", ActorID: "usr_a", Action: "refund.create", Target: "tgt_one", At: day(1, 10), IP: "203.0.113.7"},
				{ID: "aud_02", ActorType: "user", ActorID: "usr_b", Action: "setting.update", Target: "tgt_two", At: day(2, 10), After: `{"secret":"[redacted]"}`},
				{ID: "aud_03", ActorType: "api_key", ActorID: "key_1", Action: "refund.create", Target: "tgt_three", At: day(3, 10)},
				{ID: "aud_04", ActorType: "system", Action: "app.create", Target: "tgt_four", At: day(4, 10)},
			} {
				r.OrgID = store.DefaultOrgID
				if err := store.InsertAudit(ctx, tx, r); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}

		// only admins read the audit log
		for _, role := range []string{"viewer", "operator"} {
			c := e.roleClient(role)
			if r, _ := e.get(c, "/dashboard/audit"); r.StatusCode != 403 {
				t.Errorf("%s: %d", role, r.StatusCode)
			}
		}
		admin := e.roleClient("admin")
		body := e.pageOK(admin, "/dashboard/audit")
		has(t, body, "tgt_four", "tgt_three", "system", "app.create", "api_key", "key_1")
		lacks(t, body, "tgt_two", "tgt_one") // page size 2, newest first

		next := regexpFind(t, `href="([^"]*after=aud_03[^"]*)"`, body)
		body = e.pageOK(admin, strings.ReplaceAll(next, "&amp;", "&"))
		has(t, body, "tgt_two", "tgt_one", "203.0.113.7", "[redacted]")
		lacks(t, body, "tgt_four", "tgt_three")

		for name, c := range map[string]struct {
			query string
			want  []string
			not   []string
		}{
			"actor id":      {"?actor=usr_b", []string{"tgt_two"}, []string{"tgt_one", "tgt_three"}},
			"actor type":    {"?actor=system", []string{"tgt_four"}, []string{"tgt_three"}},
			"action prefix": {"?action=refund.", []string{"tgt_three", "tgt_one"}, []string{"tgt_two", "tgt_four"}},
			"date range":    {"?from=2026-10-02&to=2026-10-03", []string{"tgt_three", "tgt_two"}, []string{"tgt_one", "tgt_four"}},
			"combined":      {"?action=refund.&from=2026-10-02", []string{"tgt_three"}, []string{"tgt_one", "tgt_two"}},
		} {
			body := e.pageOK(admin, "/dashboard/audit"+c.query)
			for _, w := range c.want {
				if !strings.Contains(body, w) {
					t.Errorf("%s: missing %s", name, w)
				}
			}
			for _, w := range c.not {
				if strings.Contains(body, w) {
					t.Errorf("%s: unexpected %s", name, w)
				}
			}
		}
		if r, _ := e.get(admin, "/dashboard/audit?from=nope"); r.StatusCode != 400 {
			t.Errorf("bad date: %d", r.StatusCode)
		}
	})
}

// period returns the HTML of one overview section.
func period(t *testing.T, body, id string) string {
	t.Helper()
	return regexpFind(t, `(?s)(<section[^>]*id="period-`+id+`".*?</section>)`, body)
}

func TestOverviewTotalsFromLedger(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		alpha, beta := e.seedApp("alpha"), e.seedApp("beta")
		now := e.clk.Now()
		e.seedPayment("pay_a", alpha.ID, "live", "succeeded", "", 1, now)
		e.seedPayment("pay_b", beta.ID, "live", "succeeded", "", 1, now)
		e.seedPayment("pay_t", alpha.ID, "test", "succeeded", "", 1, now)
		type led struct {
			pay, kind, cur, mode string
			amt                  int64
			ago                  time.Duration
		}
		err := db.Write(ctx, func(tx store.Tx) error {
			for i, l := range []led{
				{"pay_a", "charge", "INR", "live", 10000, time.Hour},
				{"pay_a", "refund", "INR", "live", -2500, 2 * time.Hour},
				{"pay_a", "charge", "INR", "live", 5000, 26 * time.Hour},
				{"pay_a", "charge", "INR", "live", 20000, 10 * 24 * time.Hour},
				{"pay_a", "charge", "INR", "live", 99900, 40 * 24 * time.Hour},
				{"pay_a", "fee", "INR", "live", -300, time.Hour},
				{"pay_b", "charge", "USD", "live", 700, 3 * time.Hour},
				{"pay_t", "charge", "INR", "test", 123456, time.Hour},
			} {
				err := store.InsertLedgerEntry(ctx, tx, store.LedgerEntry{ID: "led_" + string(rune('a'+i)), OrgID: store.DefaultOrgID, Mode: l.mode,
					Kind: l.kind, Amount: l.amt, Currency: l.cur, PaymentID: l.pay, OccurredAt: now.Add(-l.ago)})
				if err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		c := e.roleClient("viewer")
		body := e.pageOK(c, "/dashboard/overview")
		today, week, month := period(t, body, "today"), period(t, body, "7d"), period(t, body, "30d")

		// today (UTC midnight .. now): Alpha 100.00 gross, 25.00 refunds; Beta 7.00
		row := func(sec, app, cur string) string {
			return regexpFind(t, `(?s)(<tr>\s*<td>`+app+`</td>\s*<td>`+cur+`</td>.*?</tr>)`, sec)
		}
		has(t, row(today, "Alpha", "INR"), "100.00 INR", "25.00 INR", "75.00 INR")
		has(t, row(today, "Beta", "USD"), "7.00 USD")
		has(t, row(week, "Alpha", "INR"), "150.00 INR", "25.00 INR", "125.00 INR")
		has(t, row(month, "Alpha", "INR"), "350.00 INR", "25.00 INR", "325.00 INR")
		lacks(t, month, "1,128.99", "999.00", "1234.56") // the 40-day-old charge and the test-mode one
		lacks(t, body, "1234.56")

		// the mode toggle applies
		e.setMode(c, "test")
		body = e.pageOK(c, "/dashboard/overview")
		has(t, period(t, body, "today"), "1234.56 INR")
		lacks(t, body, "100.00 INR")
	})
}

func TestOverviewEmpty(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		body := e.pageOK(e.roleClient("viewer"), "/dashboard/overview")
		for _, id := range []string{"today", "7d", "30d"} {
			has(t, period(t, body, id), "No activity")
		}
	})
}

func TestUserManagementIsAudited(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		admin := e.roleClient("admin")
		tok := csrfOf(t, e.pageOK(admin, "/dashboard/users"))
		e.post(admin, "/dashboard/users", url.Values{"email": {"n@example.com"}, "role": {"viewer"}, "password": {goodPW}, "csrf": {tok}})
		u, _ := db.UserByEmail(ctx, "n@example.com")
		e.post(admin, "/dashboard/users/"+u.ID+"/role", url.Values{"role": {"operator"}, "csrf": {tok}})
		e.post(admin, "/dashboard/users/"+u.ID+"/disable", url.Values{"action": {"disable"}, "csrf": {tok}})
		for _, act := range []string{"user.create", "user.role", "user.disable"} {
			body := e.pageOK(admin, "/dashboard/audit?action="+act)
			has(t, body, act, u.ID)
			lacks(t, body, goodPW)
		}
	})
}
